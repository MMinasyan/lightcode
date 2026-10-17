package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/pathutil"
	"github.com/MMinasyan/lightcode/internal/safefs"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/protocol"
)

// This file owns the deferred code-snapshot adjunct and the bounded viewer:
// the target per-Operation code groups over the existing snapshot.CodeStore
// tree, the retained pre-cutover snapshot-only view, and one contained 1 MiB
// workspace-file read. Both restore paths share one result projection; no
// route, Harness durability kind, legacy Store, or conversation/recovery
// authority enters here.

// codeGroup is one derivable target code group: the owning Operation and its
// admitted input entry — the group key, never a filesystem segment supplied by
// a caller.
type codeGroup struct {
	operationID string
	entryID     string
}

// codeGroups derives the Session's target code groups from one captured
// history view's entry headers: every input entry owned by an Operation
// (header Operation ownership nonempty), in the headers' own sequence order.
// The group key is the admitted-entry identity, never the Operation identity
// (every delivered message — steering included — owns exactly one Operation
// and one admitted input, so every normally delivered head derives its own
// group) and never admission timestamps (headers arrive in sequence order, so
// filtering preserves it). A manual-compaction Operation has no admitted entry
// and derives no group; an ordinary message Operation derives its group even
// when no snapshots were recorded for it, because a later group still
// rewinds.
func codeGroups(headers []harness.HistoryEntryHeader) []codeGroup {
	groups := make([]codeGroup, 0, len(headers))
	for _, header := range headers {
		if header.Kind != harness.EntryInput || header.OperationID == "" {
			continue
		}
		groups = append(groups, codeGroup{
			operationID: header.OperationID,
			entryID:     header.EntryID,
		})
	}
	return groups
}

// targetCodeGroupDir is the one target group layout: DataDir/code/<session>/
// <admitted-entry>. The session and entry identities come from the validated
// Harness history headers, never from caller data.
func targetCodeGroupDir(dataDir, sessionID, entryID string) string {
	return filepath.Join(dataDir, "code", sessionID, entryID)
}

// sessionArtifactDir is the one server-derived Session artifact directory:
// the interval key every committed opener, restore, committed deletion and
// sweep candidate coordinates on.
func sessionArtifactDir(dataDir, sessionID string) string {
	return filepath.Join(dataDir, "code", sessionID)
}

// listSessionCodeSnapshots projects every recorded target code group of one
// Session, ascending by the owning Operation's admitted-entry sequence, from
// one admitted narrow history capture. Only groups that recorded files are
// listed; a valid Session with none returns an empty, non-nil list.
func (r *Runtime) listSessionCodeSnapshots(ctx context.Context, sessionID string) ([]protocol.CodeSnapshotGroup, error) {
	var groups []protocol.CodeSnapshotGroup
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		view, err := h.ReadHistoryView(ctx, sessionID)
		if err != nil {
			return err
		}
		projected, err := projectCodeSnapshotGroups(sessionID, view.HistoryHeaders(), r.dataDir)
		if err != nil {
			return err
		}
		groups = projected
		return nil
	}); err != nil {
		return nil, err
	}
	return groups, nil
}

// projectCodeSnapshotGroups maps one captured history view's derivable groups
// to the generated list shape through the standalone CodeStore: the recorded
// repository files with their retained display path and preimage existence.
func projectCodeSnapshotGroups(sessionID string, headers []harness.HistoryEntryHeader, dataDir string) ([]protocol.CodeSnapshotGroup, error) {
	groups := codeGroups(headers)
	out := make([]protocol.CodeSnapshotGroup, 0, len(groups))
	for _, group := range groups {
		store, err := snapshot.OpenCodeStore(targetCodeGroupDir(dataDir, sessionID, group.entryID))
		if err != nil {
			return nil, err
		}
		turns, err := store.ListTurns()
		if err != nil {
			return nil, err
		}
		files := make([]protocol.SnapshotFile, 0)
		for _, turn := range turns {
			for _, meta := range turn.Files {
				files = append(files, protocol.SnapshotFile{Path: meta.OriginalPath, Existed: meta.Existed})
			}
		}
		if len(files) == 0 {
			continue // only groups that recorded snapshots are listed
		}
		out = append(out, protocol.CodeSnapshotGroup{
			OperationId: group.operationID,
			GroupItemId: projectItemID(sessionID, group.entryID),
			Files:       files,
		})
	}
	return out, nil
}

// revertSessionCode restores one target Session's recorded code groups at or
// after the boundary Operation in reverse admission order. It takes the
// Session's artifact interval nonblockingly — contention is a conflict — and
// captures and validates its one narrow history view while holding the token,
// then traverses under it. Every group's partial result is accumulated before
// its error is handled; a traversal failure returns that accumulated result
// with the internal error class, while a pre-traversal refusal returns no
// result.
func (r *Runtime) revertSessionCode(ctx context.Context, sessionID, boundaryOperationID string) (protocol.CodeRevertResult, error) {
	var result protocol.CodeRevertResult
	releaseArtifacts, err := r.artifacts.acquireTry(sessionArtifactDir(r.dataDir, sessionID))
	if err != nil {
		return result, err
	}
	defer releaseArtifacts()
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		view, err := h.ReadHistoryView(ctx, sessionID)
		if err != nil {
			return err
		}
		projected, err := restoreSessionCode(sessionID, view, r.dataDir, boundaryOperationID)
		result = projected
		return err
	}); err != nil {
		return result, err
	}
	return result, nil
}

// restoreSessionCode is the pure restore body over one captured history view:
// the one idle gate, boundary resolution, then one traversal from the last
// admitted group down to the boundary. The shared whole-invocation traversal
// owns the skip and reported-skip ledgers across every group, so an identity
// skipped in a newer group is never restored by an older one. Its existing
// canonical and last-write proof reports changed or unproven files as skipped,
// never overwritten, and removes only the snapshot entries it restored.
func restoreSessionCode(sessionID string, view harness.HistoryReadView, dataDir, boundaryOperationID string) (protocol.CodeRevertResult, error) {
	if err := codeRestoreRefusal(sessionID, view); err != nil {
		return protocol.CodeRevertResult{}, err
	}
	groups := codeGroups(view.HistoryHeaders())
	boundary := -1
	for i, group := range groups {
		if group.operationID == boundaryOperationID {
			boundary = i
			break
		}
	}
	if boundary < 0 {
		return protocol.CodeRevertResult{}, fmt.Errorf("boundary operation %q names no admitted message of session %q: %w", boundaryOperationID, sessionID, harness.ErrInvalid)
	}
	directories := make([]string, 0, len(groups)-boundary)
	for i := len(groups) - 1; i >= boundary; i-- {
		directories = append(directories, targetCodeGroupDir(dataDir, sessionID, groups[i].entryID))
	}
	result := newCodeRevertResult()
	traversed, err := snapshot.RevertCodeGroups(directories, 0)
	accumulateCodeRevert(&result, traversed)
	if err != nil {
		return codeRevertFailure(result, err)
	}
	return result, nil
}

// codeRestoreRefusal is the one deferred-restore state gate: running, reserved
// or retiring work, buffered steering or queued input, or a live background
// member refuses the whole restore before any group is opened. The read is a
// check, not a reservation, and cannot prevent later admission or another
// writer.
func codeRestoreRefusal(sessionID string, view harness.HistoryReadView) error {
	if view.ExecutionBusy || view.HasPending || view.HasBackground {
		return fmt.Errorf("session %q is not idle for code restore: %w", sessionID, harness.ErrConflict)
	}
	return nil
}

// newCodeRevertResult is the one shared restore projector: both restore paths
// return non-nil restored and skipped arrays.
func newCodeRevertResult() protocol.CodeRevertResult {
	return protocol.CodeRevertResult{
		Restored: make([]string, 0),
		Skipped:  make([]protocol.SkippedFile, 0),
	}
}

// accumulateCodeRevert appends one group's or turn's partial result to the
// accumulated wire result in traversal order.
func accumulateCodeRevert(result *protocol.CodeRevertResult, group snapshot.RevertResult) {
	result.Restored = append(result.Restored, group.Restored...)
	for _, skipped := range group.Skipped {
		result.Skipped = append(result.Skipped, protocol.SkippedFile{Path: skipped.Path, Reason: skipped.Reason})
	}
}

// codeRevertFailure projects a traversal failure onto the same accumulated
// result DTO: the internal error class rides the result while the Go error
// stays non-nil, so a caller can never read the partial work as an empty
// success or a completed rollback. It never fills Error.SessionId — that
// generated field names a target Session, never a caller-supplied legacy ID.
func codeRevertFailure(result protocol.CodeRevertResult, err error) (protocol.CodeRevertResult, error) {
	result.Error = &protocol.Error{Code: protocol.Internal, Message: err.Error()}
	return result, fmt.Errorf("restore code snapshots: %w", err)
}

// isLegacySessionID reports the one retained ID shape: exactly 8 lowercase
// hexadecimal characters, distinct from every 32-hex target Session ID.
func isLegacySessionID(id string) bool {
	if len(id) != 8 {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// retainedCodeProof is the one old metadata shape this reader interprets:
// exactly the id, project_path and project_hash members under their existing
// JSON names. Those three fields must match the explicit old ID and the
// normalized Workspace proof; every other old metadata member is irrelevant
// to this reader and is not interpreted as Session, model or lifecycle truth.
type retainedCodeProof struct {
	ID          string `json:"id"`
	ProjectPath string `json:"project_path"`
	ProjectHash string `json:"project_hash"`
}

// retainedSessionDir proves one pre-cutover snapshot-only Session directory:
// the exact 8-hex ID, the normalized workspace root, the deterministic
// p-<full sha256> workspace storage key (the same key the permission record
// directory uses), and an existing meta whose ID, ProjectPath, and recorded
// 16-hex project-hash prefix all match. Missing or
// mismatched proof fails before any snapshot read. The proof reads the
// complete regular meta file as one JSON document through the no-follow
// directory and regular-FD helpers and never loads turns, messages, claims,
// recovery, or model metadata.
func (r *Runtime) retainedSessionDir(workspace, legacyID string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("workspace must be non-empty: %w", harness.ErrInvalid)
	}
	if !isLegacySessionID(legacyID) {
		return "", fmt.Errorf("legacy session id %q is not exactly 8 lowercase hex: %w", legacyID, harness.ErrInvalid)
	}
	normalized, err := filepath.Abs(workspace)
	if err != nil {
		return "", fmt.Errorf("normalize workspace: %v: %w", err, harness.ErrInvalid)
	}
	normalized = filepath.Clean(normalized)
	dirID, err := config.WorkspacePermissionDirID(normalized)
	if err != nil {
		return "", fmt.Errorf("derive workspace storage key: %v: %w", err, harness.ErrInvalid)
	}
	// The meta proof records the 16-hex digest prefix of the same storage key
	// the directory is derived from, so the proof and the location can never
	// disagree about the workspace identity.
	hashPrefix := strings.TrimPrefix(dirID, "p-")[:16]
	sessionDir := filepath.Join(r.config.loader.Home(), ".lightcode", "projects", dirID, "sessions", legacyID)
	dir, err := safefs.OpenDirectory(sessionDir)
	if err != nil {
		return "", noFollowOpenError("legacy session directory", sessionDir, err)
	}
	_ = dir.Close()
	metaPath := filepath.Join(sessionDir, "meta.json")
	file, err := safefs.OpenExisting(metaPath, os.O_RDONLY)
	if err != nil {
		return "", noFollowOpenError("legacy session meta", metaPath, err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("read legacy session meta %s: %w", metaPath, err)
	}
	var meta retainedCodeProof
	if err := json.Unmarshal(data, &meta); err != nil {
		return "", fmt.Errorf("legacy session meta %s is malformed: %v: %w", metaPath, err, harness.ErrInvalid)
	}
	if meta.ID != legacyID || meta.ProjectPath != normalized || meta.ProjectHash != hashPrefix {
		return "", fmt.Errorf("legacy session meta %s does not prove id, workspace and project hash for the requested root: %w", metaPath, harness.ErrInvalid)
	}
	return sessionDir, nil
}

// noFollowOpenError classifies one read-only no-follow regular-FD open
// failure for both the retained proof and the viewer: a missing path is
// not-found, a link/hardlink/non-regular refusal is invalid, and any other
// I/O error passes through. label names the caller's target in the message.
func noFollowOpenError(label, path string, err error) error {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("%s %s not found: %w", label, path, harness.ErrNotFound)
	case errors.Is(err, safefs.ErrHardlink), errors.Is(err, safefs.ErrNonRegular),
		errors.Is(err, syscall.ELOOP), errors.Is(err, syscall.ENOTDIR):
		return fmt.Errorf("%s %s refused by the no-follow regular-file rule: %w", label, path, harness.ErrInvalid)
	default:
		return fmt.Errorf("open %s %s: %w", label, path, err)
	}
}

// listRetainedCodeSnapshots projects the pre-cutover per-turn snapshot groups
// through the standalone CodeStore on the proven session directory: the
// retained turn order and display-path files, and nothing of the old
// conversation, claim, recovery, or model authority.
func (r *Runtime) listRetainedCodeSnapshots(ctx context.Context, workspace, legacyID string) (protocol.RetainedTurns, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.RetainedTurns{}, err
	}
	defer release()
	sessionDir, err := r.retainedSessionDir(workspace, legacyID)
	if err != nil {
		return protocol.RetainedTurns{}, err
	}
	store, err := snapshot.OpenCodeStore(sessionDir)
	if err != nil {
		return protocol.RetainedTurns{}, err
	}
	turns, err := store.ListTurns()
	if err != nil {
		return protocol.RetainedTurns{}, err
	}
	out := protocol.RetainedTurns{Turns: make([]protocol.RetainedTurn, 0, len(turns))}
	for _, turn := range turns {
		files := make([]protocol.SnapshotFile, 0, len(turn.Files))
		for _, meta := range turn.Files {
			files = append(files, protocol.SnapshotFile{Path: meta.OriginalPath, Existed: meta.Existed})
		}
		out.Turns = append(out.Turns, protocol.RetainedTurn{Turn: turn.Turn, Files: files})
	}
	return out, nil
}

// revertRetainedCode restores the proven legacy directory's recorded turns
// after the caller's turn through the shared whole-invocation traversal,
// canonical and last-write proof, and partial-result contract. The retained
// proof occurs before the directory is leased: the proved session directory
// is the one artifact interval the restore takes nonblockingly — contention
// is a conflict — and the token remains held through the traversal and every
// exit. `after_turn` is used directly; no target Operation ID is fabricated
// for a legacy turn choice.
func (r *Runtime) revertRetainedCode(ctx context.Context, req protocol.RetainedRevertRequest) (protocol.CodeRevertResult, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.CodeRevertResult{}, err
	}
	defer release()
	sessionDir, err := r.retainedSessionDir(req.Workspace, req.SessionId)
	if err != nil {
		return protocol.CodeRevertResult{}, err
	}
	releaseArtifacts, err := r.artifacts.acquireTry(sessionDir)
	if err != nil {
		return protocol.CodeRevertResult{}, err
	}
	defer releaseArtifacts()
	restored, err := snapshot.RevertCodeGroups([]string{sessionDir}, req.AfterTurn)
	result := newCodeRevertResult()
	accumulateCodeRevert(&result, restored)
	if err != nil {
		return codeRevertFailure(result, err)
	}
	return result, nil
}

// maxViewPrefixBytes is the one viewer prefix cap: 1 MiB of raw file bytes.
const maxViewPrefixBytes = 1 << 20

// readWorkspaceFile returns one bounded contained view of a workspace file:
// the canonical target, at most the first 1 MiB of raw bytes, and whether more
// bytes existed. The raw prefix is converted with the maximal invalid-byte
// replacement, so a regular text and a binary file follow the same display
// rule and conversion may expand the returned content beyond 1 MiB.
func (r *Runtime) readWorkspaceFile(ctx context.Context, req protocol.ReadFileRequest) (protocol.ReadFileResult, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return protocol.ReadFileResult{}, err
	}
	defer release()
	canonical, err := viewerCanonicalTarget(req.Workspace, req.Path)
	if err != nil {
		return protocol.ReadFileResult{}, err
	}
	file, err := safefs.OpenExisting(canonical, os.O_RDONLY)
	if err != nil {
		return protocol.ReadFileResult{}, noFollowOpenError("workspace file", canonical, err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxViewPrefixBytes+1))
	if err != nil {
		return protocol.ReadFileResult{}, fmt.Errorf("read workspace file %s: %w", canonical, err)
	}
	truncated := len(raw) > maxViewPrefixBytes
	if truncated {
		raw = raw[:maxViewPrefixBytes]
	}
	return protocol.ReadFileResult{
		CanonicalPath: canonical,
		Content:       strings.ToValidUTF8(string(raw), "\uFFFD"),
		Truncated:     truncated,
	}, nil
}

// viewerCanonicalTarget applies the mandatory viewer boundary: the workspace
// root and the target are canonicalized, the target must stay inside the root
// after resolution, and a sensitive-name leaf is refused. The caller then
// opens the canonical target through the no-follow regular-FD helper, so
// hardlinks and non-regular leaves are refused too.
func viewerCanonicalTarget(workspace, path string) (string, error) {
	if workspace == "" {
		return "", fmt.Errorf("workspace must be non-empty: %w", harness.ErrInvalid)
	}
	if path == "" {
		return "", fmt.Errorf("path must be non-empty: %w", harness.ErrInvalid)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	canonicalRoot, _, err := pathutil.ResolveAbsPath(workspace)
	if err != nil {
		return "", fmt.Errorf("resolve workspace %s: %v: %w", workspace, err, harness.ErrInvalid)
	}
	canonicalPath, _, err := pathutil.ResolveAbsPath(path)
	if err != nil {
		return "", fmt.Errorf("resolve viewer path %s: %v: %w", path, err, harness.ErrInvalid)
	}
	rel, err := filepath.Rel(canonicalRoot, canonicalPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("viewer path %s is outside workspace %s: %w", path, workspace, harness.ErrInvalid)
	}
	if isSensitivePath(canonicalPath) {
		return "", fmt.Errorf("viewer path %s is a sensitive name: %w", canonicalPath, harness.ErrInvalid)
	}
	return canonicalPath, nil
}

// sensitiveNames and sensitiveGlobs are the viewer's sensitive-name
// basename and pattern data.
var sensitiveNames = map[string]bool{
	".env":       true,
	".netrc":     true,
	".npmrc":     true,
	".pypirc":    true,
	"id_rsa":     true,
	"id_ed25519": true,
	"id_ecdsa":   true,
	"id_dsa":     true,
}

var sensitiveGlobs = []string{
	".env.*",
	"*.pem",
	"*.key",
	"*.p12",
	"*.pfx",
	"*.keystore",
	"*.jks",
	"id_rsa.*",
	"id_ed25519.*",
	"id_ecdsa.*",
	"id_dsa.*",
	"credentials*.json",
	"credentials*.yaml",
	"credentials*.yml",
}

// isSensitivePath is the viewer's sensitive-name boundary: a sensitive
// basename or pattern refuses the leaf in the contained viewer.
func isSensitivePath(path string) bool {
	base := filepath.Base(path)
	if sensitiveNames[base] {
		return true
	}
	for _, g := range sensitiveGlobs {
		if matched, _ := filepath.Match(g, base); matched {
			return true
		}
	}
	return false
}
