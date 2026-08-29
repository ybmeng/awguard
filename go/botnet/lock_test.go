package botnet

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	modelselector "stdtools/go/lib/modelSelector"
)

// TestSecondOpenIsRefusedWhileLocked is the single-writer guarantee: a second
// Open of the same file fails with ErrLocked naming the holder, the first
// store keeps working, and the lock releases on Close so the file can be
// opened again.
func TestSecondOpenIsRefusedWhileLocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "net.db")
	first, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}

	second, err := Open(path)
	if err == nil {
		second.Close()
		t.Fatal("second Open succeeded; want single-writer refusal")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("second Open error = %v, want ErrLocked", err)
	}
	// The refusal names the holder — both PIDs are this test process's.
	if pid := strconv.Itoa(os.Getpid()); !strings.Contains(err.Error(), pid) {
		t.Errorf("refusal %q does not name holder pid %s", err, pid)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("refusal %q does not name the database %s", err, path)
	}

	// The refused Open must leave the holder unharmed: still writable, and
	// crucially not swept — an awaiting turn survives.
	net, err := first.CreateNet("held")
	if err != nil {
		t.Fatalf("first store broken after refused second open: %v", err)
	}
	bot, err := first.CreateBot(net.ID, "Ada", "prompt", modelselector.DeepSeekV4.ID)
	if err != nil {
		t.Fatalf("create bot: %v", err)
	}
	msg, err := first.AppendMessage(bot.ID, "user", "in flight", StatusAwaiting)
	if err != nil {
		t.Fatalf("append awaiting: %v", err)
	}
	if _, err := Open(path); !errors.Is(err, ErrLocked) {
		t.Fatalf("open with a turn in flight = %v, want ErrLocked", err)
	}
	got, err := first.GetMessage(msg.ID)
	if err != nil {
		t.Fatalf("get message: %v", err)
	}
	if got.Status != StatusAwaiting {
		t.Fatalf("in-flight turn status = %q after refused open, want %q", got.Status, StatusAwaiting)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	third, err := Open(path)
	if err != nil {
		t.Fatalf("open after Close released the lock: %v", err)
	}
	third.Close()
}

// TestMemoryStoreTakesNoLock: ":memory:" databases are process-private, so two
// may coexist and no sidecar appears anywhere.
func TestMemoryStoreTakesNoLock(t *testing.T) {
	a, err := Open(":memory:")
	if err != nil {
		t.Fatalf("first :memory: open: %v", err)
	}
	defer a.Close()
	b, err := Open(":memory:")
	if err != nil {
		t.Fatalf("second :memory: open: %v", err)
	}
	defer b.Close()
}

// TestLockSidecarNamesThePid: the sidecar holds the owner's PID while open, so
// an operator can see who to stop without waiting for a refusal.
func TestLockSidecarNamesThePid(t *testing.T) {
	path := filepath.Join(t.TempDir(), "net.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	data, err := os.ReadFile(path + ".lock")
	if err != nil {
		t.Fatalf("read sidecar: %v", err)
	}
	if got, want := strings.TrimSpace(string(data)), strconv.Itoa(os.Getpid()); got != want {
		t.Fatalf("sidecar holds %q, want pid %s", got, want)
	}
}
