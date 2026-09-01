package automations

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// runOnce enqueues one manual run and executes it synchronously through the
// real runner path, returning the recorded row.
func runOnce(t *testing.T, s *Service, name string) Run {
	t.Helper()
	id, err := s.enqueue(name, "manual")
	if err != nil {
		t.Fatalf("enqueue %s: %v", name, err)
	}
	s.execute(context.Background(), <-s.queue)
	run, err := s.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func discoverInto(t *testing.T, s *Service) {
	t.Helper()
	autos := s.discover()
	s.mu.Lock()
	s.autos = autos
	s.mu.Unlock()
}

func TestRunnerRecordsEnvelopeRun(t *testing.T) {
	repo := t.TempDir()
	writeAutomation(t, repo, "good", "echo working >&2\necho progress\necho '"+okEnvelope+"'", "")
	s := newProbe(t, repo)
	discoverInto(t, s)

	run := runOnce(t, s, "good")
	if run.Status != "ok" || run.ExitCode != 0 || run.FormUsed != 3 || run.Error != "" {
		t.Fatalf("run = %+v", run)
	}
	if run.Trigger != "manual" || run.Started == "" || run.Finished == "" {
		t.Errorf("run bookkeeping = %+v", run)
	}
	var env Envelope
	if err := json.Unmarshal([]byte(run.Envelope), &env); err != nil || env.Artifacts[0].Newest != "2026-02" {
		t.Errorf("recorded envelope %q did not round-trip: %v", run.Envelope, err)
	}
	if !strings.Contains(run.StderrTail, "working") {
		t.Errorf("stderr tail = %q, want the script's stderr", run.StderrTail)
	}
	// Timestamps are fixed-width RFC3339 UTC to the second.
	if ok, _ := regexp.MatchString(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$`, run.Started); !ok {
		t.Errorf("started %q is not fixed-width RFC3339 UTC", run.Started)
	}
}

// TestRunnerDegradedKeepsEnvelopeAndExitIndependent: degraded exits 1 by spec
// and still carries a valid envelope — neither fact is inferred from the other.
func TestRunnerDegradedKeepsEnvelopeAndExitIndependent(t *testing.T) {
	repo := t.TempDir()
	degraded := strings.Replace(okEnvelope, `"status":"ok"`, `"status":"degraded"`, 1)
	writeAutomation(t, repo, "deg", "echo '"+degraded+"'\nexit 1", "")
	s := newProbe(t, repo)
	discoverInto(t, s)

	run := runOnce(t, s, "deg")
	if run.Status != "degraded" || run.ExitCode != 1 || run.Envelope == "" || run.Error != "" {
		t.Fatalf("run = %+v, want degraded envelope with exit 1", run)
	}
}

func TestRunnerNoEnvelopeIsAnError(t *testing.T) {
	repo := t.TempDir()
	writeAutomation(t, repo, "chatty", "echo just some text", "")
	writeAutomation(t, repo, "silent", "true", "")
	writeAutomation(t, repo, "badstatus", `echo '{"automation":"x","status":"almost"}'`, "")
	s := newProbe(t, repo)
	discoverInto(t, s)

	for _, name := range []string{"chatty", "silent", "badstatus"} {
		run := runOnce(t, s, name)
		if run.Status != StatusError || !strings.HasPrefix(run.Error, "no envelope") {
			t.Errorf("%s: status %q error %q, want error with a no-envelope explanation", name, run.Status, run.Error)
		}
		if run.ExitCode != 0 {
			t.Errorf("%s: exit = %d, want 0 recorded independently of the envelope defect", name, run.ExitCode)
		}
	}
}

func TestRunnerTimeoutKillsProcessGroup(t *testing.T) {
	repo := t.TempDir()
	// The child sleep outlives the sh parent unless the whole group is killed.
	writeAutomation(t, repo, "slow", "sleep 30\necho '"+okEnvelope+"'", "")
	s := newProbe(t, repo)
	s.timeout = 200 * time.Millisecond
	discoverInto(t, s)

	started := time.Now()
	run := runOnce(t, s, "slow")
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("timeout took %s, the process group was not killed", elapsed)
	}
	if run.Status != StatusError || !strings.Contains(run.Error, "timed out") {
		t.Fatalf("run = %+v, want a timed-out error run", run)
	}
}

func TestEnqueueGuards(t *testing.T) {
	repo := t.TempDir()
	writeAutomation(t, repo, "one", "echo '"+okEnvelope+"'", "")
	s := newProbe(t, repo)
	discoverInto(t, s)

	if _, err := s.enqueue("nope", "manual"); !errors.Is(err, errUnknownAutomation) {
		t.Errorf("unknown enqueue = %v, want errUnknownAutomation", err)
	}
	if _, err := s.enqueue("one", "manual"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.enqueue("one", "manual"); !errors.Is(err, errBusy) {
		t.Errorf("second enqueue = %v, want errBusy", err)
	}
	s.execute(context.Background(), <-s.queue)
	if _, err := s.enqueue("one", "manual"); err != nil {
		t.Errorf("enqueue after completion = %v, want accepted again", err)
	}
}

// TestSerialExecution proves runs are serial across automations: two scripts
// that would interleave under concurrency append start/end markers to a shared
// file, and the recorded order must be strictly S E S E.
func TestSerialExecution(t *testing.T) {
	repo := t.TempDir()
	marks := filepath.Join(repo, "marks.txt")
	script := func(tag string) string {
		return "echo S" + tag + " >> " + marks + "\nsleep 0.1\necho E" + tag + " >> " + marks + "\necho '" + okEnvelope + "'"
	}
	writeAutomation(t, repo, "left", script("L"), "")
	writeAutomation(t, repo, "right", script("R"), "")
	s := newProbe(t, repo)
	discoverInto(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.worker(ctx) }()

	idL, err := s.enqueue("left", "manual")
	if err != nil {
		t.Fatal(err)
	}
	idR, err := s.enqueue("right", "manual")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		rL, _ := s.store.Get(idL)
		rR, _ := s.store.Get(idR)
		if rL.Finished != "" && rR.Finished != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runs did not finish: %+v %+v", rL, rR)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	data, err := os.ReadFile(marks)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(string(data))
	if len(got) != 4 || got[0][0] != 'S' || got[1][0] != 'E' || got[2][0] != 'S' || got[3][0] != 'E' ||
		got[0][1] != got[1][1] || got[2][1] != got[3][1] {
		t.Fatalf("marks = %v, want one run fully finishing before the next starts", got)
	}
}

func TestStoreSweepAndOrdering(t *testing.T) {
	s := newProbe(t, "")
	// Three rows in the same second: List must come back newest-first by
	// insertion (rowid) despite the started_at tie.
	for i, id := range []string{newID("run_"), newID("run_"), newID("run_")} {
		r := Run{ID: id, Automation: "auto", Trigger: "manual", Started: "2026-06-15T12:00:00Z", Status: "ok", FormUsed: 3, ExitCode: 0}
		if i == 2 {
			r.Status = StatusRunning
		}
		if err := s.store.Insert(r); err != nil {
			t.Fatal(err)
		}
	}
	runs, err := s.store.List("auto", 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 3 || runs[0].Status != StatusRunning {
		t.Fatalf("List = %+v, want newest (the running row) first", runs)
	}

	if err := s.store.SweepInterrupted("2026-06-15T12:01:00Z"); err != nil {
		t.Fatal(err)
	}
	runs, _ = s.store.List("auto", 50)
	if runs[0].Status != StatusError || !strings.Contains(runs[0].Error, "interrupted") {
		t.Errorf("swept run = %+v, want an interrupted error", runs[0])
	}
	if runs[1].Status != "ok" || runs[2].Status != "ok" {
		t.Errorf("sweep touched finished runs: %+v", runs)
	}

	if _, err := s.store.Get("run_00000000000000000000000000"); !errors.Is(err, ErrRunNotFound) {
		t.Errorf("Get missing = %v, want ErrRunNotFound", err)
	}
}
