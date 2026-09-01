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

// TestFoundationCompositionOverRealTransport composes the two public packages exactly as a future caller will own them, from outside both packages and with no production adapter or test-only production API: a real model.Transport speaking to an httptest OpenAI-compatible SSE server, the normal Agent assembly callback invoked from inside a test model effect, a fake tool effect, a context source whose snapshot changes between the two model requests, and the complete Agent run loop over one tool round and a final text response.
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
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}

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
	snapshots := [][]model.Message{base, roundTrip}

	sourceCalls := 0
	contextSource := func(context.Context) ([]model.Message, error) {
		sourceCalls++
		if sourceCalls > len(snapshots) {
			return nil, fmt.Errorf("context source ran out of snapshots after %d calls", sourceCalls)
		}
		return snapshots[sourceCalls-1], nil
	}

	var reqs []model.Request
	modelEffect := func(ctx context.Context, req model.Request, cb agent.AssemblyCallback) (agent.ModelSettlement, error) {
		reqs = append(reqs, req)
		stream, _, err := transport.Stream(ctx, req, nil) // real public transport: one physical attempt, SSE body owned by the returned stream.
		if err != nil {
			return agent.ModelSettlement{}, err
		}
		out, err := cb(ref, stream) // the normal Agent assembly callback over the live stream.
		if err != nil {
			return agent.ModelSettlement{}, err
		}
		return agent.ModelSettlement{Disposition: agent.DispoReady, Output: &out}, nil
	}

	var dispatched []model.ToolCall
	toolEffect := func(_ context.Context, call model.ToolCall) (model.ToolResult, error) {
		dispatched = append(dispatched, call)
		return model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "echoed"}, nil
	}

	res, err := agent.Run(context.Background(), agent.Invocation{
		ExpectedModel: ref,
		Tools:         []model.ToolDefinition{{Name: "echo", Description: "echo its argument", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Context:       contextSource,
		ModelEffect:   modelEffect,
		ToolEffect:    toolEffect,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	if res.Status != agent.TerminalSuccess || res.Detail != "" {
		t.Fatalf("terminal = %+v, want success with empty detail", res)
	}
	if res.LastOutput == nil || res.LastOutput.Status != model.OutputCompleted {
		t.Fatalf("last output = %+v, want a completed output", res.LastOutput)
	}
	if res.LastOutput.Source != ref {
		t.Errorf("output source = %v, want %v", res.LastOutput.Source, ref)
	}
	if got := res.LastOutput.Message.TextContent(); got != "all done" {
		t.Errorf("final text = %q, want %q", got, "all done")
	}

	// The complete loop: fresh context before each of the two model effects, changed snapshot reaching the second request.
	if sourceCalls != 2 || len(reqs) != 2 {
		t.Fatalf("loop ran %d context calls and %d model effects, want 2 and 2", sourceCalls, len(reqs))
	}
	if len(reqs[0].Messages) != 1 || len(reqs[1].Messages) != 3 {
		t.Errorf("context snapshots carried %d then %d messages, want 1 then 3", len(reqs[0].Messages), len(reqs[1].Messages))
	}

	// The tool call assembled from the live stream arrives at the caller's tool boundary with its raw arguments intact.
	if len(dispatched) != 1 {
		t.Fatalf("tool boundary saw %d calls, want 1", len(dispatched))
	}
	if dispatched[0].ID != "call-1" || dispatched[0].Name != "echo" || string(dispatched[0].Arguments) != `{"x":1}` {
		t.Errorf("dispatched call = %+v, want call-1/echo with raw arguments {\"x\":1}", dispatched[0])
	}

	// The real transport dialed once per effect and carried the changed context onto the wire.
	if len(bodies) != 2 {
		t.Fatalf("provider saw %d requests, want 2", len(bodies))
	}
	for _, want := range []string{`"model":"m-1"`, `"name":"echo"`} {
		if !strings.Contains(bodies[0], want) {
			t.Errorf("first request body lacks %s: %s", want, bodies[0])
		}
	}
	if !strings.Contains(bodies[1], `"role":"tool"`) {
		t.Errorf("second request body does not carry the tool round trip: %s", bodies[1])
	}
}

func mustMessage(t *testing.T, m model.Message) model.Message {
	t.Helper()
	out, err := model.NewMessage(m)
	if err != nil {
		t.Fatalf("NewMessage(role %s): %v", m.Role, err)
	}
	return out
}
