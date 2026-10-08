package runtime

import (
	"net/http"

	"github.com/MMinasyan/lightcode/protocol"
)

// This file implements the generated protocol server interface on the one
// private transport receiver: the exported method names and signatures are
// the generated transport contract, not a public backend forwarding API —
// each method is one body decode over the generated request type, one call
// into the Runtime's private producers, and one qualified response. The
// receiver embeds *Runtime so authentication, admission, and producer calls
// stay identical; the compile-time assertion pins the complete interface, so
// a generated signature change breaks this build, not a client at runtime.
type protocolHandlers struct {
	*Runtime
}

var _ protocol.ServerInterface = (*protocolHandlers)(nil)

// GetHealth serves the authenticated, versioned readiness read of the live
// owner: one admitted call answers with the attached server's minted
// instance identity and the one wire protocol version.
func (rt *protocolHandlers) GetHealth(w http.ResponseWriter, r *http.Request) {
	release, err := rt.enter(r.Context())
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	defer release()
	health := protocol.Health{InstanceId: rt.protocolInstance(), ProtocolVersion: protocol.N1}
	rt.writeQualifiedJSON(w, http.StatusOK, &health)
}

// ReloadConfiguration serves the one publication operation: the admitted
// Reload result wraps as the revision-only response, a publication and not
// an edit, so no result member exists.
func (rt *protocolHandlers) ReloadConfiguration(w http.ResponseWriter, r *http.Request) {
	revision, err := rt.Reload(r.Context())
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	response := protocol.RevisionResponse{
		ConfigurationRevision: protocol.ConfigurationRevision{Generation: revision},
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &response)
}

// GetConfiguration serves the revisioned configuration view.
func (rt *protocolHandlers) GetConfiguration(w http.ResponseWriter, r *http.Request) {
	view, err := rt.getConfiguration(r.Context())
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &view)
}

// UpdateConfigurationSettings serves the whole-section settings write.
func (rt *protocolHandlers) UpdateConfigurationSettings(w http.ResponseWriter, r *http.Request) {
	var body protocol.UpdateSettingsRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	mutation, err := rt.updateSettings(r.Context(), body.Settings)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &mutation)
}

// SetAgentTypeModel serves one Agent type's model edit; the type's name is a
// required query value.
func (rt *protocolHandlers) SetAgentTypeModel(w http.ResponseWriter, r *http.Request, params protocol.SetAgentTypeModelParams) {
	var body protocol.SetAgentTypeModelRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	mutation, err := rt.setAgentTypeModel(r.Context(), params.AgentType, string(body.Model))
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &mutation)
}

// ListModels serves the flat model picker under its one boolean selection.
func (rt *protocolHandlers) ListModels(w http.ResponseWriter, r *http.Request, params protocol.ListModelsParams) {
	list, err := rt.listModels(r.Context(), params)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &list)
}

// ListProviders serves every provider view.
func (rt *protocolHandlers) ListProviders(w http.ResponseWriter, r *http.Request) {
	list, err := rt.listProviders(r.Context())
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &list)
}

// CreateProvider serves the custom-provider add: its write-only key never
// enters any user-owned layer through this response.
func (rt *protocolHandlers) CreateProvider(w http.ResponseWriter, r *http.Request) {
	var body protocol.CreateProviderRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	mutation, err := rt.addProvider(r.Context(), body.Id, body.Provider, body.Models, body.ApiKey)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &mutation)
}

// GetProviderDetail serves one provider's complete view.
func (rt *protocolHandlers) GetProviderDetail(w http.ResponseWriter, r *http.Request, params protocol.GetProviderDetailParams) {
	detail, err := rt.getProvider(r.Context(), params.ProviderId)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &detail)
}

// UpdateProviderDetail serves the provider patch edit.
func (rt *protocolHandlers) UpdateProviderDetail(w http.ResponseWriter, r *http.Request, params protocol.UpdateProviderDetailParams) {
	var body protocol.UpdateProviderDetailRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	mutation, err := rt.updateProvider(r.Context(), params.ProviderId, body.Provider)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &mutation)
}

// DeleteProviderDetail serves the provider's user-node removal; the mutation
// envelope carries the surviving effective post-state, or null when no
// subject remains.
func (rt *protocolHandlers) DeleteProviderDetail(w http.ResponseWriter, r *http.Request, params protocol.DeleteProviderDetailParams) {
	mutation, err := rt.deleteProvider(r.Context(), params.ProviderId)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &mutation)
}

// DiscoverProviderCandidates serves the unpersisted provider discovery read.
func (rt *protocolHandlers) DiscoverProviderCandidates(w http.ResponseWriter, r *http.Request) {
	var body protocol.DiscoveryRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	candidates, err := rt.discoverProvider(r.Context(), body)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &candidates)
}

// ConnectProvider serves the managed-key connection.
func (rt *protocolHandlers) ConnectProvider(w http.ResponseWriter, r *http.Request, params protocol.ConnectProviderParams) {
	var body protocol.ConnectRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	mutation, err := rt.connectProvider(r.Context(), params.ProviderId, body.ApiKey)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &mutation)
}

// DisconnectProvider serves the managed-key disconnection.
func (rt *protocolHandlers) DisconnectProvider(w http.ResponseWriter, r *http.Request, params protocol.DisconnectProviderParams) {
	mutation, err := rt.disconnectProvider(r.Context(), params.ProviderId)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &mutation)
}

// ListProviderModels serves one provider's complete model views.
func (rt *protocolHandlers) ListProviderModels(w http.ResponseWriter, r *http.Request, params protocol.ListProviderModelsParams) {
	list, err := rt.listProviderModels(r.Context(), params.ProviderId)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &list)
}

// UpdateProviderModel serves the model patch edit; both identifiers are
// query values so a slash-containing model ID stays addressable.
func (rt *protocolHandlers) UpdateProviderModel(w http.ResponseWriter, r *http.Request, params protocol.UpdateProviderModelParams) {
	var body protocol.UpdateProviderModelRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	mutation, err := rt.saveModel(r.Context(), params.ProviderId, params.ModelId, body.Model)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &mutation)
}

// DeleteProviderModel serves the user-model removal.
func (rt *protocolHandlers) DeleteProviderModel(w http.ResponseWriter, r *http.Request, params protocol.DeleteProviderModelParams) {
	mutation, err := rt.deleteModel(r.Context(), params.ProviderId, params.ModelId)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &mutation)
}

// DiscoverProviderModelCandidates serves the provider's model discovery read,
// excluding its already-included usable models.
func (rt *protocolHandlers) DiscoverProviderModelCandidates(w http.ResponseWriter, r *http.Request, params protocol.DiscoverProviderModelCandidatesParams) {
	candidates, err := rt.discoverProviderModels(r.Context(), params.ProviderId)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &candidates)
}

// ResetProviderField serves the one provider user-layer field reset; the
// field is a closed path enum and the provider identity is a query value.
func (rt *protocolHandlers) ResetProviderField(w http.ResponseWriter, r *http.Request, field protocol.ProviderField, params protocol.ResetProviderFieldParams) {
	mutation, err := rt.resetProviderField(r.Context(), params.ProviderId, field)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &mutation)
}

// ResetProviderModelField serves the one model user-layer field reset.
func (rt *protocolHandlers) ResetProviderModelField(w http.ResponseWriter, r *http.Request, field protocol.ModelField, params protocol.ResetProviderModelFieldParams) {
	mutation, err := rt.resetModelField(r.Context(), params.ProviderId, params.ModelId, field)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &mutation)
}

// GetWarnings serves the unfiltered Runtime-owned warning read.
func (rt *protocolHandlers) GetWarnings(w http.ResponseWriter, r *http.Request) {
	snapshot, err := rt.getWarnings(r.Context())
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &snapshot)
}

// ListSessions serves the filtered Session list; both filter members are
// required query values.
func (rt *protocolHandlers) ListSessions(w http.ResponseWriter, r *http.Request, params protocol.ListSessionsParams) {
	sessions, err := rt.listSessions(r.Context(), params)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &sessions)
}

// CreateSession serves the root Session creation with the created header.
func (rt *protocolHandlers) CreateSession(w http.ResponseWriter, r *http.Request) {
	var body protocol.CreateSessionRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	header, err := rt.createSessionHeader(r.Context(), body.Workspace, body.AgentType)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusCreated, &header)
}

// DeleteSession serves the archived-Session deletion with its idempotent
// no-content result.
func (rt *protocolHandlers) DeleteSession(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	if err := rt.deleteSession(r.Context(), id); err != nil {
		writeProtocolError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ArchiveSession serves the archive control; the returned header is the
// narrow post-transition metadata read.
func (rt *protocolHandlers) ArchiveSession(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	header, err := rt.archiveSession(r.Context(), id)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &header)
}

// ReopenSession serves the reopen control.
func (rt *protocolHandlers) ReopenSession(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	header, err := rt.reopenSession(r.Context(), id)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &header)
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
	result, err := rt.submitSession(r.Context(), id, body)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &result)
}

// CompactSession serves the manual compaction command.
func (rt *protocolHandlers) CompactSession(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	var body protocol.CompactRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	operation, err := rt.compactSession(r.Context(), id, body)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &operation)
}

// InterruptSession serves the interrupt control: the control is issued
// immediately and its no-content result does not wait for the terminal
// commit. The POST is argument-free: the authenticated addressed Session and
// the existing control transition decide the outcome, with no request body.
func (rt *protocolHandlers) InterruptSession(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	if err := rt.interruptSession(r.Context(), id); err != nil {
		writeProtocolError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// StopSession serves the stop control; its no-content result follows the
// background convergence wait. The POST is argument-free, like interrupt.
func (rt *protocolHandlers) StopSession(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	if err := rt.stopSession(r.Context(), id); err != nil {
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
	result, err := rt.forkSession(r.Context(), id, body)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &result)
}

// SetSessionAgentType serves the Agent-type change; only subsequent
// admissions use the new selection.
func (rt *protocolHandlers) SetSessionAgentType(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	var body protocol.SetSessionAgentTypeRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	header, err := rt.setSessionAgentType(r.Context(), id, body)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &header)
}

// GetSessionHydration serves the one coherent Session projection captured at
// one revision.
func (rt *protocolHandlers) GetSessionHydration(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	hydration, err := rt.buildHydration(r.Context(), id)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &hydration)
}

// GetSessionHistory serves one anchored page of conversation items.
func (rt *protocolHandlers) GetSessionHistory(w http.ResponseWriter, r *http.Request, id protocol.SessionID, params protocol.GetSessionHistoryParams) {
	page, err := rt.getHistory(r.Context(), id, params.Cursor)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &page)
}

// GetSessionCodeSnapshots serves the per-Operation snapshot groups.
func (rt *protocolHandlers) GetSessionCodeSnapshots(w http.ResponseWriter, r *http.Request, id protocol.SessionID) {
	groups, err := rt.listSessionCodeSnapshots(r.Context(), id)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &groups)
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
	result, err := rt.revertSessionCode(r.Context(), id, body.BoundaryOperationId)
	rt.writeCodeRevertResponse(w, result, err)
}

// RevertRetainedCode serves the retained pre-cutover restore under the same
// partial-result contract.
func (rt *protocolHandlers) RevertRetainedCode(w http.ResponseWriter, r *http.Request) {
	var body protocol.RetainedRevertRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	result, err := rt.revertRetainedCode(r.Context(), body)
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
		rt.writeQualifiedJSON(w, status, &result)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &result)
}

// ListWorkspaces serves the distinct Session-derived Workspace navigation
// roots.
func (rt *protocolHandlers) ListWorkspaces(w http.ResponseWriter, r *http.Request) {
	workspaces, err := rt.listWorkspaces(r.Context())
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &workspaces)
}

// ReadWorkspaceFile serves the bounded contained file view; the read is a
// client view, not a model tool effect.
func (rt *protocolHandlers) ReadWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	var body protocol.ReadFileRequest
	if err := decodeRequestBody(r, &body); err != nil {
		writeProtocolError(w, err)
		return
	}
	result, err := rt.readWorkspaceFile(r.Context(), body)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &result)
}

// GetRetainedCodeSnapshots serves the legacy turn listing of one proven
// pre-cutover Session directory.
func (rt *protocolHandlers) GetRetainedCodeSnapshots(w http.ResponseWriter, r *http.Request, params protocol.GetRetainedCodeSnapshotsParams) {
	turns, err := rt.listRetainedCodeSnapshots(r.Context(), params.Workspace, params.SessionId)
	if err != nil {
		writeProtocolError(w, err)
		return
	}
	rt.writeQualifiedJSON(w, http.StatusOK, &turns)
}
