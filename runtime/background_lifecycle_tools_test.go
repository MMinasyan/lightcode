package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// The real-jobs background-lifecycle rows: the jobs service, the run_command
// background start, and the process tool run against real background
// processes through the composed tools fixture — the directly constructed
// baseline tools over the real jobs service on one data root and a real
// Harness — with the per-Session job limit, the retained immediate and limit
// texts, and the per-Session stop scope proving no process-global stop.

// backgroundIDPattern extracts one started background job identity from a
// retained immediate result.
var backgroundIDPattern = regexp.MustCompile("Command running in the background with ID: `([0-9a-f]{8})`")

// backgroundJobID extracts the last started background job identity from one
// model request's accumulated text.
func backgroundJobID(text string) string {
	matches := backgroundIDPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return ""
	}
	return matches[len(matches)-1][1]
}

// lifecycleRequestText joins every text part of every message of one model
// request.
func lifecycleRequestText(req model.Request) string {
	var b strings.Builder
	for _, msg := range req.Messages {
		for _, part := range msg.Content {
			if part.Kind == model.PartText {
				b.WriteString(part.Text)
			}
		}
	}
	return b.String()
}

// requestUserText returns the last user message's text of one model request.
func requestUserText(req model.Request) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role != "user" {
			continue
		}
		var text strings.Builder
		for _, part := range req.Messages[i].Content {
			if part.Kind == model.PartText {
				text.WriteString(part.Text)
			}
		}
		return text.String()
	}
	return ""
}

// lastToolText returns the final tool message's text content — empty when
// the final message is not a tool result.
func lastToolText(req model.Request) string {
	if len(req.Messages) == 0 {
		return ""
	}
	last := req.Messages[len(req.Messages)-1]
	if last.Role != "tool" {
		return ""
	}
	var text strings.Builder
	for _, part := range last.Content {
		if part.Kind == model.PartText {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

// requestMessageRole returns the wire role of one model request's final
// message: a tool result ends the current operation's follow-up turn, while
// an earlier operation's tool results must not short-circuit a new one.
func requestMessageRole(req model.Request) model.Role {
	if len(req.Messages) == 0 {
		return ""
	}
	return req.Messages[len(req.Messages)-1].Role
}

// lifecycleFindRuntimeInput returns one Session's first runtime-origin input
// entry whose text contains want, failing when absent.
func lifecycleFindRuntimeInput(t *testing.T, th *toolsHarness, sessionID, want string) string {
	t.Helper()
	entries, err := th.store.ReadEntries(context.Background(), sessionID, 0)
	testassert.NoError(t, err)
	for _, entry := range entries {
		if entry.Kind != harness.EntryInput {
			continue
		}
		var wire struct {
			OperationID string `json:"operation_id"`
			Origin      string `json:"origin"`
			Content     []struct {
				Kind string `json:"kind"`
				Text string `json:"text"`
			} `json:"content"`
		}
		testassert.NoError(t, json.Unmarshal(entry.Payload, &wire))
		if wire.Origin != "runtime" {
			continue
		}
		text := ""
		for _, part := range wire.Content {
			if part.Kind == "text" {
				text += part.Text
			}
		}
		if strings.Contains(text, want) {
			return wire.OperationID
		}
	}
	t.Fatalf("no runtime-origin input containing %q on session %s", want, sessionID)
	return ""
}

// TestBackgroundLifecycleJobsAcrossSessions proves the jobs rows against
// real processes: the run_command background start and the process actions
// against real short-lived processes in two Sessions with the retained
// immediate and limit texts, the per-Session limit enforcing the jobs
// capability's own bound, and stopping one Session killing only its own job
// while the other Session's job stays live — no process-global stop.
func TestBackgroundLifecycleJobsAcrossSessions(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		testJobsAcrossSessions(t, storage.NewMemory())
	})
	t.Run("sqlite", func(t *testing.T) {
		testJobsAcrossSessions(t, nil)
	})
}

// testJobsAcrossSessions drives the real two-Session row against one store:
// the managed key is absent from the spawned job's environment and the
// external key is present.
func testJobsAcrossSessions(t *testing.T, store harness.Storage) {
	t.Setenv("LIGHTCODE_BG_EXTERNAL", "external-value")
	testassert.NoError(t, os.Unsetenv("LIGHTCODE_BG_MANAGED"))
	background := &harnessBackground{}
	// The managed key connects through a real manager AFTER the tools
	// plugin opened: the call-time subprocess-environment producer, not any
	// Open-captured copy, must scrub it from the spawned job.
	managed := config.NewManagedEnvForTest(filepath.Join(t.TempDir(), ".env"))
	// The read loop's bounded rendezvous: every retry gets a fresh call ID,
	// and the final assertion reads this last one — valid only after the
	// loop observed the real output.
	reads := 0
	readCallID := "call-b-read"
	modelFn := func(_ context.Context, req model.Request) (model.Stream, error) {
		prompt := requestUserText(req)
		if requestMessageRole(req) == "tool" {
			// Readiness rendezvous through the model loop: while the turn
			// following the read call still lacks the process output (the
			// capture returned its placeholder), emit another read instead
			// of the terminal turn — no sleep, no window, no stale poll.
			if prompt == "read b" && !strings.Contains(lastToolText(req), "b-out|external-value|") {
				reads++
				if reads > 20 {
					return nil, fmt.Errorf("the background job output never became readable after %d reads", reads)
				}
				id := backgroundJobID(lifecycleRequestText(req))
				if id == "" {
					return nil, fmt.Errorf("no background job identity in the conversation")
				}
				readCallID = fmt.Sprintf("call-b-read-%d", reads)
				return toolCallStream(readCallID, "process", fmt.Sprintf(`{"action":"read","id":%q}`, id)), nil
			}
			return stopStream("done"), nil
		}
		switch prompt {
		case "start a":
			return toolCallStream("call-a-start", "run_command", `{"command":"printf a-out; sleep 30","background":true}`), nil
		case "limit a":
			return toolCallStream("call-a-limit", "run_command", `{"command":"true","background":true}`), nil
		case "list a":
			return toolCallStream("call-a-list", "process", `{"action":"list"}`), nil
		case "start b":
			return toolCallStream("call-b-start", "run_command", `{"command":"printf 'b-out|%s|%s' \"$LIGHTCODE_BG_EXTERNAL\" \"$LIGHTCODE_BG_MANAGED\"; sleep 30","background":true}`), nil
		case "list b":
			return toolCallStream("call-b-list", "process", `{"action":"list"}`), nil
		case "list b again":
			return toolCallStream("call-b-list-again", "process", `{"action":"list"}`), nil
		case "read b":
			id := backgroundJobID(lifecycleRequestText(req))
			if id == "" {
				return nil, fmt.Errorf("no background job identity in the conversation")
			}
			return toolCallStream("call-b-read", "process", fmt.Sprintf(`{"action":"read","id":%q}`, id)), nil
		default:
			return stopStream("done"), nil
		}
	}
	invocation, err := ConfiguredInvocationForTest("", `{"max_background_processes":1}`, "", nil)
	testassert.NoError(t, err)
	th := newToolsHarnessWith(t, modelFn, toolsHarnessOpts{
		realJobs:      true,
		advertise:     []string{"run_command", "process"},
		background:    background,
		invocation:    invocation,
		subprocessEnv: managed.SubprocessEnv,
		store:         store,
	})
	// The connect happens after the plugin scope opened, before any command
	// starts.
	testassert.NoError(t, managed.TrySet("LIGHTCODE_BG_MANAGED", "managed-secret"))
	sessionA := th.createSession()
	sessionB := th.createSession()

	// Session A starts one real background process and is rejected at the
	// per-Session limit on the second start.
	th.submit(sessionA, "op-a1", "start a")
	{
		rec := th.awaitSettled(sessionA, "op-a1")
		testassert.Require(t, rec.State.Status == harness.OperationSuccess, "op-a1 settled %q: %+v", rec.State.Status, rec.State.Terminal)
	}
	resultsA := th.readToolResults(sessionA)
	immediate := resultsA["call-a-start"].Content
	testassert.Require(t, strings.HasPrefix(immediate, "Command running in the background with ID: `"), "A immediate result = %q, want the retained background template", immediate)
	jobA := backgroundJobID(immediate)
	testassert.Require(t, jobA != "", "A immediate result = %q carries no job identity", immediate)
	th.submitAllowBuffered(sessionA, "op-a2", "limit a")
	th.awaitSettled(sessionA, "op-a2")
	resultsA = th.readToolResults(sessionA)
	wantLimit := "run_command: background start: process: background process limit reached (1/1). Kill existing processes or wait for them to exit"
	testassert.Equal(t, resultsA["call-a-limit"].Content, wantLimit)

	// Session B's own start succeeds: the limit is per-Session.
	th.submit(sessionB, "op-b1", "start b")
	th.awaitSettled(sessionB, "op-b1")
	resultsB := th.readToolResults(sessionB)
	immediateB := resultsB["call-b-start"].Content
	jobB := backgroundJobID(immediateB)
	testassert.Require(t, jobB != "" && jobB != jobA, "B immediate result = %q carries no distinct job identity", immediateB)
	th.submitAllowBuffered(sessionB, "op-b2", "list b")
	th.awaitSettled(sessionB, "op-b2")
	resultsB = th.readToolResults(sessionB)
	listB := resultsB["call-b-list"].Content
	testassert.Require(t, strings.Contains(listB, jobB) && strings.Contains(listB, "(running for") && !strings.Contains(listB, jobA), "B list = %q, want the running line of B's own job %s; B list = %q leaks A's job %s", listB, jobB, listB, jobA)

	// Stopping Session A kills only A's job; B's job stays live.
	testassert.NoError(t, th.h.Stop(context.Background(), sessionA))
	completionOp := lifecycleFindRuntimeInput(t, th, sessionA, "Background process "+jobA)
	th.awaitSettled(sessionA, completionOp)

	// B's job is still running and readable after A's stop.
	th.submit(sessionB, "op-b3", "list b again")
	th.awaitSettled(sessionB, "op-b3")
	resultsB = th.readToolResults(sessionB)
	{
		listAfter := resultsB["call-b-list-again"].Content
		testassert.Require(t, strings.Contains(listAfter, jobB) && strings.Contains(listAfter, "(running for"), "B list after A's stop = %q, want B's job %s still running", listAfter, jobB)
	}
	th.submitAllowBuffered(sessionB, "op-b4", "read b")
	rec := th.awaitSettled(sessionB, "op-b4")
	testassert.Require(t, rec.State.Status == harness.OperationSuccess, "op-b4 settled %q: %+v", rec.State.Status, rec.State)
	resultsB = th.readToolResults(sessionB)
	// The read loop terminated only after its final call observed the real
	// output, so the last call's result is the readiness proof.
	got := resultsB[readCallID].Content
	testassert.Require(t, strings.Contains(got, "b-out|external-value|") && !strings.Contains(got, "managed-secret"), "%s = %q, want the live job's output with the external key present and the managed key absent", readCallID, got)

	// A's killed job left no records behind.
	th.submitAllowBuffered(sessionA, "op-a3", "list a")
	th.awaitSettled(sessionA, "op-a3")
	resultsA = th.readToolResults(sessionA)
	testassert.Equal(t, resultsA["call-a-list"].Content, "No background processes.")

	// The managed shutdown kills B's stray job through the capability's own
	// scope close (the test's composed-scope cleanup), so the suite leaves no
	// process behind.
}
