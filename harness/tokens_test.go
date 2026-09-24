package harness

import (
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/model"
	"github.com/pkoukk/tiktoken-go"
)

// mustEstimateMessage validates the fixture message into its owned form.
func mustEstimateMessage(t *testing.T, in model.Message) model.Message {
	t.Helper()
	msg, err := model.NewMessage(in)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	return msg
}

// liveEstimateEncoder returns the real cl100k_base encoder for expected-value
// computation, mirroring the production acquisition path.
func liveEstimateEncoder(t *testing.T) *tiktoken.Tiktoken {
	t.Helper()
	enc, err := tiktoken.GetEncoding("cl100k_base")
	if err != nil {
		t.Fatalf("GetEncoding: %v", err)
	}
	return enc
}

// TestEstimateTokensTextOnlyMessages pins the legacy rule over text-only
// messages: per message, the encoded text content (all text parts joined
// without separators) plus the flat +4.
func TestEstimateTokensTextOnlyMessages(t *testing.T) {
	enc := liveEstimateEncoder(t)
	messages := []model.Message{
		mustEstimateMessage(t, model.Message{
			Role:    model.RoleSystem,
			Content: []model.ContentPart{{Kind: model.PartText, Text: "You are a helpful assistant."}},
		}),
		mustEstimateMessage(t, model.Message{
			Role: model.RoleUser,
			Content: []model.ContentPart{
				{Kind: model.PartText, Text: "hello "},
				{Kind: model.PartText, Text: "world"},
			},
		}),
		mustEstimateMessage(t, model.Message{
			Role:    model.RoleAssistant,
			Source:  model.ModelRef{Provider: "p", Model: "m"},
			Content: []model.ContentPart{{Kind: model.PartText, Text: "hi there"}},
		}),
	}
	want := 0
	for _, text := range []string{"You are a helpful assistant.", "hello world", "hi there"} {
		want += len(enc.Encode(text, nil, nil))
	}
	want += 4 * len(messages)
	if got := estimateTokens("", messages); got != want {
		t.Fatalf("estimateTokens = %d, want %d", got, want)
	}
}

// TestEstimateTokensToolCalls pins the legacy rule for assistant tool calls:
// each call contributes its name and raw argument bytes as encoded text.
func TestEstimateTokensToolCalls(t *testing.T) {
	enc := liveEstimateEncoder(t)
	messages := []model.Message{
		mustEstimateMessage(t, model.Message{
			Role:    model.RoleAssistant,
			Source:  model.ModelRef{Provider: "p", Model: "m"},
			Content: []model.ContentPart{{Kind: model.PartText, Text: "calling"}},
			ToolCalls: []model.ToolCall{
				{ID: "c1", Name: "read_file", Arguments: []byte(`{"path":"a.go"}`)},
				{ID: "c2", Name: "edit", Arguments: []byte(`{"x":1}`)},
			},
		}),
	}
	want := len(enc.Encode("calling", nil, nil))
	for _, call := range []struct{ name, args string }{
		{"read_file", `{"path":"a.go"}`},
		{"edit", `{"x":1}`},
	} {
		want += len(enc.Encode(call.name, nil, nil))
		want += len(enc.Encode(call.args, nil, nil))
	}
	want += 4 * len(messages)
	if got := estimateTokens("", messages); got != want {
		t.Fatalf("estimateTokens = %d, want %d", got, want)
	}
}

// TestEstimateTokensFlatOverheadPerMessage pins the flat +4 per message: an
// empty conversation estimates zero, and each empty-text message adds exactly
// four.
func TestEstimateTokensFlatOverheadPerMessage(t *testing.T) {
	if got := estimateTokens("", nil); got != 0 {
		t.Fatalf("estimateTokens of no messages = %d, want 0", got)
	}
	one := []model.Message{mustEstimateMessage(t, model.Message{Role: model.RoleUser})}
	if got := estimateTokens("", one); got != 4 {
		t.Fatalf("estimateTokens of one empty message = %d, want 4", got)
	}
	two := append(one, mustEstimateMessage(t, model.Message{Role: model.RoleUser}))
	if got := estimateTokens("", two); got != 8 {
		t.Fatalf("estimateTokens of two empty messages = %d, want 8", got)
	}
}

// TestEstimateTokensSystemPromptPlainText pins the system-prompt rule: the
// prompt is estimated as plain text with no per-message overhead, once, on
// top of the messages.
func TestEstimateTokensSystemPromptPlainText(t *testing.T) {
	enc := liveEstimateEncoder(t)
	prompt := "You are Lightcode, a coding agent."
	if got, want := estimateTokens(prompt, nil), len(enc.Encode(prompt, nil, nil)); got != want {
		t.Fatalf("estimateTokens(prompt, nil) = %d, want %d", got, want)
	}
	messages := []model.Message{mustEstimateMessage(t, model.Message{
		Role:    model.RoleUser,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "hi"}},
	})}
	want := len(enc.Encode(prompt, nil, nil)) + len(enc.Encode("hi", nil, nil)) + 4
	if got := estimateTokens(prompt, messages); got != want {
		t.Fatalf("estimateTokens(prompt, messages) = %d, want %d", got, want)
	}
}

// TestEstimateTokensCharsFallbackThroughSeam forces the encoder acquisition to
// fail through the in-package seam and pins the characters/3 fallback: the
// same accumulated text (system prompt, message texts, tool call names and
// arguments) divided by three.
func TestEstimateTokensCharsFallbackThroughSeam(t *testing.T) {
	original := encodingFor
	encodingFor = func(string) (*tiktoken.Tiktoken, error) { return nil, errors.New("unavailable") }
	t.Cleanup(func() { encodingFor = original })

	prompt := "abcdef" // 6 chars
	messages := []model.Message{
		mustEstimateMessage(t, model.Message{
			Role:    model.RoleUser,
			Content: []model.ContentPart{{Kind: model.PartText, Text: "gh"}},
		}),
		mustEstimateMessage(t, model.Message{
			Role:   model.RoleAssistant,
			Source: model.ModelRef{Provider: "p", Model: "m"},
			ToolCalls: []model.ToolCall{
				{ID: "c1", Name: "f", Arguments: []byte(`{}`)},
			},
		}),
	}
	// 6 + 2 + 1 + 2 = 11 chars -> 11/3 = 3
	if got := estimateTokens(prompt, messages); got != 3 {
		t.Fatalf("fallback estimateTokens = %d, want 3", got)
	}
	if got := estimateTokens("", nil); got != 0 {
		t.Fatalf("fallback estimateTokens of no messages = %d, want 0", got)
	}
}
