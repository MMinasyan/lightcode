// Package snapshot owns ~/.lightcode/sessions/<id>/ — meta.json,
// snapshots/<turn>/, and turns/<turn>/ (message persistence). It backs
// both the conversation-anchored revert and the sessions foundation:
// session lifecycle (active / archived / deleted), message persistence,
// and session resume.
//
// On-disk layout:
//
//	~/.lightcode/sessions/<session-id>/
//	├── meta.json
//	├── tokens.json              # monotonic, untouched by revert
//	├── snapshots/
//	│   └── <turn>/
//	│       └── <path-hash>/
//	│           ├── original     # pre-edit bytes, absent if file didn't exist
//	│           └── meta.json    # {original_path, tool_name, existed}
//	└── turns/
//	    └── <turn>/
//	        ├── messages.jsonl   # one serialized message per line
//	        └── complete         # empty marker written on turn_end
//
// Turn numbers are integers starting at 1. A turn without a `complete`
// marker is considered interrupted by a crash and is discarded on
// session load.
package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
)

// SessionState values stored in meta.json.
const (
	StateActive   = "active"
	StateArchived = "archived"
)

// SessionMeta is persisted to the session directory's meta.json. The
// project_hash lets the session list filter sessions by project without
// re-resolving paths. state / archived_at / last_activity drive the
// lifecycle sweep.
type SessionMeta struct {
	ID               string `json:"id"`
	CreatedAt        string `json:"created_at"` // RFC3339
	ProjectPath      string `json:"project_path"`
	ProjectHash      string `json:"project_hash"`
	LightcodeVersion string `json:"lightcode_version"`
	State            string `json:"state"`         // "active" | "archived"
	ArchivedAt       int64  `json:"archived_at"`   // unix seconds, 0 if active
	LastActivity     int64  `json:"last_activity"` // unix seconds of last user message
	Provider         string `json:"provider,omitempty"`
	Model            string `json:"model,omitempty"`
	ParentSessionID  string `json:"parent_session_id,omitempty"`
}

// SnapshotMeta is written alongside each snapshotted file. OriginalPath
// is stored so Revert can restore the file without reversing the path
// hash. Existed is false when the snapshot represents a file that did
// not exist before the turn — on revert, that file is deleted.
type SnapshotMeta struct {
	OriginalPath string `json:"original_path"`
	Existed      bool   `json:"existed"`
}

// CompactionRecord is persisted to compaction.json when context
// lifecycle management summarizes the conversation.
type CompactionRecord struct {
	Summary         string `json:"summary"`
	BoundaryTurn    int    `json:"boundary_turn"`
	CompactedAt     string `json:"compacted_at"`
	SummarizerModel string `json:"summarizer_model"`
}

// writeJSON serializes v to path with 0o600 permissions and a trailing
// newline. Overwrites if the file already exists.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", path, err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
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
