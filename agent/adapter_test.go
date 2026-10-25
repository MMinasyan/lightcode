package agent_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/agent"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

func requireBoundaryViolation(t *testing.T, err error, want string) {
	t.Helper()
	var pe *agent.ProtocolError
	testassert.Require(t, errors.Is(err, agent.ErrBoundaryProtocol) && errors.As(err, &pe) && pe.Boundary == want && pe.Detail != "", "err = %v, want a typed boundary-protocol violation naming %q with non-empty detail", err, want)
}

func unavailableResult(call model.ToolCall) model.ToolResult {
	return model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: fmt.Sprintf("Tool %q is not available.", call.Name)}
}

func echoCall(id, name string) model.ToolCall {
	return model.ToolCall{ID: id, Name: name, Arguments: json.RawMessage(`{}`)}
}

// rejectCallback fails the test if an adapter dispatches model work it should never have accepted.
func rejectCallback(t *testing.T) agent.AssemblyCallback {
	return func(model.ModelRef, model.Stream) (model.Output, error) {
		t.Errorf("assembly callback ran unexpectedly")
		return model.Output{}, errors.New("unexpected assembly")
	}
}

// TestAdaptToolsCopiesMapAtConstruction pins the construction barrier: replacing a registered entry and adding a new one after AdaptTools returns cannot reach the adapter — the original implementation still runs and the late name settles unavailable as if never registered.
func TestAdaptToolsCopiesMapAtConstruction(t *testing.T) {
	ranOriginal, ranReplacement := 0, 0
	tools := map[string]agent.ToolFunc{"echo": func(_ context.Context, c model.ToolCall) (model.ToolResult, error) {
		ranOriginal++
		return model.ToolResult{CallID: c.ID, Status: model.ResultSuccess, Content: "original"}, nil
	}}
	effect := agent.AdaptTools(tools)
	tools["echo"] = func(_ context.Context, c model.ToolCall) (model.ToolResult, error) {
		ranReplacement++
		return model.ToolResult{CallID: c.ID, Status: model.ResultSuccess, Content: "replacement"}, nil
	}
	tools["late"] = func(_ context.Context, c model.ToolCall) (model.ToolResult, error) {
		t.Errorf("tool registered after construction ran")
		return model.ToolResult{CallID: c.ID, Status: model.ResultSuccess}, nil
	}

	res, err := effect(context.Background(), echoCall("c1", "echo"), []string{"echo"})
	testassert.NoError(t, err)
	testassert.Require(t, ranOriginal == 1 && ranReplacement == 0 && res.Content == "original", "dispatch after map mutation = %+v (original ran %d, replacement ran %d), want the construction-time implementation only", res, ranOriginal, ranReplacement)

	res, err = effect(context.Background(), echoCall("c2", "late"), []string{"late"})
	testassert.NoError(t, err)
	testassert.Equal(t, res, unavailableResult(echoCall("c2", "late")))
}

// TestAdaptToolsUnavailableGate pins both halves of the gate: a registered name absent from the advertised names, and a call name missing from the copied registry, each settle the ordinary unavailable-tool error result without invoking any registered implementation.
func TestAdaptToolsUnavailableGate(t *testing.T) {
	ran := 0
	tools := map[string]agent.ToolFunc{"echo": func(_ context.Context, c model.ToolCall) (model.ToolResult, error) {
		ran++
		return model.ToolResult{CallID: c.ID, Status: model.ResultSuccess}, nil
	}}
	effect := agent.AdaptTools(tools)

	rows := []struct {
		name  string
		call  model.ToolCall
		names []string
	}{
		{"registered-but-unadvertised", echoCall("c1", "echo"), []string{"other"}},
		{"unregistered", echoCall("c2", "ghost"), []string{"echo"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			res, err := effect(context.Background(), row.call, row.names)
			testassert.NoError(t, err)
			testassert.Equal(t, res, unavailableResult(row.call))
			testassert.Require(t, ran == 0, "registered implementation ran %d times for a gated call, want 0", ran)
		})
	}
}

// TestAdaptToolsNilRegisteredFunc pins that a selected nil ToolFunc is answered by the ordinary unavailable-tool error result under the same one gate rule as an unadvertised or unregistered name: no implementation is ever invoked.
func TestAdaptToolsNilRegisteredFunc(t *testing.T) {
	effect := agent.AdaptTools(map[string]agent.ToolFunc{"x": nil})
	res, err := effect(context.Background(), echoCall("c1", "x"), []string{"x"})
	testassert.NoError(t, err)
	testassert.Equal(t, res, unavailableResult(echoCall("c1", "x")))
}

// TestAdaptToolsForwardsUnchanged pins that an invoked implementation's result and error come back verbatim: the adapter validates and reinterprets nothing on either value.
func TestAdaptToolsForwardsUnchanged(t *testing.T) {
	errBoom := errors.New("implementation blew up")
	want := model.ToolResult{CallID: "c1", Status: model.ResultDenied, Content: "no"}
	effect := agent.AdaptTools(map[string]agent.ToolFunc{"x": func(_ context.Context, c model.ToolCall) (model.ToolResult, error) {
		return want, errBoom
	}})
	res, err := effect(context.Background(), echoCall("c1", "x"), []string{"x"})
	testassert.Equal(t, res, want)
	testassert.Check(t, err == errBoom, "error = %v, want the implementation error forwarded verbatim", err)
}

// twoToolCallSSE is a streaming body publishing two completed tool calls in one turn: the first name the fixture advertises, the second a registered name the request never advertised.
const twoToolCallSSE = `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"working"}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"echo","arguments":"{}"}},{"index":1,"id":"c2","type":"function","function":{"name":"ghost","arguments":"{}"}}]}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`

// TestRunToolEffectNamesArePerCallOwned proves the loop's per-call ownership of the advertised names: a wrapper ToolEffect that mutates the names slice it received in place after the first sibling's dispatch cannot hand the next sibling a different advertisement — the unadvertised registered name stays gated out, its implementation never runs, the unavailable settlement keeps the batch eligible, and the run still settles successfully.
func TestRunToolEffectNamesArePerCallOwned(t *testing.T) {
	ref := model.ModelRef{Provider: "acme", Model: "m-1"}
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		if attempts == 1 {
			fmt.Fprint(w, twoToolCallSSE)
		} else {
			fmt.Fprint(w, finalTextSSE)
		}
	}))
	defer srv.Close()
	transport, err := model.NewTransport(model.ResolvedTransport{Model: ref, BaseURL: srv.URL})
	testassert.NoError(t, err)

	echoRan, ghostRan := 0, 0
	inner := agent.AdaptTools(map[string]agent.ToolFunc{
		"echo": func(_ context.Context, c model.ToolCall) (model.ToolResult, error) {
			echoRan++
			return model.ToolResult{CallID: c.ID, Status: model.ResultSuccess, Content: "echoed"}, nil
		},
		"ghost": func(_ context.Context, c model.ToolCall) (model.ToolResult, error) {
			ghostRan++
			return model.ToolResult{CallID: c.ID, Status: model.ResultSuccess, Content: "ghost implementation ran"}, nil
		},
	})
	var secondCallNames []string
	var secondCallResult model.ToolResult
	mutatedTo := ""
	toolEffect := func(ctx context.Context, call model.ToolCall, names []string) (model.ToolResult, error) {
		res, err := inner(ctx, call, names)    // the adapter gate consults the names exactly as handed.
		if call.ID == "c1" && len(names) > 0 { // after the first sibling settles, mutate the received names slice in place.
			names[0] = "ghost"
			mutatedTo = names[0]
		}
		if call.ID == "c2" {
			secondCallNames = append([]string(nil), names...)
			secondCallResult = res
		}
		return res, err
	}

	res, err := agent.Run(context.Background(), agent.Invocation{
		ExpectedModel: ref,
		Request: func(context.Context) (model.Request, error) {
			return model.Request{Messages: []model.Message{mustMessage(t, model.Message{
				Role:    model.RoleUser,
				Content: []model.ContentPart{{Kind: model.PartText, Text: "run the tools"}},
			})}, Tools: []model.ToolDefinition{{Name: "echo", Description: "echo", Parameters: json.RawMessage(`{"type":"object"}`)}}}, nil
		},
		ModelEffect: agent.AdaptModelEffect(ref, transport),
		ToolEffect:  toolEffect,
	})
	testassert.NoError(t, err)
	testassert.Require(t, res.Status == agent.TerminalSuccess, "terminal = %+v, want success: the unavailable settlement never stops the batch", res)
	testassert.Require(t, mutatedTo == "ghost", "the wrapper's in-place mutation of the received names did not happen: %q", mutatedTo)
	testassert.Require(t, echoRan == 1, "echo ran %d times, want the advertised call dispatched once", echoRan)
	testassert.Require(t, ghostRan == 0, "the unadvertised registered tool ran %d times, want 0: a sibling's names mutation granted it no authority", ghostRan)
	testassert.Require(t, len(secondCallNames) == 1 && secondCallNames[0] == "echo", "second call names = %v, want its own pristine [echo] copy", secondCallNames)
	testassert.Require(t, secondCallResult.Status == model.ResultError && secondCallResult.CallID == "c2" && secondCallResult.Content == `Tool "ghost" is not available.`, "second call settled %+v, want the ordinary unavailable-tool error", secondCallResult)
	testassert.Require(t, res.LastOutput != nil && res.LastOutput.Message.TextContent() == "all done", "last output = %+v, want the final text after the continued batch", res.LastOutput)
	testassert.Require(t, attempts == 2, "transport attempts = %d, want the batch to continue to a second model round", attempts)
}

// TestAdaptToolsEmptyMapZeroToolRun pins that an empty (here nil) tool map is a valid effect for a run whose request advertises no tools and whose response carries none: the composition reaches terminal success without any dispatch.
func TestAdaptToolsEmptyMapZeroToolRun(t *testing.T) {
	ref := model.ModelRef{Provider: "acme", Model: "m-1"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, finalTextSSE)
	}))
	defer srv.Close()
	transport, err := model.NewTransport(model.ResolvedTransport{Model: ref, BaseURL: srv.URL})
	testassert.NoError(t, err)

	requestSource := func(context.Context) (model.Request, error) {
		return model.Request{Messages: []model.Message{mustMessage(t, model.Message{
			Role:    model.RoleUser,
			Content: []model.ContentPart{{Kind: model.PartText, Text: "no tools here"}},
		})}}, nil
	}

	res, err := agent.Run(context.Background(), agent.Invocation{
		ExpectedModel: ref,
		Request:       requestSource,
		ModelEffect:   agent.AdaptModelEffect(ref, transport),
		ToolEffect:    agent.AdaptTools(nil),
	})
	testassert.Require(t, err == nil && res.Status == agent.TerminalSuccess, "run = %+v/%v, want terminal success", res, err)
}

// TestAdaptModelEffectRejectsNilTransportAndIncompleteSource pins both pre-dispatch guards as typed boundary-protocol violations naming the "model" boundary with a zero settlement, before any transport attempt runs — the incomplete-source row proves it with a recording server that saw nothing.
func TestAdaptModelEffectRejectsNilTransportAndIncompleteSource(t *testing.T) {
	complete := model.ModelRef{Provider: "acme", Model: "m-1"}
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	transport, err := model.NewTransport(model.ResolvedTransport{Model: complete, BaseURL: srv.URL})
	testassert.NoError(t, err)

	request := model.Request{Messages: []model.Message{mustMessage(t, model.Message{
		Role:    model.RoleUser,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}},
	})}}

	rows := []struct {
		name   string
		source model.ModelRef
		t      *model.Transport
	}{
		{"nil-transport", complete, nil},
		{"incomplete-source", model.ModelRef{Provider: "acme"}, transport},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			before := attempts
			set, err := agent.AdaptModelEffect(row.source, row.t)(context.Background(), request, rejectCallback(t))
			requireBoundaryViolation(t, err, "model")
			testassert.Equal(t, set, agent.ModelSettlement{})
			testassert.Check(t, attempts == before, "rejected adapter attempted %d transport dispatches, want none", attempts-before)
		})
	}
}

// TestAdaptModelEffectTransportFailureSettlesFailure pins the pre-acceptance failure mapping: a non-2xx attempt on a live run context settles the failure disposition with the transport error's text as detail and no output, never a Go error.
func TestAdaptModelEffectTransportFailureSettlesFailure(t *testing.T) {
	ref := model.ModelRef{Provider: "acme", Model: "m-1"}
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"error":{"message":"provider said no"}}`)
	}))
	defer srv.Close()
	transport, err := model.NewTransport(model.ResolvedTransport{Model: ref, BaseURL: srv.URL})
	testassert.NoError(t, err)

	request := model.Request{Messages: []model.Message{mustMessage(t, model.Message{
		Role:    model.RoleUser,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}},
	})}}
	set, err := agent.AdaptModelEffect(ref, transport)(context.Background(), request, rejectCallback(t))
	testassert.NoError(t, err)
	testassert.Require(t, set.Disposition == agent.DispoFailure && set.Output == nil && set.Detail != "", "settlement = %+v, want failure with no output and non-empty detail", set)
	testassert.Check(t, strings.Contains(set.Detail, "500"), "failure detail = %q, want it to carry the transport status failure", set.Detail)
	testassert.Equal(t, attempts, 1) // no hidden retry: the failure mapping makes exactly one physical attempt.
	_, verr := agent.ValidateModelSettlement(ref, set)
	testassert.NoError(t, verr)
}

// TestAdaptModelEffectCancellationSettlesInterruption pins that a cancellation-shaped transport failure under a genuinely done run context settles the interruption disposition with the shared interruption detail and no output — an active model effect never reports cancellation as a Go error.
func TestAdaptModelEffectCancellationSettlesInterruption(t *testing.T) {
	ref := model.ModelRef{Provider: "acme", Model: "m-1"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, finalTextSSE)
	}))
	defer srv.Close()
	transport, err := model.NewTransport(model.ResolvedTransport{Model: ref, BaseURL: srv.URL})
	testassert.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the run context is already done before the attempt starts.

	request := model.Request{Messages: []model.Message{mustMessage(t, model.Message{
		Role:    model.RoleUser,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}},
	})}}
	set, err := agent.AdaptModelEffect(ref, transport)(ctx, request, rejectCallback(t))
	testassert.NoError(t, err)
	testassert.Require(t, set.Disposition == agent.DispoInterruption && set.Output == nil && set.Detail == "agent interrupted", "settlement = %+v, want interruption with no output and the shared interruption detail", set)
	_, verr := agent.ValidateModelSettlement(ref, set)
	testassert.NoError(t, verr)
}

// TestAdaptModelEffectPreservesCallbackError pins the assembler-error route: when the assembly callback fails over an accepted stream, the adapter returns that exact error verbatim with a zero settlement — never reinterpreted as one — and releases the callback-rejected stream, which the follow-up Recv reporting io.EOF proves.
func TestAdaptModelEffectPreservesCallbackError(t *testing.T) {
	ref := model.ModelRef{Provider: "acme", Model: "m-1"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, finalTextSSE)
	}))
	defer srv.Close()
	transport, err := model.NewTransport(model.ResolvedTransport{Model: ref, BaseURL: srv.URL})
	testassert.NoError(t, err)

	errScript := errors.New("scripted assembler failure")
	var accepted model.Stream
	cb := func(_ model.ModelRef, stream model.Stream) (model.Output, error) {
		accepted = stream // capture the accepted stream so the assertion below can prove the adapter released it.
		return model.Output{}, errScript
	}

	request := model.Request{Messages: []model.Message{mustMessage(t, model.Message{
		Role:    model.RoleUser,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}},
	})}}
	set, err := agent.AdaptModelEffect(ref, transport)(context.Background(), request, cb)
	testassert.Check(t, err == errScript, "err = %v, want the callback error returned verbatim", err)
	testassert.Equal(t, set, agent.ModelSettlement{})
	_, rerr := accepted.Recv()
	testassert.Require(t, errors.Is(rerr, io.EOF), "Recv after the callback failure = %v, want io.EOF: the adapter must have closed the callback-rejected stream instead of leaving it open", rerr)
}

// erroredSSE is a streaming body with a partial payload under an unsupported finish reason: assembly finalizes it as an errored output that retains the partial assistant text.
const erroredSSE = `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"partial"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}

data: [DONE]

`

// TestAdaptModelEffectOutputMapping pins the finalized-output side of the settlement algebra over live streams: an errored assembly maps to failure retaining the partial output and its own detail, and an interrupted assembly maps to interruption carrying its output and detail.
func TestAdaptModelEffectOutputMapping(t *testing.T) {
	ref := model.ModelRef{Provider: "acme", Model: "m-1"}
	assemble := func(ctx context.Context) agent.AssemblyCallback {
		return func(source model.ModelRef, stream model.Stream) (model.Output, error) {
			return agent.Assemble(ctx, source, stream)
		}
	}
	request := model.Request{Messages: []model.Message{mustMessage(t, model.Message{
		Role:    model.RoleUser,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "x"}},
	})}}

	t.Run("errored-output-settles-failure-retaining-partial", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, erroredSSE)
		}))
		defer srv.Close()
		transport, err := model.NewTransport(model.ResolvedTransport{Model: ref, BaseURL: srv.URL})
		testassert.NoError(t, err)

		ctx := context.Background()
		set, err := agent.AdaptModelEffect(ref, transport)(ctx, request, assemble(ctx))
		testassert.NoError(t, err)
		testassert.Require(t, set.Disposition == agent.DispoFailure && set.Output != nil && set.Output.Status == model.OutputErrored && set.Detail != "", "settlement = %+v, want failure carrying the errored output with non-empty detail", set)
		testassert.Check(t, set.Detail == set.Output.Detail, "failure detail = %q, want the output's own detail preserved", set.Detail)
		testassert.Check(t, set.Output.Message != nil && set.Output.Message.TextContent() == "partial", "retained partial = %+v, want the errored output to carry its assistant payload", set.Output.Message)
		_, verr := agent.ValidateModelSettlement(ref, set)
		testassert.NoError(t, verr)
	})

	t.Run("interrupted-output-settles-interruption", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // accepts the attempt, then carries the response open with no body until the client cancels.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		defer srv.Close()
		transport, err := model.NewTransport(model.ResolvedTransport{Model: ref, BaseURL: srv.URL})
		testassert.NoError(t, err)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		cb := func(source model.ModelRef, stream model.Stream) (model.Output, error) {
			cancel() // cancellation arrives after acceptance, while the assembly is reading the live stream.
			return agent.Assemble(ctx, source, stream)
		}

		set, err := agent.AdaptModelEffect(ref, transport)(ctx, request, cb)
		testassert.NoError(t, err)
		testassert.Require(t, set.Disposition == agent.DispoInterruption && set.Output != nil && set.Output.Status == model.OutputInterrupted && set.Detail != "", "settlement = %+v, want interruption carrying the interrupted output with non-empty detail", set)
		testassert.Check(t, set.Detail == set.Output.Detail, "interruption detail = %q, want the output's own detail preserved", set.Detail)
		_, verr := agent.ValidateModelSettlement(ref, set)
		testassert.NoError(t, verr)
	})
}
