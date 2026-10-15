package acpclient_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/clientops"
	"github.com/MMinasyan/lightcode/internal/plugins/builtin"
	"github.com/MMinasyan/lightcode/protocol"
	"github.com/MMinasyan/lightcode/runtime"
)

// The mounted fixture drives the real isolated Runtime behind the real
// bridge: one production owner, its real listener and discovery record, and
// one parked OpenAI-compatible model endpoint whose admitted work proves the
// departing bridge cancels nothing.

const mountedConfigDocument = `{"providers":{"prov":{"transport":{"base_url":%q,"api_key_env":"ACP_MOUNTED_KEY"},"discovery":false,"models":{"m":{"name":"M","context_window":4096,"max_output_tokens":512}}}}}`

const mountedAgentsDocument = `{"integrated":{"model":"prov/m","system_prompt":"simple"}}`

func writeServiceFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// isolateBundledCredentials empties every api_key_env declared by the bundled
// catalog so a captured build never starts a provider discovery attempt from
// an ambient shell key.
func isolateBundledCredentials(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "..", "internal", "catalog", "builtin"))
	if err != nil {
		t.Fatalf("read bundled catalog: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("..", "..", "internal", "catalog", "builtin", entry.Name()))
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

// newMountedRuntime opens one production-composition Runtime over isolated
// HOME, data, and config paths.
func newMountedRuntime(t *testing.T, baseURL string) (*runtime.Runtime, string, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ACP_MOUNTED_KEY", "mounted-test-key")
	isolateBundledCredentials(t)
	dataDir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	writeServiceFile(t, configPath, fmt.Sprintf(mountedConfigDocument, baseURL))
	writeServiceFile(t, agents.PathForConfig(configPath), mountedAgentsDocument)
	r, err := runtime.Open(context.Background(), runtime.Options{DataDir: dataDir, ConfigPath: configPath, Plugins: builtin.Plugins()})
	if err != nil {
		t.Fatalf("runtime.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := r.Close(context.Background()); err != nil {
			t.Errorf("runtime.Close: %v", err)
		}
	})
	return r, dataDir, configPath
}

// parkedModel is one loopback model endpoint whose first response holds until
// released, recording whether the request context was canceled while parked.
type parkedModel struct {
	*httptest.Server

	arrived     chan struct{}
	release     chan struct{}
	arriveOnce  sync.Once
	releaseOnce sync.Once
	mu          sync.Mutex
	canceled    bool
}

func newParkedModel(t *testing.T) *parkedModel {
	t.Helper()
	m := &parkedModel{arrived: make(chan struct{}), release: make(chan struct{})}
	m.Server = httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(func() {
		m.unblock()
		m.Server.Close()
	})
	return m
}

func (m *parkedModel) serve(w http.ResponseWriter, r *http.Request) {
	m.arriveOnce.Do(func() { close(m.arrived) })
	select {
	case <-m.release:
	case <-r.Context().Done():
		m.mu.Lock()
		m.canceled = true
		m.mu.Unlock()
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"released\"},\"finish_reason\":null}]}\n\n")
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func (m *parkedModel) unblock() {
	m.releaseOnce.Do(func() { close(m.release) })
}

func (m *parkedModel) wasCanceled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.canceled
}

// startMountedBridge starts one Runner over the mounted owner's real
// discovery record.
func startMountedBridge(t *testing.T, dataDir, configPath, workspace string) *bridge {
	t.Helper()
	return startRunner(t, clienthost.Options{
		DataDir:    dataDir,
		ConfigPath: configPath,
		Executable: filepath.Join(dataDir, "absent-runtime"),
	}, workspace, nil)
}

// TestACPMountedRuntimeDepartureKeepsAcceptedWork drives one real admitted
// Operation through the bridge, ends the bridge while the model call is
// parked, and proves the accepted work still settles through the owner.
func TestACPMountedRuntimeDepartureKeepsAcceptedWork(t *testing.T) {
	model := newParkedModel(t)
	owner, dataDir, configPath := newMountedRuntime(t, model.URL)
	ps, err := owner.OpenProtocol(context.Background())
	if err != nil {
		t.Fatalf("OpenProtocol: %v", err)
	}
	record := filepath.Join(dataDir, "runtime.json")
	if err := ps.PublishDiscovery(record); err != nil {
		t.Fatalf("PublishDiscovery: %v", err)
	}
	workspace := t.TempDir()
	direct, err := client.Connect(context.Background(), record)
	if err != nil {
		t.Fatalf("direct connect: %v", err)
	}

	b := startMountedBridge(t, dataDir, configPath, workspace)
	initialized := b.call(t, "1", "initialize", "")
	var description struct {
		Runtime protocol.Health `json:"runtime"`
	}
	if err := json.Unmarshal([]byte(resultOf(t, initialized)), &description); err != nil {
		t.Fatalf("initialize result: %v", err)
	}
	if description.Runtime.InstanceId != direct.InstanceID() {
		t.Fatalf("initialize runtime instance = %q, want the mounted owner %q", description.Runtime.InstanceId, direct.InstanceID())
	}

	created := b.call(t, "2", "runtime/CreateSession", fmt.Sprintf(`{"body":{"workspace":%q,"agent_type":"integrated"}}`, workspace))
	var session struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal([]byte(resultOf(t, created)), &session); err != nil || session.SessionID == "" {
		t.Fatalf("CreateSession result = %s (%v)", resultOf(t, created), err)
	}
	b.call(t, "3", "runtime/SubmitSession", fmt.Sprintf(`{"session_id":%q,"body":{"operation_id":"op-depart","mode":"regular","content":[{"kind":"text","text":"depart"}]}}`, session.SessionID))
	select {
	case <-model.arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("the admitted Operation never reached the model")
	}

	// The host departs while its accepted work is parked in the model.
	b.eof()
	if err := b.awaitRun(t); err != nil {
		t.Fatalf("Run after EOF = %v, want nil", err)
	}
	if model.wasCanceled() {
		t.Fatal("the departing bridge canceled the accepted model work")
	}

	model.unblock()
	awaitOperationSuccess(t, direct, session.SessionID, "op-depart")
	if model.wasCanceled() {
		t.Fatal("the accepted model work was canceled during settlement")
	}
}

// awaitOperationSuccess polls the real owner's hydration until the operation
// reaches success.
func awaitOperationSuccess(t *testing.T, c *client.Client, sessionID, operationID string) {
	t.Helper()
	params := json.RawMessage(fmt.Sprintf(`{"session_id":%q}`, sessionID))
	deadline := time.Now().Add(30 * time.Second)
	for {
		raw, err := clientops.Call(context.Background(), c, "GetSessionHydration", params)
		if err != nil {
			t.Fatalf("GetSessionHydration: %v", err)
		}
		var hydration protocol.Hydration
		if err := json.Unmarshal(raw, &hydration); err != nil {
			t.Fatalf("hydration: %v", err)
		}
		for _, operation := range hydration.Operations {
			if operation.OperationId == operationID && operation.Status == protocol.OperationStatusSuccess {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation %s never reached success: %s", operationID, raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
