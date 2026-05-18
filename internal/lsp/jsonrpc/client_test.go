package jsonrpc

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

type writeCloserBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *writeCloserBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *writeCloserBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *writeCloserBuffer) Close() error { return nil }

func TestClientCallSuccessAndServerError(t *testing.T) {
	stdin := &writeCloserBuffer{}
	stdoutR, stdoutW := io.Pipe()
	client := NewClient(stdin, stdoutR, nil, nil)
	client.Start()
	defer client.Close()

	resultCh := make(chan struct {
		data json.RawMessage
		err  error
	}, 1)
	go func() {
		data, err := client.Call(context.Background(), "workspace/symbol", map[string]any{"query": "x"})
		resultCh <- struct {
			data json.RawMessage
			err  error
		}{data: data, err: err}
	}()
	waitForWrittenRequest(t, stdin, "workspace/symbol")
	writeJSONRPCMessage(t, stdoutW, message{JSONRPC: "2.0", ID: intPtr(0), Result: json.RawMessage(`{"ok":true}`)})
	got := <-resultCh
	if got.err != nil || string(got.data) != `{"ok":true}` {
		t.Fatalf("Call success = %s, %v", got.data, got.err)
	}

	go func() {
		data, err := client.Call(context.Background(), "bad", nil)
		resultCh <- struct {
			data json.RawMessage
			err  error
		}{data: data, err: err}
	}()
	waitForWrittenRequest(t, stdin, `"bad"`)
	writeJSONRPCMessage(t, stdoutW, message{JSONRPC: "2.0", ID: intPtr(1), Error: &ResponseError{Code: -32000, Message: "boom"}})
	got = <-resultCh
	if got.err == nil || !strings.Contains(got.err.Error(), "boom") {
		t.Fatalf("Call server error = %s, %v; want boom error", got.data, got.err)
	}
}

func TestClientNotifyAndOnNotify(t *testing.T) {
	stdin := &writeCloserBuffer{}
	stdoutR, stdoutW := io.Pipe()
	notifyCh := make(chan string, 1)
	client := NewClient(stdin, stdoutR, nil, func(method string, params json.RawMessage) { notifyCh <- method + ":" + string(params) })
	client.Start()
	defer client.Close()

	if err := client.Notify("initialized", map[string]any{"ok": true}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if !strings.Contains(stdin.String(), "initialized") {
		t.Fatalf("stdin after Notify = %q", stdin.String())
	}
	writeJSONRPCMessage(t, stdoutW, message{JSONRPC: "2.0", Method: "window/logMessage", Params: json.RawMessage(`{"message":"hi"}`)})
	select {
	case got := <-notifyCh:
		if !strings.Contains(got, "window/logMessage") || !strings.Contains(got, "hi") {
			t.Fatalf("notification = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("OnNotify not called")
	}
}

func TestClientCallContextCanceledAndClose(t *testing.T) {
	stdin := &writeCloserBuffer{}
	stdoutR, _ := io.Pipe()
	client := NewClient(stdin, stdoutR, nil, nil)
	client.Start()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := client.Call(ctx, "slow", nil); err == nil {
		t.Fatal("Call with canceled context error = nil, want error")
	}
	client.Close()
	if !client.IsClosed() {
		t.Fatal("IsClosed = false after Close")
	}
	if err := client.Notify("x", nil); err == nil {
		t.Fatal("Notify after Close error = nil, want error")
	}
}

func waitForWrittenRequest(t *testing.T, buf *writeCloserBuffer, contains string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), contains) {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("buffer never contained %q: %q", contains, buf.String())
}

func writeJSONRPCMessage(t *testing.T, w io.Writer, msg message) {
	t.Helper()
	body, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteMessage(w, body); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
}

func intPtr(v int) *int { return &v }
