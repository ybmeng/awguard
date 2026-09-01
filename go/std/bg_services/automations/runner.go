package automations

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Sentinel errors the API maps to status codes.
var (
	errUnknownAutomation = errors.New("unknown automation")
	errBusy              = errors.New("a run is already in flight or queued for this automation")
)

// stderrTailCap is how much stderr is kept per run (the last 8KB); stdoutTailCap
// bounds the stdout capture the envelope is parsed out of — the envelope is the
// LAST non-empty line, so a tail is all that is ever needed.
const (
	stderrTailCap = 8 << 10
	stdoutTailCap = 256 << 10
)

// job is one unit of work for the serial runner: the run row already exists
// (status queued); the Automation is snapshotted at enqueue time so a manifest
// edit mid-queue cannot change what was asked for.
type job struct {
	id string
	a  Automation
}

// enqueue records a queued run and hands it to the serial runner. Fire-
// triggered runs pass their window bounds (fixed-width RFC3339 UTC); manual
// runs pass "". It returns errUnknownAutomation for a name not in the current
// registry and errBusy when that automation already has a run in flight or
// queued.
func (s *Service) enqueue(name, trigger, windowStart, windowEnd string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var a Automation
	found := false
	for _, cand := range s.autos {
		if cand.Name == name {
			a, found = cand, true
			break
		}
	}
	if !found {
		return "", fmt.Errorf("%w: %q (GET /v1/automations lists the registry)", errUnknownAutomation, name)
	}
	if s.pending[name] {
		return "", errBusy
	}
	id := newID("run_")
	run := Run{
		ID: id, Automation: name, Trigger: trigger,
		Started: fmtTime(time.Now()), // provisional; MarkStarted overwrites
		Status:  StatusQueued, ExitCode: -1,
		WindowStart: windowStart, WindowEnd: windowEnd,
	}
	if err := s.store.Insert(run); err != nil {
		return "", err
	}
	select {
	case s.queue <- job{id: id, a: a}:
		s.pending[name] = true
		return id, nil
	default:
		_ = s.store.Finish(id, fmtTime(time.Now()), -1, StatusError, 0, "", "", "runner queue full")
		return "", errors.New("runner queue is full")
	}
}

// worker is the single serial runner: one subprocess at a time across ALL
// automations, so repo-tree writes stay single-writer.
func (s *Service) worker(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case j := <-s.queue:
			s.execute(ctx, j)
		}
	}
}

// execute runs one job to completion and records the outcome. It never fails
// the service: every ending — envelope, no envelope, timeout, shutdown — lands
// in the run row.
func (s *Service) execute(ctx context.Context, j job) {
	defer func() {
		s.mu.Lock()
		delete(s.pending, j.a.Name)
		s.mu.Unlock()
	}()

	started := time.Now()
	if err := s.store.MarkStarted(j.id, fmtTime(started)); err != nil {
		s.logger.Printf("automations: %s: %v", j.id, err)
	}

	runCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, "sh", "-c", j.a.Form3)
	cmd.Dir = filepath.Join(s.repoDir, j.a.Dir)
	cmd.Env = pathExtendedEnv()
	// Run the subprocess in its own process group and kill the whole group on
	// timeout, so python children die with their sh parent.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second

	stdout := &tailWriter{cap: stdoutTailCap}
	stderr := &tailWriter{cap: stderrTailCap}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	runErr := cmd.Run()

	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	status, formUsed, envelopeJSON, errText := "", 0, "", ""
	switch {
	case cmd.ProcessState == nil:
		status, errText = StatusError, fmt.Sprintf("could not start: %v", runErr)
	case runCtx.Err() == context.DeadlineExceeded:
		status, errText = StatusError, fmt.Sprintf("timed out after %s (process group killed)", s.timeout)
	case ctx.Err() != nil:
		status, errText = StatusError, "interrupted by service shutdown"
	default:
		// Exit code and envelope status are independent facts: a degraded run
		// exits 1 by spec and still carries a valid envelope.
		env, raw, err := parseEnvelope(stdout.bytes())
		if err != nil {
			status, errText = StatusError, "no envelope: "+err.Error()
		} else {
			status, formUsed, envelopeJSON = env.Status, env.FormUsed, raw
		}
	}

	if err := s.store.Finish(j.id, fmtTime(time.Now()), exitCode, status, formUsed,
		envelopeJSON, string(stderr.bytes()), errText); err != nil {
		s.logger.Printf("automations: %s: %v", j.id, err)
		return
	}
	if errText != "" {
		s.logger.Printf("automations: %s %s: exit %d, %s: %s", j.a.Name, j.id, exitCode, status, errText)
	} else {
		s.logger.Printf("automations: %s %s: exit %d, envelope %s", j.a.Name, j.id, exitCode, status)
	}
}

// pathExtendedEnv is stdd's environment with /usr/local/bin:/opt/homebrew/bin
// appended to PATH — a launchd agent's PATH lacks the Homebrew dirs the
// automations' python3 lives in.
func pathExtendedEnv() []string {
	const extra = "/usr/local/bin:/opt/homebrew/bin"
	env := os.Environ()
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = kv + ":" + extra
			return env
		}
	}
	return append(env, "PATH="+extra)
}

// tailWriter keeps the last cap bytes written to it.
type tailWriter struct {
	cap int
	buf []byte
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.cap {
		w.buf = w.buf[len(w.buf)-w.cap:]
	}
	return len(p), nil
}

func (w *tailWriter) bytes() []byte { return w.buf }
