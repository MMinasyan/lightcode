package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/MMinasyan/lightcode/internal/agent"
)

func (s *Server) handleAdapterSSE(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("token") != s.token {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	ch, client, unsub := s.adapter.subscribeClient("")
	defer unsub()
	s.replayPendingPermissionPrompts(client)

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			_, _ = w.Write(msg)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

type adapterRPCRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

var errBadRPCParams = errors.New("invalid params")

type rpcHandler func(s *Server, raw json.RawMessage) (any, error)

func rpc[P any](fn func(*Server, P) (any, error)) rpcHandler {
	return func(s *Server, raw json.RawMessage) (any, error) {
		var p P
		if len(raw) == 0 {
			raw = []byte(`{}`)
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, errBadRPCParams
		}
		return fn(s, p)
	}
}

func rpc0(fn func(*Server) (any, error)) rpcHandler {
	return func(s *Server, _ json.RawMessage) (any, error) { return fn(s) }
}

func okResult(err error) (any, error) {
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

type sessionParam struct {
	SessionID string `json:"session_id"`
}
type idParam struct {
	ID string `json:"id"`
}
type providerParam struct {
	ProviderID string `json:"provider_id"`
}
type adapterIDParam struct {
	AdapterID string `json:"adapter_id"`
}
type stateParam struct {
	State string `json:"state"`
}
type refParam struct {
	Ref string `json:"ref"`
}
type submitParam struct {
	SessionID string `json:"session_id"`
	Content   string `json:"content"`
}
type permActionParam struct {
	SessionID string `json:"session_id"`
	ID        string `json:"id"`
	Action    string `json:"action"`
}
type suggestSessionParam struct {
	SessionID string `json:"session_id"`
	Tool      string `json:"tool"`
	Arg       string `json:"arg"`
}
type suggestProjectParam struct {
	ProjectID string `json:"project_id"`
	Tool      string `json:"tool"`
	Arg       string `json:"arg"`
}
type savePermParam struct {
	SessionID string   `json:"session_id"`
	ID        string   `json:"id"`
	Patterns  []string `json:"patterns"`
}
type modelSwitchParam struct {
	SessionID string `json:"session_id"`
	Ref       string `json:"ref"`
}
type modelHiddenParam struct {
	Ref    string `json:"ref"`
	Hidden bool   `json:"hidden"`
}
type providerHiddenParam struct {
	ProviderID string `json:"provider_id"`
	Hidden     bool   `json:"hidden"`
}
type completeModelParam struct {
	Ref        string                `json:"ref"`
	Completion agent.ModelCompletion `json:"completion"`
}
type connectProviderParam struct {
	ProviderID string `json:"provider_id"`
	APIKey     string `json:"api_key"`
}
type customProviderParam struct {
	Request agent.CustomProviderRequest `json:"request"`
}
type providerConfigParam struct {
	ProviderID string                    `json:"provider_id"`
	Config     agent.ProviderConfigInput `json:"config"`
}
type providerFieldParam struct {
	ProviderID string `json:"provider_id"`
	Field      string `json:"field"`
}
type saveModelParam struct {
	ProviderID string                 `json:"provider_id"`
	ModelID    string                 `json:"model_id"`
	Config     agent.ModelConfigInput `json:"config"`
}
type modelIDParam struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
}
type modelFieldParam struct {
	ProviderID string `json:"provider_id"`
	ModelID    string `json:"model_id"`
	Field      string `json:"field"`
}
type runtimeConfigParam struct {
	Settings agent.RuntimeConfigSettings `json:"settings"`
}
type newSessionParam struct {
	ProjectID string `json:"project_id"`
	AgentType string `json:"agent_type"`
}
type newSessionPathParam struct {
	ProjectPath string `json:"project_path"`
	AgentType   string `json:"agent_type"`
}
type turnActionParam struct {
	SessionID      string `json:"session_id"`
	Turn           int    `json:"turn"`
	Action         string `json:"action"`
	AlsoRevertCode bool   `json:"also_revert_code"`
}
type turnParam struct {
	SessionID string `json:"session_id"`
	Turn      int    `json:"turn"`
}
type filePathParam struct {
	Path string `json:"path"`
}
type projectFileParam struct {
	ProjectPath string `json:"project_path"`
	Path        string `json:"path"`
}
type projectPathParam struct {
	ProjectPath string `json:"project_path"`
}
type listPathParam struct {
	ProjectPath string `json:"project_path"`
	State       string `json:"state"`
}

var rpcHandlers = map[string]rpcHandler{
	"AttachAdapter": rpc0(func(s *Server) (any, error) {
		return s.AttachAdapter()
	}),
	"DetachAdapter": rpc(func(s *Server, p adapterIDParam) (any, error) {
		if !s.DetachAdapter(p.AdapterID) {
			return nil, fmt.Errorf("unknown adapter")
		}
		return map[string]any{"ok": true}, nil
	}),
	"CurrentWarnings": rpc0(func(s *Server) (any, error) {
		return warningSnapshot(s.agent.CurrentWarnings()), nil
	}),
	"SubmitToSession": rpc(func(s *Server, p submitParam) (any, error) {
		return s.agent.SubmitToSession(s.srvCtx, p.SessionID, p.Content)
	}),
	"QueueSnapshotForSession": rpc(func(s *Server, p sessionParam) (any, error) {
		return s.agent.QueueSnapshotForSession(p.SessionID)
	}),
	"CancelSession": rpc(func(s *Server, p sessionParam) (any, error) {
		if err := s.agent.CancelSession(p.SessionID); err != nil {
			return nil, err
		}
		s.clearPermissionStateForSession(p.SessionID)
		return map[string]any{"ok": true}, nil
	}),
	"CompactNowForSession": rpc(func(s *Server, p sessionParam) (any, error) {
		return okResult(s.agent.CompactNowForSession(s.srvCtx, p.SessionID))
	}),
	"RespondPermissionActionForSession": rpc(func(s *Server, p permActionParam) (any, error) {
		if err := s.agent.RespondPermissionActionForSession(p.SessionID, p.ID, p.Action); err != nil {
			return nil, err
		}
		s.cancelPermissionTimer(p.ID)
		return map[string]any{"ok": true}, nil
	}),
	"PermissionSuggestForSession": rpc(func(s *Server, p suggestSessionParam) (any, error) {
		return s.agent.PermissionSuggestForSession(p.SessionID, p.Tool, p.Arg)
	}),
	"PermissionSuggestForProject": rpc(func(s *Server, p suggestProjectParam) (any, error) {
		return s.agent.PermissionSuggestForProject(p.ProjectID, p.Tool, p.Arg)
	}),
	"SaveProjectPermissionForSession": rpc(func(s *Server, p savePermParam) (any, error) {
		if err := s.agent.SaveProjectPermissionForSession(p.SessionID, p.ID, p.Patterns); err != nil {
			return nil, err
		}
		s.cancelPermissionTimer(p.ID)
		return map[string]any{"ok": true}, nil
	}),
	"SwitchModelForSession": rpc(func(s *Server, p modelSwitchParam) (any, error) {
		return okResult(s.agent.SwitchModelForSession(p.SessionID, p.Ref))
	}),
	"CurrentModelForSession": rpc(func(s *Server, p sessionParam) (any, error) {
		return s.agent.CurrentModelForSession(p.SessionID)
	}),
	"ModelList": rpc0(func(s *Server) (any, error) {
		return s.agent.ModelList(), nil
	}),
	"AllModelList": rpc0(func(s *Server) (any, error) {
		return s.agent.AllModelList(), nil
	}),
	"SetDefaultModel": rpc(func(s *Server, p refParam) (any, error) {
		return okResult(s.agent.SetDefaultModel(p.Ref))
	}),
	"SetModelHidden": rpc(func(s *Server, p modelHiddenParam) (any, error) {
		return okResult(s.agent.SetModelHidden(p.Ref, p.Hidden))
	}),
	"SetProviderHidden": rpc(func(s *Server, p providerHiddenParam) (any, error) {
		return okResult(s.agent.SetProviderHidden(p.ProviderID, p.Hidden))
	}),
	"CompleteModelEntry": rpc(func(s *Server, p completeModelParam) (any, error) {
		return okResult(s.agent.CompleteModelEntry(p.Ref, p.Completion))
	}),
	"ProviderList": rpc0(func(s *Server) (any, error) {
		return s.agent.ProviderList(), nil
	}),
	"ConnectProvider": rpc(func(s *Server, p connectProviderParam) (any, error) {
		return okResult(s.agent.ConnectProvider(p.ProviderID, p.APIKey))
	}),
	"DiscoverCustomProvider": rpc(func(s *Server, p customProviderParam) (any, error) {
		return s.agent.DiscoverCustomProvider(p.Request)
	}),
	"AddCustomProvider": rpc(func(s *Server, p customProviderParam) (any, error) {
		return okResult(s.agent.AddCustomProvider(p.Request))
	}),
	"DisconnectProvider": rpc(func(s *Server, p providerParam) (any, error) {
		return okResult(s.agent.DisconnectProvider(p.ProviderID))
	}),
	"RemoveProvider": rpc(func(s *Server, p providerParam) (any, error) {
		return okResult(s.agent.RemoveProvider(p.ProviderID))
	}),
	"GenerateAPIKeyEnvName": rpc(func(s *Server, p providerParam) (any, error) {
		return s.agent.GenerateAPIKeyEnvName(p.ProviderID), nil
	}),
	"GetProviderConfig": rpc(func(s *Server, p providerParam) (any, error) {
		return s.agent.GetProviderConfig(p.ProviderID)
	}),
	"DiscoverableModels": rpc(func(s *Server, p providerParam) (any, error) {
		return s.agent.DiscoverableModels(p.ProviderID)
	}),
	"SetProviderConfig": rpc(func(s *Server, p providerConfigParam) (any, error) {
		return okResult(s.agent.SetProviderConfig(p.ProviderID, p.Config))
	}),
	"ResetProviderField": rpc(func(s *Server, p providerFieldParam) (any, error) {
		return okResult(s.agent.ResetProviderField(p.ProviderID, p.Field))
	}),
	"SaveModel": rpc(func(s *Server, p saveModelParam) (any, error) {
		return okResult(s.agent.SaveModel(p.ProviderID, p.ModelID, p.Config))
	}),
	"DeleteModel": rpc(func(s *Server, p modelIDParam) (any, error) {
		return okResult(s.agent.DeleteModel(p.ProviderID, p.ModelID))
	}),
	"ResetModelField": rpc(func(s *Server, p modelFieldParam) (any, error) {
		return okResult(s.agent.ResetModelField(p.ProviderID, p.ModelID, p.Field))
	}),
	"Reload": rpc0(func(s *Server) (any, error) {
		return okResult(s.agent.Reload())
	}),
	"GetRuntimeConfig": rpc0(func(s *Server) (any, error) {
		return s.agent.GetRuntimeConfig(), nil
	}),
	"SetRuntimeConfig": rpc(func(s *Server, p runtimeConfigParam) (any, error) {
		return okResult(s.agent.SetRuntimeConfig(p.Settings))
	}),
	"TokenUsageForSession": rpc(func(s *Server, p sessionParam) (any, error) {
		return s.agent.TokenUsageForSession(p.SessionID)
	}),
	"SessionSummaryForSession": rpc(func(s *Server, p sessionParam) (any, error) {
		return s.agent.SessionSummaryForSession(p.SessionID)
	}),
	"SessionPayloadForSession": rpc(func(s *Server, p sessionParam) (any, error) {
		return s.agent.SessionPayloadForSession(p.SessionID)
	}),
	"SessionList": rpc(func(s *Server, p stateParam) (any, error) {
		return s.agent.SessionList(p.State)
	}),
	"OpenSession": rpc(func(s *Server, p idParam) (any, error) {
		return s.agent.OpenSession(p.ID)
	}),
	"NewSession": rpc(func(s *Server, p newSessionParam) (any, error) {
		return s.agent.NewSession(p.ProjectID, p.AgentType)
	}),
	"NewSessionForProjectPath": rpc(func(s *Server, p newSessionPathParam) (any, error) {
		return s.agent.NewSessionForProjectPath(p.ProjectPath, p.AgentType)
	}),
	"SessionArchive": rpc(func(s *Server, p idParam) (any, error) {
		if err := s.agent.SessionArchive(p.ID); err != nil {
			return nil, err
		}
		s.clearPermissionStateForSession(p.ID)
		return map[string]any{"ok": true}, nil
	}),
	"SessionDelete": rpc(func(s *Server, p idParam) (any, error) {
		if err := s.agent.SessionDelete(p.ID); err != nil {
			return nil, err
		}
		s.clearPermissionStateForSession(p.ID)
		return map[string]any{"ok": true}, nil
	}),
	"SessionMessagesFor": rpc(func(s *Server, p idParam) (any, error) {
		return s.agent.SessionMessagesFor(p.ID)
	}),
	"ApplyTurnActionForSession": rpc(func(s *Server, p turnActionParam) (any, error) {
		return s.agent.ApplyTurnActionForSession(p.SessionID, p.Turn, p.Action, p.AlsoRevertCode)
	}),
	"RevertCodeForSession": rpc(func(s *Server, p turnParam) (any, error) {
		return s.agent.RevertCodeForSession(p.SessionID, p.Turn)
	}),
	"RevertHistoryForSession": rpc(func(s *Server, p turnParam) (any, error) {
		return okResult(s.agent.RevertHistoryForSession(p.SessionID, p.Turn))
	}),
	"SnapshotListForSession": rpc(func(s *Server, p sessionParam) (any, error) {
		return s.agent.SnapshotListForSession(p.SessionID)
	}),
	"ReadFileContent": rpc(func(s *Server, p filePathParam) (any, error) {
		return s.agent.ReadFileContent(p.Path)
	}),
	"ReadFileContentForProjectPath": rpc(func(s *Server, p projectFileParam) (any, error) {
		return s.agent.ReadFileContentForProjectPath(p.ProjectPath, p.Path)
	}),
	"ProjectName": rpc0(func(s *Server) (any, error) {
		return s.agent.ProjectName(), nil
	}),
	"ProjectRoot": rpc0(func(s *Server) (any, error) {
		return s.agent.ProjectRoot(), nil
	}),
	"ProjectCurrent": rpc0(func(s *Server) (any, error) {
		return s.agent.ProjectCurrent(), nil
	}),
	"ProjectCurrentForPath": rpc(func(s *Server, p projectPathParam) (any, error) {
		return s.agent.ProjectCurrentForPath(p.ProjectPath)
	}),
	"ProjectList": rpc0(func(s *Server) (any, error) {
		return s.agent.ProjectList()
	}),
	"SessionListForProjectPath": rpc(func(s *Server, p listPathParam) (any, error) {
		return s.agent.SessionListForProjectPath(p.ProjectPath, p.State)
	}),
}

func (s *Server) handleAdapterRPC(w http.ResponseWriter, r *http.Request) {
	var req adapterRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	h, ok := rpcHandlers[req.Method]
	if !ok {
		jsonError(w, "unknown adapter method", http.StatusNotFound)
		return
	}
	out, err := h(s, req.Params)
	if err != nil {
		if errors.Is(err, errBadRPCParams) {
			jsonError(w, "invalid params", http.StatusBadRequest)
			return
		}
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	jsonResp(w, http.StatusOK, out)
}
