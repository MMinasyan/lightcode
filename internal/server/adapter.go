package server

import (
	"encoding/json"
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

	ch, unsub := s.adapter.subscribe("")
	defer unsub()

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

func (s *Server) handleAdapterRPC(w http.ResponseWriter, r *http.Request) {
	var req adapterRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, "invalid body", http.StatusBadRequest)
		return
	}
	var out any
	var err error
	switch req.Method {
	case "AttachAdapter":
		out, err = s.AttachAdapter()
	case "DetachAdapter":
		var p struct {
			AdapterID string `json:"adapter_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		if !s.DetachAdapter(p.AdapterID) {
			err = fmt.Errorf("unknown adapter")
		}
		out = map[string]any{"ok": true}
	case "CurrentWarnings":
		out = warningSnapshot(s.agent.CurrentWarnings())
	case "SubmitToSession":
		var p struct {
			SessionID string `json:"session_id"`
			Content   string `json:"content"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.SubmitToSession(s.srvCtx, p.SessionID, p.Content)
	case "QueueSnapshotForSession":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.QueueSnapshotForSession(p.SessionID)
	case "CancelSession":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.CancelSession(p.SessionID)
		out = map[string]any{"ok": err == nil}
	case "CompactNowForSession":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.CompactNowForSession(s.srvCtx, p.SessionID)
		out = map[string]any{"ok": err == nil}
	case "RespondPermissionActionForSession":
		var p struct {
			SessionID string `json:"session_id"`
			ID        string `json:"id"`
			Action    string `json:"action"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.RespondPermissionActionForSession(p.SessionID, p.ID, p.Action)
		out = map[string]any{"ok": err == nil}
	case "PermissionSuggestForSession":
		var p struct {
			SessionID string `json:"session_id"`
			Tool      string `json:"tool"`
			Arg       string `json:"arg"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.PermissionSuggestForSession(p.SessionID, p.Tool, p.Arg)
	case "PermissionSuggestForProject":
		var p struct {
			ProjectID string `json:"project_id"`
			Tool      string `json:"tool"`
			Arg       string `json:"arg"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.PermissionSuggestForProject(p.ProjectID, p.Tool, p.Arg)
	case "SaveProjectPermissionForSession":
		var p struct {
			SessionID string   `json:"session_id"`
			ID        string   `json:"id"`
			Patterns  []string `json:"patterns"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.SaveProjectPermissionForSession(p.SessionID, p.ID, p.Patterns)
		out = map[string]any{"ok": err == nil}
	case "SwitchModelForSession":
		var p struct {
			SessionID string `json:"session_id"`
			Ref       string `json:"ref"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.SwitchModelForSession(p.SessionID, p.Ref)
		out = map[string]any{"ok": err == nil}
	case "CurrentModelForSession":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.CurrentModelForSession(p.SessionID)
	case "ModelList":
		out = s.agent.ModelList()
	case "AllModelList":
		out = s.agent.AllModelList()
	case "SetDefaultModel":
		var p struct {
			Ref string `json:"ref"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.SetDefaultModel(p.Ref)
		out = map[string]any{"ok": err == nil}
	case "SetModelHidden":
		var p struct {
			Ref    string `json:"ref"`
			Hidden bool   `json:"hidden"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.SetModelHidden(p.Ref, p.Hidden)
		out = map[string]any{"ok": err == nil}
	case "SetProviderHidden":
		var p struct {
			ProviderID string `json:"provider_id"`
			Hidden     bool   `json:"hidden"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.SetProviderHidden(p.ProviderID, p.Hidden)
		out = map[string]any{"ok": err == nil}
	case "CompleteModelEntry":
		var p struct {
			Ref        string                `json:"ref"`
			Completion agent.ModelCompletion `json:"completion"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.CompleteModelEntry(p.Ref, p.Completion)
		out = map[string]any{"ok": err == nil}
	case "ProviderList":
		out = s.agent.ProviderList()
	case "ConnectProvider":
		var p struct {
			ProviderID string `json:"provider_id"`
			APIKey     string `json:"api_key"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.ConnectProvider(p.ProviderID, p.APIKey)
		out = map[string]any{"ok": err == nil}
	case "DiscoverCustomProvider":
		var p struct {
			Request agent.CustomProviderRequest `json:"request"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.DiscoverCustomProvider(p.Request)
	case "AddCustomProvider":
		var p struct {
			Request agent.CustomProviderRequest `json:"request"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.AddCustomProvider(p.Request)
		out = map[string]any{"ok": err == nil}
	case "DisconnectProvider":
		var p struct {
			ProviderID string `json:"provider_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.DisconnectProvider(p.ProviderID)
		out = map[string]any{"ok": err == nil}
	case "RemoveProvider":
		var p struct {
			ProviderID string `json:"provider_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.RemoveProvider(p.ProviderID)
		out = map[string]any{"ok": err == nil}
	case "GenerateAPIKeyEnvName":
		var p struct {
			ProviderID string `json:"provider_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out = s.agent.GenerateAPIKeyEnvName(p.ProviderID)
	case "GetProviderConfig":
		var p struct {
			ProviderID string `json:"provider_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.GetProviderConfig(p.ProviderID)
	case "DiscoverableModels":
		var p struct {
			ProviderID string `json:"provider_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.DiscoverableModels(p.ProviderID)
	case "SetProviderConfig":
		var p struct {
			ProviderID string                    `json:"provider_id"`
			Config     agent.ProviderConfigInput `json:"config"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.SetProviderConfig(p.ProviderID, p.Config)
		out = map[string]any{"ok": err == nil}
	case "ResetProviderField":
		var p struct {
			ProviderID string `json:"provider_id"`
			Field      string `json:"field"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.ResetProviderField(p.ProviderID, p.Field)
		out = map[string]any{"ok": err == nil}
	case "SaveModel":
		var p struct {
			ProviderID string                 `json:"provider_id"`
			ModelID    string                 `json:"model_id"`
			Config     agent.ModelConfigInput `json:"config"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.SaveModel(p.ProviderID, p.ModelID, p.Config)
		out = map[string]any{"ok": err == nil}
	case "DeleteModel":
		var p struct {
			ProviderID string `json:"provider_id"`
			ModelID    string `json:"model_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.DeleteModel(p.ProviderID, p.ModelID)
		out = map[string]any{"ok": err == nil}
	case "ResetModelField":
		var p struct {
			ProviderID string `json:"provider_id"`
			ModelID    string `json:"model_id"`
			Field      string `json:"field"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.ResetModelField(p.ProviderID, p.ModelID, p.Field)
		out = map[string]any{"ok": err == nil}
	case "Reload":
		err = s.agent.Reload()
		out = map[string]any{"ok": err == nil}
	case "GetRuntimeConfig":
		out = s.agent.GetRuntimeConfig()
	case "SetRuntimeConfig":
		var p struct {
			Settings agent.RuntimeConfigSettings `json:"settings"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.SetRuntimeConfig(p.Settings)
		out = map[string]any{"ok": err == nil}
	case "TokenUsageForSession":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.TokenUsageForSession(p.SessionID)
	case "SessionSummaryForSession":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.SessionSummaryForSession(p.SessionID)
	case "SessionPayloadForSession":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.SessionPayloadForSession(p.SessionID)
	case "SessionList":
		var p struct {
			State string `json:"state"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.SessionList(p.State)
	case "OpenSession":
		var p struct {
			ID string `json:"id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.OpenSession(p.ID)
	case "NewSession":
		var p struct {
			ProjectID string `json:"project_id"`
			AgentType string `json:"agent_type"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.NewSession(p.ProjectID, p.AgentType)
	case "NewSessionForProjectPath":
		var p struct {
			ProjectPath string `json:"project_path"`
			AgentType   string `json:"agent_type"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.NewSessionForProjectPath(p.ProjectPath, p.AgentType)
	case "SessionArchive":
		var p struct {
			ID string `json:"id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.SessionArchive(p.ID)
	case "SessionDelete":
		var p struct {
			ID string `json:"id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.SessionDelete(p.ID)
	case "SessionMessagesFor":
		var p struct {
			ID string `json:"id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.SessionMessagesFor(p.ID)
	case "ApplyTurnActionForSession":
		var p struct {
			SessionID      string `json:"session_id"`
			Turn           int    `json:"turn"`
			Action         string `json:"action"`
			AlsoRevertCode bool   `json:"also_revert_code"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.ApplyTurnActionForSession(p.SessionID, p.Turn, p.Action, p.AlsoRevertCode)
	case "RevertCodeForSession":
		var p struct {
			SessionID string `json:"session_id"`
			Turn      int    `json:"turn"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.RevertCodeForSession(p.SessionID, p.Turn)
	case "RevertHistoryForSession":
		var p struct {
			SessionID string `json:"session_id"`
			Turn      int    `json:"turn"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		err = s.agent.RevertHistoryForSession(p.SessionID, p.Turn)
		out = map[string]any{"ok": err == nil}
	case "SnapshotListForSession":
		var p struct {
			SessionID string `json:"session_id"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.SnapshotListForSession(p.SessionID)
	case "ReadFileContent":
		var p struct {
			Path string `json:"path"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.ReadFileContent(p.Path)
	case "ReadFileContentForProjectPath":
		var p struct {
			ProjectPath string `json:"project_path"`
			Path        string `json:"path"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.ReadFileContentForProjectPath(p.ProjectPath, p.Path)
	case "ProjectName":
		out = s.agent.ProjectName()
	case "ProjectRoot":
		out = s.agent.ProjectRoot()
	case "ProjectCurrent":
		out = s.agent.ProjectCurrent()
	case "ProjectCurrentForPath":
		var p struct {
			ProjectPath string `json:"project_path"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.ProjectCurrentForPath(p.ProjectPath)
	case "ProjectList":
		out, err = s.agent.ProjectList()
	case "SessionListForProjectPath":
		var p struct {
			ProjectPath string `json:"project_path"`
			State       string `json:"state"`
		}
		if !decodeRPCParams(w, req.Params, &p) {
			return
		}
		out, err = s.agent.SessionListForProjectPath(p.ProjectPath, p.State)
	default:
		jsonError(w, "unknown adapter method", http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusConflict)
		return
	}
	jsonResp(w, http.StatusOK, out)
}

func decodeRPCParams(w http.ResponseWriter, raw json.RawMessage, dst any) bool {
	if len(raw) == 0 {
		raw = []byte(`{}`)
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		jsonError(w, "invalid params", http.StatusBadRequest)
		return false
	}
	return true
}
