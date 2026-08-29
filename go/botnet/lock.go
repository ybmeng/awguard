package botnet

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Single-writer enforcement. Two processes over one database is not a
// concurrency problem but a correctness one: Open's startup sweep fails every
// awaiting turn, so a second opener destroys the first's in-flight work even
// when it then exits immediately (say, losing the race for the port). The lock
// makes the sweep unreachable while another process holds the database.

// ErrLocked is returned by Open when another process holds the database's
// single-writer lock.
var ErrLocked = errors.New("botnet: database is locked by another process")

// acquireLock takes the single-writer lock for the database at path: an
// exclusive, non-blocking flock(2) on a sidecar file next to it, with the
// holder's PID inside so a refusal names the other process. The lock is held
// by the kernel against the open file, so a dying holder releases it
// implicitly — there is no stale-lock state to recover from, which matters
// because the failure this guards against involves a process exiting
// unexpectedly.
//
// The lock is advisory: it binds only openers that take it, which is every
// in-tree binary, because they all come through Open. A ":memory:" database is
// process-private and takes no lock.
func acquireLock(dbPath string) (*os.File, error) {
	if dbPath == ":memory:" || strings.Contains(dbPath, "mode=memory") {
		return nil, nil
	}
	lockPath := dbPath + ".lock"
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %q: %w", lockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := lockHolder(f)
		f.Close()
		if holder != "" {
			return nil, fmt.Errorf("%w: another botnetd (pid %s) is using %s", ErrLocked, holder, dbPath)
		}
		return nil, fmt.Errorf("%w: another botnetd is using %s", ErrLocked, dbPath)
	}
	// Record the holder for the refusal message above. Truncate first so a
	// short PID cannot leave trailing digits of a longer former one.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())), 0)
	return f, nil
}

// lockHolder reads the PID the current holder wrote; best-effort, "" if the
// file is empty or unreadable.
func lockHolder(f *os.File) string {
	buf := make([]byte, 32)
	n, _ := f.ReadAt(buf, 0)
	return strings.TrimSpace(string(buf[:n]))
}

// releaseLock drops the flock and closes the sidecar. The file itself stays in
// place: unlinking it would let a racing opener lock a fresh inode while
// another process still holds the old one, splitting the lock in two.
func releaseLock(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
