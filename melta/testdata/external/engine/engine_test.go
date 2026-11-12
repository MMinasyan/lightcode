package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Syntheresis/melta/agent"
	"github.com/Syntheresis/melta/model"
)

// writeTextTurn and writeToolTurn are this package's own SSE turns: the
// engine-only test shares no helpers with the baseline acceptance package.
func writeTextTurn(w http.ResponseWriter, text string) {
	writeSSE(w,
		`{"choices":[{"delta":{"role":"assistant","content":`+strconv.Quote(text)+`},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	)
}

func writeToolTurn(w http.ResponseWriter, id, name, arguments string) {
	writeSSE(w,
		`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":`+strconv.Quote(id)+`,"type":"function","function":{"name":`+strconv.Quote(name)+`,"arguments":`+strconv.Quote(arguments)+`}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	)
}

func writeSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, event := range events {
		fmt.Fprintf(w, "data: %s\n\n", event)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// TestEngineOnlyRun exercises the public engine surface from the same
// independent module: the included model client against a local SSE server,
// the standalone model/tool adapters, and one agent run with a tool call.
func TestEngineOnlyRun(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer engine-secret" {
			t.Errorf("engine request authorization = %q, want the resolved key", r.Header.Get("Authorization"))
		}
		if attempts.Add(1) == 1 {
			writeToolTurn(w, "call-echo", "echo", `{}`)
			return
		}
		writeTextTurn(w, "engine done")
	}))
	defer server.Close()

	source := model.ModelRef{Provider: "local", Model: "engine"}
	transport, err := model.NewTransport(model.ResolvedTransport{
		Model:   source,
		BaseURL: server.URL,
		APIKey:  "engine-secret",
	})
	if err != nil {
		t.Fatalf("NewTransport: %v", err)
	}
	request, err := model.NewRequest(model.Request{
		Messages: []model.Message{{
			Role:    model.RoleUser,
			Content: []model.ContentPart{{Kind: model.PartText, Text: "use echo"}},
		}},
		Tools: []model.ToolDefinition{{
			Name:        "echo",
			Description: "echo one call",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		}},
	})
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	var calls atomic.Int32
	toolEffect := agent.AdaptTools(map[string]agent.ToolFunc{
		"echo": func(_ context.Context, call model.ToolCall) (model.ToolResult, error) {
			calls.Add(1)
			return model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "echoed"}, nil
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	result, err := agent.Run(ctx, agent.Invocation{
		ExpectedModel: source,
		Request: func(context.Context) (agent.RequestResult, error) {
			owned, err := model.NewRequest(request)
			if err != nil {
				return agent.RequestResult{}, err
			}
			return agent.RequestResult{Request: &owned}, nil
		},
		ModelEffect: agent.AdaptModelEffect(source, transport),
		ToolEffect:  toolEffect,
	})
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if result.Status != agent.TerminalSuccess || result.LastOutput == nil || result.LastOutput.Message == nil || result.LastOutput.Message.TextContent() != "engine done" {
		t.Fatalf("run = %+v, want the terminal success carrying the final text", result)
	}
	if calls.Load() != 1 {
		t.Fatalf("tool calls = %d, want one dispatched call", calls.Load())
	}
	if attempts.Load() != 2 {
		t.Fatalf("model attempts = %d, want the tool turn and its follow-up", attempts.Load())
	}
}
