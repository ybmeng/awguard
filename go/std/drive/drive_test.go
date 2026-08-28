package drive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"awguard/go/std/bg_services/artifacts"
)

// Compile-time check that ArtifactsSyncer satisfies the artifacts contract.
var _ artifacts.Syncer = (*ArtifactsSyncer)(nil)

// fakeDrive is an in-memory Drive v3 backend implementing the endpoints the
// client uses: token refresh, file query, folder create, multipart upload,
// media update, and media download.
type fakeDrive struct {
	mu     sync.Mutex
	nextID int
	files  map[string]*fakeItem // id -> item
	tokens int                  // token refreshes served
}

type fakeItem struct {
	id      string
	name    string
	parent  string
	mime    string
	content []byte
}

var (
	qName   = regexp.MustCompile(`name = '((?:[^'\\]|\\.)*)'`)
	qParent = regexp.MustCompile(`'((?:[^'\\]|\\.)*)' in parents`)
	qMime   = regexp.MustCompile(`mimeType = '([^']*)'`)
)

func unescapeQ(s string) string {
	s = strings.ReplaceAll(s, `\'`, `'`)
	return strings.ReplaceAll(s, `\\`, `\`)
}

func (d *fakeDrive) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		d.tokens++
		d.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"access_token": "test-token", "expires_in": 3600})
	})

	auth := func(w http.ResponseWriter, r *http.Request) bool {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return false
		}
		return true
	}

	mux.HandleFunc("GET /drive/files", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		q := r.URL.Query().Get("q")
		var name, parent, mimeType string
		if m := qName.FindStringSubmatch(q); m != nil {
			name = unescapeQ(m[1])
		}
		if m := qParent.FindStringSubmatch(q); m != nil {
			parent = unescapeQ(m[1])
		}
		if m := qMime.FindStringSubmatch(q); m != nil {
			mimeType = m[1]
		}

		d.mu.Lock()
		defer d.mu.Unlock()
		var files []map[string]string
		for _, it := range d.files {
			if it.name != name {
				continue
			}
			if parent != "" && it.parent != parent {
				continue
			}
			if mimeType != "" && it.mime != mimeType {
				continue
			}
			files = append(files, map[string]string{"id": it.id, "name": it.name})
		}
		json.NewEncoder(w).Encode(map[string]any{"files": files})
	})

	mux.HandleFunc("GET /drive/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		d.mu.Lock()
		it, ok := d.files[r.PathValue("id")]
		d.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(it.content)
	})

	mux.HandleFunc("POST /drive/files", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		var meta struct {
			Name     string   `json:"name"`
			MimeType string   `json:"mimeType"`
			Parents  []string `json:"parents"`
		}
		if err := json.NewDecoder(r.Body).Decode(&meta); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := d.add(&fakeItem{name: meta.Name, mime: meta.MimeType, parent: first(meta.Parents)})
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	})

	mux.HandleFunc("POST /upload/files", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			http.Error(w, "want multipart", http.StatusBadRequest)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		metaPart, err := mr.NextPart()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var meta struct {
			Name    string   `json:"name"`
			Parents []string `json:"parents"`
		}
		if err := json.NewDecoder(metaPart).Decode(&meta); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mediaPart, err := mr.NextPart()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		content, _ := io.ReadAll(mediaPart)
		id := d.add(&fakeItem{name: meta.Name, parent: first(meta.Parents), content: content})
		json.NewEncoder(w).Encode(map[string]string{"id": id})
	})

	mux.HandleFunc("PATCH /upload/files/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !auth(w, r) {
			return
		}
		content, _ := io.ReadAll(r.Body)
		d.mu.Lock()
		it, ok := d.files[r.PathValue("id")]
		if ok {
			it.content = content
		}
		d.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"id": it.id})
	})

	return mux
}

func (d *fakeDrive) add(it *fakeItem) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nextID++
	it.id = fmt.Sprintf("gdrive-%d", d.nextID)
	if d.files == nil {
		d.files = map[string]*fakeItem{}
	}
	d.files[it.id] = it
	return it.id
}

func first(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// byPath resolves "<folder>/<sub>/<name>" in the fake backend.
func (d *fakeDrive) byPath(path string) *fakeItem {
	d.mu.Lock()
	defer d.mu.Unlock()
	parent := ""
	var found *fakeItem
	for _, seg := range strings.Split(path, "/") {
		found = nil
		for _, it := range d.files {
			if it.name == seg && it.parent == parent {
				found = it
				break
			}
		}
		if found == nil {
			return nil
		}
		parent = found.id
	}
	return found
}

func newTestClient(t *testing.T, d *fakeDrive) *Client {
	t.Helper()
	srv := httptest.NewServer(d.handler(t))
	t.Cleanup(srv.Close)
	c, err := NewClient(Config{
		ClientID:     "id",
		ClientSecret: "secret",
		RefreshToken: "refresh",
		TokenURL:     srv.URL + "/token",
		APIBase:      srv.URL + "/drive",
		UploadBase:   srv.URL + "/upload",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestFindOrCreateFolderIsIdempotent(t *testing.T) {
	d := &fakeDrive{}
	c := newTestClient(t, d)
	ctx := context.Background()

	id1, err := c.FindOrCreateFolder(ctx, "std_artifacts", "")
	if err != nil || id1 == "" {
		t.Fatalf("create: id=%q err=%v", id1, err)
	}
	id2, err := c.FindOrCreateFolder(ctx, "std_artifacts", "")
	if err != nil || id2 != id1 {
		t.Fatalf("second call: id=%q err=%v, want %q (no duplicate)", id2, err, id1)
	}
	if d.tokens != 1 {
		t.Errorf("token refreshes = %d, want 1 (cached)", d.tokens)
	}
}

func TestUploadCreatesThenUpdates(t *testing.T) {
	d := &fakeDrive{}
	c := newTestClient(t, d)
	ctx := context.Background()

	folder, err := c.FindOrCreateFolder(ctx, "root", "")
	if err != nil {
		t.Fatal(err)
	}
	id1, err := c.Upload(ctx, "a.txt", folder, strings.NewReader("v1"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	id2, err := c.Upload(ctx, "a.txt", folder, strings.NewReader("v2"))
	if err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	if id2 != id1 {
		t.Errorf("re-upload created new file %q, want update of %q", id2, id1)
	}
	if it := d.byPath("root/a.txt"); it == nil || string(it.content) != "v2" {
		t.Errorf("remote content = %v, want v2", it)
	}
}

// TestStoreWithDriveSyncerEndToEnd runs the full artifacts pipeline against
// the fake Drive: insert -> force sync -> id, then local eviction -> Fetch
// fallback.
func TestStoreWithDriveSyncerEndToEnd(t *testing.T) {
	d := &fakeDrive{}
	syncer := NewArtifactsSyncer(newTestClient(t, d), "std_artifacts")
	st, err := artifacts.NewStore(filepath.Join(t.TempDir(), "managed"), syncer, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	ctx := context.Background()

	src := filepath.Join(t.TempDir(), "report.txt")
	if err := os.WriteFile(src, []byte("quarterly numbers"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := st.Insert(ctx, src)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// The managed dir is mirrored on Drive before the id was handed out.
	remote := d.byPath("std_artifacts/" + id.String() + "/report.txt")
	if remote == nil || string(remote.content) != "quarterly numbers" {
		t.Fatalf("remote copy = %v, want mirrored content", remote)
	}

	// Evict the local copy: Open must serve from Drive.
	if err := os.Remove(st.Path(id, "report.txt")); err != nil {
		t.Fatal(err)
	}
	r, err := st.Open(ctx, id, "report.txt")
	if err != nil {
		t.Fatalf("Open after eviction: %v", err)
	}
	defer r.Close()
	got, _ := io.ReadAll(r)
	if string(got) != "quarterly numbers" {
		t.Errorf("fetched content = %q", got)
	}

	// A second store instance (fresh process) can also fetch it.
	syncer2 := NewArtifactsSyncer(newTestClient(t, d), "std_artifacts")
	r2, err := syncer2.Fetch(ctx, id, "report.txt")
	if err != nil {
		t.Fatalf("Fetch from fresh syncer: %v", err)
	}
	defer r2.Close()
	got2, _ := io.ReadAll(r2)
	if string(got2) != "quarterly numbers" {
		t.Errorf("fresh fetch = %q", got2)
	}

	if _, err := syncer2.Fetch(ctx, id, "missing.txt"); err == nil {
		t.Error("expected error fetching a file that was never synced")
	}
}

func TestForceSyncRetryDoesNotDuplicate(t *testing.T) {
	d := &fakeDrive{}
	syncer := NewArtifactsSyncer(newTestClient(t, d), "std_artifacts")
	ctx := context.Background()

	dir := filepath.Join(t.TempDir(), "42")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := syncer.ForceSync(ctx, dir); err != nil {
		t.Fatalf("ForceSync: %v", err)
	}
	if err := syncer.ForceSync(ctx, dir); err != nil {
		t.Fatalf("ForceSync (retry): %v", err)
	}

	d.mu.Lock()
	count := 0
	for _, it := range d.files {
		if it.name == "a.txt" {
			count++
		}
	}
	d.mu.Unlock()
	if count != 1 {
		t.Errorf("remote copies of a.txt = %d, want 1", count)
	}
}
