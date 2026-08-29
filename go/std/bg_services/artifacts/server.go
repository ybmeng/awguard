package artifacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// SocketPath returns the unix socket the artifacts service listens on for a
// given root dir. Living inside the root, it scopes the API to that store.
func SocketPath(root string) string {
	return filepath.Join(root, ".artifacts.sock")
}

// serve exposes the store over a unix socket until ctx ends. It refuses to
// start when another live service already serves this root, and clears a
// stale socket left by a dead process.
func (s *Service) serve(ctx context.Context) error {
	sock := SocketPath(s.root)
	if c, err := Dial(ctx, s.root); err == nil {
		c.Close()
		return fmt.Errorf("artifacts: another service is already serving %s", s.root)
	}
	_ = os.Remove(sock)

	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("artifacts: listen %s: %w", sock, err)
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

	s.logger.Printf("std_artifacts: serving API on %s", sock)
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

	mux.HandleFunc("POST /v1/insert", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Paths []string `json:"paths"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
			return
		}
		if len(req.Paths) == 0 {
			writeErr(w, http.StatusBadRequest, errors.New("paths is required"))
			return
		}
		for _, p := range req.Paths {
			if !filepath.IsAbs(p) {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("path %q must be absolute (the service resolves nothing)", p))
				return
			}
		}
		id, err := s.store.Insert(r.Context(), req.Paths...)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]ID{"id": id})
	})

	mux.HandleFunc("GET /v1/list", func(w http.ResponseWriter, r *http.Request) {
		statuses, err := s.store.List()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, statuses)
	})

	mux.HandleFunc("GET /v1/status/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		status, err := s.store.Status(id)
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeJSON(w, http.StatusOK, status)
	})

	mux.HandleFunc("GET /v1/open/{id}/{name}", func(w http.ResponseWriter, r *http.Request) {
		id, err := parseID(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		rc, err := s.store.Open(r.Context(), id, r.PathValue("name"))
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		defer rc.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.Copy(w, rc)
	})

	return mux
}

func parseID(s string) (ID, error) {
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid id %q", s)
	}
	return ID(v), nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}
