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
	"time"

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
	qName    = regexp.MustCompile(`name = '((?:[^'\\]|\\.)*)'`)
	qParent  = regexp.MustCompile(`'((?:[^'\\]|\\.)*)' in parents`)
	qMime    = regexp.MustCompile(`mimeType = '([^']*)'`)
	qMimeNot = regexp.MustCompile(`mimeType != '([^']*)'`)
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
		var name, parent, mimeType, mimeNot string
		if m := qName.FindStringSubmatch(q); m != nil {
			name = unescapeQ(m[1])
		}
		if m := qParent.FindStringSubmatch(q); m != nil {
			parent = unescapeQ(m[1])
		}
		if m := qMime.FindStringSubmatch(q); m != nil {
			mimeType = m[1]
		}
		if m := qMimeNot.FindStringSubmatch(q); m != nil {
			mimeNot = m[1]
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
			if mimeNot != "" && it.mime == mimeNot {
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
	return newTestClientHandler(t, d.handler(t))
}

func newTestClientHandler(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
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

func TestFindFileExcludesFolders(t *testing.T) {
	d := &fakeDrive{}
	c := newTestClient(t, d)
	ctx := context.Background()

	parent, err := c.FindOrCreateFolder(ctx, "root", "")
	if err != nil {
		t.Fatal(err)
	}
	// A folder that shares the file's name must never match FindFile.
	if _, err := c.FindOrCreateFolder(ctx, "a.txt", parent); err != nil {
		t.Fatal(err)
	}
	if id, err := c.FindFile(ctx, "a.txt", parent); err != nil || id != "" {
		t.Fatalf("FindFile matched a folder: id=%q err=%v", id, err)
	}

	// Upload therefore creates a real file alongside the folder instead of
	// patching the folder's "content".
	fileID, err := c.Upload(ctx, "a.txt", parent, strings.NewReader("data"))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	d.mu.Lock()
	it := d.files[fileID]
	d.mu.Unlock()
	if it == nil || it.mime == folderMIME || string(it.content) != "data" {
		t.Fatalf("uploaded item = %+v, want a plain file with content", it)
	}
	if id, err := c.FindFile(ctx, "a.txt", parent); err != nil || id != fileID {
		t.Errorf("FindFile = %q (err=%v), want the file %q", id, err, fileID)
	}
}

func TestSyncFileRefSizeMatchesStreamedBytes(t *testing.T) {
	d := &fakeDrive{}
	syncer := NewArtifactsSyncer(newTestClient(t, d), "std_artifacts")
	ctx := context.Background()

	remoteDir, err := syncer.CreateDir(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "grow.txt")
	if err := os.WriteFile(local, []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref, err := syncer.SyncFile(ctx, remoteDir, local)
	if err != nil {
		t.Fatalf("SyncFile: %v", err)
	}

	// The ref must describe exactly the bytes Drive holds: size and sha256
	// both derived from the streamed content, never a separate Stat.
	remote := d.byPath("std_artifacts/1/grow.txt")
	if remote == nil {
		t.Fatal("no remote copy")
	}
	if ref.Size != int64(len(remote.content)) {
		t.Errorf("ref.Size = %d, remote holds %d bytes", ref.Size, len(remote.content))
	}
	sum := sha256.Sum256(remote.content)
	if ref.SHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("ref.SHA256 = %s, remote content hashes to %s", ref.SHA256, hex.EncodeToString(sum[:]))
	}
}

// flakyHandler serves h, but answers the next `fail` non-token requests with
// the given status first.
type flakyHandler struct {
	h        http.Handler
	mu       sync.Mutex
	fail     int
	status   int
	requests int // non-token requests served (failed or not)
}

func (f *flakyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/token" {
		f.mu.Lock()
		f.requests++
		shouldFail := f.fail > 0
		if shouldFail {
			f.fail--
		}
		f.mu.Unlock()
		if shouldFail {
			http.Error(w, "injected failure", f.status)
			return
		}
	}
	f.h.ServeHTTP(w, r)
}

// fastRetries shrinks the retry backoff for the duration of a test.
func fastRetries(t *testing.T) {
	t.Helper()
	old := syncBackoff
	syncBackoff = time.Millisecond
	t.Cleanup(func() { syncBackoff = old })
}

func TestSyncerRetriesTransientDriveFailures(t *testing.T) {
	fastRetries(t)
	d := &fakeDrive{}
	flaky := &flakyHandler{h: d.handler(t), status: http.StatusBadGateway}
	syncer := NewArtifactsSyncer(newTestClientHandler(t, flaky), "std_artifacts")
	ctx := context.Background()

	// Stage 2: the first attempt dies on a 502, the retry succeeds.
	flaky.mu.Lock()
	flaky.fail = 1
	flaky.mu.Unlock()
	remoteDir, err := syncer.CreateDir(ctx, 7)
	if err != nil {
		t.Fatalf("CreateDir with one 502 = %v, want retried success", err)
	}

	// Stage 3: same — one 502, then success, with no duplicate remote copy.
	local := filepath.Join(t.TempDir(), "a.txt")
	if err := os.WriteFile(local, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	flaky.mu.Lock()
	flaky.fail = 1
	flaky.mu.Unlock()
	ref, err := syncer.SyncFile(ctx, remoteDir, local)
	if err != nil {
		t.Fatalf("SyncFile with one 502 = %v, want retried success", err)
	}
	wantSum := sha256.Sum256([]byte("payload"))
	if ref.SHA256 != hex.EncodeToString(wantSum[:]) || ref.Size != int64(len("payload")) {
		t.Errorf("ref after retry = %+v, want fresh hash/size", ref)
	}
	d.mu.Lock()
	copies := 0
	for _, it := range d.files {
		if it.name == "a.txt" {
			copies++
		}
	}
	d.mu.Unlock()
	if copies != 1 {
		t.Errorf("remote copies of a.txt = %d, want 1", copies)
	}
}

func TestSyncerGivesUpAfterBoundedAttemptsAndSkipsPermanentErrors(t *testing.T) {
	fastRetries(t)
	ctx := context.Background()

	// Persistent 503: bounded retries, then the error surfaces.
	d := &fakeDrive{}
	flaky := &flakyHandler{h: d.handler(t), status: http.StatusServiceUnavailable, fail: 1 << 30}
	syncer := NewArtifactsSyncer(newTestClientHandler(t, flaky), "std_artifacts")
	if _, err := syncer.CreateDir(ctx, 1); err == nil {
		t.Fatal("CreateDir against a dead backend should fail")
	}
	flaky.mu.Lock()
	attempts := flaky.requests
	flaky.mu.Unlock()
	if attempts != syncAttempts {
		t.Errorf("attempts = %d, want %d", attempts, syncAttempts)
	}

	// Permanent 403: no retry at all.
	flaky2 := &flakyHandler{h: d.handler(t), status: http.StatusForbidden, fail: 1 << 30}
	syncer2 := NewArtifactsSyncer(newTestClientHandler(t, flaky2), "std_artifacts")
	if _, err := syncer2.CreateDir(ctx, 1); err == nil {
		t.Fatal("CreateDir with 403 should fail")
	}
	flaky2.mu.Lock()
	attempts2 := flaky2.requests
	flaky2.mu.Unlock()
	if attempts2 != 1 {
		t.Errorf("attempts on 403 = %d, want 1 (no retry)", attempts2)
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
