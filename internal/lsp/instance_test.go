package lsp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/internal/lsp/protocol"
	"github.com/MMinasyan/lightcode/internal/lsp/server"
)

func TestInstanceDiagnosticsAndReadyNotification(t *testing.T) {
	inst := newInstance(&server.Definition{Name: "fake"}, t.TempDir(), t.TempDir(), context.Background(), nil)
	// A readiness notification only ever arrives from a launch that is under
	// way, so the instance must already be starting.
	inst.mu.Lock()
	inst.state = stateStarting
	inst.mu.Unlock()
	uri := protocol.URIFromPath("/tmp/main.go")
	severity := protocol.SeverityError
	params, err := json.Marshal(protocol.PublishDiagnosticsParams{
		URI:         uri,
		Diagnostics: []protocol.Diagnostic{{Severity: &severity, Message: "bad", Range: protocol.Range{Start: protocol.Position{Line: 3}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	inst.handleNotification("textDocument/publishDiagnostics", params, inst.readyCh)
	diags := inst.fileDiagnostics(uri)
	if len(diags) != 1 || diags[0].Message != "bad" || diags[0].Range.Start.Line != 3 {
		t.Fatalf("diagnostics = %+v", diags)
	}

	progress, err := json.Marshal(protocol.ProgressParams{Value: json.RawMessage(`{"kind":"end"}`)})
	if err != nil {
		t.Fatal(err)
	}
	inst.handleNotification("$/progress", progress, inst.readyCh)
	if err := inst.waitReady(context.Background()); err != nil {
		t.Fatalf("waitReady after progress end: %v", err)
	}
}

// TestCaptureDiagnosticsNeverSamplesClearedCacheAsLive pins the capture
// atomicity: the liveness decision and the diagnostic sample share one
// instance-mutex hold, so a transition that takes the mutex, starts a new
// attempt and clears the cache under the nested diagnostics lock cannot slip
// between them. Each iteration parks a capture on the instance mutex, creates
// that transition while the capture is parked, and releases the mutex, so the
// capture passes its liveness check while the transition is already contending
// for the same mutex and cache. A live sample must carry the seeded error, and
// the only other valid outcome is the unavailable refusal. A live-but-empty
// sample is the bug shape a separated liveness check and sample reproduces.
func TestCaptureDiagnosticsNeverSamplesClearedCacheAsLive(t *testing.T) {
	inst := newInstance(&server.Definition{Name: "fake"}, t.TempDir(), t.TempDir(), context.Background(), nil)
	path := filepath.Join(t.TempDir(), "seeded.go")
	uri := protocol.URIFromPath(path)
	severity := protocol.SeverityError
	seeded := []protocol.Diagnostic{{
		Severity: &severity,
		Message:  "seeded error",
		Range:    protocol.Range{Start: protocol.Position{Line: 2}},
	}}

	for i := 0; i < 256; i++ {
		// The no-process fixture: ready, with an open attempt channel and a
		// seeded error for the requested path.
		inst.mu.Lock()
		inst.state = stateReady
		inst.procDone = make(chan struct{})
		inst.mu.Unlock()
		inst.diagMu.Lock()
		inst.diagnostics = map[string][]protocol.Diagnostic{uri: seeded}
		inst.diagMu.Unlock()

		var wg sync.WaitGroup
		var sample [][]protocol.Diagnostic
		var ok bool

		// Park the capture on the instance mutex, then create the transition
		// while the capture is parked: releasing the mutex lets the parked
		// capture own it first and reach its liveness check while the
		// transition contends for the same mutex.
		inst.mu.Lock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			sample, ok = inst.captureDiagnostics([]string{path})
		}()
		runtime.Gosched()
		wg.Add(1)
		go func() {
			defer wg.Done()
			// The transition: a new attempt starts and its reset clears the
			// cache under the nested diagnostics lock.
			inst.mu.Lock()
			inst.state = stateStarting
			inst.diagMu.Lock()
			inst.diagnostics = make(map[string][]protocol.Diagnostic)
			inst.diagMu.Unlock()
			inst.mu.Unlock()
		}()
		inst.mu.Unlock()
		wg.Wait()

		if !ok {
			continue
		}
		if len(sample) != 1 || len(sample[0]) != 1 || sample[0][0].Message != "seeded error" {
			t.Fatalf("iteration %d: live sample = %+v, want the seeded error or the unavailable refusal", i, sample)
		}
	}
}

func TestInstanceWaitReadyCanceledWhenStarting(t *testing.T) {
	inst := newInstance(&server.Definition{Name: "fake"}, t.TempDir(), t.TempDir(), context.Background(), nil)
	// A launch is already under way; waiting on it must observe the caller's
	// cancellation rather than anything else.
	inst.mu.Lock()
	inst.state = stateStarting
	inst.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := inst.waitReady(ctx); err != context.Canceled {
		t.Fatalf("waitReady canceled = %v, want context.Canceled", err)
	}
}
