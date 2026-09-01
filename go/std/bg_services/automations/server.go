package automations

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// DBPath returns the sqlite file the automations service owns for a given root.
func DBPath(root string) string {
	return filepath.Join(root, Dir, "automations.db")
}

// SocketPath returns the unix socket the automations service listens on for a
// given root dir. Living inside the root, it scopes the API to that store.
func SocketPath(root string) string {
	return filepath.Join(root, Dir, "automations.sock")
}

// Client talks to a running automations service over its unix socket.
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
	req, err := http.NewRequestWithContext(healthCtx, http.MethodGet, "http://automations/v1/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("automations: no running service for %s: %w", root, err)
	}
	defer resp.Body.Close()
	var health struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil || !health.OK {
		c.Close()
		return nil, fmt.Errorf("automations: no running service for %s (bad health response)", root)
	}
	return c, nil
}

// Close releases the client's idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// serve exposes the service over its unix socket until ctx ends. The caller
// (Run) has already refused a busy root, so any existing socket here is stale
// — left by a dead process — and safe to clear.
func (s *Service) serve(ctx context.Context) error {
	sock := SocketPath(s.root)
	_ = os.Remove(sock)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("automations: listen %s: %w", sock, err)
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

	s.logger.Printf("automations: serving API on %s", sock)
	err = srv.Serve(ln)
	<-done
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// scheduleJSON echoes a manifest's schedule block through the API.
type scheduleJSON struct {
	RRULE      string `json:"rrule"`
	At         string `json:"at"`
	TZ         string `json:"tz"`
	RetryEvery string `json:"retryEvery"`
	RetryFor   string `json:"retryFor"`
}

func (sc *Schedule) json() *scheduleJSON {
	return &scheduleJSON{RRULE: sc.RRULE, At: sc.At, TZ: sc.TZ, RetryEvery: sc.retryEveryRaw, RetryFor: sc.retryForRaw}
}

// runSummary is a run without its envelope body and stderr tail.
type runSummary struct {
	ID               string  `json:"id"`
	Automation       string  `json:"automation"`
	Trigger          string  `json:"trigger"`
	Started          string  `json:"started"`
	Finished         string  `json:"finished"`
	ExitCode         int     `json:"exitCode"`
	Status           string  `json:"status"`
	FormUsed         int     `json:"formUsed"`
	EscalationReason *string `json:"escalationReason"`
}

func summarize(r Run) runSummary {
	sum := runSummary{
		ID: r.ID, Automation: r.Automation, Trigger: r.Trigger,
		Started: r.Started, Finished: r.Finished, ExitCode: r.ExitCode,
		Status: r.Status, FormUsed: r.FormUsed,
	}
	var env Envelope
	if r.Envelope != "" && json.Unmarshal([]byte(r.Envelope), &env) == nil {
		sum.EscalationReason = env.EscalationReason
	}
	return sum
}

// runJSON is the full run row: the summary plus envelope, stderr tail and
// service-side error.
type runJSON struct {
	runSummary
	Envelope   json.RawMessage `json:"envelope"`
	StderrTail string          `json:"stderrTail"`
	Error      string          `json:"error"`
}

func fullRun(r Run) runJSON {
	out := runJSON{runSummary: summarize(r), StderrTail: r.StderrTail, Error: r.Error}
	if r.Envelope != "" {
		out.Envelope = json.RawMessage(r.Envelope)
	}
	return out
}

// automationView is one registry row as the API serves it. There is no
// nextDue: the botnet calendar owns the future.
type automationView struct {
	Name          string        `json:"name"`
	Goal          string        `json:"goal"`
	Dir           string        `json:"dir"`
	Schedule      *scheduleJSON `json:"schedule"`
	ScheduleError *string       `json:"scheduleError"`
	Freshness     string        `json:"freshness"`
	LastRun       *runSummary   `json:"lastRun"`
	Runs          []runSummary  `json:"runs,omitempty"` // GET /v1/automations/{name} only
}

// view assembles one automation's API row at the given instant.
func (s *Service) view(a Automation, now time.Time) automationView {
	v := automationView{Name: a.Name, Goal: a.Goal, Dir: a.Dir}
	if a.ScheduleError != "" {
		e := a.ScheduleError
		v.ScheduleError = &e
	}
	last, hasLast, err := s.store.Latest(a.Name)
	if err != nil {
		s.logger.Printf("automations: %s: %v", a.Name, err)
	}
	var lastRun *Run
	if hasLast {
		sum := summarize(last)
		v.LastRun = &sum
		lastRun = &last
	}
	var st windowState
	if a.Schedule != nil {
		v.Schedule = a.Schedule.json()
		if st, err = s.windowFromFires(a, now); err != nil {
			s.logger.Printf("automations: %s: %v", a.Name, err)
		}
	}
	v.Freshness = freshness(a, st, lastRun)
	return v
}

// automation looks a name up in the latest discovery snapshot.
func (s *Service) automation(name string) (Automation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.autos {
		if a.Name == name {
			return a, true
		}
	}
	return Automation{}, false
}

func (s *Service) apiMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /v1/automations", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		autos := append([]Automation(nil), s.autos...)
		s.mu.Unlock()
		now := time.Now()
		views := []automationView{} // never null
		for _, a := range autos {
			views = append(views, s.view(a, now))
		}
		writeJSON(w, http.StatusOK, views)
	})

	mux.HandleFunc("GET /v1/automations/{name}", func(w http.ResponseWriter, r *http.Request) {
		a, ok := s.automation(r.PathValue("name"))
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("%w: %q", errUnknownAutomation, r.PathValue("name")))
			return
		}
		v := s.view(a, time.Now())
		runs, err := s.store.List(a.Name, 20)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		v.Runs = []runSummary{}
		for _, run := range runs {
			v.Runs = append(v.Runs, summarize(run))
		}
		writeJSON(w, http.StatusOK, v)
	})

	mux.HandleFunc("GET /v1/automations/{name}/runs", func(w http.ResponseWriter, r *http.Request) {
		a, ok := s.automation(r.PathValue("name"))
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("%w: %q", errUnknownAutomation, r.PathValue("name")))
			return
		}
		limit := 50
		if raw := r.URL.Query().Get("limit"); raw != "" {
			n, err := strconv.Atoi(raw)
			if err != nil || n < 1 {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("limit %q must be a positive integer", raw))
				return
			}
			limit = min(n, 500)
		}
		runs, err := s.store.List(a.Name, limit)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		out := []runSummary{}
		for _, run := range runs {
			out = append(out, summarize(run))
		}
		writeJSON(w, http.StatusOK, out)
	})

	mux.HandleFunc("POST /v1/automations/{name}/run", func(w http.ResponseWriter, r *http.Request) {
		id, err := s.enqueue(r.PathValue("name"), "manual", "", "")
		switch {
		case errors.Is(err, errUnknownAutomation):
			writeErr(w, http.StatusNotFound, err)
		case errors.Is(err, errBusy):
			writeErr(w, http.StatusConflict, err)
		case err != nil:
			writeErr(w, http.StatusInternalServerError, err)
		default:
			writeJSON(w, http.StatusAccepted, map[string]string{"runId": id})
		}
	})

	// The firing pipeline's arbiter endpoint: execcal forwards each active
	// calendar instance here, and the runs table alone decides whether this
	// fire is a no-op (satisfied, paced) or becomes a run. Idempotent by
	// construction — a repeated, late or double fire changes nothing.
	mux.HandleFunc("POST /v1/automations/{name}/fire", func(w http.ResponseWriter, r *http.Request) {
		a, ok := s.automation(r.PathValue("name"))
		if !ok {
			writeErr(w, http.StatusNotFound, fmt.Errorf("%w: %q", errUnknownAutomation, r.PathValue("name")))
			return
		}
		var body struct {
			WindowStart string `json:"windowStart"`
			WindowEnd   string `json:"windowEnd"`
			EventID     string `json:"eventId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("fire body must be JSON {windowStart, windowEnd, eventId}: %v", err))
			return
		}
		ws, err := time.Parse(time.RFC3339, body.WindowStart)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("windowStart %q must be RFC3339: %v", body.WindowStart, err))
			return
		}
		we, err := time.Parse(time.RFC3339, body.WindowEnd)
		if err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("windowEnd %q must be RFC3339: %v", body.WindowEnd, err))
			return
		}
		if !ws.Before(we) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("windowStart %s must precede windowEnd %s", body.WindowStart, body.WindowEnd))
			return
		}
		verdict, err := s.fireVerdict(a, ws, we, time.Now())
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if verdict != "enqueued" {
			writeJSON(w, http.StatusOK, map[string]string{"verdict": verdict})
			return
		}
		id, err := s.enqueue(a.Name, "schedule", fmtTime(ws), fmtTime(we))
		switch {
		case errors.Is(err, errBusy):
			writeErr(w, http.StatusConflict, err)
		case err != nil:
			writeErr(w, http.StatusInternalServerError, err)
		default:
			writeJSON(w, http.StatusOK, map[string]string{"verdict": "enqueued", "runId": id})
		}
	})

	// The ping service's maintenance tick: rescan manifests and ensure the
	// calendar registration. Botnet trouble is logged and retried on the next
	// tick — the pipeline's clock must never see it as a failure.
	mux.HandleFunc("POST /tick", func(w http.ResponseWriter, r *http.Request) {
		if err := s.tick(); err != nil {
			s.logger.Printf("automations: tick: %v", err)
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /v1/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !validRunID(id) {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("invalid run id %q (want run_ + 26-char ULID)", id))
			return
		}
		run, err := s.store.Get(id)
		if errors.Is(err, ErrRunNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, fullRun(run))
	})

	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
