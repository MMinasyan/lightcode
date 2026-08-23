package lsp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/MMinasyan/lightcode/internal/lsp/server"
)

type Manager struct {
	mu          sync.Mutex
	instances   map[string]*instance
	projectRoot string
	home        string
	onWarning   func(kind, message string)
	onSignal    func(content string)
	// closed is set by ShutdownAll so a server still starting during shutdown is
	// torn down instead of registered after the shutdown snapshot.
	closed bool
}

func NewManager(projectRoot, home string) *Manager {
	return &Manager{
		instances:   make(map[string]*instance),
		projectRoot: projectRoot,
		home:        home,
	}
}

func (m *Manager) SetWarningHandler(fn func(kind, message string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onWarning = fn
}

func (m *Manager) SetSignalHandler(fn func(content string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onSignal = fn
}

func (m *Manager) Detect(ctx context.Context) {
	defs := server.DetectFromProject(m.projectRoot)
	var wg sync.WaitGroup
	for _, def := range defs {
		def := def
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.startServer(ctx, def)
		}()
	}
	wg.Wait()
}

// startServerProbe is a test seam invoked immediately after the provisional
// map insertion and m.mu release, immediately before inst.start: the
// deterministic barrier after provisional mapping. It exists solely to hold a
// start in the post-handoff window; nil in production.
var startServerProbe = func() {}

// startServer launches one detected server. The instance is provisionally
// mapped before its start: the mapping is the admission handoff that lets a
// concurrent ShutdownAll snapshot it, so a server that starts after close
// began is marked terminal and self-reaps instead of becoming an untracked
// process. A closed manager rejects the start before instance.start, so no
// process is ever launched after close.
func (m *Manager) startServer(ctx context.Context, def *server.Definition) {
	// One short early admission check before any binary resolution, install,
	// instance construction, or resource/process start: a closed manager
	// rejects immediately. The map-time closed check and provisional mapping
	// below stay exactly as-is — close can race a pre-admitted slow
	// installation, and the mapping is what lets the close snapshot catch an
	// admitted start.
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return
	}
	binary := server.ResolveBinary(m.home, def)
	if binary == "" {
		if err := m.install(def); err != nil {
			m.emitWarning("lsp_install_failed",
				fmt.Sprintf("Failed to install %s language server: %v", def.Name, err))
			m.emitSignal(fmt.Sprintf("The %s language server could not be installed (%v). "+
				"LSP tools (diagnostics, go_to_definition, find_references, hover, go_to_implementation) "+
				"will not work for %s files. Use read_file and run_command (grep) instead.",
				def.Name, err, strings.Join(def.Extensions, ", ")))
			return
		}
		binary = server.ResolveBinary(m.home, def)
		if binary == "" {
			m.emitWarning("lsp_install_failed",
				fmt.Sprintf("Installed %s but binary not found", def.Name))
			return
		}
	}

	inst := newInstance(def, m.projectRoot, m.home, func(name string) {
		m.emitWarning("lsp_server_unavailable",
			fmt.Sprintf("Language server %s has crashed repeatedly and is unavailable.", name))
		m.emitSignal(fmt.Sprintf("The %s language server is unavailable due to repeated crashes. "+
			"LSP tools will not work for %s files. Use read_file and run_command (grep) instead.",
			name, strings.Join(def.Extensions, ", ")))
	})

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		// Permanent close won before this start was admitted: reject before
		// instance.start, so no process is launched after close.
		return
	}
	m.instances[def.Name] = inst
	m.mu.Unlock()

	// The post-handoff probe: the deterministic barrier after provisional
	// mapping. It never runs in production.
	startServerProbe()

	if _, err := inst.start(ctx); err != nil {
		m.mu.Lock()
		if m.instances[def.Name] == inst {
			delete(m.instances, def.Name)
		}
		m.mu.Unlock()
		m.emitWarning("lsp_server_unavailable",
			fmt.Sprintf("Failed to start %s: %v", def.Name, err))
		return
	}
}

func (m *Manager) ForFile(path string) *instance {
	ext := filepath.Ext(path)
	if ext == "" {
		return nil
	}
	def := server.ForExtension(ext)
	if def == nil {
		return nil
	}
	m.mu.Lock()
	inst := m.instances[def.Name]
	m.mu.Unlock()
	return inst
}

func (m *Manager) AllInstances() []*instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*instance, 0, len(m.instances))
	for _, inst := range m.instances {
		out = append(out, inst)
	}
	return out
}

// CloseAdmission stops new server starts without waiting: under m.mu it sets
// closed, snapshots every mapped instance, releases the lock, and marks each
// instance's admission closed (nonblocking). It is the owner's LSP-admission
// boundary, taken at the start of shutdown; ShutdownAll then kills and reaps
// the snapshot.
func (m *Manager) CloseAdmission() {
	m.mu.Lock()
	m.closed = true
	instances := make([]*instance, 0, len(m.instances))
	for _, inst := range m.instances {
		instances = append(instances, inst)
	}
	m.mu.Unlock()

	for _, inst := range instances {
		inst.closeAdmission()
	}
}

// ShutdownAll closes LSP admission and tears down every mapped instance for
// the owner's shutdown: each server's process is killed and reaped directly
// instead of negotiating a shutdown, because the owner is exiting on every
// host and the servers are our own child processes. Nothing here waits on a
// server's answer, so one unresponsive server cannot delay the teardown of
// the others.
func (m *Manager) ShutdownAll() {
	m.CloseAdmission()
	for _, inst := range m.AllInstances() {
		inst.killForTeardown()
	}
}

func (m *Manager) install(def *server.Definition) error {
	if def.Install == nil {
		return fmt.Errorf("%s must be installed via your system package manager (apt, dnf, pacman, brew, etc.)", def.Name)
	}
	cacheDir := server.CacheDir(m.home, def.Name)
	if err := os.MkdirAll(cacheDir, 0755); err != nil {
		return err
	}
	return def.Install(cacheDir)
}

func (m *Manager) emitWarning(kind, message string) {
	m.mu.Lock()
	fn := m.onWarning
	m.mu.Unlock()
	if fn != nil {
		fn(kind, message)
	}
}

func (m *Manager) emitSignal(content string) {
	m.mu.Lock()
	fn := m.onSignal
	m.mu.Unlock()
	if fn != nil {
		fn(content)
	}
}
