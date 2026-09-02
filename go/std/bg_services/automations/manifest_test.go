package automations

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bgservices "stdtools/go/std/bg_services"
)

// Compile-time check that Service satisfies the bg service contract.
var _ bgservices.Service = (*Service)(nil)

func discardLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// newProbe returns a Service rooted in a temp dir with no repo; tests that
// need a registry set s.autos directly or write a repo and call discover.
func newProbe(t *testing.T, repoDir string) *Service {
	t.Helper()
	svc, err := New(Config{Root: t.TempDir(), RepoDir: repoDir, Logger: discardLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { svc.store.Close() })
	return svc
}

// writeAutomation writes one automation dir (README + emit.sh) under repo.
// schedule lines are appended verbatim to the frontmatter when non-empty.
func writeAutomation(t *testing.T, repo, name, script, schedule string) string {
	t.Helper()
	dir := filepath.Join(repo, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "---\nname: " + name + "\ngoal: test automation " + name + "\nforms:\n  \"3\": sh emit.sh\nverify: \"true\"\ncadence: whenever\n" + schedule + "---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "emit.sh"), []byte("#!/bin/sh\n"+script+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

const okEnvelope = `{"automation":"test","status":"ok","form_used":3,"artifacts":[{"path":"data/a.csv","rows":3,"newest":"2026-02"}],"escalation_reason":null}`

// TestLiveManifestsParse runs the parser over BOTH live automation READMEs in
// the repo, verbatim — they are the fixtures the parser exists for.
func TestLiveManifestsParse(t *testing.T) {
	repoRoot := filepath.Join("..", "..", "..", "..")
	cases := []struct {
		path, name, form3, rrule, at, tz string
		retryEvery, retryFor             time.Duration
	}{
		{
			path: filepath.Join(repoRoot, "skills", "econ_data", "fred_m2", "README.md"),
			name: "fred-m2", form3: "python3 scripts/fetch_fred.py all",
			rrule: "FREQ=MONTHLY;BYDAY=4TU", at: "13:05", tz: "America/New_York",
			retryEvery: 2 * time.Hour, retryFor: 30 * time.Hour,
		},
		{
			path: filepath.Join(repoRoot, "skills", "web_control", "korea_trass", "README.md"),
			name: "korea-trass", form3: "python3 scripts/fetch_kcs.py all",
			rrule: "FREQ=MONTHLY;BYMONTHDAY=1,11,15,21", at: "09:05", tz: "Asia/Seoul",
			retryEvery: time.Hour, retryFor: 24 * time.Hour,
		},
	}
	for _, tc := range cases {
		data, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatalf("read live manifest: %v", err)
		}
		m, err := parseManifest(data)
		if err != nil {
			t.Fatalf("%s: parseManifest: %v", tc.path, err)
		}
		if m == nil {
			t.Fatalf("%s: not recognized as an automation manifest", tc.path)
		}
		if m.name != tc.name || m.form3 != tc.form3 {
			t.Errorf("%s: name %q form3 %q, want %q %q", tc.path, m.name, m.form3, tc.name, tc.form3)
		}
		if m.goal == "" {
			t.Errorf("%s: goal not parsed", tc.path)
		}
		if m.schedule == nil {
			t.Fatalf("%s: schedule block not found", tc.path)
		}
		sc, err := parseSchedule(m.schedule)
		if err != nil {
			t.Fatalf("%s: parseSchedule: %v", tc.path, err)
		}
		if sc.RRULE != tc.rrule || sc.At != tc.at || sc.TZ != tc.tz ||
			sc.RetryEvery != tc.retryEvery || sc.RetryFor != tc.retryFor {
			t.Errorf("%s: schedule = %+v, want rrule %q at %q tz %q retry %v/%v",
				tc.path, sc, tc.rrule, tc.at, tc.tz, tc.retryEvery, tc.retryFor)
		}
	}
}

func TestParseManifestTable(t *testing.T) {
	korea := "---\nname: k\ngoal: g\nforms:\n  \"3\": python3 f.py all\n  \"2\": recipes/r.md\ncadence: \"KST releases: full-month on the 1st ~09:00, 1~10 on the 11th\"\nhuman_gates: \"reCAPTCHA — one click: per session\"\n---\nbody\n"
	cases := []struct {
		name, in   string
		wantNil    bool
		wantErr    bool
		checkForm3 string
	}{
		{name: "no frontmatter", in: "# just a readme\n", wantNil: true},
		{name: "unterminated frontmatter", in: "---\nname: x\n", wantErr: true},
		{name: "no forms.3", in: "---\nname: x\nforms:\n  \"1\": SKILL.md\n---\n", wantNil: true},
		{name: "no name", in: "---\nforms:\n  \"3\": run.sh\n---\n", wantNil: true},
		{name: "quoted colon values tolerated", in: korea, checkForm3: "python3 f.py all"},
		{name: "unknown keys and stray lines skipped", in: "---\nname: x\nweird stuff without colon\nunknown: fine\nforms:\n  \"3\": go run .\n---\n", checkForm3: "go run ."},
		{name: "single quotes", in: "---\nname: 'x'\nforms:\n  '3': 'sh a.sh'\n---\n", checkForm3: "sh a.sh"},
	}
	for _, tc := range cases {
		m, err := parseManifest([]byte(tc.in))
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: want error, got %+v", tc.name, m)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if tc.wantNil {
			if m != nil {
				t.Errorf("%s: want nil manifest, got %+v", tc.name, m)
			}
			continue
		}
		if m == nil || m.form3 != tc.checkForm3 {
			t.Errorf("%s: manifest = %+v, want form3 %q", tc.name, m, tc.checkForm3)
		}
	}
}

func TestDiscover(t *testing.T) {
	repo := t.TempDir()
	writeAutomation(t, repo, "alpha", "echo "+okEnvelope, "")
	writeAutomation(t, repo, "beta", "echo hi", `schedule:
  rrule: "FREQ=DAILY"
  at: "09:05"
  tz: "UTC"
  retry_every: 1h
  retry_for: 6h
`)
	// A duplicate name in a later walk dir loses to the first found.
	writeAutomation(t, repo, "gamma", "echo hi", "")
	dupe := "---\nname: alpha\ngoal: impostor\nforms:\n  \"3\": echo no\n---\n"
	if err := os.WriteFile(filepath.Join(repo, "gamma", "README.md"), []byte(dupe), 0o644); err != nil {
		t.Fatal(err)
	}
	// A README inside a data/ dir is never even read.
	if err := os.MkdirAll(filepath.Join(repo, "alpha", "data"), 0o755); err != nil {
		t.Fatal(err)
	}
	inData := "---\nname: hidden\nforms:\n  \"3\": echo no\n---\n"
	if err := os.WriteFile(filepath.Join(repo, "alpha", "data", "README.md"), []byte(inData), 0o644); err != nil {
		t.Fatal(err)
	}
	// An invalid schedule block surfaces as ScheduleError, not a crash.
	writeAutomation(t, repo, "delta", "echo hi", `schedule:
  rrule: "FREQ=DAILY"
  at: "09:05"
  tz: "Mars/Olympus"
  retry_every: 1h
  retry_for: 6h
`)

	svc := newProbe(t, repo)
	autos := svc.discover()
	byName := map[string]Automation{}
	for _, a := range autos {
		byName[a.Name] = a
	}
	if len(autos) != 3 {
		t.Fatalf("discover = %d automations (%v), want 3 (alpha, beta, delta)", len(autos), byName)
	}
	if a := byName["alpha"]; a.Goal == "impostor" || a.Dir != "alpha" {
		t.Errorf("duplicate name did not keep the first found: %+v", a)
	}
	if a := byName["beta"]; a.Schedule == nil || a.Schedule.RetryFor != 6*time.Hour {
		t.Errorf("beta schedule = %+v", a.Schedule)
	}
	if a := byName["delta"]; a.Schedule != nil || !strings.Contains(a.ScheduleError, "unknown tz") {
		t.Errorf("delta = schedule %+v error %q, want nil schedule and tz error", a.Schedule, a.ScheduleError)
	}
	if _, ok := byName["hidden"]; ok {
		t.Error("README under data/ must not be discovered")
	}
}

func TestDiscoverEmptyRepoDir(t *testing.T) {
	svc := newProbe(t, "")
	if autos := svc.discover(); autos == nil || len(autos) != 0 {
		t.Errorf("discover with empty repo = %v, want empty non-nil slice", autos)
	}
}
