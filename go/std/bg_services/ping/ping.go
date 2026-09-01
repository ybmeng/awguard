// Package ping is the std ping service: the only clock in the firing
// pipeline. It POSTs to each configured target on that target's interval and
// logs failures — nothing else. The pinged services are idempotent by
// contract, so there is no backoff logic: the next interval IS the retry.
//
// Extra targets can be dropped into <Root>/ping/targets.json (same three
// fields, interval as a Go duration string); a malformed file is logged and
// ignored so a typo can never take the built-in clocks down.
package ping

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Dir is the subdirectory of Root the service owns.
const Dir = "ping"

// requestTimeout bounds one POST to one target.
const requestTimeout = 30 * time.Second

// Target is one pinged endpoint. URL is either http(s)://... or
// unix://<socket path>/<request path> — the socket path must end in ".sock",
// which is where the request path begins.
type Target struct {
	Name     string
	URL      string
	Interval time.Duration
}

// Config configures a Service.
type Config struct {
	// Root is the local directory the service operates in. The service
	// creates Root/ping if it does not exist and merges extra targets from
	// Root/ping/targets.json when present.
	Root string

	// Targets are the built-in targets, validated at New.
	Targets []Target

	// Logger receives lifecycle and failure lines. Nil means the standard
	// logger.
	Logger *log.Logger
}

// Service pings every target on its interval. It implements
// bgservices.Service.
type Service struct {
	root    string
	targets []Target
	logger  *log.Logger
}

// New validates cfg, creates the ping directory, and returns a ready-to-run
// Service.
func New(cfg Config) (*Service, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("ping: root directory is required")
	}
	root, err := filepath.Abs(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("ping: resolve root: %w", err)
	}
	s := &Service{root: root, targets: cfg.Targets, logger: cfg.Logger}
	if s.logger == nil {
		s.logger = log.Default()
	}
	for _, tgt := range cfg.Targets {
		if err := validateTarget(tgt); err != nil {
			return nil, fmt.Errorf("ping: %w", err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, Dir), 0o755); err != nil {
		return nil, fmt.Errorf("ping: create %s: %w", filepath.Join(root, Dir), err)
	}
	return s, nil
}

func validateTarget(tgt Target) error {
	if tgt.Name == "" {
		return fmt.Errorf("target %+v: name is required", tgt)
	}
	if tgt.Interval <= 0 {
		return fmt.Errorf("target %s: interval must be positive, got %s", tgt.Name, tgt.Interval)
	}
	if _, _, err := splitURL(tgt.URL); err != nil {
		return fmt.Errorf("target %s: %w", tgt.Name, err)
	}
	return nil
}

// splitURL validates a target URL and, for unix targets, splits it into the
// socket path and the request path.
func splitURL(raw string) (sock, path string, err error) {
	if rest, ok := strings.CutPrefix(raw, "unix://"); ok {
		i := strings.Index(rest, ".sock")
		if i < 0 {
			return "", "", fmt.Errorf("unix url %q: socket path must end in .sock (unix://<socket>.sock/<path>)", raw)
		}
		sock, path = rest[:i+len(".sock")], rest[i+len(".sock"):]
		if path == "" {
			path = "/"
		}
		return sock, path, nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", "", fmt.Errorf("url %q must be http(s)://... or unix://<socket>.sock/<path>", raw)
	}
	return "", "", nil
}

// Name implements bgservices.Service.
func (s *Service) Name() string { return "ping" }

// Root returns the absolute root directory the service operates in.
func (s *Service) Root() string { return s.root }

// Run pings every target — built-in plus any from targets.json — on its own
// interval until ctx is canceled. The first ping fires immediately, so the
// system converges right at startup instead of one interval later.
func (s *Service) Run(ctx context.Context) error {
	targets := append([]Target(nil), s.targets...)
	targets = append(targets, s.fileTargets()...)
	if len(targets) == 0 {
		s.logger.Print("ping: no targets configured; idling")
	}

	var wg sync.WaitGroup
	for _, tgt := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.pingLoop(ctx, tgt)
		}()
	}
	wg.Wait()
	return ctx.Err()
}

// fileTargets reads <Root>/ping/targets.json. A missing file is normal; a
// malformed file or entry is logged and skipped, never fatal.
func (s *Service) fileTargets() []Target {
	path := filepath.Join(s.root, Dir, "targets.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		s.logger.Printf("ping: read targets.json: %v (ignored)", err)
		return nil
	}
	var raw []struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		Interval string `json:"interval"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		s.logger.Printf("ping: targets.json does not parse: %v (ignored)", err)
		return nil
	}
	var out []Target
	for _, e := range raw {
		d, err := time.ParseDuration(e.Interval)
		if err != nil {
			s.logger.Printf("ping: targets.json entry %q: interval %q is not a Go duration (skipped)", e.Name, e.Interval)
			continue
		}
		tgt := Target{Name: e.Name, URL: e.URL, Interval: d}
		if err := validateTarget(tgt); err != nil {
			s.logger.Printf("ping: targets.json: %v (skipped)", err)
			continue
		}
		out = append(out, tgt)
	}
	return out
}

// pingLoop POSTs to one target forever: immediately, then every interval.
func (s *Service) pingLoop(ctx context.Context, tgt Target) {
	ticker := time.NewTicker(tgt.Interval)
	defer ticker.Stop()
	for {
		s.ping(ctx, tgt)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ping sends one POST and logs any error or non-2xx answer.
func (s *Service) ping(ctx context.Context, tgt Target) {
	client, reqURL, err := clientFor(tgt)
	if err != nil {
		s.logger.Printf("ping: %s: %v", tgt.Name, err)
		return
	}
	defer client.CloseIdleConnections()

	reqCtx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, reqURL, nil)
	if err != nil {
		s.logger.Printf("ping: %s: %v", tgt.Name, err)
		return
	}
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Printf("ping: %s (%s): %v", tgt.Name, tgt.URL, err)
		}
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		s.logger.Printf("ping: %s (%s): status %d", tgt.Name, tgt.URL, resp.StatusCode)
	}
}

// clientFor builds the HTTP client and request URL for a target — a plain
// client for http(s), a unix-socket dialer for unix://.
func clientFor(tgt Target) (*http.Client, string, error) {
	sock, path, err := splitURL(tgt.URL)
	if err != nil {
		return nil, "", err
	}
	if sock == "" {
		return &http.Client{Timeout: requestTimeout}, tgt.URL, nil
	}
	client := &http.Client{
		Timeout: requestTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", sock)
			},
		},
	}
	return client, "http://" + tgt.Name + path, nil
}

// Verify is a fast self-contained check: a throwaway HTTP target must receive
// a ping within a short synthetic interval. No real roots, no real sockets.
func (s *Service) Verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	hits := make(chan struct{}, 16)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hits <- struct{}{}:
		default:
		}
	})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("ping verify: %w", err)
	}
	go srv.Serve(ln)
	defer srv.Close()

	tmp, err := os.MkdirTemp("", "std_ping_verify_")
	if err != nil {
		return fmt.Errorf("ping verify: %w", err)
	}
	defer os.RemoveAll(tmp)
	probe, err := New(Config{
		Root:    tmp,
		Targets: []Target{{Name: "probe", URL: "http://" + ln.Addr().String(), Interval: 10 * time.Millisecond}},
		Logger:  log.New(os.Stderr, "", 0),
	})
	if err != nil {
		return fmt.Errorf("ping verify: %w", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() { defer close(done); probe.Run(runCtx) }()

	select {
	case <-hits:
	case <-time.After(2 * time.Second):
		return fmt.Errorf("ping verify: target never received a ping")
	}
	cancel()
	<-done
	return nil
}
