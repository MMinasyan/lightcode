package provider

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
	"time"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	openai "github.com/sashabaranov/go-openai"
)

func TestStreamRecvParsesChunkedMultilineSSEDone(t *testing.T) {
	payload := `{"id":"chunk-1",` + "\n" + `"choices":[{"index":0,"delta":{"content":"hello"}}]}`
	body := "data: " + strings.Replace(payload, "\n", "\ndata: ", 1) + "\n\n" +
		": keepalive\n\n" +
		"data: [DONE]\n\n"
	stream := NewStream(&chunkedReadCloser{data: body, chunk: 7})

	chunk, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv returned error: %v", err)
	}
	if chunk.Typed.ID != "chunk-1" || len(chunk.Typed.Choices) != 1 || chunk.Typed.Choices[0].Delta.Content != "hello" {
		t.Fatalf("chunk = %#v, want chunk-1 content hello", chunk)
	}
	if string(chunk.Raw) != payload {
		t.Fatalf("raw = %q, want original payload", string(chunk.Raw))
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv after [DONE] error = %v, want io.EOF", err)
	}
}

func TestStreamRecvReportsMalformedSSEJSON(t *testing.T) {
	stream := NewStream(io.NopCloser(strings.NewReader("data: {not-json}\n\n")))

	_, err := stream.Recv()
	if err == nil {
		t.Fatalf("Recv returned nil error for malformed JSON")
	}
}

func TestStreamRecvPreservesValidSDKIncompatibleJSON(t *testing.T) {
	payload := `{"choices":[{"index":0,"delta":{"content":[{"type":"thinking","thinking":"hidden","signature":"sig"}]}}]}`
	stream := NewStream(io.NopCloser(strings.NewReader("data: " + payload + "\n\n")))

	chunk, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv returned error: %v", err)
	}
	if string(chunk.Raw) != payload {
		t.Fatalf("raw = %q, want payload", string(chunk.Raw))
	}
	if len(chunk.Typed.Choices) != 0 {
		t.Fatalf("typed choices = %#v, want empty fallback for SDK-incompatible chunk", chunk.Typed.Choices)
	}
}

func TestBuildRequestBodyAppliesRolesSidecarsReservedKeysAndMaxTokensField(t *testing.T) {
	prov := testProvider("https://example.com/v1", "KEY_ENV", "model-a")
	prov.SystemRole = catalog.RoleUser
	prov.MaxTokensField = "max_completion_tokens"
	prov.ExtraBody = map[string]any{"temperature": float64(0.1), "top_p": float64(0.9)}
	model := prov.Models["model-a"]
	model.SystemRole = catalog.RoleDeveloper
	model.MaxOutputTokens = 1234
	model.UsageInStream = true
	model.ExtraBody = map[string]any{"temperature": float64(0.2), "reasoning_effort": "medium"}

	body, err := buildRequestBody(requestConfig{
		provider: prov,
		model:    model,
		messages: []message.Message{
			message.NewText(message.RoleSystem, "instructions"),
			message.NewText(message.RoleUser, "hello"),
		},
		tools:         []openai.Tool{{Type: openai.ToolTypeFunction, Function: &openai.FunctionDefinition{Name: "lookup", Parameters: map[string]any{"type": "object"}}}},
		runtimeExtras: map[string]any{"seed": float64(7)},
	})
	if err != nil {
		t.Fatalf("buildRequestBody returned error: %v", err)
	}
	if body["model"] != "model-a" || body["stream"] != true || body["n"] != 1 {
		t.Fatalf("core fields = %#v", body)
	}
	if _, ok := body["max_tokens"]; ok {
		t.Fatalf("body has max_tokens: %#v", body)
	}
	if _, ok := body["max_completion_tokens"]; ok {
		t.Fatalf("body has max_completion_tokens; catalog MaxOutputTokens must not auto-emit: %#v", body)
	}
	messages, ok := body["messages"].([]map[string]any)
	if !ok || len(messages) != 2 || messages[0]["role"] != "developer" || messages[1]["role"] != "user" {
		t.Fatalf("messages = %#v, want first system message rewritten to developer", body["messages"])
	}
	if body["tool_choice"] != "auto" {
		t.Fatalf("tool_choice = %#v, want auto", body["tool_choice"])
	}
	if streamOptions, ok := body["stream_options"].(map[string]any); !ok || streamOptions["include_usage"] != true {
		t.Fatalf("stream_options = %#v, want include_usage true", body["stream_options"])
	}
	if body["temperature"] != float64(0.2) || body["top_p"] != float64(0.9) || body["reasoning_effort"] != "medium" || body["seed"] != float64(7) {
		t.Fatalf("sidecar merge = %#v", body)
	}
}

func TestBuildRequestBodyRejectsReservedSidecarKeys(t *testing.T) {
	prov := testProvider("https://example.com/v1", "KEY_ENV", "model-a")
	prov.ExtraBody = map[string]any{"model": "bad"}

	_, err := buildRequestBody(requestConfig{provider: prov, model: prov.Models["model-a"]})
	var reserved *ReservedKeyError
	if !errors.As(err, &reserved) {
		t.Fatalf("buildRequestBody error = %v, want ReservedKeyError", err)
	}
	if len(reserved.Keys) != 1 || reserved.Keys[0] != "model" {
		t.Fatalf("reserved keys = %#v, want [model]", reserved.Keys)
	}
}

func TestBuildHeadersAddsAuthAndProviderHeaders(t *testing.T) {
	prov := testProvider("https://example.com/v1", "KEY_ENV", "model-a")
	prov.Transport.Headers = map[string]string{"HTTP-Referer": "https://lightcode.local", "X-Title": "Lightcode"}

	headers := buildHeaders(prov, "secret")
	if headers.Get("Authorization") != "Bearer secret" {
		t.Fatalf("Authorization = %q", headers.Get("Authorization"))
	}
	if headers.Get("Accept") != "text/event-stream" || headers.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %#v", headers)
	}
	if headers.Get("HTTP-Referer") != "https://lightcode.local" || headers.Get("X-Title") != "Lightcode" {
		t.Fatalf("provider headers = %#v", headers)
	}
}

func TestClientChatStreamPostsExpectedRequestAndParsesResponse(t *testing.T) {
	var gotPath string
	var gotHeader http.Header
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeader = r.Header.Clone()
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"id\":\"resp-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	prov := testProvider(server.URL+"/v1/", "KEY_ENV", "model-a")
	prov.Transport.Headers = map[string]string{"X-Test": "yes"}
	prov.MaxTokensField = "max_completion_tokens"
	model := prov.Models["model-a"]
	model.SystemRole = catalog.RoleDeveloper
	model.MaxOutputTokens = 77
	client := New(prov, model, "secret")

	stream, err := client.ChatStream(context.Background(), []message.Message{
		message.NewText(message.RoleSystem, "sys"),
		{
			Role:    message.RoleAssistant,
			Content: []message.ContentPart{{Type: message.ContentPartText, Text: "using tool"}},
			ToolCalls: []message.ToolCall{{
				ID:       "call_1",
				Type:     "function",
				Function: message.FunctionCall{Name: "lookup", Arguments: `{}`},
				Extra:    message.Extra{"extra_content": json.RawMessage(`{"google":{"thought_signature":"sig"}}`)},
			}},
			Extra:  message.Extra{"reasoning_details": json.RawMessage(`[{"type":"reasoning.text","text":"think"}]`)},
			Source: catalog.ModelRef{Provider: "test", Model: "model-a"},
		},
	}, nil, map[string]any{"temperature": float64(0.3)})
	if err != nil {
		t.Fatalf("ChatStream returned error: %v", err)
	}
	defer stream.Close()
	chunk, err := stream.Recv()
	if err != nil {
		t.Fatalf("Recv returned error: %v", err)
	}
	if chunk.Typed.ID != "resp-1" || len(chunk.Typed.Choices) != 1 || chunk.Typed.Choices[0].Delta.Content != "ok" {
		t.Fatalf("chunk = %#v", chunk)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotHeader.Get("Authorization") != "Bearer secret" || gotHeader.Get("X-Test") != "yes" || gotHeader.Get("Accept") != "text/event-stream" {
		t.Fatalf("headers = %#v", gotHeader)
	}
	messages := gotBody["messages"].([]any)
	first := messages[0].(map[string]any)
	if first["role"] != "developer" {
		t.Fatalf("first message role = %#v, want developer", first["role"])
	}
	second := messages[1].(map[string]any)
	if _, ok := second["reasoning_details"]; !ok {
		t.Fatalf("assistant extra missing from request body: %#v", second)
	}
	toolCalls := second["tool_calls"].([]any)
	toolCall := toolCalls[0].(map[string]any)
	if _, ok := toolCall["extra_content"]; !ok {
		t.Fatalf("tool extra missing from request body: %#v", toolCall)
	}
	if _, ok := gotBody["max_completion_tokens"]; ok {
		t.Fatalf("body has max_completion_tokens; catalog MaxOutputTokens must not auto-emit: %#v", gotBody)
	}
	if gotBody["temperature"] != float64(0.3) || gotBody["stream"] != true {
		t.Fatalf("body = %#v", gotBody)
	}
}

func TestClientChatStreamReturnsHTTPStatusError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"slow down"}}`, http.StatusTooManyRequests)
	}))
	defer server.Close()

	prov := testProvider(server.URL, "KEY_ENV", "model-a")
	client := New(prov, prov.Models["model-a"], "secret")

	_, err := client.ChatStream(context.Background(), nil, nil, nil)
	var status *HTTPStatusError
	if !errors.As(err, &status) {
		t.Fatalf("ChatStream error = %v, want HTTPStatusError", err)
	}
	if status.StatusCode != http.StatusTooManyRequests || !status.Retryable() {
		t.Fatalf("status = %#v, want retryable 429", status)
	}
}

func TestClientChatStreamHonorsCanceledContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
	}))
	defer server.Close()

	prov := testProvider(server.URL, "KEY_ENV", "model-a")
	client := New(prov, prov.Models["model-a"], "secret")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.ChatStream(ctx, nil, nil, nil)
	if err == nil {
		t.Fatalf("ChatStream returned nil error for canceled context")
	}
}

func TestClientChatStreamCancelMidFlight(t *testing.T) {
	chunkSent := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			return
		}
		fmt.Fprint(w, "data: {\"id\":\"1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n")
		flusher.Flush()
		chunkSent <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()

	prov := testProvider(server.URL, "KEY_ENV", "model-a")
	client := New(prov, prov.Models["model-a"], "secret")
	ctx, cancel := context.WithCancel(context.Background())

	stream, err := client.ChatStream(ctx, nil, nil, nil)
	if err != nil {
		t.Fatalf("ChatStream returned error: %v", err)
	}
	defer stream.Close()

	_, err = stream.Recv()
	if err != nil {
		t.Fatalf("first Recv returned error: %v", err)
	}

	<-chunkSent
	cancel()
	_, err = stream.Recv()
	if err == nil {
		t.Fatalf("Recv after cancel should return an error, got nil")
	}
}

func TestClientChatPostsNonStreamingRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if _, ok := body["stream"]; ok {
			t.Fatalf("non-streaming Chat request included stream field: %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"chat-1","model":"model-a","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()

	prov := testProvider(server.URL, "KEY_ENV", "model-a")
	client := New(prov, prov.Models["model-a"], "secret")

	resp, err := client.Chat(context.Background(), []message.Message{message.NewText(message.RoleUser, "hi")}, nil)
	if err != nil {
		t.Fatalf("Chat returned error: %v", err)
	}
	if resp.ID != "chat-1" || len(resp.Choices) != 1 || resp.Choices[0].Message.Content != "ok" {
		t.Fatalf("response = %#v", resp)
	}
}

type chunkedReadCloser struct {
	data  string
	chunk int
}

func (r *chunkedReadCloser) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, io.EOF
	}
	n := r.chunk
	if n <= 0 || n > len(r.data) {
		n = len(r.data)
	}
	if n > len(p) {
		n = len(p)
	}
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func (r *chunkedReadCloser) Close() error { return nil }

func testProvider(baseURL, apiKeyEnv, modelID string) *catalog.Provider {
	return &catalog.Provider{
		ID:             "test",
		Name:           "Test",
		MaxTokensField: "max_tokens",
		Transport: catalog.Transport{
			BaseURL:   baseURL,
			APIKeyEnv: apiKeyEnv,
		},
		SystemRole:    catalog.RoleSystem,
		UsageInStream: true,
		ExtraBody:     map[string]any{},
		Models: map[string]*catalog.Model{
			modelID: {
				ID:              modelID,
				Name:            modelID,
				ContextWindow:   128000,
				MaxOutputTokens: 4096,
				InputModalities: []catalog.Modality{catalog.ModalityText},
				SystemRole:      catalog.RoleSystem,
				UsageInStream:   true,
				ExtraBody:       map[string]any{},
			},
		},
	}
}
