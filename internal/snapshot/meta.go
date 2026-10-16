// Package snapshot owns the deferred copy-based code capture per target code
// group: snapshots/<turn>/<path-hash>/ holding each touched file's preimage
// and metadata, with first-write-wins capture, last-write identity checks and
// the shared restore traversal. It reads no conversation, claim or session
// lifecycle state.
//
// On-disk layout of one code group:
//
//	<group-dir>/
//	└── snapshots/
//	    └── <turn>/
//	        └── <path-hash>/
//	            ├── original     # pre-edit bytes, absent if file didn't exist
//	            └── meta.json    # {original_path, canonical_path, existed, last_write}
//
// Turn numbers are integers starting at 1. Retained pre-cutover session
// directories keep their old meta.json/turns/claims files untouched; the
// retained reader in runtime proves only the explicit old ID, project path
// and project hash members and never interprets the rest.
package snapshot

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

// SnapshotMeta is written alongside each snapshotted file. OriginalPath
// is the user-facing path shown in history and affected-file output.
// CanonicalPath is the hidden restore target for new snapshots. Old
// metadata without CanonicalPath falls back to OriginalPath.
// Existed is false when the snapshot represents a file that did not
// exist before the turn — on revert, that file is deleted.
type SnapshotMeta struct {
	OriginalPath  string                   `json:"original_path"`
	CanonicalPath string                   `json:"canonical_path,omitempty"`
	Existed       bool                     `json:"existed"`
	LastWrite     *SnapshotContentIdentity `json:"last_write,omitempty"`
}

// SnapshotContentIdentity identifies the state produced by the session's
// latest successful mutation for a snapshot entry.
type SnapshotContentIdentity struct {
	Hash   string `json:"hash,omitempty"`
	Absent bool   `json:"absent,omitempty"`
}

// RevertResult reports the restore work performed by RevertCode.
type RevertResult struct {
	Restored []string        `json:"restored,omitempty"`
	Skipped  []SkippedRevert `json:"skipped,omitempty"`
}

// SkippedRevert is reported when a restore is skipped because the current
// on-disk state no longer belongs to this session's last write.
type SkippedRevert struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// writeJSON serializes v to path with 0o600 permissions and a trailing
// newline. Overwrites if the file already exists.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	data = append(data, '\n')
	if err := atomicfs.Write(path, data, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// readJSON reads and parses a JSON file at path into v.
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}
