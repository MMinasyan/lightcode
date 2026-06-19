package tool

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/MMinasyan/lightcode/internal/safefs"
	"golang.org/x/sys/unix"
)

// applyPatchApplyAtRoot is the shared engine for apply_patch. It is called
// directly by ApplyPatch.Execute (commit 3) and by the staged-flush dispatch
// branch (commit 5). The flow is validate-first all-or-nothing: every op is
// resolved and located against the current files before any mutation, so a
// failed validation writes nothing. The only path to a partial apply is a
// mid-write I/O error after validation passed; on that rare failure the
// committed files stay on disk, remain snapshotted, and are recoverable via
// the turn revert. The immediate path wraps the partial-failure error in
// *ExitError so the model sees the committed-files A/M/D summary (Decision
// 13); the staged path returns the same summary in BatchResult.Error
// (Invariant 5, no struct change).
//
// The engine captures per-op pre / post / hunk data during apply and
// returns it as the second value. The caller (immediate Execute, staged
// branch) stashes the previews on the tool or on its BatchResult so
// DisplayMetadata (commit 6) can build the edit_preview_files map from
// captured data, not post-write disk reads. (Read-after-apply is not
// viable: updates have overwritten the source, deletes have removed the
// file, moves have unlinked the source.)
func applyPatchApplyAtRoot(ctx context.Context, root string, store SnapshotStore, tracker *FileTracker, params map[string]any) (string, []appliedFilePreview, error) {
	input, _ := params["input"].(string)
	p, err := parsePatch(input)
	if err != nil {
		return "", nil, err
	}

	// Phase 1: validate (read-only).
	plans, err := buildApplyPlans(p, root, params, store)
	if err != nil {
		return "", nil, err
	}

	// Phase 2: apply (mutations). Each op tracks its own committed state
	// so a later op's failure can report the earlier committed files.
	previews := make([]appliedFilePreview, 0, len(plans))
	committed := make([]appliedOp, 0, len(plans))
	for i := range plans {
		pl := &plans[i]
		preview, err := applyOne(ctx, pl, store, tracker, &committed)
		if err != nil {
			return "", nil, err
		}
		previews = append(previews, preview...)
	}
	return buildSuccessSummary(committed), previews, nil
}

type appliedOp struct {
	kind string // "A", "M", "D"
	path string
}

// appliedFilePreview is the per-file data the engine captures during
// apply so DisplayMetadata (commit 6) can build the edit_preview_files
// metadata without post-write disk reads. Pre is the pre-mutation
// content (nil for Add); Post is the post-mutation content (nil for
// Delete); Hunks is the list of applied hunks (only for Update /
// Move destination). A Move produces two entries: the destination
// (M, with the new content and the applied hunks) and the source
// (D, with the original content and no hunks).
type appliedFilePreview struct {
	Op    string
	Path  string
	Pre   []string
	Post  []string
	Hunks []appliedHunkPreview
}

// appliedHunkPreview carries the data needed to reconstruct a hunk's
// diff rows in DisplayMetadata without re-reading the file. StartLine
// is the 1-based line in Pre where the hunk's old pattern was
// matched; Old is the matched pattern (context + remove lines, in
// order); New is the hunk's new content (context + add lines, in
// order). Diff rows are derived from these by the same row-derivation
// the existing editpreview package uses.
type appliedHunkPreview struct {
	StartLine int
	Old       []string
	New       []string
}

type applyPlan struct {
	op             fileOp
	canonicalPath  string
	displayAbsPath string
	newContent     string // for Add / Update / Move
	moveCanonical  string // for Move
	moveDisplay    string
	preLines       []string             // pre-mutation content (for Update/Delete/Move source)
	hunks          []appliedHunkPreview // applied hunks (for Update/Move destination)
}

func buildApplyPlans(p *patch, root string, params map[string]any, store SnapshotStore) ([]applyPlan, error) {
	plans := make([]applyPlan, 0, len(p.ops))
	for _, op := range p.ops {
		canonicalPath, err := fileSecurityPathAtRoot(root, params, op.path)
		if err != nil {
			return nil, fmt.Errorf("apply_patch: %s: %w", op.path, err)
		}
		displayAbsPath, err := fileDisplayAbsPathAtRoot(root, op.path)
		if err != nil {
			return nil, fmt.Errorf("apply_patch: %s: %w", op.path, err)
		}
		pl := applyPlan{op: op, canonicalPath: canonicalPath, displayAbsPath: displayAbsPath}

		switch op.kind {
		case opAdd:
			existed, shapeErr := ensureRegularExistingTarget(canonicalPath)
			if shapeErr != nil {
				return nil, fmt.Errorf("apply_patch: %s: %w", op.path, shapeErr)
			}
			if existed {
				return nil, fmt.Errorf("apply_patch: %s already exists (Add File requires the path to not exist)", op.path)
			}
			pl.newContent = buildAddContent(op)
		case opUpdate:
			existed, shapeErr := ensureRegularExistingTarget(canonicalPath)
			if shapeErr != nil {
				return nil, fmt.Errorf("apply_patch: %s: %w", op.path, shapeErr)
			}
			if !existed {
				return nil, fmt.Errorf("apply_patch: %s does not exist (Update requires the path to exist)", op.path)
			}
			preLines, content, hunks, err := computeUpdatedContent(canonicalPath, op, root, params)
			if err != nil {
				return nil, err
			}
			pl.preLines = preLines
			pl.newContent = content
			pl.hunks = hunks
		case opDelete:
			existed, shapeErr := ensureRegularExistingTarget(canonicalPath)
			if shapeErr != nil {
				return nil, fmt.Errorf("apply_patch: %s: %w", op.path, shapeErr)
			}
			if !existed {
				return nil, fmt.Errorf("apply_patch: %s does not exist (Delete requires the path to exist)", op.path)
			}
			// Capture pre-mutation content for the preview stash so the
			// GUI / CLI can show the file's pre-delete state.
			data, readErr := readFileBytes(canonicalPath)
			if readErr != nil {
				return nil, fmt.Errorf("apply_patch: read %s: %w", op.path, readErr)
			}
			pl.preLines = splitLinesRaw(data)
		}

		if op.movePath != "" {
			moveCanonical, err := fileSecurityPathAtRoot(root, params, op.movePath)
			if err != nil {
				return nil, fmt.Errorf("apply_patch: %s (move destination): %w", op.movePath, err)
			}
			moveDisplay, err := fileDisplayAbsPathAtRoot(root, op.movePath)
			if err != nil {
				return nil, fmt.Errorf("apply_patch: %s (move destination): %w", op.movePath, err)
			}
			existed, shapeErr := ensureRegularExistingTarget(moveCanonical)
			if shapeErr != nil {
				return nil, fmt.Errorf("apply_patch: %s (move destination): %w", op.movePath, shapeErr)
			}
			if existed {
				return nil, fmt.Errorf("apply_patch: %s (move destination) already exists (Move over an existing file is not allowed)", op.movePath)
			}
			pl.moveCanonical = moveCanonical
			pl.moveDisplay = moveDisplay
		}

		plans = append(plans, pl)
	}
	return plans, nil
}

// buildAddContent joins an Add File's hunk lines back into a single string.
// Each hunk carries a single + line (the parser's Add shape), and a file's
// trailing newline is preserved only if the patch's last line had one.
func buildAddContent(op fileOp) string {
	parts := make([]string, 0, len(op.hunks))
	for _, h := range op.hunks {
		for _, l := range h.lines {
			parts = append(parts, l.text)
		}
	}
	// If the original input ended with a newline (it usually does), the parser
	// preserves that as a final empty line in the model's content. We do not
	// re-inject it here; the model controls the trailing-newline behavior of
	// Add by including or omitting a bare "+" line at the end.
	return strings.Join(parts, "\n")
}

// computeUpdatedContent reads canonicalPath, applies every hunk in order via
// the Codex fuzzy ladder, and returns the resulting file content as a
// single string (lines joined by \n, with no trailing newline). It also
// returns the pre-mutation content (so DisplayMetadata in commit 6 can
// show the file's pre-update state) and the applied-hunk descriptors
// (StartLine 1-based, Old pattern, New content) so the diff rows can be
// reconstructed without re-reading the file. Hunk location failures
// (Failed to find context / Failed to find expected lines) bubble up
// unchanged so the model sees Codex's exact error string.
func computeUpdatedContent(canonicalPath string, op fileOp, root string, params map[string]any) (preLines []string, content string, hunks []appliedHunkPreview, err error) {
	data, err := readFileBytes(canonicalPath)
	if err != nil {
		return nil, "", nil, fmt.Errorf("apply_patch: read %s: %w", op.path, err)
	}
	preLines = splitLinesRaw(data)
	lines := preLines
	cursor := 0
	for _, h := range op.hunks {
		next, end, hunkPrev, lerr := applyHunk(lines, h, op.path, cursor)
		if lerr != nil {
			return nil, "", nil, lerr
		}
		lines = next
		cursor = end
		hunks = append(hunks, hunkPrev)
	}
	return preLines, strings.Join(lines, "\n"), hunks, nil
}

// applyHunk locates h's pattern (context + remove lines) starting at cursor
// and replaces the matched block with the new content (context + add lines).
// Returns the new lines slice, the new cursor (positioned just past the new
// content), and the appliedHunkPreview so the diff rows can be reconstructed
// without re-reading the file. Anchor handling is inside locate: the anchor
// is located first if present, advancing the cursor; the pattern is then
// located at the advanced cursor.
func applyHunk(fileLines []string, h hunk, path string, cursor int) ([]string, int, appliedHunkPreview, error) {
	start, err := locate(fileLines, h, path, cursor)
	if err != nil {
		return nil, 0, appliedHunkPreview{}, err
	}
	oldLines, newLines := hunkTransform(h)
	end := start + len(oldLines)
	out := make([]string, 0, len(fileLines)+len(newLines)-len(oldLines))
	out = append(out, fileLines[:start]...)
	out = append(out, newLines...)
	out = append(out, fileLines[end:]...)
	return out, start + len(newLines), appliedHunkPreview{StartLine: start + 1, Old: oldLines, New: newLines}, nil
}

func hunkTransform(h hunk) (oldLines, newLines []string) {
	for _, hl := range h.lines {
		switch hl.kind {
		case lineContext:
			oldLines = append(oldLines, hl.text)
			newLines = append(newLines, hl.text)
		case lineRemove:
			oldLines = append(oldLines, hl.text)
		case lineAdd:
			newLines = append(newLines, hl.text)
		}
	}
	return
}

// applyOne applies a single op's mutation and returns the per-file
// preview data captured during the apply (so the engine can stash it
// for DisplayMetadata). On a mid-write failure it wraps the
// partial-failure summary in *ExitError so the immediate Execute path
// surfaces the committed-files A/M/D list to the model (Decision 13);
// on a pre-mutation failure (e.g. snapshot error) it returns a plain
// error. committed is the running list of ops that have already
// landed on disk; applyOne appends to it on success and reads it for
// the partial-failure summary.
func applyOne(ctx context.Context, pl *applyPlan, store SnapshotStore, tracker *FileTracker, committed *[]appliedOp) ([]appliedFilePreview, error) {
	_ = ctx

	// Move: write the destination first (so it lands), then snapshot + delete the source.
	if pl.op.movePath != "" {
		if err := writeWithSnapshot(store, pl.moveDisplay, pl.moveCanonical, pl.newContent, true, tracker); err != nil {
			return nil, &ExitError{Output: buildPartialSummary(*committed) + "\n\n" + err.Error()}
		}
		*committed = append(*committed, appliedOp{kind: "M", path: pl.op.movePath})
	}

	// Now the original-path mutation (Add / Update / Delete), tracked separately for Move.
	origErr := applyOriginalMutation(pl, store, tracker, committed)
	if origErr != nil {
		return nil, &ExitError{Output: buildPartialSummary(*committed) + "\n\n" + origErr.Error()}
	}
	return buildOpPreviews(pl), nil
}

// buildOpPreviews assembles the per-op preview data captured during
// buildApplyPlans / apply for DisplayMetadata to read. Move produces
// two entries (destination M with the new content and the applied
// hunks, source D with the original content and no hunks); other ops
// produce one entry each.
func buildOpPreviews(pl *applyPlan) []appliedFilePreview {
	switch {
	case pl.moveCanonical != "":
		postLines := splitLinesRaw([]byte(pl.newContent))
		return []appliedFilePreview{
			{Op: "M", Path: pl.op.movePath, Post: postLines, Hunks: pl.hunks},
			{Op: "D", Path: pl.op.path, Pre: pl.preLines},
		}
	case pl.op.kind == opAdd:
		postLines := splitLinesRaw([]byte(pl.newContent))
		return []appliedFilePreview{{Op: "A", Path: pl.op.path, Post: postLines}}
	case pl.op.kind == opUpdate:
		postLines := splitLinesRaw([]byte(pl.newContent))
		return []appliedFilePreview{{Op: "M", Path: pl.op.path, Pre: pl.preLines, Post: postLines, Hunks: pl.hunks}}
	case pl.op.kind == opDelete:
		return []appliedFilePreview{{Op: "D", Path: pl.op.path, Pre: pl.preLines}}
	}
	return nil
}

func applyOriginalMutation(pl *applyPlan, store SnapshotStore, tracker *FileTracker, committed *[]appliedOp) error {
	if pl.moveCanonical != "" {
		// For Move the source-path delete is the original-path mutation; the
		// kind is "D" of the source.
		if err := deleteWithSnapshot(store, pl.displayAbsPath, pl.canonicalPath); err != nil {
			return err
		}
		*committed = append(*committed, appliedOp{kind: "D", path: pl.op.path})
		return nil
	}

	switch pl.op.kind {
	case opAdd:
		if err := writeWithSnapshot(store, pl.displayAbsPath, pl.canonicalPath, pl.newContent, true, tracker); err != nil {
			return err
		}
		*committed = append(*committed, appliedOp{kind: "A", path: pl.op.path})
	case opUpdate:
		if err := writeWithSnapshot(store, pl.displayAbsPath, pl.canonicalPath, pl.newContent, false, tracker); err != nil {
			return err
		}
		*committed = append(*committed, appliedOp{kind: "M", path: pl.op.path})
	case opDelete:
		if err := deleteWithSnapshot(store, pl.displayAbsPath, pl.canonicalPath); err != nil {
			return err
		}
		*committed = append(*committed, appliedOp{kind: "D", path: pl.op.path})
	}
	return nil
}

// writeWithSnapshot snapshots the target then writes the new content. The
// snapshot is retained on any post-snapshot error so the partial state is
// recoverable via the turn revert. forNew enforces the Add / Move-dest
// exclusive-create guarantee: if the path was validated to not exist and
// OpenForWrite reports it now does (concurrent create between validate and
// apply), the write is refused.
func writeWithSnapshot(store SnapshotStore, displayAbsPath, canonicalPath, content string, forNew bool, tracker *FileTracker) error {
	var snap snapshotEntry
	hasSnap := false
	if store != nil {
		s, err := snapshotFileForMutation(store, store.CurrentTurn(), displayAbsPath, canonicalPath)
		if err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
		snap = s
		hasSnap = true
	}

	f, existed, err := safefs.OpenForWrite(canonicalPath, 0o644)
	if err != nil {
		if hasSnap {
			_ = discardUnmutatedSnapshot(snap)
		}
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	if forNew && existed {
		_ = f.Close()
		if hasSnap {
			_ = discardUnmutatedSnapshot(snap)
		}
		return fmt.Errorf("path %s appeared during apply (Add requires the path to not exist)", canonicalPath)
	}

	if err := f.Truncate(0); err != nil {
		if hasSnap {
			retainMutatedSnapshot(snap)
		}
		return fmt.Errorf("truncate: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		if hasSnap {
			retainMutatedSnapshot(snap)
		}
		return fmt.Errorf("seek: %w", err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		if hasSnap {
			retainMutatedSnapshot(snap)
		}
		return fmt.Errorf("write: %w", err)
	}
	if err := f.Sync(); err != nil {
		if hasSnap {
			retainMutatedSnapshot(snap)
		}
		return fmt.Errorf("sync: %w", err)
	}
	if hasSnap {
		retainMutatedSnapshot(snap)
	}
	if tracker != nil {
		if info, err := f.Stat(); err == nil {
			tracker.UpdateAfterWriteIdentity(canonicalPath, FileIdentityFromFileInfoAndData(info, []byte(content)))
		}
	}
	return nil
}

// deleteWithSnapshot snapshots the target then removes it. The snapshot
// captures the file's content so RevertCode can restore it.
func deleteWithSnapshot(store SnapshotStore, displayAbsPath, canonicalPath string) error {
	var snap snapshotEntry
	hasSnap := false
	if store != nil {
		s, err := snapshotFileForMutation(store, store.CurrentTurn(), displayAbsPath, canonicalPath)
		if err != nil {
			return fmt.Errorf("snapshot: %w", err)
		}
		snap = s
		hasSnap = true
	}
	if err := safefs.RemoveLeaf(canonicalPath); err != nil {
		if hasSnap {
			_ = discardUnmutatedSnapshot(snap)
		}
		return fmt.Errorf("remove: %w", err)
	}
	if hasSnap {
		retainMutatedSnapshot(snap)
	}
	return nil
}

func buildSuccessSummary(committed []appliedOp) string {
	if len(committed) == 0 {
		return "Success. Updated the following files:"
	}
	var b strings.Builder
	b.WriteString("Success. Updated the following files:\n")
	for _, op := range committed {
		b.WriteString(op.kind)
		b.WriteByte(' ')
		b.WriteString(op.path)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

func buildPartialSummary(committed []appliedOp) string {
	if len(committed) == 0 {
		return "Success. Updated the following files:"
	}
	return buildSuccessSummary(committed)
}

// readFileBytes reads the entire file at canonicalPath with O_NOFOLLOW and
// O_NONBLOCK and returns the raw bytes. Used by the Update/Move path to
// read the file's current content; raw bytes (including \r) ride through
// unchanged per Decision 17.
func readFileBytes(canonicalPath string) ([]byte, error) {
	f, err := safefs.OpenExisting(canonicalPath, os.O_RDONLY|unix.O_NONBLOCK)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// splitLinesRaw splits data on \n and returns the lines. A trailing \n
// produces a final empty string in the slice, matching the V4A convention
// that a file's blank last line is a context line of text "". Raw bytes
// (including \r) ride through inside each line.
func splitLinesRaw(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	return strings.Split(string(data), "\n")
}
