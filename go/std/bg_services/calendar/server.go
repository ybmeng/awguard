package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DBPath returns the sqlite file the calendar service owns for a given root.
func DBPath(root string) string {
	return filepath.Join(root, Dir, "calendar.db")
}

// SocketPath returns the unix socket the calendar service listens on for a
// given root dir. Living inside the root, it scopes the API to that store.
func SocketPath(root string) string {
	return filepath.Join(root, Dir, "calendar.sock")
}

// Client talks to a running calendar service over its unix socket.
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
	req, err := http.NewRequestWithContext(healthCtx, http.MethodGet, "http://calendar/v1/health", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.Close()
		return nil, fmt.Errorf("calendar: no running service for %s: %w", root, err)
	}
	defer resp.Body.Close()
	var health struct {
		OK bool `json:"ok"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&health); err != nil || !health.OK {
		c.Close()
		return nil, fmt.Errorf("calendar: no running service for %s (bad health response)", root)
	}
	return c, nil
}

// Close releases the client's idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }

// serve exposes the store over a unix socket until ctx ends. It refuses to
// start when another live service already serves this root, and clears a
// stale socket left by a dead process.
func (s *Service) serve(ctx context.Context) error {
	sock := SocketPath(s.root)
	if c, err := Dial(ctx, s.root); err == nil {
		c.Close()
		return fmt.Errorf("calendar: another service is already serving %s", s.root)
	}
	_ = os.Remove(sock)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("calendar: listen %s: %w", sock, err)
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

	s.logger.Printf("calendar: serving API on %s", sock)
	err = srv.Serve(ln)
	<-done
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (s *Service) apiMux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /v1/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	mux.HandleFunc("POST /v1/events", func(w http.ResponseWriter, r *http.Request) {
		var ev Event
		if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
			return
		}
		if err := validateEvent(ev); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		created, err := s.store.Create(ev)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]EventID{"id": created.ID})
	})

	mux.HandleFunc("GET /v1/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseEventID(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		ev, err := s.store.Get(id)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, ev)
	})

	mux.HandleFunc("PATCH /v1/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseEventID(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		// Last-write-wins partial update: nil fields leave the stored value
		// unchanged; the merged event is re-validated as a whole.
		var patch struct {
			Title       *string   `json:"title"`
			Description *string   `json:"description"`
			Location    *string   `json:"location"`
			AllDay      *bool     `json:"allDay"`
			Start       *string   `json:"start"`
			End         *string   `json:"end"`
			TZ          *string   `json:"tz"`
			RRULE       *string   `json:"rrule"`
			EXDATE      *[]string `json:"exdate"`
		}
		if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
			return
		}
		ev, err := s.store.Get(id)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		if patch.Title != nil {
			ev.Title = *patch.Title
		}
		if patch.Description != nil {
			ev.Description = *patch.Description
		}
		if patch.Location != nil {
			ev.Location = *patch.Location
		}
		if patch.AllDay != nil {
			ev.AllDay = *patch.AllDay
		}
		if patch.Start != nil {
			ev.Start = *patch.Start
		}
		if patch.End != nil {
			ev.End = *patch.End
		}
		if patch.TZ != nil {
			ev.TZ = *patch.TZ
		}
		if patch.RRULE != nil {
			ev.RRULE = *patch.RRULE
		}
		if patch.EXDATE != nil {
			ev.EXDATE = *patch.EXDATE
		}
		if err := validateEvent(ev); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		updated, err := s.store.Update(ev)
		if err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, updated)
	})

	mux.HandleFunc("DELETE /v1/events/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseEventID(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := s.store.Delete(id); err != nil {
			writeErr(w, statusFor(err), err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})

	mux.HandleFunc("GET /v1/instances", func(w http.ResponseWriter, r *http.Request) {
		from, to, err := parseWindow(r.URL.Query().Get("from"), r.URL.Query().Get("to"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		events, err := s.store.List()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		instances := []Instance{}
		for _, ev := range events {
			ins, err := Expand(ev, from, to)
			if err != nil {
				// Stored events are validated on write, so this is internal.
				writeErr(w, http.StatusInternalServerError, err)
				return
			}
			instances = append(instances, ins...)
		}
		sort.Slice(instances, func(i, j int) bool {
			if !instances[i].Start.Equal(instances[j].Start) {
				return instances[i].Start.Before(instances[j].Start)
			}
			return instances[i].EventID < instances[j].EventID
		})
		writeJSON(w, http.StatusOK, instances)
	})

	return mux
}

func parseEventID(raw string) (EventID, error) {
	id := EventID(raw)
	if !validEventID(id) {
		return "", fmt.Errorf("invalid event id %q (want evt_ + 26-char ULID)", raw)
	}
	return id, nil
}

// parseWindow reads the required from/to query params as RFC3339 absolute
// times bounding a half-open [from, to) window.
func parseWindow(rawFrom, rawTo string) (from, to time.Time, err error) {
	if rawFrom == "" || rawTo == "" {
		return from, to, errors.New("from and to are required (RFC3339, e.g. 2024-03-01T00:00:00Z)")
	}
	if from, err = time.Parse(time.RFC3339, rawFrom); err != nil {
		return from, to, fmt.Errorf("bad from %q: want RFC3339 with offset", rawFrom)
	}
	if to, err = time.Parse(time.RFC3339, rawTo); err != nil {
		return from, to, fmt.Errorf("bad to %q: want RFC3339 with offset", rawTo)
	}
	if !to.After(from) {
		return from, to, fmt.Errorf("to %q must be after from %q", rawTo, rawFrom)
	}
	return from, to, nil
}

func statusFor(err error) int {
	if errors.Is(err, ErrNotFound) {
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
