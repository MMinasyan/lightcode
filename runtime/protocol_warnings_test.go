package runtime

import (
	"context"
	"encoding/json"
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
func warningOf(warnings []protocol.Warning, source protocol.WarningSource, kind, sessionID string) *protocol.Warning {
	for i := range warnings {
		warning := &warnings[i]
		if warning.Source == source && warning.Kind == kind && (sessionID == "" || (warning.SessionId != nil && *warning.SessionId == sessionID)) {
			return warning
		}
	}
	return nil
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

		// Successful preparation: the assembled prompt warning replaces the
		// (absent) prior group under the admitted Session identity.
		submitThroughRuntime(t, r, sessionID, "op-1", "please write")
		awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess)
		awaitSessionNotBusy(t, r, sessionID)
		first, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings: %v", err)
		}
		found := warningOf(first.Warnings, "prompt", "rules_not_found", sessionID)
		if found == nil || found.Message != "No AGENTS.md found" {
			t.Fatalf("prompt warning after successful preparation = %+v, want the assembled rules warning for the session", found)
		}
		if warningOf(first.Warnings, "setup", "setup_no_model", "") == nil {
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
		if warningOf(cleared.Warnings, "prompt", "rules_not_found", sessionID) != nil {
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
		if warningOf(all.Warnings, "prompt", "rules_not_found", sessionA.Identity.SessionID) == nil {
			t.Fatalf("unfiltered read misses session A's prompt warning: %+v", all.Warnings)
		}
		if warningOf(all.Warnings, "prompt", "rules_read_error", sessionB.Identity.SessionID) == nil {
			t.Fatalf("unfiltered read misses session B's prompt warning: %+v", all.Warnings)
		}
		if warningOf(all.Warnings, "setup", "setup_no_model", "") == nil {
			t.Fatalf("unfiltered read misses the global setup warning: %+v", all.Warnings)
		}
		catalogWarning := warningOf(all.Warnings, "catalog", "incomplete_model", "")
		if catalogWarning == nil || !strings.Contains(catalogWarning.Message, "prov/inc") {
			t.Fatalf("unfiltered read misses the real catalog warning: %+v", all.Warnings)
		}
		if warningOf(all.Warnings, "agents", "invalid_agent_type", "") == nil {
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
		if warningOf(hydrationA.Warnings, "prompt", "rules_not_found", sessionA.Identity.SessionID) == nil {
			t.Fatalf("hydration A misses its own prompt warning: %+v", hydrationA.Warnings)
		}
		if warningOf(hydrationA.Warnings, "prompt", "rules_read_error", sessionB.Identity.SessionID) != nil {
			t.Fatalf("hydration A contains the sibling's prompt warning: %+v", hydrationA.Warnings)
		}
		if warningOf(hydrationA.Warnings, "agents", "invalid_agent_type", "") == nil {
			t.Fatalf("hydration A misses the global agents warning: %+v", hydrationA.Warnings)
		}
		if warningOf(hydrationB.Warnings, "prompt", "rules_read_error", sessionB.Identity.SessionID) == nil {
			t.Fatalf("hydration B misses its own prompt warning: %+v", hydrationB.Warnings)
		}
		if warningOf(hydrationB.Warnings, "prompt", "rules_not_found", sessionA.Identity.SessionID) != nil {
			t.Fatalf("hydration B contains the sibling's prompt warning: %+v", hydrationB.Warnings)
		}
		for _, hydration := range []protocol.Hydration{hydrationA, hydrationB} {
			if warningOf(hydration.Warnings, "setup", "setup_no_model", "") == nil {
				t.Fatalf("hydration misses the global setup warning: %+v", hydration.Warnings)
			}
			if warningOf(hydration.Warnings, "catalog", "incomplete_model", "") == nil {
				t.Fatalf("hydration misses the global catalog warning: %+v", hydration.Warnings)
			}
			if warningOf(hydration.Warnings, "agents", "invalid_agent_type", "") == nil {
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

		// Turn 1: the tool call turn succeeds; the follow-up re-encodes the
		// replay-kept assistant-with-tool-calls message and the missing
		// must-preserve field fires once per attempt.
		submitThroughRuntime(t, r, sessionID, "op-1", "please write")
		awaitOperation(t, r, sessionID, "op-1", harness.OperationSuccess)
		first, err := r.getWarnings(ctx)
		if err != nil {
			t.Fatalf("getWarnings: %v", err)
		}
		found := warningOf(first.Warnings, "protocol", "protocol_must_preserve_missing", sessionID)
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
			if warning.Source == "protocol" && warning.SessionId != nil && *warning.SessionId == sessionID {
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
		if warningOf(third.Warnings, "protocol", "protocol_must_preserve_missing", failing.Identity.SessionID) == nil {
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
// model's transport closure through the real prepared-execution opener: a
// compaction-shaped real request over the compact transport's MustPreserve
// metadata records its diagnostics under the admission's Session identity
// even when the physical request fails after encoding, dedupes identical
// re-reports, and ignores every report after owner closure.
func TestWarningsCompactClosureRecordsWithPostEncodeFailure(t *testing.T) {
	{
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
		obs := newObservation()
		warnings := newWarningStore()
		svc := newConfigurationService(owner, comp, sh.loader, sh.configPath, obs)
		svc.attachWarnings(warnings)
		if _, err := svc.publish(context.Background()); err != nil {
			t.Fatalf("publish: %v", err)
		}
		ws := newWorkspaceScopes(owner, comp, []*scope{runtimeScope}, obs)
		p := newPreparation(svc, comp, runtimeScope, ws, sh.home, nil, warnings, nil, nil)

		prepared, err := p.bind()(context.Background(), compactPrepRequest())
		if err != nil {
			t.Fatalf("bind: %v", err)
		}
		sessionID := compactPrepRequest().Session.Identity.SessionID
		execution, err := prepared.Open(context.Background(), harness.OperationAdmission{
			SessionID:   sessionID,
			OperationID: "op-compact",
			RequestKind: harness.RequestKindMessage,
			AdmittedAt:  time.Now(),
		})
		if err != nil {
			t.Fatalf("open: %v", err)
		}

		// The compaction shape's system+user messages carry no diagnostics;
		// an assistant-with-tool-calls message replay-kept for the compact
		// model fires the missing must-preserve field.
		request := model.Request{Messages: []model.Message{
			{Role: model.RoleUser, Content: []model.ContentPart{{Kind: model.PartText, Text: "summarize"}}},
			{Role: model.RoleAssistant, Source: model.ModelRef{Provider: "prov", Model: "m2"},
				ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{}`)}}},
		}}
		if _, err := execution.CompactModel(context.Background(), request); err == nil {
			t.Fatal("compact transport request over the failing endpoint succeeded, want the transport failure")
		}
		revision, storeWarnings := warnings.hydrate(sessionID)
		if warningOf(storeWarnings, "protocol", "protocol_must_preserve_missing", sessionID) == nil {
			t.Fatalf("the compact closure dropped its post-encode diagnostics: %+v", storeWarnings)
		}
		if revision == 0 {
			t.Fatalf("warning revision = %d, want the advanced counter", revision)
		}

		// An identical re-report dedupes and advances nothing.
		_, _ = execution.CompactModel(context.Background(), request)
		deduped, dedupedWarnings := warnings.hydrate(sessionID)
		if deduped != revision || !slices.Equal(warningBodies(dedupedWarnings), warningBodies(storeWarnings)) {
			t.Fatalf("an identical re-report advanced the store: revision %d → %d", revision, deduped)
		}

		// Reports after the owner's warning-store closure are ignored.
		warnings.close()
		_, _ = execution.CompactModel(context.Background(), request)
		closed, closedWarnings := warnings.hydrate(sessionID)
		if closed != deduped || !slices.Equal(warningBodies(closedWarnings), warningBodies(dedupedWarnings)) {
			t.Fatalf("a report after closure changed the store")
		}
		if err := execution.Close(); err != nil {
			t.Fatalf("execution close: %v", err)
		}
	}
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
	store.setGlobal("setup", []protocol.Warning{{Source: "setup", Kind: "setup_k", Message: "setup_m"}})
	promptID := managedID
	store.setSessionPrompt(managedID, []protocol.Warning{{Source: "prompt", Kind: "prompt_k", Message: "prompt_m", SessionId: &promptID}})
	protocolID := managedID
	store.appendProtocol(managedID, []protocol.Warning{{Source: "protocol", Kind: "protocol_k", Message: "protocol_m", SessionId: &protocolID}})

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
		case "setup":
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
	store.setSessionPrompt(managedID, []protocol.Warning{{Source: "prompt", Kind: "prompt_k", Message: "prompt_m", SessionId: &sameID}})
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
