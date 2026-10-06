package protocol_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/plugins/builtin"
	"github.com/MMinasyan/lightcode/runtime"
)

// The mounted generated-client composition fixture: one real Runtime over
// isolated HOME, data root, and configuration paths, one real loopback
// listener with its real mode-0600 discovery record, and one Node process
// driving the generated TypeScript fetch SDK against that owner. The Go side
// owns readiness, the discovery-file handshake, and the process bound; the
// credential travels only through the discovery file. The runtime import is
// test-only and intentional: the repository's dependency-isolation guard
// rejects it from production files.
const mountedOwnerConfig = `{"providers":{"prov":{"transport":{"base_url":"http://127.0.0.1:1/v1","api_key_env":"MOUNTED_PROTOCOL_KEY"},"discovery":false,"models":{"m":{"name":"M","context_window":4096}}}}}`
const mountedOwnerAgents = `{"solo":{"model":"prov/m","system_prompt":"simple"},"..":{"model":"prov/m","system_prompt":"simple"}}`

// mountedDiscoveryEnv is the one handshake value the Node process receives:
// the path to the temporary discovery record, never its contents.
const mountedDiscoveryEnv = "LIGHTCODE_PROTOCOL_DISCOVERY"

func writeMountedFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create directory for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// isolateMountedBundledCredentials empties every api_key_env declared by the
// bundled catalog so the isolated owner never starts a provider discovery
// attempt from an ambient shell key.
func isolateMountedBundledCredentials(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "internal", "catalog", "builtin"))
	if err != nil {
		t.Fatalf("read bundled catalog: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("..", "internal", "catalog", "builtin", entry.Name()))
		if err != nil {
			t.Fatalf("read bundled provider %s: %v", entry.Name(), err)
		}
		var provider struct {
			Transport struct {
				APIKeyEnv string `json:"api_key_env"`
			} `json:"transport"`
		}
		if err := json.Unmarshal(data, &provider); err != nil {
			t.Fatalf("decode bundled provider %s: %v", entry.Name(), err)
		}
		if provider.Transport.APIKeyEnv != "" {
			t.Setenv(provider.Transport.APIKeyEnv, "")
		}
	}
}

// mountedReporterPlugin is the mounted owner's real-attribution fixture: one
// Runtime-scoped factory that reports one warning through its
// composition-bound callback at Open, so the generated TypeScript client reads
// a real plugin:<registered ID> source over the wire.
func mountedReporterPlugin() runtime.Plugin {
	return runtime.Plugin{
		ID:       "mounted_reporter",
		Scope:    runtime.ScopeRuntime,
		Provides: []runtime.CapabilitySpec{runtime.Spec[any]("mounted.reporter")},
		Open: func(_ context.Context, info runtime.ScopeInfo, _ runtime.Bindings) (runtime.Instance, error) {
			if info.ReportWarning != nil {
				info.ReportWarning("mounted_notice", "mounted plugin warning")
			}
			return runtime.Instance{Values: map[string]any{"mounted.reporter": "ok"}}, nil
		},
	}
}

// TestMountedGeneratedTypeScriptClient mounts the real isolated owner once
// and proves the generated TypeScript SDK addresses its special provider and
// Agent identity query values over the real discovery-file handshake. The
// generated Go client's identical identity coverage lives with the Runtime
// transport tests.
func TestMountedGeneratedTypeScriptClient(t *testing.T) {
	frontendDir, err := filepath.Abs(filepath.Join("..", "frontend"))
	if err != nil {
		t.Fatalf("resolve frontend directory: %v", err)
	}
	vitest := filepath.Join(frontendDir, "node_modules", ".bin", "vitest")
	if _, err := os.Stat(vitest); err != nil {
		t.Skipf("frontend toolchain is not installed: %v", err)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skipf("node is not on PATH: %v", err)
	}

	t.Setenv("HOME", t.TempDir())
	t.Setenv("MOUNTED_PROTOCOL_KEY", "mounted-protocol-key")
	isolateMountedBundledCredentials(t)
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	writeMountedFile(t, configPath, mountedOwnerConfig)
	writeMountedFile(t, agents.PathForConfig(configPath), mountedOwnerAgents)
	owner, err := runtime.Open(context.Background(), runtime.Options{
		DataDir:    dataDir,
		ConfigPath: configPath,
		Plugins:    append(builtin.Plugins(), mountedReporterPlugin()),
	})
	if err != nil {
		t.Fatalf("runtime.Open: %v", err)
	}
	defer func() {
		if err := owner.Close(context.Background()); err != nil {
			t.Errorf("runtime.Close: %v", err)
		}
	}()

	server, err := owner.OpenProtocol(context.Background())
	if err != nil {
		t.Fatalf("OpenProtocol: %v", err)
	}
	record := filepath.Join(t.TempDir(), "discovery.json")
	if err := server.PublishDiscovery(record); err != nil {
		t.Fatalf("PublishDiscovery: %v", err)
	}
	recordBytes, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read discovery record: %v", err)
	}
	var discovery struct {
		Credential string `json:"credential"`
	}
	if err := json.Unmarshal(recordBytes, &discovery); err != nil {
		t.Fatalf("decode discovery record: %v", err)
	}
	if info, err := os.Stat(record); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("discovery record mode = %v (%v), want 0600", info, err)
	}

	// The generated TypeScript client: one Node process reads the discovery
	// record (never its contents through the environment) and drives the
	// generated fetch SDK over the special identities. The helper is bounded
	// and joined before the owner and temporary roots are torn down.
	nodeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	// This composed run uses the Go-owned deadline, not Vitest's unit-test
	// timeout across its entire sequence of real backend calls.
	cmd := exec.CommandContext(nodeCtx, vitest, "run", "protocol-mounted.test.js", "--testTimeout", "0")
	cmd.Dir = frontendDir
	cmd.Env = append(os.Environ(), mountedDiscoveryEnv+"="+record)
	cmd.WaitDelay = 15 * time.Second
	output, err := cmd.CombinedOutput()
	// The credential guard runs before any diagnostic prints the output, so a
	// failing process that echoes the credential is reported as a leak and
	// never echoed itself.
	if strings.Contains(string(output), discovery.Credential) {
		t.Fatal("the mounted generated TypeScript client output contains the credential")
	}
	if err != nil {
		t.Fatalf("mounted generated TypeScript client: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "1 passed") {
		t.Fatalf("mounted TypeScript row did not pass:\n%s", output)
	}
}
