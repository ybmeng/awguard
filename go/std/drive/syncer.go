package drive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"stdtools/go/std/bg_services/artifacts"
)

// ArtifactsSyncer implements artifacts.Syncer on Google Drive. Managed dirs
// mirror to Drive as <rootFolder>/<id>/<files>. CreateDir is the machine's
// stage 2, SyncFile its stage 3 (each upload acknowledged before returning a
// static reference), and Fetch the serving fallback by Drive file id.
type ArtifactsSyncer struct {
	client     *Client
	rootFolder string

	mu     sync.Mutex
	rootID string
}

// NewArtifactsSyncer returns a syncer mirroring managed dirs under the named
// top-level Drive folder (created on first use).
func NewArtifactsSyncer(client *Client, rootFolder string) *ArtifactsSyncer {
	return &ArtifactsSyncer{client: client, rootFolder: rootFolder}
}

// rootFolderID resolves and caches the top-level Drive folder.
func (s *ArtifactsSyncer) rootFolderID(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rootID != "" {
		return s.rootID, nil
	}
	id, err := s.client.FindOrCreateFolder(ctx, s.rootFolder, "")
	if err != nil {
		return "", err
	}
	s.rootID = id
	return id, nil
}

// syncAttempts and syncBackoff bound the retry loop for transient Drive
// failures during the remote stages. Vars so tests stay fast.
var (
	syncAttempts = 3
	syncBackoff  = 250 * time.Millisecond
)

// withRetry runs op, retrying transient Drive failures (429, 5xx, network
// errors) a bounded number of times with short backoff. Sound because every
// remote op is idempotent: find-or-create for folders, replace-by-name for
// uploads.
func withRetry(ctx context.Context, op func() error) error {
	for attempt := 1; ; attempt++ {
		err := op()
		if err == nil || attempt >= syncAttempts || !Transient(err) {
			return err
		}
		select {
		case <-ctx.Done():
			return err
		case <-time.After(time.Duration(attempt) * syncBackoff):
		}
	}
}

// CreateDir implements artifacts.Syncer (stage 2): it creates the Drive
// folder for one managed dir and returns the folder id as the remote dir.
// Transient Drive failures are retried a few times before giving up.
func (s *ArtifactsSyncer) CreateDir(ctx context.Context, id artifacts.ID) (string, error) {
	var folder string
	err := withRetry(ctx, func() error {
		root, err := s.rootFolderID(ctx)
		if err != nil {
			return err
		}
		folder, err = s.client.FindOrCreateFolder(ctx, id.String(), root)
		return err
	})
	if err != nil {
		return "", fmt.Errorf("drive: create remote dir for %s: %w", id, err)
	}
	return folder, nil
}

// SyncFile implements artifacts.Syncer (stage 3): it uploads one file into
// the remote dir, blocking until Drive acknowledges it, and returns the
// static reference (Drive file id, size, sha256). Re-syncing replaces remote
// content instead of duplicating it, which also makes transient Drive
// failures safely retryable — each attempt re-reads the file from the start.
func (s *ArtifactsSyncer) SyncFile(ctx context.Context, remoteDir, localPath string) (artifacts.FileRef, error) {
	name := filepath.Base(localPath)
	var ref artifacts.FileRef
	err := withRetry(ctx, func() error {
		f, err := os.Open(localPath)
		if err != nil {
			return fmt.Errorf("drive: open %s: %w", localPath, err)
		}
		defer f.Close()

		// Size and hash both come from the bytes actually streamed to
		// Drive, so the ref is self-consistent even if the file changes
		// between attempts.
		h := sha256.New()
		var cw countingWriter
		fileID, err := s.client.Upload(ctx, name, remoteDir, io.TeeReader(f, io.MultiWriter(h, &cw)))
		if err != nil {
			return err
		}
		ref = artifacts.FileRef{
			Name:     name,
			RemoteID: fileID,
			Size:     cw.n,
			SHA256:   hex.EncodeToString(h.Sum(nil)),
		}
		return nil
	})
	if err != nil {
		return artifacts.FileRef{}, fmt.Errorf("drive: upload %s: %w", name, err)
	}
	return ref, nil
}

// countingWriter counts the bytes written through it.
type countingWriter struct{ n int64 }

func (w *countingWriter) Write(p []byte) (int, error) {
	w.n += int64(len(p))
	return len(p), nil
}

// Fetch implements artifacts.Syncer: it streams a file back from Drive by
// the static id recorded in its reference.
func (s *ArtifactsSyncer) Fetch(ctx context.Context, ref artifacts.FileRef) (io.ReadCloser, error) {
	if ref.RemoteID == "" {
		return nil, fmt.Errorf("drive: reference for %s has no remote id", ref.Name)
	}
	return s.client.Download(ctx, ref.RemoteID)
}
