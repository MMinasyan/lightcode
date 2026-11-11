package runtime

import (
	"net/http"

	"github.com/MMinasyan/lightcode/melta"
	"github.com/MMinasyan/lightcode/protocol"
)

// This file implements the generated protocol server interface on the one
// private transport receiver: the exported method names and signatures are
// the generated transport contract, not a public backend forwarding API —
// each method is one body decode over the generated request type, one call
// into the public owner command with the authenticated caller, and one
// response. The compile-time assertion pins the complete interface,
// so a generated signature change breaks this build, not a client at runtime.
type protocolHandlers struct {
	owner  *melta.Melta
	server *ProtocolServer
}

var _ protocol.ServerInterface = (*protocolHandlers)(nil)

// GetHealth serves the authenticated, versioned readiness read of the live
// owner: one admitted call answers with the owner's minted instance identity
// and the one wire protocol version.
func (rt *protocolHandlers) GetHealth(w http.ResponseWriter, r *http.Request) {
	health, err := rt.owner.Health(r.Context(), rt.server.caller)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &health)
}

// ReloadConfiguration serves the one publication operation: the admitted
// Reload result wraps as the revision-only response, a publication and not
// an edit, so no result member exists.
func (rt *protocolHandlers) ReloadConfiguration(w http.ResponseWriter, r *http.Request) {
	revision, err := rt.owner.Reload(r.Context(), rt.server.caller)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	response := protocol.RevisionResponse{ConfigurationRevision: revision}
	rt.writeJSON(w, http.StatusOK, &response)
}

// ListExtensionPackages serves every loaded extension and selected next-start
// package; absence is the schema's ordinary not-selected state.
func (rt *protocolHandlers) ListExtensionPackages(w http.ResponseWriter, r *http.Request) {
	packages, err := rt.owner.ListExtensionPackages(r.Context(), rt.server.caller)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, packages)
}

// InstallExtensionPackage serves one local source package's next-start
// selection. The request names only the operator-supplied source directory.
func (rt *protocolHandlers) InstallExtensionPackage(w http.ResponseWriter, r *http.Request) {
	var body protocol.InstallExtensionPackageRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	extensionPackage, err := rt.owner.InstallExtensionPackage(r.Context(), rt.server.caller, body.SourceDir)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &extensionPackage)
}

// RemoveExtensionPackage serves the selection removal; package copies and
// configuration are retained.
func (rt *protocolHandlers) RemoveExtensionPackage(w http.ResponseWriter, r *http.Request, params protocol.RemoveExtensionPackageParams) {
	if err := rt.owner.RemoveExtensionPackage(r.Context(), rt.server.caller, params.Id); err != nil {
		writeProtocolError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// GetConfiguration serves the revisioned configuration view.
func (rt *protocolHandlers) GetConfiguration(w http.ResponseWriter, r *http.Request) {
	view, err := rt.owner.GetConfiguration(r.Context(), rt.server.caller)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &view)
}

// UpdateConfigurationSettings serves the settings/extension-edit writer.
func (rt *protocolHandlers) UpdateConfigurationSettings(w http.ResponseWriter, r *http.Request) {
	var body protocol.UpdateSettingsRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	mutation, err := rt.owner.UpdateSettings(r.Context(), rt.server.caller, body)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &mutation)
}

// ListModels serves the flat model picker under its one boolean selection.
func (rt *protocolHandlers) ListModels(w http.ResponseWriter, r *http.Request, params protocol.ListModelsParams) {
	list, err := rt.owner.ListModels(r.Context(), rt.server.caller, params)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &list)
}

// ListProviders serves every provider view.
func (rt *protocolHandlers) ListProviders(w http.ResponseWriter, r *http.Request) {
	list, err := rt.owner.ListProviders(r.Context(), rt.server.caller)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &list)
}

// CreateProvider serves the custom-provider add: its write-only key never
// enters any user-owned layer through this response.
func (rt *protocolHandlers) CreateProvider(w http.ResponseWriter, r *http.Request) {
	var body protocol.CreateProviderRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	mutation, err := rt.owner.AddProvider(r.Context(), rt.server.caller, body.Id, body.Provider, body.Models, body.ApiKey)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &mutation)
}

// GetProviderDetail serves one provider's complete view.
func (rt *protocolHandlers) GetProviderDetail(w http.ResponseWriter, r *http.Request, params protocol.GetProviderDetailParams) {
	detail, err := rt.owner.GetProvider(r.Context(), rt.server.caller, params.ProviderId)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &detail)
}

// UpdateProviderDetail serves the provider patch edit.
func (rt *protocolHandlers) UpdateProviderDetail(w http.ResponseWriter, r *http.Request, params protocol.UpdateProviderDetailParams) {
	var body protocol.UpdateProviderDetailRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	mutation, err := rt.owner.UpdateProvider(r.Context(), rt.server.caller, params.ProviderId, body.Provider)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &mutation)
}

// DeleteProviderDetail serves the provider's user-node removal; the mutation
// envelope carries the surviving effective post-state, or null when no
// subject remains.
func (rt *protocolHandlers) DeleteProviderDetail(w http.ResponseWriter, r *http.Request, params protocol.DeleteProviderDetailParams) {
	mutation, err := rt.owner.DeleteProvider(r.Context(), rt.server.caller, params.ProviderId)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &mutation)
}

// DiscoverProviderCandidates serves the unpersisted provider discovery read.
func (rt *protocolHandlers) DiscoverProviderCandidates(w http.ResponseWriter, r *http.Request) {
	var body protocol.DiscoveryRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	candidates, err := rt.owner.DiscoverProvider(r.Context(), rt.server.caller, body)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &candidates)
}

// ConnectProvider serves the managed-key connection.
func (rt *protocolHandlers) ConnectProvider(w http.ResponseWriter, r *http.Request, params protocol.ConnectProviderParams) {
	var body protocol.ConnectRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	mutation, err := rt.owner.ConnectProvider(r.Context(), rt.server.caller, params.ProviderId, body.ApiKey)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &mutation)
}

// DisconnectProvider serves the managed-key disconnection.
func (rt *protocolHandlers) DisconnectProvider(w http.ResponseWriter, r *http.Request, params protocol.DisconnectProviderParams) {
	mutation, err := rt.owner.DisconnectProvider(r.Context(), rt.server.caller, params.ProviderId)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &mutation)
}

// ListProviderModels serves one provider's complete model views.
func (rt *protocolHandlers) ListProviderModels(w http.ResponseWriter, r *http.Request, params protocol.ListProviderModelsParams) {
	list, err := rt.owner.ListProviderModels(r.Context(), rt.server.caller, params.ProviderId)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &list)
}

// UpdateProviderModel serves the model patch edit; both identifiers are
// query values so a slash-containing model ID stays addressable.
func (rt *protocolHandlers) UpdateProviderModel(w http.ResponseWriter, r *http.Request, params protocol.UpdateProviderModelParams) {
	var body protocol.UpdateProviderModelRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	mutation, err := rt.owner.SaveModel(r.Context(), rt.server.caller, params.ProviderId, params.ModelId, body.Model)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &mutation)
}

// DeleteProviderModel serves the user-model removal.
func (rt *protocolHandlers) DeleteProviderModel(w http.ResponseWriter, r *http.Request, params protocol.DeleteProviderModelParams) {
	mutation, err := rt.owner.DeleteModel(r.Context(), rt.server.caller, params.ProviderId, params.ModelId)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &mutation)
}

// DiscoverProviderModelCandidates serves the provider's model discovery read,
// excluding its already-included usable models.
func (rt *protocolHandlers) DiscoverProviderModelCandidates(w http.ResponseWriter, r *http.Request, params protocol.DiscoverProviderModelCandidatesParams) {
	candidates, err := rt.owner.DiscoverProviderModels(r.Context(), rt.server.caller, params.ProviderId)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &candidates)
}

// ResetProviderField serves the one provider user-layer field reset; the
// field is a closed path enum and the provider identity is a query value.
func (rt *protocolHandlers) ResetProviderField(w http.ResponseWriter, r *http.Request, field protocol.ProviderField, params protocol.ResetProviderFieldParams) {
	mutation, err := rt.owner.ResetProviderField(r.Context(), rt.server.caller, params.ProviderId, field)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &mutation)
}

// ResetProviderModelField serves the one model user-layer field reset.
func (rt *protocolHandlers) ResetProviderModelField(w http.ResponseWriter, r *http.Request, field protocol.ModelField, params protocol.ResetProviderModelFieldParams) {
	mutation, err := rt.owner.ResetModelField(r.Context(), rt.server.caller, params.ProviderId, params.ModelId, field)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &mutation)
}

// GetWarnings serves the unfiltered owner warning read.
func (rt *protocolHandlers) GetWarnings(w http.ResponseWriter, r *http.Request) {
	snapshot, err := rt.owner.GetWarnings(r.Context(), rt.server.caller)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &snapshot)
}

// ListSessions serves the filtered Session list; both filter members are
// required query values.
func (rt *protocolHandlers) ListSessions(w http.ResponseWriter, r *http.Request, params protocol.ListSessionsParams) {
	sessions, err := rt.owner.ListSessions(r.Context(), rt.server.caller, params)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &sessions)
}

// CreateSession serves the root Session creation with the created header.
func (rt *protocolHandlers) CreateSession(w http.ResponseWriter, r *http.Request) {
	var body protocol.CreateSessionRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	header, err := rt.owner.CreateSession(r.Context(), rt.server.caller, body.Workspace, body.AgentSelection)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusCreated, &header)
}

func (rt *protocolHandlers) SetSessionModel(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	var body protocol.SetSessionModelRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	header, err := rt.owner.SetSessionModel(r.Context(), rt.server.caller, id, body.Model)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &header)
}

// DeleteSession serves the archived-Session deletion with its structured
// post-commit cleanup result.
func (rt *protocolHandlers) DeleteSession(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	result, err := rt.owner.DeleteSession(r.Context(), rt.server.caller, id)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &result)
}

// ArchiveSession serves the archive control; the returned header is the
// narrow post-transition metadata read.
func (rt *protocolHandlers) ArchiveSession(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	header, err := rt.owner.ArchiveSession(r.Context(), rt.server.caller, id)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &header)
}

// ReopenSession serves the reopen control.
func (rt *protocolHandlers) ReopenSession(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	header, err := rt.owner.ReopenSession(r.Context(), rt.server.caller, id)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &header)
}

// SubmitSession serves the message submission with its stable Operation
// identity; a buffered item's response keeps its disposition with no
// invented Operation.
func (rt *protocolHandlers) SubmitSession(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	var body protocol.SubmitRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	result, err := rt.owner.SubmitSession(r.Context(), rt.server.caller, id, body)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &result)
}

// CompactSession serves the manual compaction command.
func (rt *protocolHandlers) CompactSession(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	var body protocol.CompactRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	operation, err := rt.owner.CompactSession(r.Context(), rt.server.caller, id, body)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &operation)
}

// InterruptSession serves the interrupt control: the control is issued
// immediately and its no-content result does not wait for the terminal
// commit. The POST is argument-free: the authenticated addressed Session and
// the existing control transition decide the outcome, with no request body.
func (rt *protocolHandlers) InterruptSession(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	if err := rt.owner.InterruptSession(r.Context(), rt.server.caller, id); err != nil {
		writeProtocolError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// StopSession serves the stop control; its no-content result follows the
// background convergence wait. The POST is argument-free, like interrupt.
func (rt *protocolHandlers) StopSession(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	if err := rt.owner.StopSession(r.Context(), rt.server.caller, id); err != nil {
		writeProtocolError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ForkSession serves the fork command with its boundary item identity.
func (rt *protocolHandlers) ForkSession(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	var body protocol.ForkRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	result, err := rt.owner.ForkSession(r.Context(), rt.server.caller, id, body)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &result)
}

// SetSessionAgentType serves the Agent-type change; only subsequent
// admissions use the new selection.
func (rt *protocolHandlers) SetSessionAgentType(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	var body protocol.SetSessionAgentTypeRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	header, err := rt.owner.SetSessionAgentType(r.Context(), rt.server.caller, id, body)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &header)
}

// GetSessionHydration serves the one coherent Session projection captured at
// one revision.
func (rt *protocolHandlers) GetSessionHydration(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	hydration, err := rt.owner.GetHydration(r.Context(), rt.server.caller, id)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &hydration)
}

// GetSessionHistory serves one anchored page of conversation items.
func (rt *protocolHandlers) GetSessionHistory(w http.ResponseWriter, r *http.Request, id protocol.SessionID, params protocol.GetSessionHistoryParams) {
	page, err := rt.owner.GetHistory(r.Context(), rt.server.caller, id, params.Cursor)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &page)
}

// GetSessionCodeSnapshots serves the per-Operation snapshot groups.
func (rt *protocolHandlers) GetSessionCodeSnapshots(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	groups, err := rt.owner.ListSessionCodeSnapshots(r.Context(), rt.server.caller, id)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &groups)
}

// RevertSessionCode serves the deferred code restore. A traversal failure
// after earlier groups changed answers with the accumulated result carrying
// its error member and the mapped non-2xx status, never a replacement empty
// error; a pre-traversal refusal answers the plain typed error.
func (rt *protocolHandlers) RevertSessionCode(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	var body protocol.RevertCodeRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	result, err := rt.owner.RevertSessionCode(r.Context(), rt.server.caller, id, body.BoundaryOperationId)
	rt.writeCodeRevertResponse(w, result, err)
}

// writeCodeRevertResponse writes the one restore contract: success carries
// the completed result; a traversal failure carries the accumulated result
// with its error member and the mapped status; a pre-traversal refusal
// carries the typed error alone.
func (rt *protocolHandlers) writeCodeRevertResponse(w http.ResponseWriter, result protocol.CodeRevertResult, err error) {
	if err != nil && result.Error == nil {
		writeProtocolError(w, err)
		return
	}
	if err != nil {
		_, status := classifyProtocolError(err)
		rt.writeJSON(w, status, &result)
		return
	}
	rt.writeJSON(w, http.StatusOK, &result)
}

// ListWorkspaces serves the distinct Session-derived Workspace navigation
// roots.
func (rt *protocolHandlers) ListWorkspaces(w http.ResponseWriter, r *http.Request) {
	workspaces, err := rt.owner.ListWorkspaces(r.Context(), rt.server.caller)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &workspaces)
}

// ReadWorkspaceFile serves the bounded contained file view; the read is a
// client view, not a model tool effect.
func (rt *protocolHandlers) ReadWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	var body protocol.ReadFileRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	result, err := rt.owner.ReadWorkspaceFile(r.Context(), rt.server.caller, body)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeJSON(w, http.StatusOK, &result)
}
