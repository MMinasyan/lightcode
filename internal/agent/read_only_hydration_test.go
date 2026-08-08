package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// newSharedHomeAgentPair builds two owners over the same projects root with
// distinct project roots, so one owner's sessions are persisted-only for the
// other and the first owner's live sessions hold their claims against it.
func newSharedHomeAgentPair(t *testing.T) (*Agent, *Agent) {
	t.Helper()
	home := t.TempDir()
	first := newCatalogBackedTestAgentForRoot(t, home, t.TempDir())
	second := newCatalogBackedTestAgentForRoot(t, home, t.TempDir())
	return first, second
}

// TestHydrateSessionReadOnlyRootForPersistedSession verifies that a root
// session that is not live in this owner hydrates as a read-only root — the
// persisted summary, the durable transcript, and the read-only flag — instead
// of falling through to the child shape, which carries no session identity
// and no read-only marker.
func TestHydrateSessionReadOnlyRootForPersistedSession(t *testing.T) {
	first, second := newSharedHomeAgentPair(t)
	id, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, err := first.AppendUserMessageToSession(id, "durable from the driving owner"); err != nil {
		t.Fatalf("AppendUserMessageToSession: %v", err)
	}

	hs, err := second.HydrateSession(id)
	if err != nil {
		t.Fatalf("HydrateSession on a non-live root: %v", err)
	}
	if !hs.ReadOnly {
		t.Fatal("non-live root hydration is not read-only")
	}
	if hs.Session.ID != id {
		t.Fatalf("read-only hydration session = %q, want %q", hs.Session.ID, id)
	}
	if hs.Session.ProjectPath == "" {
		t.Fatalf("read-only hydration carries no project path: %+v", hs.Session)
	}
	if got := userContents(hs.Messages); !equalStrings(got, []string{"durable from the driving owner"}) {
		t.Fatalf("read-only hydration messages = %#v, want the durable transcript", got)
	}
	// Nothing live is captured: tokens read zero, and no tail, queue, model,
	// or activity state is claimed.
	if len(hs.Tail) != 0 || len(hs.Errors) != 0 || len(hs.Queue.Items) != 0 || hs.Tokens.Total.Input != 0 || hs.Busy {
		t.Fatalf("read-only hydration carries live state: tail=%d errors=%d queue=%d tokens=%+v busy=%v",
			len(hs.Tail), len(hs.Errors), len(hs.Queue.Items), hs.Tokens, hs.Busy)
	}
}

// TestHydrateSessionReadOnlyBranchLeavesChildrenToChildPath verifies the
// read-only root branch does not intercept child sessions: a child id still
// hydrates as a child transcript, with no root summary and no read-only flag,
// and an unknown id still errors through the child fallback.
func TestHydrateSessionReadOnlyBranchLeavesChildrenToChildPath(t *testing.T) {
	first, second := newSharedHomeAgentPair(t)
	parentID, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession parent: %v", err)
	}

	first.ensureRuntime().mu.Lock()
	childStore, err := snapshot.NewForSessionsRoot(first.projects.SessionsRoot(first.session.projectID), first.projects.Root(), first.session.projectID)
	if err != nil {
		first.ensureRuntime().mu.Unlock()
		t.Fatalf("child store: %v", err)
	}
	if err := childStore.BeginChildSession(first.ProjectRoot(), parentID); err != nil {
		first.ensureRuntime().mu.Unlock()
		t.Fatalf("BeginChildSession: %v", err)
	}
	childID := childStore.SessionID()
	first.ensureRuntime().mu.Unlock()

	hs, err := second.HydrateSession(childID)
	if err != nil {
		t.Fatalf("HydrateSession(child): %v", err)
	}
	if hs.ReadOnly {
		t.Fatal("a child id hydrated as a read-only root")
	}
	if hs.Session.ID != "" {
		t.Fatalf("child hydration carries a root summary: %+v", hs.Session)
	}
	if _, err := second.HydrateSession("missing-session"); err == nil {
		t.Fatal("HydrateSession(missing) = nil error, want unknown session")
	}
}

// TestHydrateSessionCorruptMetaReturnsError pins the non-live branch's
// metadata-read failure: a persisted root session whose meta.json cannot be
// parsed must surface that error instead of falling through to the child
// path, which would present an empty-identity success. Corrupting the
// compaction record instead would reach a different failure and pass either
// way, so the metadata is corrupted specifically. The same session with
// meta.json deleted still errors through the child path, and must continue
// to.
func TestHydrateSessionCorruptMetaReturnsError(t *testing.T) {
	first, second := newSharedHomeAgentPair(t)
	id, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if _, err := first.AppendUserMessageToSession(id, "durable from the driving owner"); err != nil {
		t.Fatalf("AppendUserMessageToSession: %v", err)
	}
	proj, err := second.projectForExistingSession(id)
	if err != nil {
		t.Fatalf("projectForExistingSession: %v", err)
	}
	metaPath := filepath.Join(second.projects.SessionsRoot(proj.ID), id, "meta.json")

	if err := os.WriteFile(metaPath, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = second.HydrateSession(id)
	if err == nil {
		t.Fatal("HydrateSession on a corrupt meta.json = nil error, want the metadata read failure")
	}
	if !strings.Contains(err.Error(), "meta.json") {
		t.Fatalf("HydrateSession on a corrupt meta.json error = %q, want the metadata read failure", err)
	}

	// Nearest sibling: the same session with meta.json deleted already errors
	// through the child path, and must still.
	if err := os.Remove(metaPath); err != nil {
		t.Fatal(err)
	}
	if _, err := second.HydrateSession(id); err == nil {
		t.Fatal("HydrateSession on a deleted meta.json = nil error, want an error")
	}
}

// TestContendedSessionErrorIsRecognisedByErrorsIs pins the wrapped-sentinel
// contract the read-only open depends on: an explicit open of a session the
// other owner drives fails with acquireClaimLocked's %w wrap of the snapshot
// sentinel, so contention is recognisable with errors.Is and never with an
// equality comparison.
func TestContendedSessionErrorIsRecognisedByErrorsIs(t *testing.T) {
	first, second := newSharedHomeAgentPair(t)
	id, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	// The first owner keeps the session live, so its claim is held for the
	// whole test. The second owner's explicit open must fail with contention.
	_, err = second.OpenSession(id)
	if err == nil {
		t.Fatal("OpenSession on a session the other owner drives = nil error, want contention")
	}
	if !errors.Is(err, snapshot.ErrSessionContended) {
		t.Fatalf("OpenSession error = %v, want errors.Is(err, snapshot.ErrSessionContended)", err)
	}
	if err == snapshot.ErrSessionContended {
		t.Fatal("contention error matched by equality — the sentinel arrives wrapped, so only errors.Is can recognise it")
	}
}

// TestContendedSessionArchiveReportsTheDrivenElsewhereMessage pins the wording
// claimPersistedOnlySession builds for a contended persisted-only mutation: the
// user-facing message without the snapshot package prefix.
func TestContendedSessionArchiveReportsTheDrivenElsewhereMessage(t *testing.T) {
	first, second := newSharedHomeAgentPair(t)
	id, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	err = second.SessionArchive(id)
	if err == nil {
		t.Fatal("SessionArchive on a session the other owner drives = nil error, want contention")
	}
	want := fmt.Sprintf("session %q is being driven by another process", id)
	if err.Error() != want {
		t.Fatalf("SessionArchive error = %q, want %q", err.Error(), want)
	}
}

// TestSessionContendedErrorMessage pins the exported contention-message
// constructor's wording and its absence of the snapshot package prefix.
func TestSessionContendedErrorMessage(t *testing.T) {
	err := SessionContendedError("sess-1")
	if err == nil || err.Error() != `session "sess-1" is being driven by another process` {
		t.Fatalf("SessionContendedError = %v", err)
	}
	if strings.Contains(err.Error(), "snapshot:") {
		t.Fatalf("contention message carries the package prefix: %v", err)
	}
}

// TestSessionSummaryForSessionOrPersistedResolvesLiveAndPersisted verifies the
// live-or-persisted summary method resolves a live id exactly as
// SessionSummaryForSession does and a persisted-only id from its project
// record, while the live-only method keeps refusing the same persisted id.
func TestSessionSummaryForSessionOrPersistedResolvesLiveAndPersisted(t *testing.T) {
	first, second := newSharedHomeAgentPair(t)
	id, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	live, err := first.SessionSummaryForSessionOrPersisted(id)
	if err != nil {
		t.Fatalf("live resolution: %v", err)
	}
	if live.ID != id || live.ProjectPath == "" {
		t.Fatalf("live summary = %+v", live)
	}

	persisted, err := second.SessionSummaryForSessionOrPersisted(id)
	if err != nil {
		t.Fatalf("persisted resolution: %v", err)
	}
	if persisted.ID != id {
		t.Fatalf("persisted summary id = %q, want %q", persisted.ID, id)
	}
	if persisted.ProjectPath != live.ProjectPath {
		t.Fatalf("persisted summary project = %q, want live %q", persisted.ProjectPath, live.ProjectPath)
	}
	if persisted.CreatedAt == "" || persisted.LastActivity == 0 || persisted.State != snapshot.StateActive {
		t.Fatalf("persisted summary = %+v", persisted)
	}

	if _, err := second.SessionSummaryForSession(id); err == nil {
		t.Fatal("live-only SessionSummaryForSession resolved a non-live id")
	}
	if _, err := second.SessionSummaryForSessionOrPersisted("missing-session"); err == nil {
		t.Fatal("SessionSummaryForSessionOrPersisted(missing) = nil error")
	}
}

// TestSessionSummaryForSessionOrPersistedTakesIdentityFromResolution pins the
// two fields the persisted path must not take from the metadata: the id that
// resolved and the project record's path. A metadata record that declares a
// different id and a stale project path must not leak either into the summary.
func TestSessionSummaryForSessionOrPersistedTakesIdentityFromResolution(t *testing.T) {
	first, second := newSharedHomeAgentPair(t)
	firstProject, err := first.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	// Plant a session whose metadata declares another id and a stale project
	// path, exactly the mismatch a read-only presentation must not surface.
	const dirID = "resolved-by-directory"
	dir := filepath.Join(first.projects.SessionsRoot(firstProject.ID), dirID)
	if err := os.MkdirAll(filepath.Join(dir, "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "turns"), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"declared-elsewhere","state":"active","project_path":"/stale/path"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := second.SessionSummaryForSessionOrPersisted(dirID)
	if err != nil {
		t.Fatalf("resolve planted session: %v", err)
	}
	if summary.ID != dirID {
		t.Fatalf("summary id = %q, want the id that resolved %q", summary.ID, dirID)
	}
	if summary.ProjectPath != firstProject.Path {
		t.Fatalf("summary project = %q, want the resolved project's path %q", summary.ProjectPath, firstProject.Path)
	}
}

// TestOpenSessionTakesIdentityFromDirectory proves the live open takes the
// session's identity from the directory it opened, not from the metadata: a
// directory whose meta declares another id opens as the directory's id, so
// routing state and every boundary built from the open agree with the
// directory that resolved.
func TestOpenSessionTakesIdentityFromDirectory(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	proj, err := a.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	const dirID = "dirA"
	dir := filepath.Join(a.projects.SessionsRoot(proj.ID), dirID)
	if err := os.MkdirAll(filepath.Join(dir, "snapshots"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "turns"), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := `{"id":"dirB","state":"active","project_path":"` + proj.Path + `"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}

	summary, err := a.OpenSession(dirID)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	if summary.ID != dirID {
		t.Fatalf("open summary id = %q, want the directory's id %q", summary.ID, dirID)
	}
	if summary.ProjectPath != proj.Path {
		t.Fatalf("open summary project = %q, want %q", summary.ProjectPath, proj.Path)
	}
}

// TestStartupResumeSkipsContendedAndUnloadableCandidates proves Init's resume
// scan skips a candidate another process drives and one whose history fails to
// load, so neither blocks startup from producing a fresh session.
func TestStartupResumeSkipsContendedAndUnloadableCandidates(t *testing.T) {
	first, second := newSharedHomeAgentPair(t)
	projectPath := second.ProjectRoot()
	if _, err := first.NewSessionForProjectPath(projectPath, "primary"); err != nil {
		t.Fatalf("NewSessionForProjectPath held: %v", err)
	}
	// Plant an active session whose history cannot load: valid meta, broken
	// compaction record. Its activity is older than the held session's, so the
	// scan meets the contended candidate first.
	proj, err := second.ensureProjectForPath(projectPath)
	if err != nil {
		t.Fatalf("ensure project: %v", err)
	}
	const unloadableID = "unloadable-session"
	sessionDir := filepath.Join(second.projects.SessionsRoot(proj.ID), unloadableID)
	if err := os.MkdirAll(filepath.Join(sessionDir, "turns"), 0o700); err != nil {
		t.Fatal(err)
	}
	meta := fmt.Sprintf(`{"id":%q,"state":"active","project_path":%q,"last_activity":%d}`+"\n",
		unloadableID, projectPath, time.Now().Unix()-100)
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), []byte(meta), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "compaction.json"), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if resumed := second.Init(ctx); resumed != "" {
		t.Fatalf("Init resumed %q, want no resume: a contended and an unloadable candidate must both be skipped", resumed)
	}
}
