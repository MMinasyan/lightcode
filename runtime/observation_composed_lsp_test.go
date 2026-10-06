package runtime_test

// The full-chain LSP ingest row: the public Open with the shipped builtin
// registration — the real LSP plugin and the concrete production preparation —
// runs one real model turn whose workspace_symbol tool call registers a
// workspace whose only detected server is unresolvable, and the real
// manager's warning reaches the Runtime's own store and bus: the global
// warning read and the runtime-scoped warning_changed hint.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/plugins/builtin"
	"github.com/MMinasyan/lightcode/protocol"
	"github.com/MMinasyan/lightcode/runtime"
)

// lspConfigDocument points the sole provider at the test model endpoint with
// a keyless transport, so no credential environment is needed.
func lspConfigDocument(endpoint string) string {
	return `{
  "providers": {
    "prov": {
      "transport": {"base_url": "` + endpoint + `", "api_key_env": ""},
      "discovery": false,
      "models": {"m": {"name": "M", "context_window": 262144}}
    }
  }
}`
}

const lspAgentsDocument = `{
  "worker": {"model": "prov/m", "system_prompt": "simple", "tools": ["workspace_symbol"], "capabilities": []}
}`

// writeLSPSSE writes one SSE turn the transport consumes.
func writeLSPSSE(w http.ResponseWriter, events ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher := w.(http.Flusher)
	for _, event := range events {
		_, _ = w.Write([]byte("data: " + event + "\n\n"))
	}
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	flusher.Flush()
}

// newLSPModelServer answers every first turn with one workspace_symbol tool
// call and every tool follow-up with a final text turn, over the SSE wire
// the transport consumes.
func newLSPModelServer(t *testing.T) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if lastJSONRole(string(body)) == "tool" {
			writeLSPSSE(w,
				`{"choices":[{"delta":{"role":"assistant","content":"done"},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
			)
			return
		}
		writeLSPSSE(w,
			`{"choices":[{"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call-lsp","type":"function","function":{"name":"workspace_symbol","arguments":"{\"query\":\"Foo\"}"}}]},"finish_reason":null}]}`,
			`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		)
	}))
	t.Cleanup(s.Close)
	return s
}

func lastJSONRole(body string) string {
	needle := `"role":"`
	if idx := strings.LastIndex(body, needle); idx >= 0 {
		rest := body[idx+len(needle):]
		if end := strings.IndexByte(rest, '"'); end > 0 {
			return rest[:end]
		}
	}
	return ""
}

// TestComposedLSPWarningLandsInGlobalStoreAndEvents runs the complete real
// chain: public Open over the shipped builtin registration, the concrete
// production preparation, a real model turn whose workspace_symbol call
// registers a clangd-marker workspace with no resolvable binary, and the real
// LSP manager's detection warning flowing into the Runtime's global store and
// the runtime-scoped warning_changed hint.
func TestComposedLSPWarningLandsInGlobalStoreAndEvents(t *testing.T) {
	e := newComposeEnv(t)
	t.Setenv("PATH", t.TempDir()) // no resolvable language server anywhere
	server := newLSPModelServer(t)
	if err := os.WriteFile(e.configPath, []byte(lspConfigDocument(server.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(e.configPath), "agents.json"), []byte(lspAgentsDocument), 0o600); err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "CMakeLists.txt"), []byte("cmake_minimum_required(VERSION 3.10)\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	r, err := runtime.Open(ctx, runtime.Options{DataDir: e.dataDir, ConfigPath: e.configPath, Plugins: builtin.Plugins()})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// The owner closes on every exit: the deferred Close is idempotent and
	// joins the same shutdown, covering an assertion failure before the
	// server and data-root cleanups run.
	defer func() {
		if err := r.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	sub, err := r.Subscribe(256)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	session, err := runtime.CreateSessionForTest(ctx, r, workspace, "worker")
	if err != nil {
		t.Fatalf("createSession: %v", err)
	}
	if err := runtime.SubmitForTest(ctx, r, session.Identity.SessionID, "op-lsp", "find Foo"); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// The real warning arrives through the real manager's report: the
	// runtime-scoped warning_changed hint, then the global store read. The
	// wait blocks on the subscription with one bounded timeout.
	deadline := time.After(30 * time.Second)
	var hint *protocol.WarningChangedEvent
	for hint == nil {
		select {
		case event, ok := <-sub.Events():
			if !ok {
				t.Fatal("the subscription closed before the LSP warning hint")
			}
			kind, err := event.Discriminator()
			if err != nil || kind != "warning_changed" {
				continue
			}
			body, err := event.AsWarningChangedEvent()
			if err != nil {
				t.Fatalf("warning event body: %v", err)
			}
			if composedScopeKind(body.Scope) == "runtime" {
				hint = &body
			}
		case <-deadline:
			t.Fatal("the real LSP warning hint never arrived")
		}
	}
	if _, err := strconv.ParseUint(hint.WarningsRevision.Revision, 10, 64); err != nil {
		t.Fatalf("warning hint revision = %q, want a decimal value", hint.WarningsRevision.Revision)
	}

	warnings, err := runtime.GetWarningsForTest(r)
	if err != nil {
		t.Fatalf("GetWarningsForTest: %v", err)
	}
	found := false
	for _, warning := range warnings.Warnings {
		if warning.Source == "plugin:lsp" && strings.Contains(warning.Kind, "lsp_") {
			found = true
		}
	}
	if !found {
		t.Fatalf("the global warning read misses the real LSP report: %+v", warnings.Warnings)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
