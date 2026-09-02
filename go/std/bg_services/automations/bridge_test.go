package automations

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// serveBridge drives one request through the exported Handler() and decodes
// the JSON body into out (which may be nil), returning the status.
func serveBridge(t *testing.T, h http.Handler, method, path string, out any) int {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	if out != nil && rec.Code < 400 {
		if err := json.Unmarshal(rec.Body.Bytes(), out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, rec.Body.String(), err)
		}
	}
	return rec.Code
}

// The botnet gateway mounts this service in-process, so Handler() must be the
// same mux the unix socket serves: the bridged routes behave identically on
// both surfaces, and /tick (handler-local, never bridged) populates the
// registry exactly as the socket's tick does.
func TestHandlerIsTheSocketMux(t *testing.T) {
	repo := t.TempDir()
	writeAutomation(t, repo, "alpha", "echo '"+okEnvelope+"'", "")
	svc := newProbe(t, repo)
	h := svc.Handler()

	if code := serveBridge(t, h, http.MethodPost, "/tick", nil); code != http.StatusOK {
		t.Fatalf("POST /tick through Handler() = %d, want 200", code)
	}
	var views []automationView
	if code := serveBridge(t, h, http.MethodGet, "/v1/automations", &views); code != http.StatusOK {
		t.Fatalf("GET /v1/automations through Handler() = %d, want 200", code)
	}
	if len(views) != 1 || views[0].Name != "alpha" {
		t.Fatalf("list through Handler() = %+v, want the one discovered automation", views)
	}
	var detail automationView
	if code := serveBridge(t, h, http.MethodGet, "/v1/automations/alpha", &detail); code != http.StatusOK {
		t.Fatalf("GET /v1/automations/alpha through Handler() = %d, want 200", code)
	}
	if detail.Name != "alpha" {
		t.Fatalf("detail through Handler() = %+v, want alpha", detail)
	}
	if code := serveBridge(t, h, http.MethodGet, "/v1/automations/nope", nil); code != http.StatusNotFound {
		t.Fatalf("GET unknown automation through Handler() = %d, want 404", code)
	}
}

// Every automation row carries path — the absolute directory (repo dir joined
// with the repo-relative dir), which is what the app hands to `open -a Cursor`.
// Dir stays repo-relative for display.
func TestAutomationRowsCarryAbsolutePath(t *testing.T) {
	repo := t.TempDir()
	writeAutomation(t, repo, "alpha", "true", "")
	svc := newProbe(t, repo)
	svc.autos = svc.discover()
	h := svc.Handler()

	var views []automationView
	if code := serveBridge(t, h, http.MethodGet, "/v1/automations", &views); code != http.StatusOK || len(views) != 1 {
		t.Fatalf("list = %d with %d rows, want 200 with 1", code, len(views))
	}
	want := filepath.Join(svc.repoDir, "alpha")
	if views[0].Path != want || !filepath.IsAbs(views[0].Path) {
		t.Errorf("list row path = %q, want absolute %q", views[0].Path, want)
	}
	if views[0].Dir != "alpha" {
		t.Errorf("list row dir = %q, want repo-relative %q", views[0].Dir, "alpha")
	}
	var detail automationView
	if code := serveBridge(t, h, http.MethodGet, "/v1/automations/alpha", &detail); code != http.StatusOK {
		t.Fatalf("detail = %d, want 200", code)
	}
	if detail.Path != want {
		t.Errorf("detail path = %q, want %q", detail.Path, want)
	}
}
