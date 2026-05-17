package lsp

import (
	"strings"
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
	if !strings.Contains(signal, "<system-signal>hello</system-signal>") {
		t.Fatalf("signal = %q", signal)
	}
}
