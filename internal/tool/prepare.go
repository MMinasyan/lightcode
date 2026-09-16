package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/pathutil"
	"github.com/MMinasyan/lightcode/internal/safefs"
)

// PreparedTarget is one canonical target bound during preparation. The
// permission-pair conversion consumes the canonical path; the permission
// name follows from the preparing tool (file.read for reads including the
// authorized suggestion-directory binding, file.write for mutations).
type PreparedTarget struct {
	CanonicalPath string
}

// PreparedCall is the shared preparation output for one read/write/edit
// call: the normalized public arguments, every declared canonical target in
// declaration order, and one execution closure. Preparation performs only
// path/stat/canonicalization and pure parsing; content reads, precondition
// checks, snapshot capture and mutation run inside Execute. The closure
// revalidates the bound canonical Workspace root, write-dir and every
// target before content access and never silently substitutes a changed
// canonical path.
type PreparedCall struct {
	Args    map[string]any
	Targets []PreparedTarget
	Execute func(context.Context) (string, error)
}

// PreparedPatchResult is the structured execution result of one prepared
// apply_patch call: the model-visible summary plus the per-file preview
// data the engine captured, in the exported engine shape, so the target
// plugin can convert previews to its metadata.
type PreparedPatchResult struct {
	Result   string
	Previews []AppliedFilePreview
}

// PreparedPatchCall is the preparation output for one apply_patch call.
// The execution closure consumes the patch parsed during preparation — no
// redecode or reparse — and revalidates the bound root and every target
// before content access.
type PreparedPatchCall struct {
	Args    map[string]any
	Targets []PreparedTarget
	Execute func(context.Context) (PreparedPatchResult, error)
}

// readTarget is the private per-target binding read preparation tracks;
// the display path stays private to the suggestion path.
type readTarget struct {
	displayPath   string
	canonicalPath string
}

// consumedIntAtUse parses one canonical-integer json.Number argument to int
// at its point of use. Normalization owns every default, clamp and
// validation; this consumption accepts only the normalized input, and any
// other value reads as zero — the same fallback the previous typed
// assertion applied to un-normalized input. Atoi is ParseInt with the
// platform-int bit size, so every error class — syntax and platform-int
// overflow alike, the latter carrying a clamped extremum that must be
// discarded — reads as zero.
func consumedIntAtUse(args map[string]any, key string) int {
	n, _ := args[key].(json.Number)
	v, err := strconv.Atoi(string(n))
	if err != nil {
		return 0
	}
	return v
}

// approvedBinding is the canonical witness carried from authorization: the
// approved canonical target path and the lexical write-dir witness (empty
// when the call reaches preparation without a binding, e.g. direct legacy
// Execute calls), plus the canonical Workspace root witness and the
// canonical write-dir boundary bound at authorization so inner preparation
// compares against them instead of re-binding fresh ones after approval;
// an empty witness falls back to the legacy re-bind from its lexical input.
type approvedBinding struct {
	canonical         string
	writeDir          string
	rootCanonical     string
	writeDirCanonical string
}

func approvedBindingFromParams(params map[string]any) approvedBinding {
	v, _ := params[canonicalPathParam].(canonicalPathValue)
	return approvedBinding{canonical: v.path, writeDir: v.writeDir, rootCanonical: v.root}
}

// canonicalBinding is the resolved target binding one preparation step
// produces: the display path, the canonical target, the canonical write-dir
// boundary (empty when unconstrained), and the leaf-existence observation.
type canonicalBinding struct {
	displayAbs        string
	canonical         string
	writeDirCanonical string
	leafExists        bool
}

// bindWriteDir resolves the lexical write-dir witness to its canonical
// boundary; an empty write-dir stays unconstrained.
func bindWriteDir(root, toolName, writeDir string) (string, error) {
	if writeDir == "" {
		return "", nil
	}
	resolved, err := pathutil.ResolveFilePathFrom(root, writeDir)
	if err != nil {
		return "", fmt.Errorf("%s: resolve write_dir %q: %w", toolName, writeDir, err)
	}
	return resolved.CanonicalPath, nil
}

// bindCanonicalTarget resolves one call's display path from the bound root,
// refuses any canonical change against the approved witness, applies the
// write-dir check, and binds the canonical write-dir boundary — the
// authorization-time witness when bound, else a fresh resolution from the
// lexical witness (the legacy receipt flow).
func bindCanonicalTarget(root, toolName, path string, approved approvedBinding) (canonicalBinding, error) {
	displayAbs, err := fileDisplayAbsPathAtRoot(root, path)
	if err != nil {
		return canonicalBinding{}, fmt.Errorf("%s: resolve path: %w", toolName, err)
	}
	resolved, err := pathutil.ResolveFilePathFrom(root, path)
	if err != nil {
		return canonicalBinding{}, fmt.Errorf("%s: resolve path: %w", toolName, err)
	}
	if approved.canonical != "" && resolved.CanonicalPath != approved.canonical {
		return canonicalBinding{}, fmt.Errorf("%s: resolve path: %w", toolName, canonicalChangedError(approved.canonical, resolved.CanonicalPath))
	}
	if err := checkWriteDirTarget(root, toolName, resolved.CanonicalPath, CapabilityOptions{WriteDir: approved.writeDir}); err != nil {
		return canonicalBinding{}, err
	}
	writeDirCanonical := approved.writeDirCanonical
	if writeDirCanonical == "" {
		writeDirCanonical, err = bindWriteDir(root, toolName, approved.writeDir)
		if err != nil {
			return canonicalBinding{}, err
		}
	}
	return canonicalBinding{displayAbs: displayAbs, canonical: resolved.CanonicalPath, writeDirCanonical: writeDirCanonical, leafExists: resolved.LeafExists}, nil
}

// bindWorkspaceRoot resolves the lexical Workspace root to its canonical
// form for binding; preparation binds the root together with the targets.
func bindWorkspaceRoot(root string) (string, error) {
	resolved, err := pathutil.ResolveFilePathFrom("", root)
	if err != nil {
		return "", err
	}
	return resolved.CanonicalPath, nil
}

// revalidateWorkspaceRoot refuses any change of the bound canonical
// Workspace root; a changed root binding fails without substituting a
// replacement.
func revalidateWorkspaceRoot(root, boundCanonical string) error {
	resolved, err := pathutil.ResolveFilePathFrom("", root)
	if err != nil {
		return err
	}
	if boundCanonical != "" && resolved.CanonicalPath != boundCanonical {
		return fmt.Errorf("approved canonical workspace root changed from %s to %s", boundCanonical, resolved.CanonicalPath)
	}
	return nil
}

func canonicalChangedError(boundCanonical, canonical string) error {
	return fmt.Errorf("approved canonical path changed from %s to %s", boundCanonical, canonical)
}

// boundCall carries the canonical bindings one prepared call revalidates
// before content access and again after snapshot creation.
type boundCall struct {
	root              string
	toolName          string
	path              string
	rootCanonical     string
	canonical         string
	writeDir          string // lexical write-dir witness (empty when unconstrained)
	writeDirCanonical string // canonical write-dir boundary bound at preparation
}

func (b boundCall) revalidate() error {
	if err := revalidateWorkspaceRoot(b.root, b.rootCanonical); err != nil {
		return err
	}
	resolved, err := pathutil.ResolveFilePathFrom(b.root, b.path)
	if err != nil {
		return err
	}
	if b.canonical != "" && resolved.CanonicalPath != b.canonical {
		return canonicalChangedError(b.canonical, resolved.CanonicalPath)
	}
	return b.revalidateWriteDir()
}

// revalidateWriteDirWitness re-resolves the lexical write-dir witness and
// refuses any change of its bound canonical boundary — so a repointed
// write-dir symlink fails even when the target would sit inside both
// directories. An empty witness stays unconstrained.
func revalidateWriteDirWitness(root, writeDir, writeDirCanonical string) error {
	if writeDir == "" {
		return nil
	}
	resolved, err := pathutil.ResolveFilePathFrom(root, writeDir)
	if err != nil {
		return err
	}
	if resolved.CanonicalPath != writeDirCanonical {
		return fmt.Errorf("approved canonical write_dir changed from %s to %s", writeDirCanonical, resolved.CanonicalPath)
	}
	return nil
}

// revalidateWriteDir re-resolves the lexical write-dir witness and refuses
// any change of its bound canonical boundary, then re-checks the target
// against the bound boundary.
func (b boundCall) revalidateWriteDir() error {
	if err := revalidateWriteDirWitness(b.root, b.writeDir, b.writeDirCanonical); err != nil {
		return err
	}
	if b.writeDir != "" && !pathInsideDir(b.canonical, b.writeDirCanonical) {
		return fmt.Errorf("%s: path %s is outside write_dir %s", b.toolName, b.canonical, b.writeDirCanonical)
	}
	return nil
}

// prepareReadCall is the legacy read_file entry: retained lenient argument
// parsing, then canonical preparation with the authorization binding.
func prepareReadCall(root string, cfg config.ToolsConfig, tracker *FileTracker, params map[string]any) (*PreparedCall, error) {
	approved := approvedBindingFromParams(params)
	args, err := normalizeReadArgsLegacy(params, cfg.ReadMaxLines)
	if err != nil {
		return nil, err
	}
	return prepareRead(root, cfg, tracker, args, approved)
}

// prepareRead binds read_file's canonical targets and builds its execution
// closure. Preparation resolves the canonical path and observes whether the
// leaf exists; when it does not, the parent directory is bound as a second
// declared target for the authorized suggestion listing. No content read
// happens here.
func prepareRead(root string, cfg config.ToolsConfig, tracker *FileTracker, args map[string]any, approved approvedBinding) (*PreparedCall, error) {
	path, _ := args["path"].(string)
	rootCanonical := approved.rootCanonical
	if rootCanonical == "" {
		var err error
		rootCanonical, err = bindWorkspaceRoot(root)
		if err != nil {
			return nil, fmt.Errorf("read_file: resolve path: %w", err)
		}
	}
	binding, err := bindCanonicalTarget(root, "read_file", path, approved)
	if err != nil {
		return nil, err
	}
	bound := boundCall{
		root: root, toolName: "read_file", path: path,
		rootCanonical: rootCanonical, canonical: binding.canonical,
		writeDir: approved.writeDir, writeDirCanonical: binding.writeDirCanonical,
	}

	targets := []PreparedTarget{{CanonicalPath: binding.canonical}}
	parent := readTarget{}
	if !binding.leafExists {
		parent = readTarget{
			displayPath:   filepath.Dir(binding.displayAbs),
			canonicalPath: filepath.Dir(binding.canonical),
		}
		targets = append(targets, PreparedTarget{CanonicalPath: parent.canonicalPath})
	}

	execute := func(_ context.Context) (string, error) {
		if err := bound.revalidate(); err != nil {
			return "", fmt.Errorf("read_file: resolve path: %w", err)
		}
		if _, err := ensureRegularExistingTarget(binding.canonical); err != nil {
			return "", fmt.Errorf("read_file: %w", err)
		}
		f, err := safefs.OpenExisting(binding.canonical, os.O_RDONLY)
		if err != nil {
			if os.IsNotExist(err) {
				// Suggestions run only when preparation observed the leaf
				// missing and bound the parent directory; a leaf that
				// existed during preparation and disappeared before
				// opening returns plain not-found with no undeclared
				// fallback I/O (parent is unbound here).
				return suggestFromBoundDirectory(parent, binding.displayAbs), nil
			}
			return "", fmt.Errorf("read_file: %w", err)
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			return "", fmt.Errorf("read_file: stat: %w", err)
		}
		if err := ensureRegularFileInfo(binding.canonical, info); err != nil {
			return "", fmt.Errorf("read_file: %w", err)
		}

		data, err := io.ReadAll(f)
		if err != nil {
			return "", fmt.Errorf("read_file: %w", err)
		}
		identity := FileIdentityFromFileInfoAndData(info, data)

		// The canonical-integer json.Number arguments parse at their point
		// of use: preparation bound targets only and performs no
		// validation or defaulting here.
		offset := consumedIntAtUse(args, "offset")
		limit := consumedIntAtUse(args, "limit")

		// Deduplication check.
		if tracker != nil {
			if dup, _ := tracker.IsDuplicateIdentity(binding.canonical, offset, limit, identity); dup {
				tracker.TrackIdentity(binding.canonical, offset, limit, identity)
				return "File unchanged since last read. The content from the earlier read in this conversation is still current.", nil
			}
		}

		// Binary detection.
		if isBinary(data) {
			return "", fmt.Errorf("read_file: %s appears to be a binary file", path)
		}

		// Track the read for mtime enforcement.
		if tracker != nil {
			tracker.TrackIdentity(binding.canonical, offset, limit, identity)
		}

		result, totalLines := formatReadOutput(data, offset, limit, cfg.MaxOutputBytes, cfg.ReadLineMaxChars)

		// Footer for truncated files.
		if offset > 1 || limit < totalLines {
			lastLine := totalLines
			if remaining := totalLines - offset + 1; limit < remaining {
				lastLine = offset + limit - 1
			}
			if result != "" {
				result += "\n"
			}
			if offset == 1 && limit < totalLines {
				result += fmt.Sprintf("(Showing lines 1-%d of %d. Use offset=%d to continue.)", lastLine, totalLines, lastLine+1)
			} else if offset > 1 {
				result += fmt.Sprintf("(Showing lines %d-%d of %d.)", offset, lastLine, totalLines)
			}
		}

		return result, nil
	}

	return &PreparedCall{Args: args, Targets: targets, Execute: execute}, nil
}

// PrepareReadCall is the exported preparation entry for target callers.
// The caller supplies already-normalized arguments (NormalizeReadArgs) and
// the canonical Workspace root witness bound at authorization, which
// preparation binds instead of re-resolving the lexical root; the lexical
// root remains the re-resolution input. Target reads run on the
// nil-tracker path and always return the bounded requested content.
// Preparation binds the canonical file target plus the parent directory
// when the leaf is missing.
func PrepareReadCall(root, rootCanonical string, cfg config.ToolsConfig, args map[string]any) (*PreparedCall, error) {
	return prepareRead(root, cfg, nil, args, approvedBinding{rootCanonical: rootCanonical})
}

// suggestFromBoundDirectory lists the bound parent directory through its
// no-follow descriptor — never the display path — and renders the scored
// filename suggestions. Any listing failure degrades to the plain
// not-found message.
func suggestFromBoundDirectory(parent readTarget, displayPath string) string {
	plain := fmt.Sprintf("read_file: file not found: %s", displayPath)
	if parent.canonicalPath == "" {
		return plain
	}
	dir, err := safefs.OpenDirectory(parent.canonicalPath)
	if err != nil {
		return plain
	}
	defer dir.Close()
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return plain
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	suggestions := scoreFileSuggestions(entries, filepath.Base(displayPath))
	if len(suggestions) == 0 {
		return plain
	}
	return formatFileSuggestions(displayPath, filepath.Base(parent.displayPath), suggestions)
}

// prepareWriteCall is the legacy write_file entry: retained lenient
// argument parsing, then canonical preparation with the snapshot store
// bound into the closure.
func prepareWriteCall(root string, store SnapshotStore, tracker *FileTracker, params map[string]any) (*PreparedCall, error) {
	approved := approvedBindingFromParams(params)
	args, err := normalizePathArgsLegacy(params, "write_file")
	if err != nil {
		return nil, err
	}
	return prepareWrite(root, store, tracker, args, approved)
}

// prepareWrite binds write_file's canonical target and builds its execution
// closure. Preparation performs path/stat/canonicalization and the write-dir
// check only; the precondition/preimage reads, required parent creation and
// the write itself run inside the closure as part of the single declared
// file.write target.
func prepareWrite(root string, store SnapshotStore, tracker *FileTracker, args map[string]any, approved approvedBinding) (*PreparedCall, error) {
	path, _ := args["path"].(string)
	content, _ := args["content"].(string)
	rootCanonical := approved.rootCanonical
	if rootCanonical == "" {
		var err error
		rootCanonical, err = bindWorkspaceRoot(root)
		if err != nil {
			return nil, fmt.Errorf("write_file: resolve path: %w", err)
		}
	}
	binding, err := bindCanonicalTarget(root, "write_file", path, approved)
	if err != nil {
		return nil, err
	}
	bound := boundCall{
		root: root, toolName: "write_file", path: path,
		rootCanonical: rootCanonical, canonical: binding.canonical,
		writeDir: approved.writeDir, writeDirCanonical: binding.writeDirCanonical,
	}

	execute := func(_ context.Context) (string, error) {
		if err := bound.revalidate(); err != nil {
			return "", fmt.Errorf("write_file: resolve path: %w", err)
		}
		if store != nil {
			return writeWithSnapshotBookkeeping(bound, store, tracker, binding.displayAbs, binding.canonical, content, path)
		}
		res, _, err := writeMutation(binding.canonical, tracker, content, path)
		return res, err
	}

	return &PreparedCall{
		Args:    args,
		Targets: []PreparedTarget{{CanonicalPath: binding.canonical}},
		Execute: execute,
	}, nil
}

// writeMutation opens the write target (creating required parents), replaces
// its content, and reports whether a mutation may have started.
func writeMutation(canonical string, tracker *FileTracker, content, displayPath string) (string, bool, error) {
	f, openedExisting, mutationStarted, err := openWriteTargetForMutation(canonical, tracker)
	if err != nil {
		return "", mutationStarted, fmt.Errorf("write_file: %w", err)
	}
	defer f.Close()

	if openedExisting {
		mutationStarted = true
	}
	if err := f.Truncate(0); err != nil {
		return "", mutationStarted, fmt.Errorf("write_file: truncate: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", mutationStarted, fmt.Errorf("write_file: seek: %w", err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		return "", mutationStarted, fmt.Errorf("write_file: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", mutationStarted, fmt.Errorf("write_file: sync: %w", err)
	}

	return fmt.Sprintf("Wrote %s.", displayPath), mutationStarted, nil
}

// writeWithSnapshotBookkeeping wraps writeMutation with the retained
// snapshot transaction: preflight, capture, the post-snapshot binding
// revalidation, and the discard-before-mutation / retain-after-mutation
// failure behavior.
func writeWithSnapshotBookkeeping(bound boundCall, store SnapshotStore, tracker *FileTracker, displayAbs, canonical, content, displayPath string) (string, error) {
	if err := preflightWriteSnapshotTarget(canonical, tracker); err != nil {
		return "", err
	}
	snapshot, err := snapshotFileForMutation(store, store.CurrentTurn(), displayAbs, canonical)
	if err != nil {
		return "", fmt.Errorf("write_file: snapshot: %w", err)
	}
	defer releaseSnapshotMutation(snapshot)
	// Retained legacy behavior: the binding is revalidated again after the
	// snapshot was captured, so a target repointed during snapshotting
	// fails before any mutation, and the unmutated snapshot claim is
	// discarded exactly like every other pre-mutation failure.
	if err := bound.revalidate(); err != nil {
		err = fmt.Errorf("write_file: resolve path: %w", err)
		if discardErr := discardUnmutatedSnapshot(snapshot); discardErr != nil {
			return "", fmt.Errorf("%w; additionally failed to discard snapshot: %v", err, discardErr)
		}
		return "", err
	}
	res, mutationStarted, err := writeMutation(canonical, tracker, content, displayPath)
	if err != nil {
		if !mutationStarted {
			if discardErr := discardUnmutatedSnapshot(snapshot); discardErr != nil {
				return "", fmt.Errorf("%w; additionally failed to discard snapshot: %v", err, discardErr)
			}
		} else {
			err = retainFailedMutatedSnapshot(snapshot, canonical, err)
		}
		return "", err
	}
	if err := recordMutatedSnapshotContent(snapshot, []byte(content)); err != nil {
		retainMutatedSnapshot(snapshot)
		return "", fmt.Errorf("write_file: record snapshot identity: %w", err)
	}
	retainMutatedSnapshot(snapshot)
	return res, nil
}

// PrepareWriteCall is the exported preparation entry for target callers.
// The caller supplies already-normalized arguments (NormalizeWriteArgs),
// the hard write-dir constraint, and the canonical Workspace root and
// write-dir witnesses bound at authorization, which preparation binds
// instead of re-resolving them; the lexical root and write-dir remain the
// re-resolution inputs.
func PrepareWriteCall(root, rootCanonical, writeDirCanonical string, opts CapabilityOptions, store SnapshotStore, args map[string]any) (*PreparedCall, error) {
	return prepareWrite(root, store, nil, args, approvedBinding{rootCanonical: rootCanonical, writeDir: opts.WriteDir, writeDirCanonical: writeDirCanonical})
}

// prepareEditCall is the legacy edit_file entry: retained lenient argument
// parsing, then canonical preparation with the snapshot store bound into
// the closure.
func prepareEditCall(root string, store SnapshotStore, tracker *FileTracker, params map[string]any) (*PreparedCall, error) {
	approved := approvedBindingFromParams(params)
	args, err := normalizePathArgsLegacy(params, "edit_file")
	if err != nil {
		return nil, err
	}
	return prepareEdit(root, store, tracker, args, approved)
}

// prepareEdit binds edit_file's canonical target and builds its execution
// closure. Preparation performs path/stat/canonicalization and the write-dir
// check only; the current-content read, old_string validation and the
// mutation run inside the closure as part of the single declared file.write
// target.
func prepareEdit(root string, store SnapshotStore, tracker *FileTracker, args map[string]any, approved approvedBinding) (*PreparedCall, error) {
	path, _ := args["path"].(string)
	oldString, _ := args["old_string"].(string)
	newString, _ := args["new_string"].(string)
	replaceAll, _ := args["replace_all"].(bool)
	rootCanonical := approved.rootCanonical
	if rootCanonical == "" {
		var err error
		rootCanonical, err = bindWorkspaceRoot(root)
		if err != nil {
			return nil, fmt.Errorf("edit_file: resolve path: %w", err)
		}
	}
	binding, err := bindCanonicalTarget(root, "edit_file", path, approved)
	if err != nil {
		return nil, err
	}
	bound := boundCall{
		root: root, toolName: "edit_file", path: path,
		rootCanonical: rootCanonical, canonical: binding.canonical,
		writeDir: approved.writeDir, writeDirCanonical: binding.writeDirCanonical,
	}

	execute := func(_ context.Context) (string, error) {
		if err := bound.revalidate(); err != nil {
			return "", fmt.Errorf("edit_file: resolve path: %w", err)
		}
		if store != nil {
			return editWithSnapshotBookkeeping(bound, store, tracker, binding.displayAbs, binding.canonical, path, oldString, newString, replaceAll)
		}
		res, _, err := editMutation(binding.canonical, tracker, path, oldString, newString, replaceAll)
		if err != nil {
			return "", err
		}
		return res.Result, nil
	}

	return &PreparedCall{
		Args:    args,
		Targets: []PreparedTarget{{CanonicalPath: binding.canonical}},
		Execute: execute,
	}, nil
}

// editMutation validates the edit against current file content and applies
// it; mutationStarted reports whether bytes may have landed.
func editMutation(canonical string, tracker *FileTracker, displayPath, oldString, newString string, replaceAll bool) (*editResult, bool, error) {
	if oldString == "" {
		return nil, false, fmt.Errorf("edit_file: old_string must not be empty")
	}
	if oldString == newString {
		return nil, false, fmt.Errorf("edit_file: old_string and new_string are identical")
	}

	if _, err := ensureRegularExistingTarget(canonical); err != nil {
		return nil, false, fmt.Errorf("edit_file: %w", err)
	}

	f, err := openExistingMutationFile(canonical, os.O_RDWR)
	if err != nil {
		return nil, false, fmt.Errorf("edit_file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, false, fmt.Errorf("edit_file: stat: %w", err)
	}
	if err := ensureRegularFileInfo(canonical, info); err != nil {
		return nil, false, fmt.Errorf("edit_file: %w", err)
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, false, fmt.Errorf("edit_file: %w", err)
	}
	content := string(data)

	res, err := ApplyEdit(content, oldString, newString, replaceAll, displayPath)
	if err != nil {
		return nil, false, err
	}

	mutationStarted := true
	if err := f.Truncate(0); err != nil {
		return nil, mutationStarted, fmt.Errorf("edit_file: truncate: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, mutationStarted, fmt.Errorf("edit_file: seek: %w", err)
	}
	if _, err := f.Write([]byte(res.UpdatedContent)); err != nil {
		return nil, mutationStarted, fmt.Errorf("edit_file: write: %w", err)
	}
	if err := f.Sync(); err != nil {
		return nil, mutationStarted, fmt.Errorf("edit_file: sync: %w", err)
	}

	return &editResult{
		Result:         res.Summary,
		UpdatedContent: res.UpdatedContent,
		LineRanges:     res.LineRanges,
		Count:          res.Count,
	}, mutationStarted, nil
}

// editWithSnapshotBookkeeping wraps editMutation with the retained snapshot
// transaction: preflight, capture, the post-snapshot binding revalidation,
// and the discard-before-mutation / retain-after-mutation failure behavior.
func editWithSnapshotBookkeeping(bound boundCall, store SnapshotStore, tracker *FileTracker, displayAbs, canonical, displayPath, oldString, newString string, replaceAll bool) (string, error) {
	if err := preflightEditSnapshotTarget(canonical, tracker); err != nil {
		return "", err
	}
	snapshot, err := snapshotFileForMutation(store, store.CurrentTurn(), displayAbs, canonical)
	if err != nil {
		return "", fmt.Errorf("edit_file: snapshot: %w", err)
	}
	defer releaseSnapshotMutation(snapshot)
	// Retained legacy behavior: the binding is revalidated again after the
	// snapshot was captured, so a target repointed during snapshotting
	// fails before any mutation, and the unmutated snapshot claim is
	// discarded exactly like every other pre-mutation failure.
	if err := bound.revalidate(); err != nil {
		err = fmt.Errorf("edit_file: resolve path: %w", err)
		if discardErr := discardUnmutatedSnapshot(snapshot); discardErr != nil {
			return "", fmt.Errorf("%w; additionally failed to discard snapshot: %v", err, discardErr)
		}
		return "", err
	}
	res, mutationStarted, err := editMutation(canonical, tracker, displayPath, oldString, newString, replaceAll)
	if err != nil {
		if !mutationStarted {
			if discardErr := discardUnmutatedSnapshot(snapshot); discardErr != nil {
				return "", fmt.Errorf("%w; additionally failed to discard snapshot: %v", err, discardErr)
			}
		} else {
			err = retainFailedMutatedSnapshot(snapshot, canonical, err)
		}
		return "", err
	}
	if err := recordMutatedSnapshotContent(snapshot, []byte(res.UpdatedContent)); err != nil {
		retainMutatedSnapshot(snapshot)
		return "", fmt.Errorf("edit_file: record snapshot identity: %w", err)
	}
	retainMutatedSnapshot(snapshot)
	return res.Result, nil
}

// PrepareEditCall is the exported preparation entry for target callers.
// The caller supplies already-normalized arguments (NormalizeEditArgs),
// the hard write-dir constraint, and the canonical Workspace root and
// write-dir witnesses bound at authorization, which preparation binds
// instead of re-resolving them; the lexical root and write-dir remain the
// re-resolution inputs.
func PrepareEditCall(root, rootCanonical, writeDirCanonical string, opts CapabilityOptions, store SnapshotStore, args map[string]any) (*PreparedCall, error) {
	return prepareEdit(root, store, nil, args, approvedBinding{rootCanonical: rootCanonical, writeDir: opts.WriteDir, writeDirCanonical: writeDirCanonical})
}

// preparePatchCall is the legacy apply_patch entry. With an approval
// receipt, the patch parsed exactly once during authorization, the approved
// target list, the write-dir witness with its canonical boundary and the
// canonical Workspace root witness bound at authorization ride the receipt;
// without one, the patch is parsed once here, the freshly resolved plan is
// authoritative, and the root is bound at effect entry. Normalization
// strips model-supplied `_lightcode_` fields, so a private receipt can
// never survive into executable authority from model arguments.
func preparePatchCall(root string, params map[string]any) (*patch, []applyPatchTarget, string, string, string, error) {
	if receipt, ok := applyPatchReceiptFromParams(params); ok {
		return receipt.parsed, receipt.targets, receipt.writeDir, receipt.writeDirCanonical, receipt.rootCanonical, nil
	}
	args, err := NormalizePatchArgs(params)
	if err != nil {
		return nil, nil, "", "", "", err
	}
	p, targets, err := preparePatchFromArgs(root, args, "")
	if err != nil {
		return nil, nil, "", "", "", err
	}
	rootCanonical, err := bindWorkspaceRoot(root)
	if err != nil {
		return nil, nil, "", "", "", fmt.Errorf("apply_patch: resolve workspace root: %w", err)
	}
	return p, targets, "", "", rootCanonical, nil
}

// preparePatchFromArgs parses the V4A patch exactly once and resolves every
// source/destination target under the write-dir constraint. Preparation
// performs only path resolution, collision and write-dir checks; no content
// read happens here, so a patch source/destination outside write_dir is
// rejected at preparation, before authorization.
func preparePatchFromArgs(root string, args map[string]any, writeDir string) (*patch, []applyPatchTarget, error) {
	input, _ := args["input"].(string)
	p, err := parsePatch(input)
	if err != nil {
		return nil, nil, err
	}
	targets, err := resolveApplyPatchTargetsFromParsed(root, p, CapabilityOptions{WriteDir: writeDir})
	if err != nil {
		return nil, nil, err
	}
	return p, targets, nil
}

// PreparePatchCall is the exported preparation entry for target callers.
// The caller supplies already-normalized arguments (NormalizePatchArgs),
// the hard write-dir constraint, and the canonical Workspace root and
// write-dir witnesses bound at authorization, which replace the internal
// resolutions: the execution closure revalidates against these witnesses,
// re-resolving the lexical root and write-dir. Preparation applies the
// write-dir constraint so every patch source/destination outside write_dir
// is rejected before authorization. The returned call's execution closure
// consumes the patch parsed here — no redecode or reparse — revalidates the
// bound root, the canonical write-dir boundary and every target before
// content access, and returns the structured result carrying the
// model-visible summary and the captured preview data.
func PreparePatchCall(root, rootCanonical, writeDirCanonical string, opts CapabilityOptions, store SnapshotStore, args map[string]any) (*PreparedPatchCall, error) {
	p, targets, err := preparePatchFromArgs(root, args, opts.WriteDir)
	if err != nil {
		return nil, err
	}
	displayTargets := make([]PreparedTarget, 0, len(targets))
	for _, t := range targets {
		displayTargets = append(displayTargets, PreparedTarget{CanonicalPath: t.CanonicalPath})
	}
	writeDir := opts.WriteDir
	execute := func(_ context.Context) (PreparedPatchResult, error) {
		result, previews, err := executeParsedPatch(root, rootCanonical, store, nil, p, targets, writeDir, writeDirCanonical)
		return PreparedPatchResult{Result: result, Previews: previews}, err
	}
	return &PreparedPatchCall{Args: args, Targets: displayTargets, Execute: execute}, nil
}
