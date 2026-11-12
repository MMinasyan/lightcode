package melta

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/protocol"
)

// ListExtensionCapabilities projects every enabled extension's immutable
// declaration: its configuration schema, captured non-secret settings, every
// declared capability and method, and its explicit exposures. The projection
// reads only the published revision and copies immutable values; it builds no
// factory, reserves no resource and executes no extension code.
func (r *Melta) ListExtensionCapabilities(ctx context.Context, c Caller) ([]protocol.ExtensionCapabilityCatalog, error) {
	release, err := r.enter(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := r.authorize(c, ctx, nil, harnessScope); err != nil {
		return nil, err
	}
	captured, err := r.config.capture(ctx)
	if err != nil {
		return nil, err
	}
	snapshot := captured.snapshot
	if snapshot == nil {
		return nil, fmt.Errorf("extension catalog requires published configuration: %w", ErrClosed)
	}
	return projectExtensionCatalog(snapshot), nil
}

// projectExtensionCatalog is the data-only catalog body: compiled declarations
// are the enabled installed set in registration order, so a zero-capability
// extension is still one entry and hidden methods remain visible.
func projectExtensionCatalog(snapshot *configuration) []protocol.ExtensionCapabilityCatalog {
	if snapshot == nil || snapshot.composition == nil || snapshot.composition.compiled == nil {
		return []protocol.ExtensionCapabilityCatalog{}
	}
	declarations := snapshot.composition.compiled.Declarations()
	out := make([]protocol.ExtensionCapabilityCatalog, 0, len(declarations))
	for _, declaration := range declarations {
		settings := json.RawMessage("{}")
		if section := snapshot.extensions[declaration.Declaration.ID]; len(section) != 0 {
			settings = section
		}
		entry := protocol.ExtensionCapabilityCatalog{
			ExtensionId:         declaration.Declaration.ID,
			Version:             declaration.Declaration.Version,
			ConfigurationSchema: string(declaration.Declaration.ConfigSchema),
			Settings:            string(settings),
			Capabilities:        make([]protocol.ExtensionCapability, 0, len(declaration.Declaration.Capabilities)),
			Exposed:             make([]protocol.ExtensionExposure, 0, len(declaration.Declaration.Exposed)),
		}
		for _, capability := range declaration.Declaration.Capabilities {
			entry.Capabilities = append(entry.Capabilities, projectExtensionCapability(capability))
		}
		for _, exposed := range declaration.Declaration.Exposed {
			entry.Exposed = append(entry.Exposed, protocol.ExtensionExposure{
				CapabilityId: exposed.Method.CapabilityID,
				Method:       exposed.Method.Method,
				Kind:         protocol.ExtensionExposureKind(exposed.Kind),
				Target:       protocol.ExtensionExposureTarget(exposed.Target),
			})
		}
		out = append(out, entry)
	}
	return out
}

func projectExtensionCapability(capability extension.Capability) protocol.ExtensionCapability {
	out := protocol.ExtensionCapability{
		CapabilityId:    capability.ID,
		ContractId:      capability.ContractID,
		ContractVersion: capability.ContractVersion,
		Kind:            protocol.ExtensionCapabilityKind(capability.Kind),
		Methods:         make([]protocol.ExtensionMethod, 0, len(capability.Methods)),
	}
	for _, method := range capability.Methods {
		projected := protocol.ExtensionMethod{
			Name:    method.Name,
			Mode:    protocol.ExtensionMethodMode(method.Mode),
			Input:   string(method.Input),
			Success: string(method.Success),
		}
		if len(method.Error) != 0 {
			value := string(method.Error)
			projected.Error = &value
		}
		if len(method.Partial) != 0 {
			value := string(method.Partial)
			projected.Partial = &value
		}
		out.Methods = append(out.Methods, projected)
	}
	if len(capability.Hooks) != 0 {
		hooks := make([]protocol.ExtensionHook, 0, len(capability.Hooks))
		for _, hook := range capability.Hooks {
			hooks = append(hooks, protocol.ExtensionHook{
				Boundary: protocol.ExtensionHookBoundary(hook.Boundary),
				Mode:     protocol.ExtensionHookMode(hook.Mode),
			})
		}
		out.Hooks = &hooks
	}
	if capability.Strategy != nil {
		out.Strategy = &protocol.ExtensionStrategy{RequiresContextWindow: capability.Strategy.RequiresContextWindow}
	}
	return out
}

// InvokeExtension resolves one declared exposure, authorizes its exact target
// against the Caller grant, validates the raw arguments against the selected
// method schema, and calls the existing named-service provider once under a
// fresh scoped view. A validated declared domain failure is Result.Failure with
// a nil Go error; every predispatch or host failure is the ordinary public Go
// error class.
func (r *Melta) InvokeExtension(ctx context.Context, c Caller, request protocol.ExtensionInvocationRequest) (extension.Result, error) {
	// Own the raw value before any slow construction or extension callback can
	// observe caller-retained bytes. A nil optional value is the JSON null.
	arguments := json.RawMessage("null")
	if request.Arguments != nil {
		arguments = append(json.RawMessage(nil), (*request.Arguments)...)
	}
	var result extension.Result
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		if err := validateInvocationAddress(request); err != nil {
			return err
		}
		captured, err := r.config.captureFor(ctx, true)
		if err != nil {
			return err
		}
		snapshot := captured.snapshot
		if snapshot == nil || snapshot.resources == nil {
			return fmt.Errorf("extension invocation requires published configuration: %w", ErrClosed)
		}
		defer func() { _ = snapshot.resources.release() }()

		capability, ok := snapshot.composition.compiled.Capability(request.CapabilityId)
		if !ok {
			return fmt.Errorf("capability %q is not declared: %w", request.CapabilityId, harness.ErrNotFound)
		}
		if capability.Declaration.Declaration.ID != request.ExtensionId {
			return fmt.Errorf("capability %q does not belong to extension %q: %w", request.CapabilityId, request.ExtensionId, harness.ErrInvalid)
		}
		if capability.ContractID != request.ContractId || capability.ContractVersion != request.ContractVersion {
			return fmt.Errorf("capability %q contract is %s/v%s, not %s/v%s: %w", request.CapabilityId, capability.ContractID, capability.ContractVersion, request.ContractId, request.ContractVersion, harness.ErrInvalid)
		}
		method, ok := capability.Methods[request.Method]
		if !ok {
			return fmt.Errorf("capability %q method %q is not declared: %w", request.CapabilityId, request.Method, harness.ErrNotFound)
		}
		exposed, ok := capability.Declaration.Exposed[extension.MethodKey{CapabilityID: request.CapabilityId, Method: request.Method}]
		if !ok {
			return fmt.Errorf("capability %q method %q is not exposed: %w", request.CapabilityId, request.Method, ErrDenied)
		}
		target, err := resolveInvocationTarget(request.Target, exposed.Target)
		if err != nil {
			return err
		}
		if err := r.authorize(c, ctx, h, target); err != nil {
			return err
		}
		sessionID, workspace := "", ""
		switch target.Kind {
		case CallerScopeSession:
			header, err := h.ReadSessionHeader(ctx, target.SessionID)
			if err != nil {
				return err
			}
			sessionID = target.SessionID
			workspace = header.Identity.Workspace
		case CallerScopeWorkspace:
			workspace = target.Workspace
		}
		if err := extension.ValidateArguments(capability.Methods, request.Method, arguments); err != nil {
			return fmt.Errorf("%w: %w", harness.ErrInvalid, err)
		}

		providerScope := snapshot.resources.runtime
		if capability.Component.Lifetime == extension.LifetimeWorkspace {
			if workspace == "" {
				return fmt.Errorf("workspace-scoped capability %q requires a workspace target: %w", capability.ID, harness.ErrInvalid)
			}
			workspaceInfo := ScopeInfo{Kind: ScopeWorkspace, DataDir: providerScope.info.DataDir, Workspace: workspace, Revision: snapshot.resources.revisionString()}
			providerScope, err = snapshot.resources.workspaces.get(ctx, workspaceInfo)
			if err != nil {
				return err
			}
		}
		provider := providerScope.capabilities[capability.ID]
		if provider == nil {
			return fmt.Errorf("capability %q is unavailable in its scope: %w", capability.ID, ErrClosed)
		}
		identity := callIdentity{
			session:   harness.SessionIdentity{SessionID: sessionID},
			workspace: workspace,
			requestID: request.RequestId,
			revision:  snapshot.resources.revisionString(),
			grant: nativeGrant{
				pure:         method.Mode == extension.ModePure,
				commands:     exposed.Kind == extension.ExposedSessionWork,
				commandScope: target,
			},
		}
		result, err = snapshot.composition.invokeAcceptedProvider(ctx, provider, capability.Methods, request.Method, arguments, nil, identity, false)
		if err != nil {
			return err
		}
		if exposed.Kind == extension.ExposedQuery && target.Kind == CallerScopeSession && result.Failure == nil {
			// A successful query must not be captured after its Session was
			// deleted. The owner read is cancellation-independent so a late
			// request cancellation cannot replace a produced result, and only
			// an actually deleted Session replaces it with not-found.
			if _, err := h.ReadSessionHeader(context.WithoutCancel(ctx), target.SessionID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return extension.Result{}, err
	}
	return result, nil
}

// validateInvocationAddress enforces the nonempty addressing strings and the
// closed target union's exact member shape before any declaration lookup.
func validateInvocationAddress(request protocol.ExtensionInvocationRequest) error {
	for name, value := range map[string]string{
		"extension_id":     request.ExtensionId,
		"capability_id":    request.CapabilityId,
		"contract_id":      request.ContractId,
		"contract_version": request.ContractVersion,
		"method":           request.Method,
		"request_id":       request.RequestId,
	} {
		if value == "" {
			return fmt.Errorf("invocation member %q must be non-empty: %w", name, harness.ErrInvalid)
		}
	}
	return nil
}

// resolveInvocationTarget validates the closed target union, its exact member
// shape for the selected branch, and its match with the declaration's target
// kind.
func resolveInvocationTarget(request protocol.ExtensionInvocationTarget, declared extension.ExposedTargetKind) (CallerScope, error) {
	kind := protocol.ExtensionInvocationTargetKind(declared)
	if request.Kind != kind {
		return CallerScope{}, fmt.Errorf("invocation target kind %q does not match the declared %q: %w", request.Kind, declared, harness.ErrInvalid)
	}
	switch request.Kind {
	case protocol.ExtensionInvocationTargetKindHarness:
		if request.Workspace != nil || request.SessionId != nil {
			return CallerScope{}, fmt.Errorf("harness target carries no Workspace or Session: %w", harness.ErrInvalid)
		}
		return harnessScope, nil
	case protocol.ExtensionInvocationTargetKindWorkspace:
		if request.Workspace == nil || request.SessionId != nil {
			return CallerScope{}, fmt.Errorf("workspace target requires exactly one Workspace path: %w", harness.ErrInvalid)
		}
		workspace := *request.Workspace
		if !filepath.IsAbs(workspace) || filepath.Clean(workspace) != workspace {
			return CallerScope{}, fmt.Errorf("workspace target requires one clean absolute path: %w", harness.ErrInvalid)
		}
		return CallerScope{Kind: CallerScopeWorkspace, Workspace: workspace}, nil
	case protocol.ExtensionInvocationTargetKindSession:
		if request.SessionId == nil || request.Workspace != nil {
			return CallerScope{}, fmt.Errorf("session target requires exactly one canonical Session ID: %w", harness.ErrInvalid)
		}
		if !canonicalSessionID(*request.SessionId) {
			return CallerScope{}, fmt.Errorf("session target requires one canonical Session ID: %w", harness.ErrInvalid)
		}
		return CallerScope{Kind: CallerScopeSession, SessionID: *request.SessionId}, nil
	default:
		return CallerScope{}, fmt.Errorf("invocation target kind %q is not one of harness, workspace or session: %w", request.Kind, harness.ErrInvalid)
	}
}
