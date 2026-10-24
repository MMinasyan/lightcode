package agent_test

import (
	"context"
	"encoding/json"
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

// toolCallSSE and finalTextSSE are OpenAI-compatible streaming bodies the fake provider serves in order: the first response is a completed tool call, the second is the final text answer.
const toolCallSSE = `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"running "}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"echo","arguments":"{\"x\":"}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"arguments":"1}"}}]}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`

const finalTextSSE = `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"all done"}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}

data: [DONE]

`

// TestFoundationCompositionOverRealTransport composes the two public packages exactly as a future caller will own them, from outside both packages and with only the shipped adapters: a real model.Transport speaking to an httptest OpenAI-compatible SSE server, agent.AdaptModelEffect supplying the model effect, agent.AdaptTools supplying the tool effect from a registered function map, a request source whose snapshot changes between the two model requests, and the complete Agent run loop over one tool round and a final text response.
func TestFoundationCompositionOverRealTransport(t *testing.T) {
	ref := model.ModelRef{Provider: "acme", Model: "m-1"}

	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		if len(bodies) == 1 {
			io.WriteString(w, toolCallSSE)
		} else {
			io.WriteString(w, finalTextSSE)
		}
	}))
	defer srv.Close()

	transport, err := model.NewTransport(model.ResolvedTransport{Model: ref, BaseURL: srv.URL})
	testassert.NoError(t, err)

	base := []model.Message{mustMessage(t, model.Message{
		Role:    model.RoleUser,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "run the echo tool"}},
	})}
	roundTrip := append(base, //nolint:gocritic // append result is never assigned back to base; the second snapshot is owned separately.
		mustMessage(t, model.Message{
			Role:      model.RoleAssistant,
			Source:    ref,
			ToolCalls: []model.ToolCall{{ID: "call-1", Name: "echo", Arguments: json.RawMessage(`{"x":1}`)}},
		}),
		mustMessage(t, model.Message{
			Role:       model.RoleTool,
			ToolCallID: "call-1",
			Content:    []model.ContentPart{{Kind: model.PartText, Text: "echoed"}},
		}))
	requests := [][]model.Message{base, roundTrip}
	advertised := []model.ToolDefinition{{Name: "echo", Description: "echo its argument", Parameters: json.RawMessage(`{"type":"object"}`)}}

	sourceCalls := 0
	var reqs []model.Request
	requestSource := func(context.Context) (model.Request, error) {
		sourceCalls++
		if sourceCalls > len(requests) {
			return model.Request{}, fmt.Errorf("request source ran out of snapshots after %d calls", sourceCalls)
		}
		req := model.Request{Messages: requests[sourceCalls-1], Tools: advertised}
		reqs = append(reqs, req) // each round's fresh snapshot as the Engine receives it.
		return req, nil
	}

	modelEffect := agent.AdaptModelEffect(ref, transport) // the shipped standalone adapter: one physical Stream attempt per round, assembled with the supplied source.

	var dispatched []model.ToolCall
	toolEffect := agent.AdaptTools(map[string]agent.ToolFunc{ // the shipped standalone adapter: dispatch is the registered implementation, gated by each round's advertised names.
		"echo": func(_ context.Context, call model.ToolCall) (model.ToolResult, error) {
			dispatched = append(dispatched, call)
			return model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "echoed"}, nil
		},
	})

	res, err := agent.Run(context.Background(), agent.Invocation{
		ExpectedModel: ref,
		Request:       requestSource,
		ModelEffect:   modelEffect,
		ToolEffect:    toolEffect,
	})
	testassert.NoError(t, err)

	testassert.Require(t, res.Status == agent.TerminalSuccess && res.Detail == "" && (res.LastOutput != nil && res.LastOutput.Status == model.OutputCompleted), "terminal = %+v, want success with empty detail; last output = %+v, want a completed output", res, res.LastOutput)
	testassert.Check(t, res.LastOutput.Source == ref, "output source = %v, want %v", res.LastOutput.Source, ref)
	{
		got := res.LastOutput.Message.TextContent()
		testassert.Check(t, got == "all done", "final text = %q, want %q", got, "all done")
	}

	// The complete loop: fresh context before each of the two model effects, changed snapshot reaching the second request.
	testassert.Require(t, sourceCalls == 2 && len(reqs) == 2, "loop ran %d context calls and %d model effects, want 2 and 2", sourceCalls, len(reqs))
	testassert.Check(t, len(reqs[0].Messages) == 1 && len(reqs[1].Messages) == 3, "context snapshots carried %d then %d messages, want 1 then 3", len(reqs[0].Messages), len(reqs[1].Messages))

	// The tool call assembled from the live stream arrives at the caller's tool boundary with its raw arguments intact.
	testassert.Equal(t, len(dispatched), 1)
	testassert.Check(t, dispatched[0].ID == "call-1" && dispatched[0].Name == "echo" && string(dispatched[0].Arguments) == `{"x":1}`, "dispatched call = %+v, want call-1/echo with raw arguments {\"x\":1}", dispatched[0])

	// The real transport dialed once per effect and carried the changed context onto the wire.
	testassert.Equal(t, len(bodies), 2)
	for _, want := range []string{`"model":"m-1"`, `"name":"echo"`} {
		testassert.Check(t, strings.Contains(bodies[0], want), "first request body lacks %s: %s", want, bodies[0])
	}
	testassert.Check(t, strings.Contains(bodies[1], `"role":"tool"`), "second request body does not carry the tool round trip: %s", bodies[1])
}

// unadvertisedCallSSE is a streaming body whose completed response calls "b" first and "echo" second: the first call's name is registered in the tool map but never advertised by the request.
const unadvertisedCallSSE = `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"mixed "}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-1","type":"function","function":{"name":"b","arguments":"{}"}}]}}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call-2","type":"function","function":{"name":"echo","arguments":"{\"x\":1}"}}]}}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: [DONE]

`

// TestFoundationCompositionUnadvertisedToolCall pins the advertisement gate at the full standalone composition: the tool map registers implementations for "echo" AND "b" while the request advertises only [echo], and the model calls registered-but-unadvertised "b" first. The "b" implementation never runs; the call answers the ordinary unavailable-tool error settlement (Run validated it and the batch continued — a stopped batch or a protocol rejection would end the run instead), the later "echo" call reaches its implementation, and the run still reaches terminal success.
func TestFoundationCompositionUnadvertisedToolCall(t *testing.T) {
	ref := model.ModelRef{Provider: "acme", Model: "m-1"}

	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		if len(bodies) == 1 {
			io.WriteString(w, unadvertisedCallSSE)
		} else {
			io.WriteString(w, finalTextSSE)
		}
	}))
	defer srv.Close()

	transport, err := model.NewTransport(model.ResolvedTransport{Model: ref, BaseURL: srv.URL})
	testassert.NoError(t, err)

	base := []model.Message{mustMessage(t, model.Message{
		Role:    model.RoleUser,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "call a tool that is not advertised"}},
	})}
	roundTrip := append(base, //nolint:gocritic // append result is never assigned back to base; the second snapshot is owned separately.
		mustMessage(t, model.Message{
			Role:   model.RoleAssistant,
			Source: ref,
			ToolCalls: []model.ToolCall{
				{ID: "call-1", Name: "b", Arguments: json.RawMessage(`{}`)},
				{ID: "call-2", Name: "echo", Arguments: json.RawMessage(`{"x":1}`)},
			},
		}),
		mustMessage(t, model.Message{
			Role:       model.RoleTool,
			ToolCallID: "call-1",
			Content:    []model.ContentPart{{Kind: model.PartText, Text: `Tool "b" is not available.`}},
		}))
	requests := [][]model.Message{base, roundTrip}
	advertised := []model.ToolDefinition{{Name: "echo", Description: "echo its argument", Parameters: json.RawMessage(`{"type":"object"}`)}}

	sourceCalls := 0
	requestSource := func(context.Context) (model.Request, error) {
		sourceCalls++
		if sourceCalls > len(requests) {
			return model.Request{}, fmt.Errorf("request source ran out of snapshots after %d calls", sourceCalls)
		}
		return model.Request{Messages: requests[sourceCalls-1], Tools: advertised}, nil
	}

	var ran []string
	toolEffect := agent.AdaptTools(map[string]agent.ToolFunc{
		"echo": func(_ context.Context, call model.ToolCall) (model.ToolResult, error) {
			ran = append(ran, "echo")
			return model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "echoed"}, nil
		},
		"b": func(_ context.Context, call model.ToolCall) (model.ToolResult, error) {
			ran = append(ran, "b")
			return model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "b ran"}, nil
		},
	})

	res, err := agent.Run(context.Background(), agent.Invocation{
		ExpectedModel: ref,
		Request:       requestSource,
		ModelEffect:   agent.AdaptModelEffect(ref, transport),
		ToolEffect:    toolEffect,
	})
	testassert.NoError(t, err)
	testassert.Require(t, res.Status == agent.TerminalSuccess && res.Detail == "" && res.LastOutput != nil && res.LastOutput.Status == model.OutputCompleted, "terminal = %+v, want success with empty detail; last output = %+v, want a completed output", res, res.LastOutput)
	testassert.Require(t, len(ran) == 1 && ran[0] == "echo", "tool implementations that ran = %v, want only echo: registered-but-unadvertised b never reached its implementation and the batch continued past its unavailable settlement", ran)
	testassert.Require(t, sourceCalls == 2 && len(bodies) == 2, "loop ran %d requests and %d transport attempts, want 2 and 2", sourceCalls, len(bodies))
}

func mustMessage(t *testing.T, m model.Message) model.Message {
	t.Helper()
	out, err := model.NewMessage(m)
	testassert.Require(t, err == nil, "NewMessage(role %s): %v", m.Role, err)
	return out
}
