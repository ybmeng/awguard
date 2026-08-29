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

	"awguard/go/std/bg_services/artifacts"
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

// CreateDir implements artifacts.Syncer (stage 2): it creates the Drive
// folder for one managed dir and returns the folder id as the remote dir.
func (s *ArtifactsSyncer) CreateDir(ctx context.Context, id artifacts.ID) (string, error) {
	root, err := s.rootFolderID(ctx)
	if err != nil {
		return "", err
	}
	folder, err := s.client.FindOrCreateFolder(ctx, id.String(), root)
	if err != nil {
		return "", fmt.Errorf("drive: create remote dir for %s: %w", id, err)
	}
	return folder, nil
}

// SyncFile implements artifacts.Syncer (stage 3): it uploads one file into
// the remote dir, blocking until Drive acknowledges it, and returns the
// static reference (Drive file id, size, sha256). Re-syncing replaces remote
// content instead of duplicating it.
func (s *ArtifactsSyncer) SyncFile(ctx context.Context, remoteDir, localPath string) (artifacts.FileRef, error) {
	f, err := os.Open(localPath)
	if err != nil {
		return artifacts.FileRef{}, fmt.Errorf("drive: open %s: %w", localPath, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return artifacts.FileRef{}, fmt.Errorf("drive: stat %s: %w", localPath, err)
	}

	name := filepath.Base(localPath)
	h := sha256.New()
	fileID, err := s.client.Upload(ctx, name, remoteDir, io.TeeReader(f, h))
	if err != nil {
		return artifacts.FileRef{}, fmt.Errorf("drive: upload %s: %w", name, err)
	}
	return artifacts.FileRef{
		Name:     name,
		RemoteID: fileID,
		Size:     info.Size(),
		SHA256:   hex.EncodeToString(h.Sum(nil)),
	}, nil
}

// Fetch implements artifacts.Syncer: it streams a file back from Drive by
// the static id recorded in its reference.
func (s *ArtifactsSyncer) Fetch(ctx context.Context, ref artifacts.FileRef) (io.ReadCloser, error) {
	if ref.RemoteID == "" {
		return nil, fmt.Errorf("drive: reference for %s has no remote id", ref.Name)
	}
	return s.client.Download(ctx, ref.RemoteID)
}
