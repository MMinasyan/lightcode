package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// benchResolved is the populated resolved input for the Stream allocation benchmark: both resolved sidecar layers, replay metadata, and header state all present so the measured path touches every ownership boundary the transport carries.
func benchResolved(baseURL string) ResolvedTransport {
	return ResolvedTransport{
		Model:             ModelRef{Provider: "openai", Model: "gpt-bench"},
		BaseURL:           baseURL,
		APIKey:            "bench-key",
		Headers:           map[string]string{"X-Bench": "bench"},
		ProviderExtraBody: Extra{"temperature": json.RawMessage(`0.2`), "top_p": json.RawMessage(`0.9`)},
		ModelExtraBody:    Extra{"frequency_penalty": json.RawMessage(`0.1`)},
		WireSystemRole:    "system",
		StreamedUsage:     true,
		ProtocolFamily:    "openai-compatible",
		MustPreserve:      []string{"reasoning_details", "tool_call_id"},
		Drop:              map[string]bool{"internal_note": true},
		SourceFamilies:    map[ModelRef]string{{Provider: "openai", Model: "gpt-bench-src"}: "openai-compatible"},
	}
}

// benchRequest is the populated logical-request fixture: system setup, user turn, an assistant replay-kept message carrying tool calls plus extras, a tool result, and two tool definitions with object schemas. It is a static Request literal — no constructor calls in setup — holding exactly the values the constructors would produce for this shape; the measured Stream boundary validates it every iteration.
func benchRequest() Request {
	call := ToolCall{
		ID:        "call_bench_1",
		Name:      "read_file",
		Arguments: json.RawMessage(`{"path":"/tmp/a.txt"}`),
		Extra:     Extra{"call_meta": json.RawMessage(`{"weight":1}`)},
	}
	return Request{
		Messages: []Message{
			{Role: RoleSystem, Content: []ContentPart{{Kind: PartText, Text: "You are a benchmark assistant."}}},
			{Role: RoleUser, Content: []ContentPart{{Kind: PartText, Text: "list the files"}, {Kind: PartImageURL, URL: "https://example.invalid/a.png"}}},
			{
				Role:      RoleAssistant,
				Source:    ModelRef{Provider: "openai", Model: "gpt-bench-src"},
				Content:   []ContentPart{{Kind: PartText, Text: "reading it now"}},
				ToolCalls: []ToolCall{call},
				Extra:     Extra{"provider_meta": json.RawMessage(`{"tokens":17}`)},
			},
			{Role: RoleTool, ToolCallID: "call_bench_1", Content: []ContentPart{{Kind: PartText, Text: "file contents"}}},
		},
		Tools: []ToolDefinition{
			{Name: "read_file", Description: "reads a file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)},
			{Name: "write_file", Description: "", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"data":{"type":"string"}}}`)},
		},
	}
}

// benchRuntimeExtras is the per-call runtime layer for the benchmark, static across iterations.
func benchRuntimeExtras() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		"runtime_tag":   json.RawMessage(`"bench"`),
		"user_metadata": json.RawMessage(`{"session":"bench"}`),
	}
}

// BenchmarkTransportStreamFullEncode measures allocations of the full Stream path over a test-local loopback endpoint: request validation, runtime-layer ownership, wire encoding, and the physical HTTP attempt with its minimal 204 response. The same static fixture is measured before and after the ownership-refactor state of this package; the numbers are evidence, not a runtime limit.
func BenchmarkTransportStreamFullEncode(b *testing.B) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	tr, err := NewTransport(benchResolved(server.URL))
	if err != nil {
		b.Fatalf("NewTransport failed: %v", err)
	}
	req := benchRequest()
	extras := benchRuntimeExtras()
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stream, warnings, err := tr.Stream(ctx, req, extras)
		if err != nil {
			b.Fatalf("Stream failed: %v", err)
		}
		if stream == nil {
			b.Fatal("Stream returned a nil stream")
		}
		if len(warnings) == 0 {
			b.Fatal("fixture must produce warnings so warning handling stays measured")
		}
		if err := stream.Close(); err != nil {
			b.Fatalf("stream Close failed: %v", err)
		}
	}
}
