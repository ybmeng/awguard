package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"stdtools/go/std/bg_services/artifacts"
	"stdtools/go/std/bg_services/automations"
	"stdtools/go/std/bg_services/botnetsvc"
)

// writeFixtureAutomation writes one automation dir (README manifest + emit.sh)
// under repo.
func writeFixtureAutomation(t *testing.T, repo, name, script string) {
	t.Helper()
	dir := filepath.Join(repo, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := "---\nname: " + name + "\ngoal: fixture " + name + "\nforms:\n  \"3\": sh emit.sh\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "emit.sh"), []byte("#!/bin/sh\n"+script+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestServicesBridgeAutomationsThroughBotnet is the wiring test for the
// automations-through-botnet bridge: services() must hand the real automations
// service's handler to botnetsvc, so the app's one TCP backend answers all
// five bridged routes — list rows with absolute paths, detail, runs, a manual
// run driven to completion — while the pipeline-internal fire and tick stay
// off it.
func TestServicesBridgeAutomationsThroughBotnet(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "")
	// The automations service binds a unix socket under root; socket paths cap
	// around 104 bytes, so the root lives directly under /tmp.
	root, err := os.MkdirTemp("/tmp", "stddwire")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	repo := filepath.Join(root, "repo")
	writeFixtureAutomation(t, repo, "echoer",
		`echo '{"automation":"echoer","status":"ok","form_used":3,"artifacts":[{"path":"data/a.csv","rows":1,"newest":"2026-08"}],"escalation_reason":null}'`)

	svcs, err := services(root, artifacts.DefaultInterval, artifacts.NopSyncer{}, botnetsvc.Config{
		Addr:    "127.0.0.1:0",
		DBPath:  filepath.Join(root, "net.db"),
		KeyPath: filepath.Join(root, "openrouter.txt"),
	}, repo)
	if err != nil {
		t.Fatalf("services: %v", err)
	}

	// Only the two ends of the bridge run; artifacts, execcal and ping are not
	// part of it.
	var bot *botnetsvc.Service
	var auto *automations.Service
	for _, svc := range svcs {
		switch s := svc.(type) {
		case *botnetsvc.Service:
			bot = s
		case *automations.Service:
			auto = s
		}
	}
	if bot == nil || auto == nil {
		t.Fatal("services() roster is missing botnet or automations")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	botDone, autoDone := make(chan error, 1), make(chan error, 1)
	go func() { botDone <- bot.Run(ctx) }()
	go func() { autoDone <- auto.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-botDone; <-autoDone })

	var addr string
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		if addr = bot.Addr(); addr != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if addr == "" {
		t.Fatal("botnet never reported a bound address")
	}
	base := "http://" + addr

	do := func(method, path string, out any) (int, string) {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if out != nil && resp.StatusCode < 400 {
			if err := json.Unmarshal(raw, out); err != nil {
				t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
			}
		}
		return resp.StatusCode, string(raw)
	}

	// Route 1, list: the startup tick populates discovery; poll until the
	// fixture shows up, then check the new path field is the absolute dir.
	type row struct {
		Name string `json:"name"`
		Dir  string `json:"dir"`
		Path string `json:"path"`
	}
	var rows []row
	for deadline := time.Now().Add(3 * time.Second); ; {
		if status, _ := do("GET", "/v1/automations", &rows); status == http.StatusOK && len(rows) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET /v1/automations through botnet never served the fixture: %+v", rows)
		}
		time.Sleep(20 * time.Millisecond)
	}
	wantPath := filepath.Join(repo, "echoer")
	if rows[0].Name != "echoer" || rows[0].Dir != "echoer" || rows[0].Path != wantPath || !filepath.IsAbs(rows[0].Path) {
		t.Fatalf("list row = %+v, want dir echoer with absolute path %q", rows[0], wantPath)
	}

	// Route 2, detail — and its 404 for an unknown name.
	var detail row
	if status, body := do("GET", "/v1/automations/echoer", &detail); status != http.StatusOK || detail.Path != wantPath {
		t.Errorf("GET detail = %d %s, want 200 with path %q", status, body, wantPath)
	}
	if status, _ := do("GET", "/v1/automations/nope", nil); status != http.StatusNotFound {
		t.Errorf("GET unknown detail = %d, want 404", status)
	}

	// Route 4, manual run: 202 with a run id.
	var accepted struct {
		RunID string `json:"runId"`
	}
	if status, body := do("POST", "/v1/automations/echoer/run", &accepted); status != http.StatusAccepted || accepted.RunID == "" {
		t.Fatalf("POST run = %d %s, want 202 with a runId", status, body)
	}

	// Route 5, run by id: poll until the real runner finishes it.
	var run struct {
		Status   string `json:"status"`
		Finished string `json:"finished"`
		ExitCode int    `json:"exitCode"`
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		if status, _ := do("GET", "/v1/runs/"+accepted.RunID, &run); status != http.StatusOK {
			t.Fatalf("GET run through botnet = %d, want 200", status)
		}
		if run.Finished != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run never finished: %+v", run)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if run.Status != "ok" || run.ExitCode != 0 {
		t.Errorf("finished run = %+v, want a clean ok run", run)
	}

	// Route 3, runs list: the finished run shows up.
	var list []json.RawMessage
	if status, _ := do("GET", "/v1/automations/echoer/runs", &list); status != http.StatusOK || len(list) != 1 {
		t.Errorf("GET runs = %d with %d rows, want 200 with 1", status, len(list))
	}

	// The pipeline-internal routes never cross the bridge.
	if status, _ := do("POST", "/v1/automations/echoer/fire", nil); status != http.StatusNotFound {
		t.Errorf("POST fire through botnet = %d, want 404", status)
	}
	if status, _ := do("POST", "/tick", nil); status != http.StatusNotFound {
		t.Errorf("POST /tick through botnet = %d, want 404", status)
	}
}
