// Package execcal is the stateless bridge of the firing pipeline: on every
// POST /tick it asks the botnet calendar which executable instances are
// active right now (GET /v1/fireable) and forwards each one to the
// automations service (POST /v1/automations/{name}/fire). It holds no state
// and no database — the calendar owns the schedule, the automations service
// owns idempotence — so a repeated, late, or double tick is always safe.
//
// The service owns <Root>/execcal/ and serves its one-route API on
// <Root>/execcal/execcal.sock.
package execcal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Dir is the subdirectory of Root the service owns.
const Dir = "execcal"

// tickTimeout bounds one whole tick: the fireable fetch plus every fire.
const tickTimeout = 30 * time.Second

// Config configures a Service.
type Config struct {
	// Root is the local directory the service operates in. The service
	// creates Root/execcal if it does not exist.
	Root string

	// BotnetAddr is the botnet HTTP listen address (host:port) the fireable
	// query goes to. Validated at use, not at New — stdd verify builds the
	// roster with no botnet running.
	BotnetAddr string

	// AutomationsSocket is the unix socket of the automations service the
	// fires are forwarded to.
	AutomationsSocket string

	// Logger receives lifecycle and per-tick lines. Nil means the standard
	// logger.
	Logger *log.Logger
}

// Service is the execcal bridge. It implements bgservices.Service.
type Service struct {
	root       string
	botnetAddr string
	autoSock   string
	logger     *log.Logger
}

// SocketPath returns the unix socket the execcal service listens on for a
// given root dir.
func SocketPath(root string) string {
	return filepath.Join(root, Dir, "execcal.sock")
}

// New validates cfg, creates the execcal directory, and returns a
// ready-to-run Service.
func New(cfg Config) (*Service, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("execcal: root directory is required")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("execcal: resolve root: %w", err)
	}
	s := &Service{root: root, botnetAddr: cfg.BotnetAddr, autoSock: cfg.AutomationsSocket, logger: cfg.Logger}
	if s.logger == nil {
		s.logger = log.Default()
	}
	if err := os.MkdirAll(filepath.Join(root, Dir), 0o755); err != nil {
		return nil, fmt.Errorf("execcal: create %s: %w", filepath.Join(root, Dir), err)
	}
	return s, nil
}

// Name implements bgservices.Service.
func (s *Service) Name() string { return "execcal" }

// Root returns the absolute root directory the service operates in.
func (s *Service) Root() string { return s.root }

// Client talks to a running execcal service over its unix socket.
type Client struct {
	http *http.Client
}

// Dial connects to the service serving root and confirms it is alive with a
// health check. It fails fast when no service is running.
func Dial(ctx context.Context, root string) (*Client, error) {
	sock := SocketPath(root)
	c := &Client{http: &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", sock)
			},
		},
	}}
	healthCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(healthCtx, http.MethodGet, "http://execcal/v1/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("execcal: no running service for %s: %w", root, err)
	}
	defer resp.Body.Close()
	var health struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil || !health.OK {
		c.Close()
		return nil, fmt.Errorf("execcal: no running service for %s (bad health response)", root)
	}
	return c, nil
}

// Close releases the client's idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// Run serves the execcal API on the root's unix socket until ctx is canceled.
// It refuses to start when another live service already serves this root.
func (s *Service) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if c, err := Dial(ctx, s.root); err == nil {
		c.Close()
		return fmt.Errorf("execcal: another service is already serving %s", s.root)
	}

	sock := SocketPath(s.root)
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("execcal: listen %s: %w", sock, err)
	}
	srv := &http.Server{Handler: s.apiMux(), BaseContext: func(net.Listener) context.Context { return ctx }}

	done := make(chan struct{})
	go func() {
		defer close(done)
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		_ = os.Remove(sock)
	}()

	s.logger.Printf("execcal: serving API on %s", sock)
	err = srv.Serve(ln)
	<-done
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// fireable is one row from the botnet's /v1/fireable answer. The window
// bounds pass through to the automations service verbatim.
type fireable struct {
	Automation  string `json:"automation"`
	EventID     string `json:"eventId"`
	WindowStart string `json:"windowStart"`
	WindowEnd   string `json:"windowEnd"`
}

// firedEntry and skippedEntry make up the /tick response.
type firedEntry struct {
	Automation string `json:"automation"`
	RunID      string `json:"runId,omitempty"`
}

type skippedEntry struct {
	Automation string `json:"automation"`
	Reason     string `json:"reason"`
}

func (s *Service) apiMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /tick", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), tickTimeout)
		defer cancel()
		rows, err := s.fetchFireable(ctx)
		if err != nil {
			s.logger.Printf("execcal: %v", err)
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
			return
		}
		fired, skipped := []firedEntry{}, []skippedEntry{} // never null
		for _, row := range rows {
			runID, reason := s.fire(ctx, row)
			if reason == "" {
				fired = append(fired, firedEntry{Automation: row.Automation, RunID: runID})
				continue
			}
			skipped = append(skipped, skippedEntry{Automation: row.Automation, Reason: reason})
		}
		writeJSON(w, http.StatusOK, map[string]any{"fired": fired, "skipped": skipped})
	})
	return mux
}

// fetchFireable asks the botnet which instances are fireable right now.
func (s *Service) fetchFireable(ctx context.Context) ([]fireable, error) {
	if s.botnetAddr == "" {
		return nil, fmt.Errorf("fireable fetch: no botnet address configured (stdd's -botnet-addr)")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.botnetAddr+"/v1/fireable", nil)
	if err != nil {
		return nil, fmt.Errorf("fireable fetch: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fireable fetch from %s: %w", s.botnetAddr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("fireable fetch from %s: status %d: %s", s.botnetAddr, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var rows []fireable
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, fmt.Errorf("fireable fetch: decode: %w", err)
	}
	return rows, nil
}

// fire forwards one fireable row to the automations service. It returns the
// run id and "" when the fire was enqueued, or ("", reason) when it was
// skipped — the automations no-op verdict, or an error string. A failing fire
// never aborts the tick; the caller records the reason and moves on.
func (s *Service) fire(ctx context.Context, row fireable) (runID, reason string) {
	body, _ := json.Marshal(map[string]string{
		"windowStart": row.WindowStart, "windowEnd": row.WindowEnd, "eventId": row.EventID,
	})
	url := "http://automations/v1/automations/" + row.Automation + "/fire"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return "", err.Error()
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", s.autoSock)
		},
	}}
	defer client.CloseIdleConnections()
	resp, err := client.Do(req)
	if err != nil {
		s.logger.Printf("execcal: fire %s: %v", row.Automation, err)
		return "", fmt.Sprintf("fire failed: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("status %d", resp.StatusCode)
		}
		s.logger.Printf("execcal: fire %s: %d %s", row.Automation, resp.StatusCode, e.Error)
		return "", e.Error
	}
	var verdict struct {
		Verdict string `json:"verdict"`
		RunID   string `json:"runId"`
	}
	if err := json.Unmarshal(raw, &verdict); err != nil {
		return "", fmt.Sprintf("fire answered unparseable body: %v", err)
	}
	if verdict.Verdict == "enqueued" {
		return verdict.RunID, ""
	}
	return "", verdict.Verdict
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// Verify is a fast, self-contained end-to-end check: a fake botnet (loopback
// HTTP) advertises two fireable rows, a fake automations server (unix socket
// in a throwaway dir) answers one enqueued and one satisfied, and one tick
// through a probe service must pass both through exactly — including a fire
// whose failure must not abort the rest. No real roots, no real services.
func (s *Service) Verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("/tmp", "std_execcal_verify_")
	if err != nil {
		return fmt.Errorf("execcal verify: %w", err)
	}
	defer os.RemoveAll(tmp)

	// Fake botnet on loopback.
	botLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("execcal verify: %w", err)
	}
	botSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[
			{"automation":"a","eventId":"evt_1","windowStart":"2026-01-01T00:00:00Z","windowEnd":"2026-01-02T00:00:00Z"},
			{"automation":"down","eventId":"evt_2","windowStart":"2026-01-01T00:00:00Z","windowEnd":"2026-01-02T00:00:00Z"},
			{"automation":"b","eventId":"evt_3","windowStart":"2026-01-01T00:00:00Z","windowEnd":"2026-01-02T00:00:00Z"}
		]`))
	})}
	go botSrv.Serve(botLn)
	defer botSrv.Close()

	// Fake automations on a unix socket.
	autoSock := filepath.Join(tmp, "automations.sock")
	autoLn, err := net.Listen("unix", autoSock)
	if err != nil {
		return fmt.Errorf("execcal verify: %w", err)
	}
	autoSrv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/automations/a/fire":
			_, _ = w.Write([]byte(`{"verdict":"enqueued","runId":"run_VERIFY"}`))
		case "/v1/automations/down/fire":
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
		default:
			_, _ = w.Write([]byte(`{"verdict":"satisfied"}`))
		}
	})}
	go autoSrv.Serve(autoLn)
	defer autoSrv.Close()

	probe, err := New(Config{
		Root:              filepath.Join(tmp, "root"),
		BotnetAddr:        botLn.Addr().String(),
		AutomationsSocket: autoSock,
		Logger:            log.New(io.Discard, "", 0),
	})
	if err != nil {
		return fmt.Errorf("execcal verify: %w", err)
	}
	rows, err := probe.fetchFireable(ctx)
	if err != nil {
		return fmt.Errorf("execcal verify: %w", err)
	}
	if len(rows) != 3 || rows[0].Automation != "a" || rows[0].WindowEnd != "2026-01-02T00:00:00Z" {
		return fmt.Errorf("execcal verify: fireable rows did not pass through: %+v", rows)
	}
	var fired, skipped int
	for _, row := range rows {
		runID, reason := probe.fire(ctx, row)
		switch row.Automation {
		case "a":
			if runID != "run_VERIFY" || reason != "" {
				return fmt.Errorf("execcal verify: enqueued fire = (%q, %q), want the run id through", runID, reason)
			}
			fired++
		case "down":
			if reason == "" {
				return fmt.Errorf("execcal verify: failing fire reported no reason")
			}
			skipped++
		case "b":
			if reason != "satisfied" {
				return fmt.Errorf("execcal verify: no-op verdict = %q, want satisfied", reason)
			}
			skipped++
		}
	}
	if fired != 1 || skipped != 2 {
		return fmt.Errorf("execcal verify: fired %d skipped %d, want 1 and 2 (a failing fire must not abort the rest)", fired, skipped)
	}
	return nil
}
