package harness

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/model"
	"github.com/pkoukk/tiktoken-go"
)

// tokenizerColdChildEnv switches this test binary into the child mode of the
// cold tokenizer regression.
const tokenizerColdChildEnv = "LIGHTCODE_COLD_TOKENIZER_CHILD"

// tokenizerColdChildDone is the child's completion marker: the parent requires
// it so a child that ran no assertion cannot pass by exiting zero.
const tokenizerColdChildDone = "cold tokenizer child: done"

// refusedTransport fails every attempt after recording it, so the child can
// prove that no tokenizer acquisition escaped to an external transport.
type refusedTransport struct{ attempts atomic.Int64 }

func (f *refusedTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.attempts.Add(1)
	return nil, errors.New("external transport is unavailable in this process")
}

// TestTokenizerColdChildNoAcquisitionIO runs one real Operation, a mid-flight
// Interrupt and a Wait in a fresh child process with an empty tokenizer cache
// environment and a transport that refuses (and records) every attempt. All
// three must finish without contacting that transport.
func TestTokenizerColdChildNoAcquisitionIO(t *testing.T) {
	if os.Getenv(tokenizerColdChildEnv) == "1" {
		runTokenizerColdChild(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.timeout=120s")
	cmd.Env = append(tokenizerFreeEnv(), tokenizerColdChildEnv+"=1", "TIKTOKEN_CACHE_DIR="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cold tokenizer child: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), tokenizerColdChildDone) {
		t.Fatalf("the cold child ran no completed work:\n%s", out)
	}
}

// tokenizerFreeEnv is the parent environment with the tokenizer cache
// variables removed: the child's acquisition sees an empty cache directory,
// never a prewarmed developer copy.
func tokenizerFreeEnv() []string {
	env := os.Environ()
	kept := env[:0]
	for _, entry := range env {
		if strings.HasPrefix(entry, "TIKTOKEN_CACHE_DIR=") || strings.HasPrefix(entry, "DATA_GYM_CACHE_DIR=") {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

// runTokenizerColdChild is the child body: a completed Operation, a parked
// Operation interrupted mid-flight, and the canceled Harness's Wait join.
func runTokenizerColdChild(t *testing.T) {
	transport := &refusedTransport{}
	http.DefaultTransport = transport

	store := emptyStore(t)
	started := make(chan struct{})
	var requests atomic.Int64
	modelFn := func(ctx context.Context, _ model.Request) (model.Stream, error) {
		if requests.Add(1) == 1 {
			return completedTurnStream(), nil
		}
		close(started) // the second Operation parks inside its model attempt
		<-ctx.Done()
		return nil, ctx.Err()
	}
	h, cancel := newCancelableHarness(t, store, modelPrepared(modelFn), nil)
	session := createSession(t, h)
	watch := watchSettlements(store)

	if _, err := submitText(t, h, session, "op-1", MessageModeRegular, "hello"); err != nil {
		t.Fatalf("first submit: %v", err)
	}
	watch.next()
	if rec := settledOperation(t, store, session, "op-1"); rec.State.Status != OperationSuccess {
		t.Fatalf("op-1 status = %s, want success", rec.State.Status)
	}
	if _, err := submitText(t, h, session, "op-2", MessageModeRegular, "park"); err != nil {
		t.Fatalf("second submit: %v", err)
	}
	<-started
	if err := h.Interrupt(context.Background(), session); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	watch.next()
	if rec := settledOperation(t, store, session, "op-2"); rec.State.Status != OperationInterruption {
		t.Fatalf("op-2 status = %s, want interruption", rec.State.Status)
	}
	cancel()
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer waitCancel()
	if err := h.Wait(waitCtx); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if attempts := transport.attempts.Load(); attempts != 0 {
		t.Fatalf("the cold child contacted the external transport %d time(s)", attempts)
	}
	fmt.Println(tokenizerColdChildDone)
}

// tokenizerVectors pins the cl100k_base token count of each fixture text over
// the bundled rank data. They are the oracle for the arithmetic below, and the
// multibyte rows are the encoding's UTF-8 vectors.
var tokenizerVectors = map[string]int{
	"You are a helpful assistant.":       6,
	"hello world":                        2,
	"hi there":                           2,
	"hi":                                 1,
	"You are Lightcode, a coding agent.": 9,
	"calling":                            1,
	"read_file":                          2,
	`{"path":"a.go"}`:                    6,
	"edit":                               1,
	`{"x":1}`:                            5,
	"Привет, мир!":                       7,
	"你好世界":                               5,
	"🙂 emoji":                            3,
	"héllo wörld":                        6,
	"日本語のテキスト":                           8,
	"hello Привет 🙂 世界":                  9,
}

// pinnedTokens returns one fixture text's pinned token count.
func pinnedTokens(t *testing.T, text string) int {
	t.Helper()
	tokens, ok := tokenizerVectors[text]
	if !ok {
		t.Fatalf("no pinned token vector for %q", text)
	}
	return tokens
}

// TestLocalEncodingAsset pins the provenance of the bundled rank table: its
// size and SHA-256 digest are the accepted copy's.
func TestLocalEncodingAsset(t *testing.T) {
	const wantDigest = "223921b76ee99bde995b7ff738513eef100fb51d18c93597a113bcffe865b2a7"
	const wantSize = 1681126
	if got := fmt.Sprintf("%x", sha256.Sum256(cl100kBaseData)); got != wantDigest {
		t.Fatalf("bundled cl100k_base digest = %s, want %s", got, wantDigest)
	}
	if got := len(cl100kBaseData); got != wantSize {
		t.Fatalf("bundled cl100k_base size = %d, want %d", got, wantSize)
	}
}

// TestLocalEncodingVectors pins the bundled encoding itself, including its
// UTF-8 vectors: every fixture text must encode to exactly its pinned count.
func TestLocalEncodingVectors(t *testing.T) {
	enc, err := localEncoding()
	if err != nil {
		t.Fatalf("localEncoding: %v", err)
	}
	for text, want := range tokenizerVectors {
		if got := len(enc.Encode(text, nil, nil)); got != want {
			t.Errorf("cl100k_base tokens of %q = %d, want %d", text, got, want)
		}
	}
}

// TestNewLocalEncodingDuplicateRankIsConstructionError pins the construction
// contract's one rejection row: a rank table whose tokens carry duplicate rank
// identities is rejected by the pinned library's construction (its reverse-map
// size check), and the memoized construction failure selects the characters/3
// fallback on the next estimate.
func TestNewLocalEncodingDuplicateRankIsConstructionError(t *testing.T) {
	tokenA := base64.StdEncoding.EncodeToString([]byte("ab"))
	tokenB := base64.StdEncoding.EncodeToString([]byte("cd"))
	table := []byte(tokenA + " 0\n" + tokenB + " 0\n")
	if _, err := newLocalEncoding(table); err == nil {
		t.Fatal("duplicate rank identities: construction succeeded, want the library's rejection")
	}

	old := localEncoding
	localEncoding = sync.OnceValues(func() (*tiktoken.Tiktoken, error) { return newLocalEncoding(table) })
	t.Cleanup(func() { localEncoding = old })
	// The latched failure is the fallback's input: 6 characters / 3.
	if got := estimateTokens("abcdef", nil); got != 2 {
		t.Fatalf("estimateTokens over the failed construction = %d, want the characters/3 fallback 2", got)
	}
}

// mustEstimateMessage validates the fixture message into its owned form.
func mustEstimateMessage(t *testing.T, in model.Message) model.Message {
	t.Helper()
	msg, err := model.NewMessage(in)
	if err != nil {
		t.Fatalf("NewMessage: %v", err)
	}
	return msg
}

// TestEstimateTokensTextOnlyMessages pins the rule over text-only messages:
// per message, the encoded text content (all text parts joined without
// separators) plus the flat +4.
func TestEstimateTokensTextOnlyMessages(t *testing.T) {
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
	want := pinnedTokens(t, "You are a helpful assistant.") + pinnedTokens(t, "hello world") + pinnedTokens(t, "hi there")
	want += 4 * len(messages)
	if got := estimateTokens("", messages); got != want {
		t.Fatalf("estimateTokens = %d, want %d", got, want)
	}
}

// TestEstimateTokensToolCalls pins the rule for assistant tool calls: each
// call contributes its name and raw argument bytes as encoded text.
func TestEstimateTokensToolCalls(t *testing.T) {
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
	want := pinnedTokens(t, "calling")
	for _, call := range []struct{ name, args string }{
		{"read_file", `{"path":"a.go"}`},
		{"edit", `{"x":1}`},
	} {
		want += pinnedTokens(t, call.name)
		want += pinnedTokens(t, call.args)
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
	prompt := "You are Lightcode, a coding agent."
	if got, want := estimateTokens(prompt, nil), pinnedTokens(t, prompt); got != want {
		t.Fatalf("estimateTokens(prompt, nil) = %d, want %d", got, want)
	}
	messages := []model.Message{mustEstimateMessage(t, model.Message{
		Role:    model.RoleUser,
		Content: []model.ContentPart{{Kind: model.PartText, Text: "hi"}},
	})}
	want := pinnedTokens(t, prompt) + pinnedTokens(t, "hi") + 4
	if got := estimateTokens(prompt, messages); got != want {
		t.Fatalf("estimateTokens(prompt, messages) = %d, want %d", got, want)
	}
}

// TestEstimateTokensCharsFallback pins the fallback a nil encoder selects: the
// same accumulated text (system prompt, message texts, tool call names and
// arguments) divided by three.
func TestEstimateTokensCharsFallback(t *testing.T) {
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
	if got := estimateTokensWithEncoding(prompt, messages, nil); got != 3 {
		t.Fatalf("fallback estimateTokens = %d, want 3", got)
	}
	if got := estimateTokensWithEncoding("", nil, nil); got != 0 {
		t.Fatalf("fallback estimateTokens of no messages = %d, want 0", got)
	}
}
