//go:build integration

package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// TestIntegrationRejectsForgedMemoryToolCalls drives each removed tool name
// through the real agent loop (the same path a model-advertised call would use)
// and asserts the ordinary unknown-tool result, so a forged call cannot reach any
// memory behavior even when it bypasses the registry-absence check. A retained
// advertised tool is exercised in TestIntegrationExecutesRetainedAdvertisedTool.
func TestIntegrationRejectsForgedMemoryToolCalls(t *testing.T) {
	for _, name := range []string{"save_memory", "search_memory", "search_history"} {
		name := name
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = readIntegrationRequest(t, r)
				if calls.Add(1) == 1 {
					// The model asks for the removed tool; the loop dispatches it and
					// then re-requests to consume the unknown-tool result.
					writeSSE(w, toolCallChunk("call-forged-"+name, "test-model", "fc-"+name, name, "{}"), stopChunk("call-forged-"+name, "test-model"), "[DONE]")
				} else {
					writeSSE(w, textChunk("resp-"+name, "test-model", "handled"), stopChunk("resp-"+name, "test-model"), "[DONE]")
				}
			}))
			t.Cleanup(server.Close)

			a := newIntegrationAgent(t, server.URL)
			log, ctx := startIntegrationAgent(t, a)
			if _, err := a.Submit(ctx, "forge "+name); err != nil {
				t.Fatalf("Submit: %v", err)
			}
			if ev := log.waitFor(t, EventTurnEnd); ev.Cancelled {
				t.Fatalf("turn cancelled: %+v", ev)
			}
			assertToolResultContains(t, a, fmt.Sprintf("unknown tool %q", name))
		})
	}
}

// TestIntegrationExecutesRetainedAdvertisedTool is the positive sibling of the
// forged-call regression: it drives a retained advertised tool (read_file)
// through the same loop path and asserts it executes normally, so the unknown-tool
// behavior above is specific to the removed tools rather than a broken dispatch.
func TestIntegrationExecutesRetainedAdvertisedTool(t *testing.T) {
	filePath := "allowed.txt"
	var calls atomic.Int32
	var advertisedReadFile bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req := readIntegrationRequest(t, r)
		for _, tool := range req.Tools {
			fn, ok := tool["function"].(map[string]any)
			if !ok {
				continue
			}
			if name, ok := fn["name"].(string); ok && name == "read_file" {
				advertisedReadFile = true
			}
		}
		switch calls.Add(1) {
		case 1:
			writeSSE(w, toolCallChunk("call-read", "test-model", "call_read", "read_file", fmt.Sprintf(`{"path":%q}`, filePath)), stopChunk("call-read", "test-model"), "[DONE]")
		default:
			writeSSE(w, textChunk("resp-read", "test-model", "read complete"), stopChunk("resp-read", "test-model"), "[DONE]")
		}
	}))
	t.Cleanup(server.Close)

	a := newIntegrationAgent(t, server.URL)
	if err := os.WriteFile(filepath.Join(a.projectRoot, filePath), []byte("file contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	log := newIntegrationEventLog()
	a.SetEventHandler(func(ev Event) {
		log.collect(ev)
		if ev.Kind == EventPermissionRequest && ev.PermReq != nil {
			id := ev.PermReq.ID
			go func() { _ = a.RespondPermission(id, true) }()
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)
	if _, err := a.Submit(ctx, "read the file"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if ev := log.waitFor(t, EventTurnEnd); ev.Cancelled {
		t.Fatalf("turn cancelled: %+v", ev)
	}
	if !advertisedReadFile {
		t.Fatal("model request did not advertise retained advertised tool read_file")
	}
	assertToolResultContains(t, a, "file contents")
	assertAssistantMessageContains(t, a, "read complete")
}
