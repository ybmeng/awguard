package drive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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

	"stdtools/go/std/bg_services/artifacts"
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

// TestStoreWithDriveSyncerEndToEnd runs the full state machine against the
// fake Drive: insert -> stages 1-5 -> id, static refs pointing at real Drive
// file ids, then local eviction -> Fetch fallback by RemoteID.
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

	// Stage 2+3 mirrored the dir on Drive before the id was handed out.
	remote := d.byPath("std_artifacts/" + id.String() + "/report.txt")
	if remote == nil || string(remote.content) != "quarterly numbers" {
		t.Fatalf("remote copy = %v, want mirrored content", remote)
	}

	// Stage 4's static refs carry the actual Drive file id and checksum.
	refs, err := st.Refs(id)
	if err != nil {
		t.Fatalf("Refs: %v", err)
	}
	ref, ok := refs.Find("report.txt")
	if !ok || ref.RemoteID != remote.id {
		t.Fatalf("ref = %+v, want RemoteID %q", ref, remote.id)
	}
	wantSum := sha256.Sum256([]byte("quarterly numbers"))
	if ref.SHA256 != hex.EncodeToString(wantSum[:]) || ref.Size != int64(len("quarterly numbers")) {
		t.Errorf("ref checksum/size = %+v", ref)
	}

	// Evict the local copy: Open must serve from Drive via the static ref.
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

	// A fresh syncer (new process) resolves the same static ref with no
	// name lookups.
	syncer2 := NewArtifactsSyncer(newTestClient(t, d), "std_artifacts")
	r2, err := syncer2.Fetch(ctx, ref)
	if err != nil {
		t.Fatalf("Fetch from fresh syncer: %v", err)
	}
	defer r2.Close()
	got2, _ := io.ReadAll(r2)
	if string(got2) != "quarterly numbers" {
		t.Errorf("fresh fetch = %q", got2)
	}

	if _, err := syncer2.Fetch(ctx, artifacts.FileRef{Name: "missing.txt"}); err == nil {
		t.Error("expected error fetching a ref with no remote id")
	}
}

func TestSyncFileRetryDoesNotDuplicate(t *testing.T) {
	d := &fakeDrive{}
	syncer := NewArtifactsSyncer(newTestClient(t, d), "std_artifacts")
	ctx := context.Background()

	remoteDir, err := syncer.CreateDir(ctx, 42)
	if err != nil {
		t.Fatalf("CreateDir: %v", err)
	}
	// CreateDir is idempotent too: a retry lands on the same folder.
	again, err := syncer.CreateDir(ctx, 42)
	if err != nil || again != remoteDir {
		t.Fatalf("CreateDir retry = %q (err=%v), want %q", again, err, remoteDir)
	}

	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref1, err := syncer.SyncFile(ctx, remoteDir, local)
	if err != nil {
		t.Fatalf("SyncFile: %v", err)
	}
	ref2, err := syncer.SyncFile(ctx, remoteDir, local)
	if err != nil {
		t.Fatalf("SyncFile (retry): %v", err)
	}
	if ref2.RemoteID != ref1.RemoteID {
		t.Errorf("retry produced new remote id %q, want %q", ref2.RemoteID, ref1.RemoteID)
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
