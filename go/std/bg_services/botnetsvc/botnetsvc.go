// Package botnetsvc is the botnet background service: it hosts the
// PrivateBotNet HTTP server in-process under stdd, so the bot library, its
// SQLite store and the UI it serves are up whenever the mac service is.
//
// It wraps the exported stdtools/go/botnet API and adds nothing to it.
// go/botnet/cmd/botnetd remains the standalone dev entry point; the two agree
// on the same address, database and key-file conventions, so only one of them
// can hold the port at a time.
package botnetsvc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"stdtools/go/botnet"
)

// shutdownTimeout bounds the graceful drain after ctx is canceled.
const shutdownTimeout = 5 * time.Second

// DefaultAddr is the listen address: $BOTNET_ADDR, else 127.0.0.1:8730.
func DefaultAddr() string {
	if v := os.Getenv("BOTNET_ADDR"); v != "" {
		return v
	}
	return "127.0.0.1:8730"
}

// ResolveAddr applies the "empty means the default" rule New applies, for the
// callers that need to KNOW the address rather than just listen on it — the
// execcal bridge and ping's projects clock both build a URL from it, and an
// unresolved empty string would send them somewhere nothing answers.
func ResolveAddr(addr string) string {
	if addr == "" {
		return DefaultAddr()
	}
	return addr
}

// DefaultDBPath is the SQLite path: $BOTNET_DB, else ~/.botnet/net.db.
func DefaultDBPath() string {
	if v := os.Getenv("BOTNET_DB"); v != "" {
		return v
	}
	return filepath.Join(home(), ".botnet", "net.db")
}

// DefaultKeyPath is where the OpenRouter key is read from and persisted to:
// ~/.config/botnet/openrouter.txt.
func DefaultKeyPath() string {
	return filepath.Join(home(), ".config", "botnet", "openrouter.txt")
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}

// Config configures a Service.
type Config struct {
	// Addr is the TCP listen address. Empty means DefaultAddr().
	Addr string

	// DBPath is the SQLite file. Empty means DefaultDBPath().
	DBPath string

	// KeyPath is the OpenRouter key file, read at startup and written by the
	// server's config endpoint. Empty means DefaultKeyPath().
	KeyPath string

	// Automations, when non-nil, is mounted into the botnet server so the
	// app's one backend also answers the automations read/run routes. stdd
	// passes the automations service's Handler(); standalone botnetd mounts
	// nothing and the routes are absent.
	Automations http.Handler

	// Logger receives startup lines. Nil means the standard logger.
	Logger *log.Logger
}

// Service is the botnet HTTP server hosted under stdd. It implements
// bgservices.Service.
type Service struct {
	addr        string
	dbPath      string
	keyPath     string
	automations http.Handler
	logger      *log.Logger

	mu    sync.Mutex
	bound string
}

// New resolves cfg against the defaults and returns a ready-to-run Service. It
// performs no I/O: nothing is created or opened until Run, so `stdd verify`
// never touches the real ~/.botnet.
func New(cfg Config) (*Service, error) {
	s := &Service{
		addr:        cfg.Addr,
		dbPath:      cfg.DBPath,
		keyPath:     cfg.KeyPath,
		automations: cfg.Automations,
		logger:      cfg.Logger,
	}
	if s.addr == "" {
		s.addr = DefaultAddr()
	}
	if s.dbPath == "" {
		s.dbPath = DefaultDBPath()
	}
	if s.keyPath == "" {
		s.keyPath = DefaultKeyPath()
	}
	if s.logger == nil {
		s.logger = log.Default()
	}
	if _, _, err := net.SplitHostPort(s.addr); err != nil {
		return nil, fmt.Errorf("botnet: %q is not a host:port listen address: %w", s.addr, err)
	}
	return s, nil
}

// Name implements bgservices.Service.
func (s *Service) Name() string { return "botnet" }

// Addr returns the address the server is currently bound to, and empty when it
// is not serving. It lets a caller drive a real request against a service
// configured with port 0, which is how the tests exercise the running server.
func (s *Service) Addr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bound
}

func (s *Service) setBound(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bound = addr
}

// Run serves the botnet API until ctx is canceled. The port is claimed first,
// so a service that loses the race for it returns without having opened — and
// migrated — the user's database. Cancellation drains the server gracefully.
func (s *Service) Run(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.addr)
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return fmt.Errorf("botnet: %s is already held by another process (a hand-run botnetd?); stop it or move this service with -botnet-addr: %w", s.addr, err)
		}
		return fmt.Errorf("botnet: listen on %s: %w", s.addr, err)
	}
	// Serve closes ln itself; this only covers the paths that return first.
	defer func() { _ = ln.Close() }()

	if err := os.MkdirAll(filepath.Dir(s.dbPath), 0o700); err != nil {
		return fmt.Errorf("botnet: create db dir: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.keyPath), 0o700); err != nil {
		return fmt.Errorf("botnet: create config dir: %w", err)
	}

	store, err := botnet.Open(s.dbPath)
	if err != nil {
		return fmt.Errorf("botnet: open store %s: %w", s.dbPath, err)
	}
	defer store.Close()

	key := s.apiKey()
	llm := botnet.NewOpenRouter(key)
	if key == "" {
		s.logger.Printf("botnet: no OpenRouter key (%s); the server and UI run, chat fails until one is set via POST /v1/config", s.keyPath)
	}

	srv, err := botnet.NewServer(store, llm)
	if err != nil {
		return fmt.Errorf("botnet: %w", err)
	}
	srv.ConfigureKeyPersistence(s.keyPath)
	// nil is the unmounted state: Handler() then leaves the routes absent.
	srv.MountAutomations(s.automations)

	// Client-side web search: offer the model our own web_search tool when a
	// backend key resolves (SEARCH_BACKEND / EXA_API_KEY / BRAVE_API_KEY /
	// TAVILY_API_KEY), else fall back to OpenRouter's fused server tool. Built
	// from the environment here, the same way botnetd does it — this is the
	// production path, so without this the router would never activate under stdd.
	search := botnet.NewRouterFromEnv()
	srv.ConfigureSearch(search)
	if search.Available() {
		s.logger.Printf("botnet: web search backends: %s", strings.Join(search.Names(), ", "))
	} else {
		s.logger.Printf("botnet: no web search backend configured; using OpenRouter's server tool")
	}

	httpSrv := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	serveErr := make(chan error, 1)
	s.setBound(ln.Addr().String())
	defer s.setBound("")
	go func() { serveErr <- httpSrv.Serve(ln) }()

	s.logger.Printf("botnet: serving on http://%s  (db: %s)", s.Addr(), s.dbPath)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		err := httpSrv.Shutdown(shutdownCtx)
		<-serveErr
		return err
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// apiKey resolves the OpenRouter key the way botnetd does: the environment
// first, then the key file. Absent is a supported state, not an error.
func (s *Service) apiKey() string {
	if k := os.Getenv("OPENROUTER_API_KEY"); k != "" {
		return k
	}
	data, err := os.ReadFile(s.keyPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// Verify is a fast self-check: it builds a whole server — store, no-key LLM,
// routed handler — over a throwaway database and serves one real request
// through it. It never touches the configured DBPath or KeyPath, makes no
// network calls, and needs no OpenRouter key.
func (s *Service) Verify(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	tmp, err := os.MkdirTemp("", "botnet_verify_")
	if err != nil {
		return fmt.Errorf("botnet verify: temp dir: %w", err)
	}
	defer os.RemoveAll(tmp)

	store, err := botnet.Open(filepath.Join(tmp, "verify.db"))
	if err != nil {
		return fmt.Errorf("botnet verify: open store: %w", err)
	}
	defer store.Close()

	srv, err := botnet.NewServer(store, botnet.NewOpenRouter(""))
	if err != nil {
		return fmt.Errorf("botnet verify: build server: %w", err)
	}

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/bots", nil))
	if rec.Code != http.StatusOK {
		return fmt.Errorf("botnet verify: GET /v1/bots returned %d, want 200 (%s)", rec.Code, strings.TrimSpace(rec.Body.String()))
	}
	var bots []json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &bots); err != nil {
		return fmt.Errorf("botnet verify: GET /v1/bots body is not a JSON array: %w", err)
	}
	return nil
}
