package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/editpreview"
)

// applyPatchDescription is the model-facing description of apply_patch: a
// terse capability summary plus imperative format rules. The format is taught
// in the tool description and JSON schema, not through extra system-prompt
// sections.
const applyPatchDescription = `Edits, creates, deletes, or renames files using the apply_patch (V4A) patch format.
- Wrap every change between *** Begin Patch and *** End Patch, with one section per file.
- Start each section with a header: *** Add File: <path> (every following line is a + line of new content), *** Update File: <path> (edit in place), or *** Delete File: <path> (nothing follows).
- To rename, put *** Move to: <new path> on the line right after *** Update File: <path>.
- In an Update, write each change as a hunk starting with @@. Prefix context lines with a space, removed lines with -, and added lines with +. Include about 3 lines of context around each change, and add @@ <enclosing function or class> when that context is not unique.
- Use file paths relative to the project root.
- Use pending=true unless the patch is your last action; pending patches apply automatically with your next tool call or after your response. Leave it off only on a final patch you want applied immediately (prefer that over execute_pending).`

// ApplyPatch implements the V4A patch tool. It is DefaultHidden so only model
// adaptations that include it can see it. The patch is validated before any
// write; Add/Update/Delete/Move go through the same safefs and snapshot
// primitives as the rest of Lightcode's file tools; and successful output uses
// the `Success. Updated the following files:` A/M/D summary. Move decomposes
// into write-new plus delete-old. Update hunks must match current file content.
//
// The tool captures per-op pre/post/hunk data during apply and stashes it as
// the most recent apply. DisplayMetadata reads that stash to build the
// edit_preview_files map, then clears it so later tool calls cannot see stale
// previews.
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

// DisplayMetadata reads and clears the preview stash captured during Execute,
// then returns the edit_preview_files map consumed by CLI and GUI renderers.
func (a *ApplyPatch) DisplayMetadata(_ context.Context, _ json.RawMessage, _ string) map[string]any {
	a.applyPreviewMu.Lock()
	previews := a.applyPreview
	a.applyPreview = nil
	a.applyPreviewMu.Unlock()
	return applyPatchPreviewMetadata(previews)
}

func applyPatchPreviewMetadata(previews []appliedFilePreview) map[string]any {
	if len(previews) == 0 {
		return nil
	}
	files := make([]editpreview.FileEntry, 0, len(previews))
	for _, p := range previews {
		files = append(files, appliedPreviewToFileEntry(p))
	}
	return map[string]any{"edit_preview_files": files}
}

// appliedPreviewToFileEntry converts the engine's captured per-file
// pre/post/hunks data into the public editpreview.FileEntry shape.
// Add produces a synthetic "all add" hunk from the post content;
// Update / Move destination builds hunks from the captured classified
// lines with StartLine as the 1-based anchor; Delete / Move source
// produces no hunks (just the D tag, which the renderer uses as a
// header).
func appliedPreviewToFileEntry(p appliedFilePreview) editpreview.FileEntry {
	return editpreview.FileEntry{
		Path:    p.Path,
		Op:      p.Op,
		Preview: buildPreviewFromCaptured(p),
	}
}

func buildPreviewFromCaptured(p appliedFilePreview) editpreview.Preview {
	if len(p.Hunks) == 0 {
		if p.Op != "A" {
			return editpreview.Preview{}
		}
		// Add: build a synthetic single hunk with all post lines as add.
		if len(p.Post) == 0 {
			return editpreview.Preview{}
		}
		rows := make([]editpreview.Row, 0, len(p.Post))
		for i, l := range p.Post {
			rows = append(rows, editpreview.Row{
				Kind: editpreview.KindAdd, NewLine: i + 1, Text: l,
			})
		}
		return editpreview.Preview{Hunks: []editpreview.Hunk{{Rows: rows}}}
	}
	hunks := make([]editpreview.Hunk, 0, len(p.Hunks))
	for _, h := range p.Hunks {
		rows := make([]editpreview.Row, 0, len(h.Lines))
		oldLine := h.StartLine
		newLine := h.StartLine
		for _, l := range h.Lines {
			switch l.kind {
			case lineContext:
				rows = append(rows, editpreview.Row{
					Kind: editpreview.KindContext, OldLine: oldLine, NewLine: newLine, Text: l.text,
				})
				oldLine++
				newLine++
			case lineRemove:
				rows = append(rows, editpreview.Row{
					Kind: editpreview.KindRemove, OldLine: oldLine, Text: l.text,
				})
				oldLine++
			case lineAdd:
				rows = append(rows, editpreview.Row{
					Kind: editpreview.KindAdd, NewLine: newLine, Text: l.text,
				})
				newLine++
			}
		}
		hunks = append(hunks, editpreview.Hunk{Rows: rows})
	}
	return editpreview.Preview{Hunks: hunks}
}

// ValidateStaged is the structure-only parse for pending calls. It must not
// touch the filesystem: permission and full filesystem validation happen when
// the staged batch is flushed.
func (*ApplyPatch) ValidateStaged(_ context.Context, args json.RawMessage) error {
	var params map[string]any
	if err := json.Unmarshal(args, &params); err != nil {
		return fmt.Errorf("apply_patch: invalid staged arguments: %w", err)
	}
	input, _ := params["input"].(string)
	if input == "" {
		return fmt.Errorf("apply_patch: input is required")
	}
	if _, err := parsePatch(input); err != nil {
		return err
	}
	return nil
}

// StagedResultMessage is the body returned to the model when the
// staged call is queued, matching edit_file / write_file.
func (*ApplyPatch) StagedResultMessage() string { return "Staged." }

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
// clears the stash. Used by DisplayMetadata and tests.
func (a *ApplyPatch) takeApplyPreview() []appliedFilePreview {
	a.applyPreviewMu.Lock()
	defer a.applyPreviewMu.Unlock()
	p := a.applyPreview
	a.applyPreview = nil
	return p
}
