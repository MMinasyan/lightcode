package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// The warning-presentation suite: the Runtime-owned store's setup refresh at
// the shared publication path, the prompt group's replace/clear on successful
// preparation with failed preparation changing nothing, the model and
// compact-model transport closures' protocol diagnostics (including a
// physical failure after encoding succeeded), the hydration filter, and the
// revision's independence and dedupe.

// warningOf finds one store warning by source, kind, and session.
func warningOf(warnings []protocol.Warning, source string, kind, sessionID string) *protocol.Warning {
	for i := range warnings {
		warning := &warnings[i]
		if string(warning.Source) == source && warning.Kind == kind && (sessionID == "" || (warning.SessionId != nil && *warning.SessionId == sessionID)) {
			return warning
		}
	}
	return nil
}

// sessionWarningPresent reports whether any stored warning belongs to the
// Session, regardless of source: the presence-only oracle used where the
// lifetime rule, not the attribution rule, is under test.
func sessionWarningPresent(warnings []protocol.Warning, sessionID string) bool {
	for i := range warnings {
		if warnings[i].SessionId != nil && *warnings[i].SessionId == sessionID {
			return true
		}
	}
	return false
}

// TestWarningsPromptReplaceClearAndFailedPreparationNoChange pins the prompt
// group's lifetime over the real production preparation: a successful
// preparation replaces the Session's group, a later warning-free preparation
// clears it, and a failed preparation or hook changes nothing.
func TestWarningsPromptReplaceClearAndFailedPreparationNoChange(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		hook := e.hookedHook()
		r, err := e.open(ctx, hook, &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer closeProjectionRuntime(r)

		session, err := r.createSession(ctx, e.workspace("prompt-ws"), "worker")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		sessionID := session.Identity.SessionID
		promptSub, err := r.Subscribe(256)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		t.Cleanup(promptSub.Close)

		// Successful preparation: the assembled prompt warning replaces the
		// (absent) prior group under the admitted Session identity and its
		// warning_changed hint names the Session scope — the Workspace from
		// the cached identity read — with the advanced revision.
		submitThroughRuntime(t, r, sessionID, "op-1", "please write")
		var promptHint *protocol.WarningChangedEvent
		progressBeforeHint := false
		for promptHint == nil {
			event, ok := nextEvent(t, promptSub)
			if !ok {
				t.Fatal("the prompt subscription closed before the preparation's warning hint")
			}
			switch eventKind(t, event) {
			case "text_delta", "tool_started", "tool_finished":
				// The preparation's publication precedes every effect of the
				// admitted Operation: a warning hint that only arrives after
				// the Operation's progress is not the preparation's hint.
				progressBeforeHint = true
				continue
			case "warning_changed":
				body, err := event.AsWarningChangedEvent()
				if err != nil {
					t.Fatalf("warning event body: %v", err)
				}
				if body.Scope.Kind == protocol.ScopeKindSession && body.Scope.SessionId != nil && *body.Scope.SessionId == sessionID {
					promptHint = &body
				}
			}
		}
		if progressBeforeHint {
			t.Fatal("the Session's first warning hint arrived after the Operation's progress, want the preparation's own publication")
		}
		if promptHint.Scope.Workspace == nil || *promptHint.Scope.Workspace != e.workspace("prompt-ws") {
			t.Fatalf("prompt warning hint scope = %+v, want the cached workspace identity", promptHint.Scope)
		}
		if revision, err := warnRevision(promptHint.WarningsRevision.Revision); err != nil || revision == 0 {
			t.Fatalf("prompt warning hint revision = %q, want an advanced counter", promptHint.WarningsRevision.Revision)
		}
		awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess)
		awaitSessionNotBusy(t, r, sessionID)
		first, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings: %v", err)
		}
		found := warningOf(first.Warnings, "runtime:prompt", "rules_not_found", sessionID)
		if found == nil || found.Message != "No AGENTS.md found" {
			t.Fatalf("prompt warning after successful preparation = %+v, want the assembled rules warning for the session", found)
		}
		if warningOf(first.Warnings, "runtime:setup", "setup_no_model", "") == nil {
			t.Fatalf("global setup warning missing: %+v", first.Warnings)
		}
		if first.WarningsRevision.Revision == "0" {
			t.Fatalf("warning revision = %q, want the advanced counter", first.WarningsRevision.Revision)
		}

		// A warning-free successful preparation replaces the prior group with
		// nothing: the same Session's next preparation clears it.
		if err := os.WriteFile(filepath.Join(e.workspace("prompt-ws"), "AGENTS.md"), []byte("# rules"), 0o600); err != nil {
			t.Fatalf("write AGENTS.md: %v", err)
		}
		submitThroughRuntime(t, r, sessionID, "op-2", "please write")
		awaitOperation(t, r, sessionID, "op-2", harness.OperationSuccess)
		awaitIdleSession(t, r, sessionID)
		cleared, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings cleared: %v", err)
		}
		if warningOf(cleared.Warnings, "runtime:prompt", "rules_not_found", sessionID) != nil {
			t.Fatalf("the warning-free preparation kept the session's prompt warning: %+v", cleared.Warnings)
		}
		if revision, err := warnRevision(cleared.WarningsRevision.Revision); err != nil || revision == 0 {
			t.Fatalf("warning revision after the clearing preparation = %q (%v), want an advanced counter", cleared.WarningsRevision.Revision, err)
		}

		// A failed preparation changes nothing: the missing credential
		// rejects admission before any publication.
		before, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings before failed preparation: %v", err)
		}
		e.t.Setenv("PRODUCTION_TEST_KEY", "")
		failedSession, err := r.createSession(ctx, e.workspace("prompt-ws-3"), "worker")
		if err != nil {
			t.Fatalf("createSession(failed): %v", err)
		}
		if err := submitExpectFailure(r, failedSession.Identity.SessionID, "op-failed"); err == nil {
			t.Fatal("the missing-credential submit succeeded, want the failed preparation")
		}
		e.t.Setenv("PRODUCTION_TEST_KEY", "production-secret-1")

		// A failed hook (capture rejected before publication) changes nothing.
		hook.set(nil, func(c harness.ExecutionCapture) harness.ExecutionCapture {
			c.Capabilities = append(c.Capabilities, "hook.second")
			return c
		})
		hookSession, err := r.createSession(ctx, e.workspace("prompt-ws-4"), "worker")
		if err != nil {
			t.Fatalf("createSession(hook): %v", err)
		}
		if err := submitExpectFailure(r, hookSession.Identity.SessionID, "op-hooked"); err == nil {
			t.Fatal("the rejected-hook submit succeeded, want the failed preparation")
		}
		hook.set(nil, nil)

		after, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings after failures: %v", err)
		}
		if after.WarningsRevision.Revision != before.WarningsRevision.Revision {
			t.Fatalf("warning revision advanced on failed preparations: %s → %s", before.WarningsRevision.Revision, after.WarningsRevision.Revision)
		}
		if !slices.Equal(warningBodies(before.Warnings), warningBodies(after.Warnings)) {
			t.Fatalf("warnings changed on failed preparations:\n%v\n%v", warningBodies(before.Warnings), warningBodies(after.Warnings))
		}
		// No warning hint fired for the failed preparation or hook: the
		// passive stream stays silent for both new Sessions. The earlier
		// successful preparations' own hints may still be queued, so only
		// the original Session's hints are tolerated.
		for {
			select {
			case event, ok := <-promptSub.Events():
				if !ok {
					t.Fatal("the prompt subscription closed around the failed preparations")
				}
				if eventKind(t, event) != "warning_changed" {
					continue
				}
				body, err := event.AsWarningChangedEvent()
				if err != nil {
					t.Fatalf("warning event body: %v", err)
				}
				if body.Scope.SessionId != nil && *body.Scope.SessionId == sessionID {
					continue
				}
				t.Fatalf("a failed preparation published a warning hint: %+v", body)
			default:
			}
			break
		}
	})
}

// awaitSessionNotBusy waits until the Session's actual execution state — the
// current operation, the run slot, the reservation — is clear. Terminal
// settlement is durably visible before the run retires, so the next admission
// needs the settled fact, not just the settled operation record.
func awaitSessionNotBusy(t *testing.T, r *Runtime, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for snapshotThroughRuntime(t, r, sessionID).ExecutionBusy {
		if time.Now().After(deadline) {
			t.Fatalf("session %q never left its busy state", sessionID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// submitExpectFailure submits one regular message expecting the admission to
// fail; the raw error is returned for the caller to ignore or inspect.
func submitExpectFailure(r *Runtime, sessionID, operationID string) error {
	return r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		_, err := h.Submit(ctx, harness.SubmitRequest{
			SessionID: sessionID, OperationID: operationID, Origin: harness.InputOriginUser,
			Content: []model.ContentPart{{Kind: model.PartText, Text: "hello"}}, Mode: harness.MessageModeRegular,
		})
		return err
	})
}

func warnRevision(rev string) (uint64, error) {
	return strconv.ParseUint(rev, 10, 64)
}

func warningBodies(warnings []protocol.Warning) []string {
	out := make([]string, 0, len(warnings))
	for _, warning := range warnings {
		session := ""
		if warning.SessionId != nil {
			session = *warning.SessionId
		}
		out = append(out, string(warning.Source)+"/"+warning.Kind+"/"+warning.Message+"/"+session)
	}
	return out
}

// TestWarningsTwoSessionsHydrationFiltersOwnAndGlobals pins the hydration
// filter over two Sessions in different Workspaces: the unfiltered read
// carries both Sessions' own groups and every global group, while each
// Session's hydration contains the globals plus its own prompt warning and
// never the sibling's.
func TestWarningsTwoSessionsHydrationFiltersOwnAndGlobals(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		// A real catalog warning rides the build: one incomplete model in the
		// configured provider layer (no context window) publishes the
		// retained incomplete_model diagnostic with the captured candidate.
		writeServiceFile(t, e.configPath, strings.Replace(
			productionConfigDocument(e.server.URL, "T1"), `"models": {`, `"models": {"inc": {"name": "I"},`, 1))
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer closeProjectionRuntime(r)

		// Each Session prepares with a distinct prompt warning: an unreadable
		// rules file carries the path in its message.
		first := e.workspace("warn-a")
		second := e.workspace("warn-b")
		if err := os.MkdirAll(filepath.Join(second, "AGENTS.md"), 0o700); err != nil {
			t.Fatalf("mkdir AGENTS.md collision: %v", err)
		}
		sessionA, err := r.createSession(ctx, first, "worker")
		if err != nil {
			t.Fatalf("createSession A: %v", err)
		}
		sessionB, err := r.createSession(ctx, second, "worker")
		if err != nil {
			t.Fatalf("createSession B: %v", err)
		}
		submitThroughRuntime(t, r, sessionA.Identity.SessionID, "op-a", "please write")
		awaitOperation(t, r, sessionA.Identity.SessionID, "op-a", harness.OperationSuccess)
		submitThroughRuntime(t, r, sessionB.Identity.SessionID, "op-b", "please write")
		awaitOperation(t, r, sessionB.Identity.SessionID, "op-b", harness.OperationSuccess)

		all, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings: %v", err)
		}
		if warningOf(all.Warnings, "runtime:prompt", "rules_not_found", sessionA.Identity.SessionID) == nil {
			t.Fatalf("unfiltered read misses session A's prompt warning: %+v", all.Warnings)
		}
		if warningOf(all.Warnings, "runtime:prompt", "rules_read_error", sessionB.Identity.SessionID) == nil {
			t.Fatalf("unfiltered read misses session B's prompt warning: %+v", all.Warnings)
		}
		if warningOf(all.Warnings, "runtime:setup", "setup_no_model", "") == nil {
			t.Fatalf("unfiltered read misses the global setup warning: %+v", all.Warnings)
		}
		catalogWarning := warningOf(all.Warnings, "runtime:catalog", "incomplete_model", "")
		if catalogWarning == nil || !strings.Contains(catalogWarning.Message, "prov/inc") {
			t.Fatalf("unfiltered read misses the real catalog warning: %+v", all.Warnings)
		}
		if warningOf(all.Warnings, "runtime:agents", "invalid_agent_type", "") == nil {
			t.Fatalf("unfiltered read misses the global agents warning: %+v", all.Warnings)
		}

		hydrationA, err := r.buildHydration(ctx, sessionA.Identity.SessionID)
		if err != nil {
			t.Fatalf("hydration A: %v", err)
		}
		hydrationB, err := r.buildHydration(ctx, sessionB.Identity.SessionID)
		if err != nil {
			t.Fatalf("hydration B: %v", err)
		}
		if warningOf(hydrationA.Warnings, "runtime:prompt", "rules_not_found", sessionA.Identity.SessionID) == nil {
			t.Fatalf("hydration A misses its own prompt warning: %+v", hydrationA.Warnings)
		}
		if warningOf(hydrationA.Warnings, "runtime:prompt", "rules_read_error", sessionB.Identity.SessionID) != nil {
			t.Fatalf("hydration A contains the sibling's prompt warning: %+v", hydrationA.Warnings)
		}
		if warningOf(hydrationA.Warnings, "runtime:agents", "invalid_agent_type", "") == nil {
			t.Fatalf("hydration A misses the global agents warning: %+v", hydrationA.Warnings)
		}
		if warningOf(hydrationB.Warnings, "runtime:prompt", "rules_read_error", sessionB.Identity.SessionID) == nil {
			t.Fatalf("hydration B misses its own prompt warning: %+v", hydrationB.Warnings)
		}
		if warningOf(hydrationB.Warnings, "runtime:prompt", "rules_not_found", sessionA.Identity.SessionID) != nil {
			t.Fatalf("hydration B contains the sibling's prompt warning: %+v", hydrationB.Warnings)
		}
		for _, hydration := range []protocol.Hydration{hydrationA, hydrationB} {
			if warningOf(hydration.Warnings, "runtime:setup", "setup_no_model", "") == nil {
				t.Fatalf("hydration misses the global setup warning: %+v", hydration.Warnings)
			}
			if warningOf(hydration.Warnings, "runtime:catalog", "incomplete_model", "") == nil {
				t.Fatalf("hydration misses the global catalog warning: %+v", hydration.Warnings)
			}
			if warningOf(hydration.Warnings, "runtime:agents", "invalid_agent_type", "") == nil {
				t.Fatalf("hydration misses the global agents warning: %+v", hydration.Warnings)
			}
			if hydration.WarningsRevision.Revision != all.WarningsRevision.Revision {
				t.Fatalf("hydration warning revision %q != captured %q", hydration.WarningsRevision.Revision, all.WarningsRevision.Revision)
			}
			if hydration.ConfigurationRevision.Generation != "1" {
				t.Fatalf("hydration configuration revision = %q, want the captured generation", hydration.ConfigurationRevision.Generation)
			}
		}
		// The warning clock is independent of the Session and configuration
		// clocks: a later report from the OTHER Session advances this
		// Session's hydration warning revision while its session revision and
		// the configuration generation stay unchanged.
		next, err := r.createSession(ctx, e.workspace("warn-c"), "worker")
		if err != nil {
			t.Fatalf("createSession C: %v", err)
		}
		submitThroughRuntime(t, r, next.Identity.SessionID, "op-c", "please write")
		awaitOperation(t, r, next.Identity.SessionID, "op-c", harness.OperationSuccess)
		later, err := r.buildHydration(ctx, sessionA.Identity.SessionID)
		if err != nil {
			t.Fatalf("hydration A later: %v", err)
		}
		beforeRevision, revErr := warnRevision(hydrationA.WarningsRevision.Revision)
		afterRevision, afterErr := warnRevision(later.WarningsRevision.Revision)
		if revErr != nil || afterErr != nil || afterRevision <= beforeRevision {
			t.Fatalf("the sibling report did not advance the store-wide warning revision: %q → %q (%v, %v)", hydrationA.WarningsRevision.Revision, later.WarningsRevision.Revision, revErr, afterErr)
		}
		if later.SessionRevision.DurableRevision != hydrationA.SessionRevision.DurableRevision || later.SessionRevision.LocalRevision != hydrationA.SessionRevision.LocalRevision {
			t.Fatalf("a warning advance changed the session revision: %+v → %+v", hydrationA.SessionRevision, later.SessionRevision)
		}
		if later.ConfigurationRevision.Generation != hydrationA.ConfigurationRevision.Generation {
			t.Fatalf("a warning advance changed the configuration generation: %q → %q", hydrationA.ConfigurationRevision.Generation, later.ConfigurationRevision.Generation)
		}

		// The captured revision is coherent: repeated unfiltered reads with no
		// store change report the same revision and list.
		again, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings again: %v", err)
		}
		stable, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings stable: %v", err)
		}
		if stable.WarningsRevision.Revision != again.WarningsRevision.Revision || !slices.Equal(warningBodies(stable.Warnings), warningBodies(again.Warnings)) {
			t.Fatalf("an unchanged store changed its read: %+v vs %+v", stable, again)
		}
		if stable.WarningsRevision.Revision != later.WarningsRevision.Revision {
			t.Fatalf("the full read's revision %q diverged from the captured hydration's %q", stable.WarningsRevision.Revision, later.WarningsRevision.Revision)
		}
	})
}

// TestWarningsModelClosureRecordsThroughRealWork pins the model Stream
// closure over real harness-driven work: the MustPreserve replay diagnostic
// of a tool-call follow-up is recorded under the admitted Session identity,
// identical re-reports dedupe without advancing the revision, and a physical
// transport failure after encoding succeeds still records its diagnostics
// while the failed settlement semantics stay unchanged.
func TestWarningsModelClosureRecordsThroughRealWork(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer closeProjectionRuntime(r)
		session, err := r.createSession(ctx, e.workspace("protocol-ws"), "worker")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		sessionID := session.Identity.SessionID
		protocolSub, err := r.Subscribe(256)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		t.Cleanup(protocolSub.Close)

		// Turn 1: the tool call turn succeeds; the follow-up re-encodes the
		// replay-kept assistant-with-tool-calls message and the missing
		// must-preserve field fires once per attempt. The diagnostic's
		// warning_changed hint is Session-scoped and the settlement stays
		// the Operation's own success.
		submitThroughRuntime(t, r, sessionID, "op-1", "please write")
		var protocolHint *protocol.WarningChangedEvent
		for protocolHint == nil {
			event, ok := nextEvent(t, protocolSub)
			if !ok {
				t.Fatal("the protocol subscription closed before the diagnostic hint")
			}
			if eventKind(t, event) != "warning_changed" {
				continue
			}
			body, err := event.AsWarningChangedEvent()
			if err != nil {
				t.Fatalf("warning event body: %v", err)
			}
			if body.Scope.Kind == protocol.ScopeKindSession && body.Scope.SessionId != nil && *body.Scope.SessionId == sessionID {
				protocolHint = &body
			}
		}
		if protocolHint.Scope.Workspace == nil || *protocolHint.Scope.Workspace != e.workspace("protocol-ws") {
			t.Fatalf("protocol diagnostic hint scope = %+v, want the cached workspace identity", protocolHint.Scope)
		}
		awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess)
		first, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings: %v", err)
		}
		found := warningOf(first.Warnings, "runtime:protocol", "protocol_must_preserve_missing", sessionID)
		if found == nil || found.Message == "" {
			t.Fatalf("protocol warning after the tool turn = %+v, want the MustPreserve diagnostic for the session", found)
		}
		revision := first.WarningsRevision.Revision

		// An identical re-report dedupes: a second identical turn advances
		// nothing.
		next, err := r.createSession(ctx, e.workspace("protocol-ws"), "worker")
		if err != nil {
			t.Fatalf("createSession(next): %v", err)
		}
		submitThroughRuntime(t, r, next.Identity.SessionID, "op-2", "please write")
		awaitOperation(t, r, next.Identity.SessionID, "op-2", harness.OperationSuccess)
		second, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings second: %v", err)
		}
		// The second turn's follow-up request has one more message, so its
		// diagnostic text differs and DOES advance; the identical first
		// warning's (kind,message) is retained exactly once.
		firstReports := 0
		for _, warning := range second.Warnings {
			if warning.Source == "runtime:protocol" && warning.SessionId != nil && *warning.SessionId == sessionID {
				firstReports++
			}
		}
		if firstReports != 1 {
			t.Fatalf("session's identical protocol diagnostics = %d, want one deduped value", firstReports)
		}
		revisionAfterSecond := second.WarningsRevision.Revision
		if revisionAfterSecond == revision {
			t.Fatalf("warning revision unchanged after a distinct report: %q", revision)
		}

		// Post-encode transport failure: reload the published configuration
		// onto a counting endpoint whose first request serves the tool-call
		// turn and whose follow-up answers 401 — encoding succeeded, the
		// physical request failed, the diagnostics are still recorded, and
		// the failed settlement semantics stay unchanged.
		attempts := &countingFailingEndpoint{}
		failingServer := httptest.NewServer(http.HandlerFunc(attempts.serve))
		defer failingServer.Close()
		writeServiceFile(t, e.configPath, productionConfigDocument(failingServer.URL, "T2"))
		if _, err := r.Reload(ctx); err != nil {
			t.Fatalf("Reload to the failing endpoint: %v", err)
		}
		failing, err := r.createSession(ctx, e.workspace("protocol-ws"), "worker")
		if err != nil {
			t.Fatalf("createSession(failing): %v", err)
		}
		submitThroughRuntime(t, r, failing.Identity.SessionID, "op-3", "please write")
		awaitOperation(t, r, failing.Identity.SessionID, "op-3", harness.OperationFailure)
		third, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings after failure: %v", err)
		}
		if warningOf(third.Warnings, "runtime:protocol", "protocol_must_preserve_missing", failing.Identity.SessionID) == nil {
			t.Fatalf("the post-encode transport failure dropped its diagnostics: %+v", third.Warnings)
		}
		if third.WarningsRevision.Revision == revisionAfterSecond {
			t.Fatalf("warning revision unchanged after the failed attempt's report: %q", third.WarningsRevision.Revision)
		}
	})
}

// countingFailingEndpoint serves exactly one tool-call turn and answers 401
// to every later request: the follow-up's encoding succeeds, its physical
// request fails.
type countingFailingEndpoint struct{ count int }

func (e *countingFailingEndpoint) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	e.count++
	if e.count == 1 && lastMessageRole(string(body)) != "tool" {
		writeToolCallTurn(w, "prod_write", productionWriteArgs)
		return
	}
	http.Error(w, `{"error":{"message":"denied"}}`, http.StatusUnauthorized)
}

// TestWarningsCompactClosureRecordsWithPostEncodeFailure pins the compact
// model's transport closure through the real prepared-execution opener over a
// real existing Harness subject: a compaction-shaped real request over the
// compact transport's MustPreserve metadata records its diagnostics under the
// admitted Session identity even when the physical request fails after
// encoding, an identical attempt dedupes, a diagnostic-free attempt replaces
// the group with nothing, a late attempt for a deleted subject changes
// nothing, and reports after owner closure are ignored.
func TestWarningsCompactClosureRecordsWithPostEncodeFailure(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer closeProjectionRuntime(r)

		// One real existing subject: the direct compact opener records under
		// this admitted Session identity, and the new
		// existence-before-publication rule observes the same Harness.
		session, err := r.createSession(ctx, e.workspace("compact-warn"), "worker")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		sessionID := session.Identity.SessionID

		sh := newServiceHarness(t)
		t.Setenv("PREP_COMPACT_KEY", "compact-secret")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, `{"error":{"message":"compact endpoint down"}}`, http.StatusUnauthorized)
		}))
		t.Cleanup(server.Close)
		writeServiceFile(t, sh.configPath, strings.ReplaceAll(warnCompactConfigDocument, "ENDPOINT", server.URL))
		writeServiceFile(t, agents.PathForConfig(sh.configPath), agentsWithCompact(`{"model":"prov/m2"}`))
		owner, cancel := context.WithCancel(context.Background())
		defer cancel()
		comp := mustComposition(t, compactToolsPlugin())
		runtimeScope := mustOpenScope(t, comp, owner, ScopeInfo{Kind: ScopeRuntime, DataDir: sh.dataDir}, nil)
		svc := newConfigurationService(owner, comp, sh.loader, sh.configPath, r.obs)
		svc.attachWarnings(r.warnings)
		if _, err := svc.publish(context.Background()); err != nil {
			t.Fatalf("publish: %v", err)
		}
		ws := newWorkspaceScopes(owner, comp, []*scope{runtimeScope}, r.obs)
		adapter := newObservationAdapter(r.obs, r.warnings)
		adapter.h = r.harness
		p := newPreparation(svc, comp, runtimeScope, ws, sh.home, nil, adapter, nil, nil)

		request := compactPrepRequest()
		request.Session.Identity.SessionID = sessionID
		prepared, err := p.bind()(context.Background(), request)
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		execution, err := prepared.Open(context.Background(), harness.OperationAdmission{
			SessionID:   sessionID,
			OperationID: "op-compact",
			RequestKind: harness.RequestKindMessage,
			AdmittedAt:  time.Now(),
		})
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer func() {
			if err := execution.Close(); err != nil {
				t.Errorf("execution close: %v", err)
			}
		}()

		// The compaction shape's system+user messages carry no diagnostics;
		// an assistant-with-tool-calls message replay-kept for the compact
		// model fires the missing must-preserve field.
		diagnostic := model.Request{Messages: []model.Message{
			{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "summarize"}}},
			{Role: model.RoleAssistant, Source: model.ModelRef{Provider: "prov", Model: "m2"},
				ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}}},
		}}
		if _, err := execution.CompactModel(context.Background(), diagnostic); err == nil {
			t.Fatal("compact transport request over the failing endpoint succeeded, want the transport failure")
		}
		revision, storeWarnings := r.warnings.hydrate(sessionID)
		if warningOf(storeWarnings, "runtime:protocol", "protocol_must_preserve_missing", sessionID) == nil {
			t.Fatalf("the compact closure dropped its post-encode diagnostics: %+v", storeWarnings)
		}

		// An identical attempt replaces with the same list: no advance.
		_, _ = execution.CompactModel(context.Background(), diagnostic)
		deduped, dedupedWarnings := r.warnings.hydrate(sessionID)
		if deduped != revision || !slices.Equal(warningBodies(dedupedWarnings), warningBodies(storeWarnings)) {
			t.Fatalf("an identical attempt advanced the store: revision %d → %d", revision, deduped)
		}

		// A diagnostic-free attempt is a complete computation: it replaces
		// the group with nothing and clears the obsolete value.
		clean := model.Request{Messages: []model.Message{
			{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "summarize"}}},
		}}
		if _, err := execution.CompactModel(context.Background(), clean); err == nil {
			t.Fatal("diagnostic-free compact request over the failing endpoint succeeded, want the transport failure")
		}
		cleared, clearedWarnings := r.warnings.hydrate(sessionID)
		if warningOf(clearedWarnings, "runtime:protocol", "protocol_must_preserve_missing", sessionID) != nil {
			t.Fatalf("the diagnostic-free attempt kept the obsolete diagnostics: %+v", clearedWarnings)
		}
		if cleared == deduped {
			t.Fatalf("clearing the group advanced nothing: revision %d", cleared)
		}

		// The committed deletion removes the subject's groups; a late attempt
		// for the deleted subject cannot reintroduce them.
		if _, err := r.archiveSession(ctx, sessionID); err != nil {
			t.Fatalf("archive: %v", err)
		}
		if err := r.deleteSession(ctx, sessionID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		beforeLate, _ := r.warnings.snapshot()
		_, _ = execution.CompactModel(context.Background(), diagnostic)
		afterLate, lateWarnings := r.warnings.snapshot()
		if afterLate != beforeLate {
			t.Fatalf("a late attempt for the deleted subject advanced the store: %d → %d", beforeLate, afterLate)
		}
		if warningOf(lateWarnings, "runtime:protocol", "protocol_must_preserve_missing", sessionID) != nil {
			t.Fatalf("a late attempt resurrected the deleted subject's diagnostics: %+v", lateWarnings)
		}

		// Reports after the owner's warning-store closure are ignored.
		r.warnings.close()
		_, _ = execution.CompactModel(context.Background(), diagnostic)
		closed, closedWarnings := r.warnings.snapshot()
		if closed != afterLate || !slices.Equal(warningBodies(closedWarnings), warningBodies(lateWarnings)) {
			t.Fatalf("a report after closure changed the store")
		}
	})
}

// warnCompactConfigDocument points both the conversation and the compact
// model at the same failing endpoint with must-preserve metadata.
const warnCompactConfigDocument = `{
  "providers": {
    "prov": {
      "transport": {"base_url": "ENDPOINT", "api_key_env": "PREP_COMPACT_KEY"},
      "discovery": false,
      "models": {
        "m": {"name": "M", "context_window": 4096, "protocol_metadata": {"must_preserve": ["proto_keep"]}},
        "m2": {"name": "M2", "context_window": 2048, "protocol_metadata": {"must_preserve": ["proto_keep"]}}
      }
    }
  }
}`

// TestWarningStoreReadOwnsReturnedWarnings pins the shared read producer's
// ownership: every returned warning — a global group's nil identity and both
// per-Session groups' identities — is a fully owned copy, so mutating the
// returned values (identity pointees, fields, slice shape) never reaches the
// groups, the dedupe, or a later read and report.
func TestWarningStoreReadOwnsReturnedWarnings(t *testing.T) {
	store := newWarningStore()
	managedID := "0123456789abcdef0123456789abcdef"
	store.setGlobal("runtime:setup", []protocol.Warning{{Source: "runtime:setup", Kind: "setup_k", Message: "setup_m"}})
	promptID := managedID
	store.setSessionPrompt(managedID, []protocol.Warning{{Source: "runtime:prompt", Kind: "prompt_k", Message: "prompt_m", SessionId: &promptID}})
	protocolID := managedID
	store.setSessionProtocol(managedID, []protocol.Warning{{Source: "runtime:protocol", Kind: "protocol_k", Message: "protocol_m", SessionId: &protocolID}})

	// The immutable serialized baseline, taken before any mutation.
	revision, warnings := store.snapshot()
	expected, err := json.Marshal(warnings)
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	if len(warnings) != 3 {
		t.Fatalf("baseline warnings = %d (%+v), want the three seeded groups' members", len(warnings), warnings)
	}
	for _, warning := range warnings {
		switch warning.Source {
		case "runtime:setup":
			if warning.SessionId != nil {
				t.Fatalf("global warning carries an identity: %+v", warning)
			}
		default:
			if warning.SessionId == nil || *warning.SessionId != managedID {
				t.Fatalf("%s warning identity = %v, want the owning session", warning.Source, warning.SessionId)
			}
		}
	}

	// Mutate everything reachable through the returned slice.
	for i := range warnings {
		warnings[i].Source = "MUTATED"
		warnings[i].Kind = "MUTATED"
		warnings[i].Message = "MUTATED"
		if warnings[i].SessionId != nil {
			*warnings[i].SessionId = "MUTATED"
		}
	}

	// The re-read is unchanged, byte-for-byte, and the revision stayed.
	again, againWarnings := store.snapshot()
	if again != revision {
		t.Fatalf("warning revision advanced without a report: %d → %d", revision, again)
	}
	got, err := json.Marshal(againWarnings)
	if err != nil {
		t.Fatalf("marshal again: %v", err)
	}
	if string(got) != string(expected) {
		t.Fatalf("a mutation of the returned warnings reached the store:\n%s\nwant\n%s", got, expected)
	}

	// The filtered hydration read is owned the same way.
	_, hydrated := store.hydrate(managedID)
	for i := range hydrated {
		if hydrated[i].SessionId != nil {
			*hydrated[i].SessionId = "MUTATED"
		}
	}
	_, stable := store.hydrate(managedID)
	for _, warning := range stable {
		if warning.SessionId != nil && *warning.SessionId != managedID {
			t.Fatalf("a mutation of a returned hydration identity reached the store: %+v", warning)
		}
	}

	// A next identical report still dedupes correctly against the untouched
	// group: no revision advance, no duplicate.
	sameID := managedID
	store.setSessionPrompt(managedID, []protocol.Warning{{Source: "runtime:prompt", Kind: "prompt_k", Message: "prompt_m", SessionId: &sameID}})
	final, finalWarnings := store.snapshot()
	if final != revision {
		t.Fatalf("an identical report advanced the revision: %d → %d", revision, final)
	}
	finalGot, err := json.Marshal(finalWarnings)
	if err != nil {
		t.Fatalf("marshal final: %v", err)
	}
	if string(finalGot) != string(expected) {
		t.Fatalf("the identical report changed the store's read:\n%s\nwant\n%s", finalGot, expected)
	}
}

// TestWarningStoreReadsDynamicDeterministicGroups pins the shared read
// producer over groups it did not know at compile time: every actually stored
// group is enumerated — global groups before Session groups, then lexical
// Session ID and lexical source — never a fixed source list, so an arbitrary
// plugin source is a first-class group.
func TestWarningStoreReadsDynamicDeterministicGroups(t *testing.T) {
	store := newWarningStore()
	sessionA := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	sessionB := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	idA, idB := sessionA, sessionB
	store.setSessionPrompt(sessionB, []protocol.Warning{{Source: "runtime:prompt", Kind: "b", Message: "b", SessionId: &idB}})
	store.setGlobal("plugin:zeta", []protocol.Warning{{Source: "plugin:zeta", Kind: "z", Message: "z"}})
	store.setSessionPrompt(sessionA, []protocol.Warning{{Source: "runtime:prompt", Kind: "a", Message: "a", SessionId: &idA}})
	store.setGlobal("runtime:setup", []protocol.Warning{{Source: "runtime:setup", Kind: "s", Message: "s"}})

	revision, warnings := store.snapshot()
	if revision != 4 {
		t.Fatalf("revision after four distinct groups = %d, want 4", revision)
	}
	want := []string{
		"plugin:zeta/z/z/",
		"runtime:setup/s/s/",
		"runtime:prompt/a/a/" + sessionA,
		"runtime:prompt/b/b/" + sessionB,
	}
	if got := warningBodies(warnings); !slices.Equal(got, want) {
		t.Fatalf("dynamic read order = %v, want %v", got, want)
	}

	// The Session filter carries every global group plus only that Session's
	// own groups, in the same deterministic order.
	_, hydrated := store.hydrate(sessionA)
	wantHydrated := []string{
		"plugin:zeta/z/z/",
		"runtime:setup/s/s/",
		"runtime:prompt/a/a/" + sessionA,
	}
	if got := warningBodies(hydrated); !slices.Equal(got, wantHydrated) {
		t.Fatalf("filtered read = %v, want %v", got, wantHydrated)
	}

	// Replacing with the identical list and clearing an absent group advance
	// nothing.
	store.setGlobal("runtime:setup", []protocol.Warning{{Source: "runtime:setup", Kind: "s", Message: "s"}})
	store.setGlobal("plugin:absent", nil)
	if after, _ := store.snapshot(); after != revision {
		t.Fatalf("an identical or absent-clear report advanced the revision: %d → %d", revision, after)
	}
}

// TestWarningStoreReplacementAlgebra pins the two mutation shapes: a complete
// computation — one model or compact Stream attempt's returned diagnostics —
// replaces its Session group, including an empty list clearing the obsolete
// value, while individual Runtime-scoped plugin reports append and dedupe
// within their own global group.
func TestWarningStoreReplacementAlgebra(t *testing.T) {
	store := newWarningStore()
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	store.setSessionProtocol(id, []protocol.Warning{{Source: "runtime:protocol", Kind: "k1", Message: "m1", SessionId: &id}})
	store.setSessionProtocol(id, []protocol.Warning{{Source: "runtime:protocol", Kind: "k2", Message: "m2", SessionId: &id}})
	_, warnings := store.snapshot()
	if warningOf(warnings, "runtime:protocol", "k1", id) != nil {
		t.Fatalf("a new attempt kept the previous attempt's diagnostics: %+v", warnings)
	}
	if warningOf(warnings, "runtime:protocol", "k2", id) == nil {
		t.Fatalf("the new attempt's diagnostics are missing: %+v", warnings)
	}
	store.setSessionProtocol(id, nil)
	_, cleared := store.snapshot()
	if warningOf(cleared, "runtime:protocol", "k2", id) != nil {
		t.Fatalf("an empty attempt kept the obsolete diagnostics: %+v", cleared)
	}

	store.appendPlugin("lsp", "notice", "one")
	store.appendPlugin("lsp", "notice", "one")
	store.appendPlugin("lsp", "other", "two")
	_, plugins := store.snapshot()
	if warningOf(plugins, "plugin:lsp", "notice", "") == nil || warningOf(plugins, "plugin:lsp", "other", "") == nil {
		t.Fatalf("plugin reports are missing from their own global group: %+v", plugins)
	}
	count := 0
	for _, warning := range plugins {
		if string(warning.Source) == "plugin:lsp" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("plugin group members after the identical report = %d, want the deduped pair", count)
	}

	// A nil store is inert for every mutation: the shared core's nil guard
	// covers the append path too.
	var absent *warningStore
	if absent.appendPlugin("lsp", "k", "m") || absent.setGlobal("runtime:setup", nil) ||
		absent.setSessionProtocol(id, nil) || absent.removeSessions([]string{id}) {
		t.Fatal("a nil store mutated")
	}
}

// TestWarningsFailedForkAndAdmissionCreateNoOrphanGroup proves prompt
// presentation is published only by the committed opener: a fork and an
// admission whose transactions fail after preparation both leave the warning
// store byte-identical, while a later successful admission still publishes its
// own Session's group.
func TestWarningsFailedForkAndAdmissionCreateNoOrphanGroup(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		wrapped := newSweepStore(e.store)
		e.store = wrapped
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer closeProjectionRuntime(r)

		// A real committed source with a valid boundary: the source's own
		// successful preparation already published its prompt group.
		source, err := r.createSession(ctx, e.workspace("orphan-src"), "worker")
		if err != nil {
			t.Fatalf("createSession: %v", err)
		}
		sourceID := source.Identity.SessionID
		submitThroughRuntime(t, r, sourceID, "op-src", "boundary")
		awaitOperation(t, r, sourceID, "op-src", harness.OperationSuccess)
		awaitIdleSession(t, r, sourceID)
		snap := snapshotThroughRuntime(t, r, sourceID)
		boundaryItem := projectItemID(sourceID, commandUserInputEntry(t, snap, "op-src"))

		beforeFork, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings before fork: %v", err)
		}
		forkFailure := errors.New("test fork transaction failure")
		wrapped.armFailAfterRelease(forkFailure)
		forkDone := make(chan error, 1)
		go func() {
			_, err := r.forkSession(ctx, sourceID, protocol.ForkRequest{
				BoundaryItemId: boundaryItem,
				OperationId:    "op-orphan-fork",
				Content:        []protocol.ContentPart{commandTextPart(t, "forked")},
			})
			forkDone <- err
		}()
		select {
		case <-wrapped.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the failed fork never reached its transaction")
		}
		wrapped.releaseBlock()
		if err := <-forkDone; !errors.Is(err, forkFailure) {
			t.Fatalf("failed fork = %v, want the injected transaction failure", err)
		}
		afterFork, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings after fork: %v", err)
		}
		if afterFork.WarningsRevision.Revision != beforeFork.WarningsRevision.Revision {
			t.Fatalf("failed fork advanced the warning revision: %s → %s", beforeFork.WarningsRevision.Revision, afterFork.WarningsRevision.Revision)
		}
		if !slices.Equal(warningBodies(beforeFork.Warnings), warningBodies(afterFork.Warnings)) {
			t.Fatalf("failed fork changed the warning store:\n%v\n%v", warningBodies(beforeFork.Warnings), warningBodies(afterFork.Warnings))
		}

		// The failed-admission sibling: the preparation runs, the admission
		// transaction fails, and the preparation's prompt values stay
		// unpublished.
		victim, err := r.createSession(ctx, e.workspace("orphan-admit"), "worker")
		if err != nil {
			t.Fatalf("createSession(victim): %v", err)
		}
		beforeAdmit, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings before admission: %v", err)
		}
		admissionFailure := errors.New("test admission transaction failure")
		wrapped.armFailAfterRelease(admissionFailure)
		admitDone := make(chan error, 1)
		go func() {
			admitDone <- submitExpectFailure(r, victim.Identity.SessionID, "op-orphan-admit")
		}()
		select {
		case <-wrapped.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the failed admission never reached its transaction")
		}
		wrapped.releaseBlock()
		if err := <-admitDone; !errors.Is(err, admissionFailure) {
			t.Fatalf("failed admission = %v, want the injected transaction failure", err)
		}
		afterAdmit, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings after admission: %v", err)
		}
		if afterAdmit.WarningsRevision.Revision != beforeAdmit.WarningsRevision.Revision {
			t.Fatalf("failed admission advanced the warning revision: %s → %s", beforeAdmit.WarningsRevision.Revision, afterAdmit.WarningsRevision.Revision)
		}
		if !slices.Equal(warningBodies(beforeAdmit.Warnings), warningBodies(afterAdmit.Warnings)) {
			t.Fatalf("failed admission changed the warning store:\n%v\n%v", warningBodies(beforeAdmit.Warnings), warningBodies(afterAdmit.Warnings))
		}

		// The oracle flips: an unarmed successful admission publishes its own
		// Session's prompt group and advances the revision.
		good, err := r.createSession(ctx, e.workspace("orphan-ok"), "worker")
		if err != nil {
			t.Fatalf("createSession(good): %v", err)
		}
		submitThroughRuntime(t, r, good.Identity.SessionID, "op-good", "please write")
		awaitOperation(t, r, good.Identity.SessionID, "op-good", harness.OperationSuccess)
		afterSuccess, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings after success: %v", err)
		}
		if afterSuccess.WarningsRevision.Revision == afterAdmit.WarningsRevision.Revision {
			t.Fatalf("a successful admission did not publish its prompt group: revision stayed %s", afterSuccess.WarningsRevision.Revision)
		}
		if warningOf(afterSuccess.Warnings, "runtime:prompt", "rules_not_found", good.Identity.SessionID) == nil {
			t.Fatalf("the successful admission's prompt group is missing: %+v", afterSuccess.Warnings)
		}
	})
}

// TestWarningsDeleteArchiveAndLateReportLifetime proves the Session group
// lifetime: archive retains the group, a committed deletion removes it in one
// observation section with one runtime-scoped warning_changed hint, a sibling
// Session's group survives, and late prompt or protocol reports for the
// deleted subject can neither write the group nor publish a hint.
func TestWarningsDeleteArchiveAndLateReportLifetime(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer closeProjectionRuntime(r)

		victim, err := r.createSession(ctx, e.workspace("warn-delete"), "worker")
		if err != nil {
			t.Fatalf("createSession(victim): %v", err)
		}
		sibling, err := r.createSession(ctx, e.workspace("warn-keep"), "worker")
		if err != nil {
			t.Fatalf("createSession(sibling): %v", err)
		}
		victimID, siblingID := victim.Identity.SessionID, sibling.Identity.SessionID
		submitThroughRuntime(t, r, victimID, "op-victim", "please write")
		awaitOperation(t, r, victimID, "op-victim", harness.OperationSuccess)
		submitThroughRuntime(t, r, siblingID, "op-sibling", "please write")
		awaitOperation(t, r, siblingID, "op-sibling", harness.OperationSuccess)

		// Archive retains the group; hydration of the archived subject still
		// carries it.
		if _, err := r.archiveSession(ctx, victimID); err != nil {
			t.Fatalf("archive: %v", err)
		}
		archived, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings after archive: %v", err)
		}
		if !sessionWarningPresent(archived.Warnings, victimID) {
			t.Fatalf("archive dropped the Session's warning group: %+v", archived.Warnings)
		}
		hydration, err := r.buildHydration(ctx, victimID)
		if err != nil {
			t.Fatalf("hydration of the archived subject: %v", err)
		}
		if !sessionWarningPresent(hydration.Warnings, victimID) {
			t.Fatalf("the archived subject's hydration dropped its group: %+v", hydration.Warnings)
		}

		sub, err := r.Subscribe(256)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		t.Cleanup(sub.Close)
		baseRevision, _ := r.warnings.snapshot()

		// The committed deletion removes the group in one observation section
		// and publishes exactly one runtime-scoped hint.
		if err := r.deleteSession(ctx, victimID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		afterDelete, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings after delete: %v", err)
		}
		if sessionWarningPresent(afterDelete.Warnings, victimID) {
			t.Fatalf("the committed deletion kept the Session's group: %+v", afterDelete.Warnings)
		}
		if warningOf(afterDelete.Warnings, "runtime:prompt", "rules_not_found", siblingID) == nil {
			t.Fatalf("the deletion removed the sibling's group: %+v", afterDelete.Warnings)
		}
		if _, err := r.buildHydration(ctx, victimID); !errors.Is(err, harness.ErrNotFound) {
			t.Fatalf("hydration of the deleted subject = %v, want the typed absence", err)
		}

		var hint *protocol.WarningChangedEvent
		for hint == nil {
			event, ok := nextEvent(t, sub)
			if !ok {
				t.Fatal("the subscription closed before the deletion's warning hint")
			}
			if eventKind(t, event) != "warning_changed" {
				continue
			}
			body, err := event.AsWarningChangedEvent()
			if err != nil {
				t.Fatalf("warning event body: %v", err)
			}
			if body.Scope.Kind != protocol.ScopeKindRuntime {
				t.Fatalf("deletion warning hint scope = %+v, want the runtime scope", body.Scope)
			}
			hint = &body
		}
		if hint.WarningsRevision.Revision != strconv.FormatUint(baseRevision+1, 10) {
			t.Fatalf("deletion warning hint revision = %q, want the single advance from %d", hint.WarningsRevision.Revision, baseRevision)
		}
		assertNoEvent(t, sub)

		// Late reports for the deleted subject through a real adapter bound to
		// the owner's Harness: neither prompt nor protocol can reintroduce the
		// group or publish a hint.
		late := newObservationAdapter(r.obs, r.warnings)
		late.h = r.harness
		lateID := victimID
		late.publishPrompt(lateID, []protocol.Warning{{Source: "runtime:prompt", Kind: "late_prompt", Message: "late", SessionId: &lateID}})
		late.replaceProtocolWarnings(lateID, []protocol.Warning{{Source: "runtime:protocol", Kind: "late_protocol", Message: "late", SessionId: &lateID}})
		assertNoEvent(t, sub)
		lateRead, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings after late reports: %v", err)
		}
		if lateRead.WarningsRevision.Revision != afterDelete.WarningsRevision.Revision {
			t.Fatalf("a late report advanced the warning revision: %s → %s", afterDelete.WarningsRevision.Revision, lateRead.WarningsRevision.Revision)
		}
		if !slices.Equal(warningBodies(afterDelete.Warnings), warningBodies(lateRead.Warnings)) {
			t.Fatalf("a late report changed the store:\n%v\n%v", warningBodies(afterDelete.Warnings), warningBodies(lateRead.Warnings))
		}

		// A committed deletion with no owned groups is a store no-op: no
		// revision advance and no warning hint.
		bare, err := r.createSession(ctx, e.workspace("warn-bare"), "worker")
		if err != nil {
			t.Fatalf("createSession(bare): %v", err)
		}
		if _, err := r.archiveSession(ctx, bare.Identity.SessionID); err != nil {
			t.Fatalf("archive(bare): %v", err)
		}
		beforeBare, _ := r.warnings.snapshot()
		if err := r.deleteSession(ctx, bare.Identity.SessionID); err != nil {
			t.Fatalf("delete(bare): %v", err)
		}
		if afterBare, _ := r.warnings.snapshot(); afterBare != beforeBare {
			t.Fatalf("a no-op deletion advanced the warning revision: %d → %d", beforeBare, afterBare)
		}
		for {
			select {
			case event, ok := <-sub.Events():
				if !ok {
					t.Fatal("the subscription closed around the no-op deletion")
				}
				if eventKind(t, event) == "warning_changed" {
					t.Fatalf("a no-op deletion published a warning hint: %+v", event)
				}
				continue
			default:
			}
			break
		}
	})
}
