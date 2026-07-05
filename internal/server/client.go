package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

type Client struct {
	baseURL     string
	token       string
	projectRoot string
	http        *http.Client

	mu      sync.Mutex
	handler func(agent.Event)
	started bool

	lifeMu    sync.Mutex
	adapterID string
}

func NewClient(lf LockFile, projectRoot string) *Client {
	return &Client{
		baseURL:     fmt.Sprintf("http://127.0.0.1:%d", lf.Port),
		token:       lf.Token,
		projectRoot: projectRoot,
		http:        &http.Client{},
	}
}

func (c *Client) SetEventHandler(fn func(agent.Event)) {
	c.mu.Lock()
	c.handler = fn
	c.mu.Unlock()
}

func (c *Client) Init(ctx context.Context) {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return
	}
	c.started = true
	c.mu.Unlock()
	go c.streamEvents(ctx)
}

func (c *Client) AttachAdapter(ctx context.Context) error {
	id, err := rpcCall[string](ctx, c, "AttachAdapter", nil)
	if err != nil {
		return err
	}
	c.lifeMu.Lock()
	c.adapterID = id
	c.lifeMu.Unlock()
	return nil
}

func (c *Client) DetachAdapter(ctx context.Context) error {
	c.lifeMu.Lock()
	id := c.adapterID
	c.lifeMu.Unlock()
	if id == "" {
		return nil
	}
	if err := c.rpc(ctx, "DetachAdapter", map[string]any{"adapter_id": id}); err != nil {
		return err
	}
	c.lifeMu.Lock()
	if c.adapterID == id {
		c.adapterID = ""
	}
	c.lifeMu.Unlock()
	return nil
}

func (c *Client) RequestShutdown(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/owner/shutdown", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = resp.Status
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func (c *Client) streamEvents(ctx context.Context) {
	u := c.baseURL + "/v1/adapter/events?token=" + url.QueryEscape(c.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var eventName string
	var data bytes.Buffer
	flush := func() {
		if eventName != "agent_event" || data.Len() == 0 {
			eventName = ""
			data.Reset()
			return
		}
		var ev agent.Event
		if err := json.Unmarshal(data.Bytes(), &ev); err == nil {
			c.mu.Lock()
			handler := c.handler
			c.mu.Unlock()
			if handler != nil {
				handler(ev)
			}
		}
		eventName = ""
		data.Reset()
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, "event: ") {
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.WriteString(strings.TrimPrefix(line, "data: "))
		}
	}
	flush()
}

type rpcEnvelope struct {
	Method string `json:"method"`
	Params any    `json:"params"`
}

func rpcCall[T any](ctx context.Context, c *Client, method string, params any) (T, error) {
	var zero T
	body, err := json.Marshal(rpcEnvelope{Method: method, Params: params})
	if err != nil {
		return zero, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/adapter/rpc", bytes.NewReader(body))
	if err != nil {
		return zero, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return zero, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(data))
		if msg == "" {
			msg = resp.Status
		}
		return zero, fmt.Errorf("%s", msg)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return zero, nil
	}
	if err := json.Unmarshal(data, &zero); err != nil {
		return zero, err
	}
	return zero, nil
}

func (c *Client) rpc(ctx context.Context, method string, params any) error {
	_, err := rpcCall[map[string]any](ctx, c, method, params)
	return err
}

func (c *Client) CurrentWarnings() []agent.PromptWarning {
	out, _ := rpcCall[[]agent.PromptWarning](context.Background(), c, "CurrentWarnings", nil)
	return out
}

func (c *Client) SubmitToSession(ctx context.Context, sessionID string, content string) (agent.SubmitResult, error) {
	return rpcCall[agent.SubmitResult](ctx, c, "SubmitToSession", map[string]any{"session_id": sessionID, "content": content})
}

func (c *Client) QueueSnapshotForSession(sessionID string) (agent.QueueState, error) {
	return rpcCall[agent.QueueState](context.Background(), c, "QueueSnapshotForSession", map[string]any{"session_id": sessionID})
}

func (c *Client) CancelSession(sessionID string) error {
	return c.rpc(context.Background(), "CancelSession", map[string]any{"session_id": sessionID})
}

func (c *Client) CompactNowForSession(ctx context.Context, sessionID string) error {
	return c.rpc(ctx, "CompactNowForSession", map[string]any{"session_id": sessionID})
}

func (c *Client) RespondPermissionActionForSession(sessionID string, id string, action string) error {
	return c.rpc(context.Background(), "RespondPermissionActionForSession", map[string]any{"session_id": sessionID, "id": id, "action": action})
}

func (c *Client) PermissionSuggestForSession(sessionID, toolName, arg string) ([]agent.PermissionSuggestion, error) {
	return rpcCall[[]agent.PermissionSuggestion](context.Background(), c, "PermissionSuggestForSession", map[string]any{"session_id": sessionID, "tool": toolName, "arg": arg})
}

func (c *Client) PermissionSuggestForProject(projectID, toolName, arg string) ([]agent.PermissionSuggestion, error) {
	return rpcCall[[]agent.PermissionSuggestion](context.Background(), c, "PermissionSuggestForProject", map[string]any{"project_id": projectID, "tool": toolName, "arg": arg})
}

func (c *Client) SaveProjectPermissionForSession(sessionID string, id string, patterns []string) error {
	return c.rpc(context.Background(), "SaveProjectPermissionForSession", map[string]any{"session_id": sessionID, "id": id, "patterns": patterns})
}

func (c *Client) SwitchModelForSession(sessionID string, refStr string) error {
	return c.rpc(context.Background(), "SwitchModelForSession", map[string]any{"session_id": sessionID, "ref": refStr})
}

func (c *Client) CurrentModelForSession(sessionID string) (agent.ModelInfo, error) {
	return rpcCall[agent.ModelInfo](context.Background(), c, "CurrentModelForSession", map[string]any{"session_id": sessionID})
}

func (c *Client) ModelList() []agent.ModelListEntry {
	out, _ := rpcCall[[]agent.ModelListEntry](context.Background(), c, "ModelList", nil)
	return out
}

func (c *Client) AllModelList() []agent.ModelListEntry {
	out, _ := rpcCall[[]agent.ModelListEntry](context.Background(), c, "AllModelList", nil)
	return out
}

func (c *Client) SetDefaultModel(refStr string) error {
	return c.rpc(context.Background(), "SetDefaultModel", map[string]any{"ref": refStr})
}

func (c *Client) SetModelHidden(refStr string, hidden bool) error {
	return c.rpc(context.Background(), "SetModelHidden", map[string]any{"ref": refStr, "hidden": hidden})
}

func (c *Client) SetProviderHidden(providerID string, hidden bool) error {
	return c.rpc(context.Background(), "SetProviderHidden", map[string]any{"provider_id": providerID, "hidden": hidden})
}

func (c *Client) CompleteModelEntry(refStr string, completion agent.ModelCompletion) error {
	return c.rpc(context.Background(), "CompleteModelEntry", map[string]any{"ref": refStr, "completion": completion})
}

func (c *Client) ProviderList() []agent.ProviderStatus {
	out, _ := rpcCall[[]agent.ProviderStatus](context.Background(), c, "ProviderList", nil)
	return out
}

func (c *Client) ConnectProvider(providerID, apiKey string) error {
	return c.rpc(context.Background(), "ConnectProvider", map[string]any{"provider_id": providerID, "api_key": apiKey})
}

func (c *Client) DiscoverCustomProvider(req agent.CustomProviderRequest) ([]agent.DiscoveryModelCandidate, error) {
	return rpcCall[[]agent.DiscoveryModelCandidate](context.Background(), c, "DiscoverCustomProvider", map[string]any{"request": req})
}

func (c *Client) AddCustomProvider(req agent.CustomProviderRequest) error {
	return c.rpc(context.Background(), "AddCustomProvider", map[string]any{"request": req})
}

func (c *Client) DisconnectProvider(providerID string) error {
	return c.rpc(context.Background(), "DisconnectProvider", map[string]any{"provider_id": providerID})
}

func (c *Client) RemoveProvider(providerID string) error {
	return c.rpc(context.Background(), "RemoveProvider", map[string]any{"provider_id": providerID})
}

func (c *Client) GenerateAPIKeyEnvName(providerID string) string {
	out, _ := rpcCall[string](context.Background(), c, "GenerateAPIKeyEnvName", map[string]any{"provider_id": providerID})
	return out
}

func (c *Client) GetProviderConfig(providerID string) (agent.ProviderConfigView, error) {
	return rpcCall[agent.ProviderConfigView](context.Background(), c, "GetProviderConfig", map[string]any{"provider_id": providerID})
}

func (c *Client) DiscoverableModels(providerID string) ([]agent.DiscoveryModelCandidate, error) {
	return rpcCall[[]agent.DiscoveryModelCandidate](context.Background(), c, "DiscoverableModels", map[string]any{"provider_id": providerID})
}

func (c *Client) SetProviderConfig(providerID string, cfg agent.ProviderConfigInput) error {
	return c.rpc(context.Background(), "SetProviderConfig", map[string]any{"provider_id": providerID, "config": cfg})
}

func (c *Client) ResetProviderField(providerID string, field string) error {
	return c.rpc(context.Background(), "ResetProviderField", map[string]any{"provider_id": providerID, "field": field})
}

func (c *Client) SaveModel(providerID, modelID string, cfg agent.ModelConfigInput) error {
	return c.rpc(context.Background(), "SaveModel", map[string]any{"provider_id": providerID, "model_id": modelID, "config": cfg})
}

func (c *Client) DeleteModel(providerID, modelID string) error {
	return c.rpc(context.Background(), "DeleteModel", map[string]any{"provider_id": providerID, "model_id": modelID})
}

func (c *Client) ResetModelField(providerID, modelID, field string) error {
	return c.rpc(context.Background(), "ResetModelField", map[string]any{"provider_id": providerID, "model_id": modelID, "field": field})
}

func (c *Client) Reload() error {
	return c.rpc(context.Background(), "Reload", nil)
}

func (c *Client) GetRuntimeConfig() agent.RuntimeConfigSettings {
	out, _ := rpcCall[agent.RuntimeConfigSettings](context.Background(), c, "GetRuntimeConfig", nil)
	return out
}

func (c *Client) SetRuntimeConfig(settings agent.RuntimeConfigSettings) error {
	return c.rpc(context.Background(), "SetRuntimeConfig", map[string]any{"settings": settings})
}

func (c *Client) TokenUsageForSession(sessionID string) (agent.TokenReport, error) {
	return rpcCall[agent.TokenReport](context.Background(), c, "TokenUsageForSession", map[string]any{"session_id": sessionID})
}

func (c *Client) SessionSummaryForSession(sessionID string) (agent.SessionSummary, error) {
	return rpcCall[agent.SessionSummary](context.Background(), c, "SessionSummaryForSession", map[string]any{"session_id": sessionID})
}

func (c *Client) SessionPayloadForSession(sessionID string) (agent.SessionPayload, error) {
	return rpcCall[agent.SessionPayload](context.Background(), c, "SessionPayloadForSession", map[string]any{"session_id": sessionID})
}

func (c *Client) SessionList(state string) ([]agent.SessionSummary, error) {
	return rpcCall[[]agent.SessionSummary](context.Background(), c, "SessionListForProjectPath", map[string]any{"project_path": c.projectRoot, "state": state})
}

func (c *Client) OpenSession(id string) (agent.SessionSummary, error) {
	return rpcCall[agent.SessionSummary](context.Background(), c, "OpenSession", map[string]any{"id": id})
}

func (c *Client) NewSession(projectID string, agentType string) (string, error) {
	if strings.TrimSpace(projectID) == "" {
		return c.NewSessionForProjectPath(c.projectRoot, agentType)
	}
	return rpcCall[string](context.Background(), c, "NewSession", map[string]any{"project_id": projectID, "agent_type": agentType})
}

func (c *Client) NewSessionForProjectPath(projectPath string, agentType string) (string, error) {
	return rpcCall[string](context.Background(), c, "NewSessionForProjectPath", map[string]any{"project_path": projectPath, "agent_type": agentType})
}

func (c *Client) SessionArchive(id string) error {
	return c.rpc(context.Background(), "SessionArchive", map[string]any{"id": id})
}

func (c *Client) SessionDelete(id string) error {
	return c.rpc(context.Background(), "SessionDelete", map[string]any{"id": id})
}

func (c *Client) SessionMessagesFor(id string) ([]agent.DisplayMessage, error) {
	return rpcCall[[]agent.DisplayMessage](context.Background(), c, "SessionMessagesFor", map[string]any{"id": id})
}

func (c *Client) ApplyTurnActionForSession(sessionID string, turn int, action string, alsoRevertCode bool) (agent.TurnActionResult, error) {
	return rpcCall[agent.TurnActionResult](context.Background(), c, "ApplyTurnActionForSession", map[string]any{"session_id": sessionID, "turn": turn, "action": action, "also_revert_code": alsoRevertCode})
}

func (c *Client) RevertCodeForSession(sessionID string, turn int) (snapshot.RevertResult, error) {
	return rpcCall[snapshot.RevertResult](context.Background(), c, "RevertCodeForSession", map[string]any{"session_id": sessionID, "turn": turn})
}

func (c *Client) RevertHistoryForSession(sessionID string, turn int) error {
	return c.rpc(context.Background(), "RevertHistoryForSession", map[string]any{"session_id": sessionID, "turn": turn})
}

func (c *Client) SnapshotListForSession(sessionID string) ([]agent.Snapshot, error) {
	return rpcCall[[]agent.Snapshot](context.Background(), c, "SnapshotListForSession", map[string]any{"session_id": sessionID})
}

func (c *Client) ReadFileContent(path string) (string, error) {
	return c.ReadFileContentForProjectPath(c.projectRoot, path)
}

func (c *Client) ReadFileContentForProjectPath(projectPath string, path string) (string, error) {
	return rpcCall[string](context.Background(), c, "ReadFileContentForProjectPath", map[string]any{"project_path": projectPath, "path": path})
}

func (c *Client) ProjectName() string {
	return filepath.Base(c.projectRoot)
}

func (c *Client) ProjectRoot() string {
	return c.projectRoot
}

func (c *Client) ProjectCurrent() agent.ProjectSummary {
	out, _ := c.ProjectCurrentForPath(c.projectRoot)
	return out
}

func (c *Client) ProjectCurrentForPath(projectPath string) (agent.ProjectSummary, error) {
	return rpcCall[agent.ProjectSummary](context.Background(), c, "ProjectCurrentForPath", map[string]any{"project_path": projectPath})
}

func (c *Client) ProjectList() ([]agent.ProjectSummary, error) {
	return rpcCall[[]agent.ProjectSummary](context.Background(), c, "ProjectList", nil)
}

func (c *Client) SessionListForProjectPath(projectPath string, state string) ([]agent.SessionSummary, error) {
	return rpcCall[[]agent.SessionSummary](context.Background(), c, "SessionListForProjectPath", map[string]any{"project_path": projectPath, "state": state})
}

var _ agent.AdapterService = (*Client)(nil)
