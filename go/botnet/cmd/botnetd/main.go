// Command botnetd is the PrivateBotNet server: it owns all state (bots,
// messages) in one SQLite file and makes the OpenRouter calls. The UI is a
// thin client that talks to it over HTTP on localhost.
//
// Config (env):
//
//	BOTNET_DB    path to the SQLite file (default ~/.botnet/net.db)
//	BOTNET_ADDR  listen address        (default 127.0.0.1:8730)
//	OPENROUTER_API_KEY  the key; if unset, falls back to ~/.config/botnet/openrouter.txt
package main

import (
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"stdtools/go/botnet"
)

func main() {
	log.SetFlags(0)

	// Bind before opening the store, matching botnetsvc: a botnetd that loses
	// the race for the port must exit without having touched — and swept — the
	// running server's database. The flock in Open enforces the same rule; this
	// keeps the failure reading "address already in use" instead of "database
	// locked" when the port is the real contention.
	addr := env("BOTNET_ADDR", "127.0.0.1:8730")
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("botnetd: %v", err)
	}

	dbPath := env("BOTNET_DB", filepath.Join(home(), ".botnet", "net.db"))
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		log.Fatalf("botnetd: create db dir: %v", err)
	}
	store, err := botnet.Open(dbPath)
	if err != nil {
		log.Fatalf("botnetd: open store: %v", err)
	}
	defer store.Close()

	keyPath := filepath.Join(home(), ".config", "botnet", "openrouter.txt")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		log.Fatalf("botnetd: create config dir: %v", err)
	}
	srv, err := botnet.NewServer(store, botnet.NewOpenRouter(apiKey()))
	if err != nil {
		log.Fatalf("botnetd: %v", err)
	}
	srv.ConfigureKeyPersistence(keyPath)

	// Client-side web search: offer the model our own web_search tool when any
	// provider key resolves; otherwise the server keeps falling back to
	// OpenRouter's fused server tool. Built from the environment here, never in
	// NewServer, so tests stay deterministic.
	search := botnet.NewRouterFromEnv()
	srv.ConfigureSearch(search)
	if search.Available() {
		log.Printf("botnetd: web search backends: %s (active: first)", strings.Join(search.Names(), ", "))
	} else {
		log.Printf("botnetd: no web search backend configured; using OpenRouter's server tool")
	}

	log.Printf("botnetd: serving on http://%s  (db: %s)", addr, dbPath)
	if err := http.Serve(ln, srv.Handler()); err != nil {
		log.Fatalf("botnetd: %v", err)
	}
}

func apiKey() string {
	if k := os.Getenv("OPENROUTER_API_KEY"); k != "" {
		return k
	}
	data, err := os.ReadFile(filepath.Join(home(), ".config", "botnet", "openrouter.txt"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func home() string {
	h, err := os.UserHomeDir()
	if err != nil {
		return "."
	}
	return h
}
