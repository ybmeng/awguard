package drive

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"awguard/go/std/bg_services/artifacts"
)

// ArtifactsSyncer implements artifacts.Syncer on Google Drive. Managed dirs
// mirror to Drive as <rootFolder>/<id>/<files>; ForceSync blocks until every
// file's upload has been acknowledged, which is what makes Insert safe to
// hand out an id.
type ArtifactsSyncer struct {
	client     *Client
	rootFolder string

	mu      sync.Mutex
	rootID  string
	folders map[string]string // managed dir name -> Drive folder id
}

// NewArtifactsSyncer returns a syncer mirroring managed dirs under the named
// top-level Drive folder (created on first use).
func NewArtifactsSyncer(client *Client, rootFolder string) *ArtifactsSyncer {
	return &ArtifactsSyncer{
		client:     client,
		rootFolder: rootFolder,
		folders:    map[string]string{},
	}
}

// folderID resolves (and caches) the Drive folder for one managed dir name.
// With create false it returns "" when the folder does not exist remotely.
func (s *ArtifactsSyncer) folderID(ctx context.Context, name string, create bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if id, ok := s.folders[name]; ok {
		return id, nil
	}
	if s.rootID == "" {
		var err error
		if create {
			s.rootID, err = s.client.FindOrCreateFolder(ctx, s.rootFolder, "")
		} else {
			s.rootID, err = s.client.FindFolder(ctx, s.rootFolder, "")
		}
		if err != nil {
			return "", err
		}
		if s.rootID == "" {
			return "", nil
		}
	}

	var id string
	var err error
	if create {
		id, err = s.client.FindOrCreateFolder(ctx, name, s.rootID)
	} else {
		id, err = s.client.FindFolder(ctx, name, s.rootID)
	}
	if err != nil {
		return "", err
	}
	if id != "" {
		s.folders[name] = id
	}
	return id, nil
}

// ForceSync implements artifacts.Syncer: it uploads every regular file of
// the managed dir and returns only once all uploads are acknowledged.
// Re-syncing the same dir replaces remote content instead of duplicating it.
func (s *ArtifactsSyncer) ForceSync(ctx context.Context, dir string) error {
	folder, err := s.folderID(ctx, filepath.Base(dir), true)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("drive: read %s: %w", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("drive: open %s: %w", name, err)
		}
		_, err = s.client.Upload(ctx, name, folder, f)
		f.Close()
		if err != nil {
			return fmt.Errorf("drive: upload %s: %w", name, err)
		}
	}
	return nil
}

// Fetch implements artifacts.Syncer: it streams one managed file back from
// Drive when the local copy is gone.
func (s *ArtifactsSyncer) Fetch(ctx context.Context, id artifacts.ID, name string) (io.ReadCloser, error) {
	folder, err := s.folderID(ctx, id.String(), false)
	if err != nil {
		return nil, err
	}
	if folder == "" {
		return nil, fmt.Errorf("drive: managed dir %s not found remotely", id)
	}
	fileID, err := s.client.FindFile(ctx, name, folder)
	if err != nil {
		return nil, err
	}
	if fileID == "" {
		return nil, fmt.Errorf("drive: %s/%s not found remotely", id, name)
	}
	return s.client.Download(ctx, fileID)
}
