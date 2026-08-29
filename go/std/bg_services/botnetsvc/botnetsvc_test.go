package botnetsvc

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// newTestService builds a service whose every path is inside t.TempDir(), so a
// test can never reach the developer's real ~/.botnet.
func newTestService(t *testing.T, addr string) *Service {
	t.Helper()
	dir := t.TempDir()
	svc, err := New(Config{
		Addr:    addr,
		DBPath:  filepath.Join(dir, "net.db"),
		KeyPath: filepath.Join(dir, "config", "openrouter.txt"),
		Logger:  log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

func TestVerifyWithoutKey(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	svc := newTestService(t, "127.0.0.1:0")

	if err := svc.Verify(context.Background()); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if _, err := os.Stat(svc.dbPath); !os.IsNotExist(err) {
		t.Fatalf("Verify touched the configured db %s (stat err: %v)", svc.dbPath, err)
	}
	if _, err := os.Stat(filepath.Dir(svc.keyPath)); !os.IsNotExist(err) {
		t.Fatalf("Verify created the configured config dir %s", filepath.Dir(svc.keyPath))
	}
}

func TestRunAddressInUse(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer held.Close()

	addr := held.Addr().String()
	svc := newTestService(t, addr)

	err = svc.Run(context.Background())
	if err == nil {
		t.Fatal("Run on a busy address returned nil, want an error")
	}
	if !strings.Contains(err.Error(), addr) {
		t.Fatalf("error %q does not name the address %s", err, addr)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("error %q lost its EADDRINUSE cause", err)
	}
	if _, err := os.Stat(svc.dbPath); !os.IsNotExist(err) {
		t.Fatalf("Run opened the database %s despite losing the port (stat err: %v)", svc.dbPath, err)
	}
}

func TestNewRejectsMalformedAddr(t *testing.T) {
	if _, err := New(Config{Addr: "definitely-not-an-address", Logger: log.New(io.Discard, "", 0)}); err == nil {
		t.Fatal("New accepted a malformed address, want an error")
	}
}

func TestRunServesAndShutsDown(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	svc := newTestService(t, "127.0.0.1:0")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- svc.Run(ctx) }()

	var addr string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if addr = svc.Addr(); addr != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("service never reported a bound address")
	}

	resp, err := http.Get("http://" + addr + "/v1/health")
	if err != nil {
		t.Fatalf("GET /v1/health: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		t.Fatalf("GET /v1/health returned %d, want 200", resp.StatusCode)
	}
	// Leave the connection idle before cancelling, so the shutdown drain has
	// nothing in flight to wait on.
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatalf("read /v1/health body: %v", err)
	}
	resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run after cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancellation")
	}
}

func TestDefaultsMatchBotnetd(t *testing.T) {
	t.Setenv("BOTNET_ADDR", "")
	t.Setenv("BOTNET_DB", "")
	if got := DefaultAddr(); got != "127.0.0.1:8730" {
		t.Errorf("DefaultAddr() = %q, want 127.0.0.1:8730", got)
	}
	if got, want := DefaultDBPath(), filepath.Join(home(), ".botnet", "net.db"); got != want {
		t.Errorf("DefaultDBPath() = %q, want %q", got, want)
	}
	if got, want := DefaultKeyPath(), filepath.Join(home(), ".config", "botnet", "openrouter.txt"); got != want {
		t.Errorf("DefaultKeyPath() = %q, want %q", got, want)
	}

	t.Setenv("BOTNET_ADDR", "127.0.0.1:9999")
	t.Setenv("BOTNET_DB", filepath.Join(t.TempDir(), "env.db"))
	if got := DefaultAddr(); got != "127.0.0.1:9999" {
		t.Errorf("DefaultAddr() ignored BOTNET_ADDR: %q", got)
	}
	if got, want := DefaultDBPath(), os.Getenv("BOTNET_DB"); got != want {
		t.Errorf("DefaultDBPath() = %q, want %q from BOTNET_DB", got, want)
	}

	svc, err := New(Config{Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if svc.addr != DefaultAddr() || svc.dbPath != DefaultDBPath() || svc.keyPath != DefaultKeyPath() {
		t.Errorf("New did not resolve empty fields to the defaults: %+v", svc)
	}
	if svc.Name() != "botnet" {
		t.Errorf("Name() = %q, want botnet", svc.Name())
	}
}
