//go:build integration

package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestIntegrationRejectsForgedExecutePendingTool drives the removed execute_pending
// tool through the real agent loop (the same path a model-advertised call would use)
// and asserts the ordinary unknown-tool result, so a forged call cannot reach any
// staged-execution behavior even when it bypasses registry absence.
func TestIntegrationRejectsForgedExecutePendingTool(t *testing.T) {
	name := "execute_pending"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = readIntegrationRequest(t, r)
		if calls.Add(1) == 1 {
			writeSSE(w, toolCallChunk("call-forged", "test-model", "fc-"+name, name, "{}"), stopChunk("call-forged", "test-model"), "[DONE]")
		} else {
			writeSSE(w, textChunk("resp", "test-model", "handled"), stopChunk("resp", "test-model"), "[DONE]")
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
}
