package tool

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/MMinasyan/lightcode/internal/config"
)

// applyPatchDescription is the model-facing description of apply_patch. The
// text is the verbatim V4A instruction set from Decision 19, trimmed to
// Lightcode's house style: one-line capability summary plus terse imperative
// bullets, no worked example, no formal-grammar block. The format is taught
// in the description and the JSON schema; no editing section is added to
// the system prompt (Decision 11).
const applyPatchDescription = `Edits, creates, deletes, or renames files using the apply_patch (V4A) patch format.
- Wrap every change between *** Begin Patch and *** End Patch, with one section per file.
- Start each section with a header: *** Add File: <path> (every following line is a + line of new content), *** Update File: <path> (edit in place), or *** Delete File: <path> (nothing follows).
- To rename, put *** Move to: <new path> on the line right after *** Update File: <path>.
- In an Update, write each change as a hunk starting with @@. Prefix context lines with a space, removed lines with -, and added lines with +. Include about 3 lines of context around each change, and add @@ <enclosing function or class> when that context is not unique.
- Use file paths relative to the project root.
- Use pending=true unless the patch is your last action; pending patches apply automatically with your next tool call or after your response. Leave it off only on a final patch you want applied immediately (prefer that over execute_pending).`

// ApplyPatch implements the V4A patch tool. It replaces edit_file + write_file
// for the GPT-5.x / Codex family (Decision 2) and is registered DefaultHidden
// so it reaches that family only through an adaptation's IncludeTools. Apply
// behavior is Codex's in full (Decision 3): the patch is validated before any
// write, Add/Update/Delete/Move go through the same safefs + snapshot
// primitives as the rest of Lightcode's file tools, and the result format is
// Codex's `Success. Updated the following files:` A/M/D summary. Move
// decomposes into write-new + delete-old (Decision 14); no read-before-edit
// gate is enforced (Decision 12). applyPatchApplyAtRoot is the engine and
// is shared with the staged flush (commit 5).
//
// The tool also captures per-op pre / post / hunk data during apply
// (applyPreview / applyPreviewMu) and stashes it as the most recent
// apply. DisplayMetadata in commit 6 reads that stash to build the
// edit_preview_files map; the stash is cleared after each read so a
// later tool call cannot see a stale preview.
type ApplyPatch struct {
	store         SnapshotStore
	tracker       *FileTracker
	cfg           config.ToolsConfig
	workspaceRoot string

	applyPreviewMu sync.Mutex
	applyPreview   []appliedFilePreview
}

func NewApplyPatchWithSnapshot(store SnapshotStore, tracker *FileTracker, cfg config.ToolsConfig) *ApplyPatch {
	return &ApplyPatch{store: store, tracker: tracker, cfg: cfg}
}

func NewApplyPatchWithSnapshotAtRoot(store SnapshotStore, tracker *FileTracker, cfg config.ToolsConfig, workspaceRoot string) *ApplyPatch {
	return &ApplyPatch{store: store, tracker: tracker, cfg: cfg, workspaceRoot: workspaceRoot}
}

func (*ApplyPatch) Name() string { return "apply_patch" }

func (*ApplyPatch) Description() string { return applyPatchDescription }

func (*ApplyPatch) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": map[string]any{
				"type":        "string",
				"description": "The full patch text, from *** Begin Patch to *** End Patch.",
			},
			"pending": map[string]any{
				"type":        "boolean",
				"description": "If true, stage this patch instead of applying it immediately; staged patches apply with the next tool call or after your response.",
			},
		},
		"required": []string{"input"},
	}
}

func (*ApplyPatch) DefaultHidden() bool { return true }

// DisplayMetadata is implemented in commit 6 (edit_preview_files). The stub
// returns nil so commit 3 ships the tool inert against the GUI/CLI diff
// metadata contract.
func (*ApplyPatch) DisplayMetadata(_ context.Context, _ json.RawMessage, _ string) map[string]any {
	return nil
}

func (a *ApplyPatch) Execute(ctx context.Context, params map[string]any) (string, error) {
	result, previews, err := applyPatchApplyAtRoot(ctx, a.workspaceRoot, a.store, a.tracker, params)
	if err != nil {
		// Clear any partial stash so a later tool call cannot read a
		// stale preview.
		a.applyPreviewMu.Lock()
		a.applyPreview = nil
		a.applyPreviewMu.Unlock()
		return result, err
	}
	a.applyPreviewMu.Lock()
	a.applyPreview = previews
	a.applyPreviewMu.Unlock()
	return result, nil
}

// takeApplyPreview returns the most recent apply's per-file previews and
// clears the stash. Used by DisplayMetadata (commit 6) and by tests.
func (a *ApplyPatch) takeApplyPreview() []appliedFilePreview {
	a.applyPreviewMu.Lock()
	defer a.applyPreviewMu.Unlock()
	p := a.applyPreview
	a.applyPreview = nil
	return p
}
