package lsp

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MMinasyan/lightcode/internal/lsp/server"
)

func TestManagerForFileAndAllInstances(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir())
	if got := m.ForFile("main.go"); got != nil {
		t.Fatalf("ForFile without instance = %+v, want nil", got)
	}
	if got := m.ForFile("README"); got != nil {
		t.Fatalf("ForFile without extension = %+v, want nil", got)
	}
	if got := m.ForFile("file.unknown"); got != nil {
		t.Fatalf("ForFile unknown extension = %+v, want nil", got)
	}

	def := server.ForExtension(".go")
	inst := newInstance(def, m.projectRoot, m.home, nil)
	m.instances[def.Name] = inst
	if got := m.ForFile("main.go"); got != inst {
		t.Fatalf("ForFile(main.go) = %+v, want inserted instance", got)
	}
	all := m.AllInstances()
	if len(all) != 1 || all[0] != inst {
		t.Fatalf("AllInstances = %+v, want inserted instance", all)
	}
}

func TestManagerConcurrentHandlersAndInstances(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir())
	def := server.ForExtension(".go")
	inst := newInstance(def, m.projectRoot, m.home, nil)
	m.mu.Lock()
	m.instances[def.Name] = inst
	m.mu.Unlock()

	var warnings atomic.Int64
	var signals atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				m.SetWarningHandler(func(string, string) { warnings.Add(1) })
				m.SetSignalHandler(func(string) { signals.Add(1) })
				m.emitWarning("kind", "message")
				m.emitSignal("signal")
				_ = m.ForFile("main.go")
				_ = m.AllInstances()
			}
		}()
	}
	wg.Wait()
	if warnings.Load() == 0 || signals.Load() == 0 {
		t.Fatalf("handlers were not called: warnings=%d signals=%d", warnings.Load(), signals.Load())
	}
}

func TestManagerWarningAndSignalHandlers(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir())
	var warningKind, warningMessage, signal string
	m.SetWarningHandler(func(kind, message string) { warningKind, warningMessage = kind, message })
	m.SetSignalHandler(func(content string) { signal = content })

	m.emitWarning("kind", "message")
	if warningKind != "kind" || warningMessage != "message" {
		t.Fatalf("warning = %q/%q", warningKind, warningMessage)
	}
	m.emitSignal("hello")
	if signal != "hello" {
		t.Fatalf("signal = %q, want raw payload", signal)
	}
}
