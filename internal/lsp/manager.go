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

func (m *Manager) startServer(ctx context.Context, def *server.Definition) {
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

	if _, err := inst.start(ctx); err != nil {
		m.emitWarning("lsp_server_unavailable",
			fmt.Sprintf("Failed to start %s: %v", def.Name, err))
		return
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		// Shutdown began while this server was starting; dispose of it the
		// same way the teardown does, so the one server that appears after
		// the shutdown snapshot is killed and reaped instead of negotiated
		// with after the owner has stopped waiting for anything.
		inst.killForTeardown()
		return
	}
	m.instances[def.Name] = inst
	m.mu.Unlock()
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

// ShutdownAll tears down every instance for the owner's shutdown: each
// server's process is killed and reaped directly instead of negotiating a
// shutdown, because the owner is exiting on every host and the servers are
// our own child processes. Nothing here waits on a server's answer, so one
// unresponsive server cannot delay the teardown of the others.
func (m *Manager) ShutdownAll() {
	m.mu.Lock()
	m.closed = true
	instances := make([]*instance, 0, len(m.instances))
	for _, inst := range m.instances {
		instances = append(instances, inst)
	}
	m.mu.Unlock()

	for _, inst := range instances {
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
