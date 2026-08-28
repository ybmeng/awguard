package artifacts

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// Stage is a state of the insert state machine. An insert walks
// INIT -> MOVED -> REMOTE_DIR -> SYNCED -> REFS -> COMPLETE; any failure
// lands in ERR, which is terminal and irrecoverable for now.
type Stage string

const (
	// StageInit: managed dir created and tagged WIP, nothing moved yet.
	StageInit Stage = "init"
	// StageMoved: stage 1 done — insert files moved into the managed dir.
	StageMoved Stage = "moved"
	// StageRemoteDir: stage 2 done — remote dir structure created on Drive.
	StageRemoteDir Stage = "remote_dir"
	// StageSynced: stage 3 done — every file uploaded and acknowledged.
	StageSynced Stage = "synced"
	// StageRefs: stage 4 done — static references written into the dir.
	StageRefs Stage = "refs"
	// StageComplete: stage 5 — WIP tag removed, id handed out.
	StageComplete Stage = "complete"
	// StageErr: terminal failure state.
	StageErr Stage = "err"
)

// Marker files inside a managed dir. Dotfiles, so they are never uploaded
// and never drained from the inbox.
const (
	wipMarker = ".wip"       // present while the machine is in flight
	errMarker = ".err"       // present after a failure (terminal)
	refsFile  = ".refs.json" // stage 4 output, static after COMPLETE
)

// FileRef is the static reference to one synced file. RemoteID is the Drive
// file id; it never changes, so serving can fetch by id without lookups.
type FileRef struct {
	Name     string `json:"name"`
	RemoteID string `json:"remote_id"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

// Refs is the full static reference set of a managed dir, stored in the dir
// as .refs.json by stage 4 and never rewritten afterwards.
type Refs struct {
	ID        ID        `json:"id"`
	RemoteDir string    `json:"remote_dir"`
	SyncedAt  time.Time `json:"synced_at"`
	Files     []FileRef `json:"files"`
}

// Find returns the reference for name, or false when the dir holds no such
// file.
func (r Refs) Find(name string) (FileRef, bool) {
	for _, f := range r.Files {
		if f.Name == name {
			return f, true
		}
	}
	return FileRef{}, false
}

// Status is the externally visible state of one managed dir.
type Status struct {
	ID        ID
	Stage     Stage
	Error     string // set when Stage == StageErr
	UpdatedAt time.Time
}

// marker is the JSON body of .wip and .err files. For .err, Stage records
// the stage whose transition failed.
type marker struct {
	Stage     Stage     `json:"stage"`
	Error     string    `json:"error,omitempty"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// writeJSONAtomic writes v as JSON via temp file + rename, so readers never
// observe a partial marker.
func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readMarker parses a .wip or .err file. A missing file returns an error
// satisfying os.IsNotExist.
func readMarker(path string) (marker, error) {
	var m marker
	b, err := os.ReadFile(path)
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil {
		return m, fmt.Errorf("artifacts: corrupt marker %s: %w", path, err)
	}
	return m, nil
}
