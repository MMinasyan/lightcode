// Contract coverage for normalization-outcome reuse in the argument-hook
// chain: a hook that returns its input unchanged consumes the chain's current
// accepted outcome — the normalized bytes or the stored producer-normalization
// failure diagnostic — and never re-runs the normalizer; a changed return
// triggers exactly one separate normalization; and a settled invalid original
// never suppresses a valid batch sibling's own preparation.
package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// seqNormalizer is the fixtures' counting, attempt-stamping normalizer: every
// invocation is numbered, a valid non-null object gains a "seq" member naming
// its attempt number, and every rejection carries its attempt in the
// diagnostic — so a reused accepted outcome and a fresh normalization are
// distinguishable byte-for-byte everywhere downstream.
type seqNormalizer struct {
	attempts int
}

func (n *seqNormalizer) normalize(call model.ToolCall) (json.RawMessage, error) {
	n.attempts++
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(call.Arguments, &obj); err != nil || obj == nil {
		return nil, fmt.Errorf("normalize attempt %d rejected", n.attempts)
	}
	obj["seq"] = json.RawMessage(strconv.Quote(strconv.Itoa(n.attempts)))
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func passthroughHook(id string, received *json.RawMessage) ToolArgumentsHook {
	return ToolArgumentsHook{ID: id, ExtensionID: "reuse-ext", Mode: HookModePure, Run: func(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
		*received = model.CloneRaw(call.Arguments)
		return model.CloneRaw(call.Arguments), nil
	}}
}

func changedHook(id string, replacement string) ToolArgumentsHook {
	return ToolArgumentsHook{ID: id, ExtensionID: "reuse-ext", Mode: HookModePure, Run: func(context.Context, model.ToolCall) (json.RawMessage, error) {
		return json.RawMessage(replacement), nil
	}}
}

// runReuseFixture admits the reuse fixture, publishes the given calls through
// its own execution, and returns the pieces the settle rows drive.
func runReuseFixture(t *testing.T, hooks []ToolArgumentsHook, calls ...model.ToolCall) (*Harness, *graphStorage, *coordinator, string, Execution, *hookToolSpy, *seqNormalizer) {
	t.Helper()
	spy := &hookToolSpy{}
	n := &seqNormalizer{}
	exec := hookFixtureExecution(spy.tool, n.normalize, hooks...)
	h, store, c, sessionID := newHookHarness(t, exec)
	publishHookCalls(t, h, c, exec, calls...)
	return h, store, c, sessionID, exec, spy, n
}

func settleCall(t *testing.T, h *Harness, c *coordinator, exec Execution, call model.ToolCall) model.ToolResult {
	t.Helper()
	got, err := h.toolEffect(c, testOpID, exec, openedConversation(t, exec), testCapture())(context.Background(), call, []string{"echo"})
	testassert.NoError(t, err)
	return got
}

func rawCall(id string, arguments string) model.ToolCall {
	return model.ToolCall{ID: id, Name: "echo", Arguments: json.RawMessage(arguments)}
}

func TestArgumentHookNormalizationOutcomeReuse(t *testing.T) {
	t.Run("a valid original returned unchanged consumes the producer normalization without another one", func(t *testing.T) {
		var received json.RawMessage
		h, store, c, sessionID, exec, spy, n := runReuseFixture(t, []ToolArgumentsHook{passthroughHook("reuse.pure", &received)}, rawCall("call-1", `{"x":1}`))
		// The producer normalization at publication is the only invocation.
		testassert.Equal(t, n.attempts, 1)
		got := settleCall(t, h, c, exec, rawCall("call-1", `{"x":1}`))
		testassert.Require(t, got.Status == model.ResultSuccess && got.Content == "done", "settled result = %+v, want the call prepared after the reused accepted outcome", got)
		testassert.Require(t, string(received) == `{"x":1}`, "hook input = %q, want the raw original bytes", string(received))
		prepared := spy.received()
		testassert.Equal(t, len(prepared), 1)
		testassert.Require(t, string(prepared[0].Arguments) == `{"seq":"1","x":1}`, "preparer arguments = %q, want the stored accepted normalization, byte-identical", string(prepared[0].Arguments))
		testassert.Equal(t, n.attempts, 1) // the unchanged return consumed the accepted outcome: no second normalization
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph validation: %v", err)
		}
	})

	t.Run("an invalid original returned unchanged settles with the exact stored diagnostic and no preparation", func(t *testing.T) {
		var received json.RawMessage
		h, store, c, sessionID, exec, spy, n := runReuseFixture(t, []ToolArgumentsHook{passthroughHook("reuse.pure", &received)}, rawCall("call-1", `[1,2]`))
		testassert.Equal(t, n.attempts, 1)
		got := settleCall(t, h, c, exec, rawCall("call-1", `[1,2]`))
		testassert.Require(t, got.Status == model.ResultError && got.Content == "normalize attempt 1 rejected",
			"settled result = %+v, want the call to consume the stored producer diagnostic verbatim", got)
		testassert.Require(t, got.Content != invalidToolResultContent, "settled content = %q, want the stored diagnostic, never generic object-parse text", got.Content)
		testassert.Equal(t, len(spy.received()), 0) // the call was never prepared
		testassert.Equal(t, n.attempts, 1)          // consuming the stored diagnostic runs no normalization
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph validation: %v", err)
		}
	})

	t.Run("a changed replacement runs exactly one separate normalization", func(t *testing.T) {
		h, store, c, sessionID, exec, spy, n := runReuseFixture(t, []ToolArgumentsHook{changedHook("reuse.changer", `{"x":2}`)}, rawCall("call-1", `{"x":1}`))
		testassert.Equal(t, n.attempts, 1)
		got := settleCall(t, h, c, exec, rawCall("call-1", `{"x":1}`))
		testassert.Require(t, got.Status == model.ResultSuccess, "settled result = %+v, want the repaired call prepared", got)
		testassert.Equal(t, n.attempts, 2) // the producer normalization plus exactly one for the changed replacement
		prepared := spy.received()
		testassert.Equal(t, len(prepared), 1)
		testassert.Require(t, string(prepared[0].Arguments) == `{"seq":"2","x":2}`, "preparer arguments = %q, want the freshly normalized replacement", string(prepared[0].Arguments))
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph validation: %v", err)
		}
	})

	t.Run("an unchanged successor consumes the chain's accepted outcome without another normalization", func(t *testing.T) {
		var seen json.RawMessage
		h, store, c, sessionID, exec, spy, n := runReuseFixture(t, []ToolArgumentsHook{
			changedHook("reuse.changer", `{"x":2}`),
			passthroughHook("reuse.successor", &seen),
		}, rawCall("call-1", `{"x":1}`))
		got := settleCall(t, h, c, exec, rawCall("call-1", `{"x":1}`))
		testassert.Require(t, got.Status == model.ResultSuccess, "settled result = %+v, want the repaired call prepared", got)
		testassert.Require(t, string(seen) == `{"seq":"2","x":2}`, "successor input = %q, want the predecessor's committed value", string(seen))
		testassert.Equal(t, n.attempts, 2) // the changed hook normalized once; the unchanged successor consumed that outcome
		prepared := spy.received()
		testassert.Equal(t, len(prepared), 1)
		testassert.Require(t, string(prepared[0].Arguments) == `{"seq":"2","x":2}`, "preparer arguments = %q, want the chain's accepted value, normalized exactly once", string(prepared[0].Arguments))
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph validation: %v", err)
		}
	})

	t.Run("a settled invalid original never suppresses a valid batch sibling", func(t *testing.T) {
		var seen json.RawMessage
		h, store, c, sessionID, exec, spy, n := runReuseFixture(t, []ToolArgumentsHook{passthroughHook("reuse.pure", &seen)},
			rawCall("call-1", `[1,2]`), rawCall("call-2", `{"x":1}`))
		testassert.Equal(t, n.attempts, 2) // the two producer normalizations at publication
		first := settleCall(t, h, c, exec, rawCall("call-1", `[1,2]`))
		testassert.Require(t, first.Status == model.ResultError && first.Content == "normalize attempt 1 rejected",
			"call-1 result = %+v, want the stored diagnostic settling the invalid original", first)
		second := settleCall(t, h, c, exec, rawCall("call-2", `{"x":1}`))
		testassert.Require(t, second.Status == model.ResultSuccess && second.Content == "done", "call-2 result = %+v, want the valid sibling prepared", second)
		prepared := spy.received()
		testassert.Require(t, len(prepared) == 1 && prepared[0].ID == "call-2" && string(prepared[0].Arguments) == `{"seq":"2","x":1}`,
			"prepared calls = %+v, want only the valid sibling over its own accepted normalization", prepared)
		testassert.Equal(t, n.attempts, 2) // neither settled call re-normalized anything
		if _, err := validateFixture(t, store, sessionID); err != nil {
			t.Fatalf("graph validation: %v", err)
		}
	})
}
