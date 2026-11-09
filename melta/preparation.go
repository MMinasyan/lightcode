package melta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
	"github.com/MMinasyan/lightcode/protocol"
)

// openInvocation turns one committed admission's execution-view selection and
// one Harness-issued Engine-call request into that invocation's opened
// callback contracts. It is the preparation's only associated continuation:
// there is no intermediate execution-plan type and no independently
// configured opener.
type openInvocation func(context.Context, harness.OperationAdmission, selection, harness.InvocationRequest) (harness.InvocationExecution, error)

// selection is one immutable preparation or execution input set: owned Agent
// data, the selected capability bindings currently supplied by the scopes
// resolved for that call, and the captured Invocation. A selected capability
// whose supplying scope is not open yet is absent from the preparation view
// and present in the execution view; neither selection is ever rebound.
type selection struct {
	inputs     harness.AgentInputs
	definition *harness.DefinitionRef
	scopes     []*scope
	selected   []string
	invocation Invocation

	// lease is the actual privately acquired execution artifact interval.
	// Every native callback running in this execution receives this same
	// borrowed pointer regardless of its implementation lifetime; nil outside
	// an admitted execution.
	lease *artifactCallLease

	// permissions is the preparation-time permission evaluator, built before
	// durable admission so a factory error or nil evaluator rejects the
	// preparation instead of failing an admitted Operation.
	permissions harness.PermissionEvaluator

	// operationSecrets are the operation-lifetime declared credential values
	// captured at preparation and retained through the prepared opener. Every
	// tool and hook view of that execution carries them; no value reaches a
	// durable capture.
	operationSecrets map[string]map[string]string

	// credentials is the preparation's one captured credential observation
	// set, sampled with the same capture as the Invocation's snapshot: it
	// carries both the provider names the catalog references and every active
	// declaration's non-secret reference name. The concrete preparation
	// resolves its transport secrets from it, the operation fields derive
	// their values from it, and no value leaves the prepared opener or the
	// durable capture.
	credentials map[string]CredentialValue

	// warnings is the per-preparation private presentation collector: the
	// concrete preparation assigns the assembled prompt warnings into it and
	// the committed opener publishes them under the admitted Session identity
	// only after the opener succeeded, so a failed fork or admission
	// publishes nothing. It is never the Invocation or the durable capture.
	warnings *preparationWarnings
}

// preparationWarnings collects one preparation's presentation values.
type preparationWarnings struct {
	prompt []prompt.Warning
}

// publish replaces the owning Session's prompt group with the collected
// values through the passive publication adapter; a nil adapter drops the
// presentation. The committed opener calls it exactly once per successful
// execution, passing the publisher and the admitted Session identity at
// publication instead of storing a second pair.
func (w *preparationWarnings) publish(publisher *observationAdapter, sessionID string) {
	if w == nil {
		return
	}
	publisher.publishPrompt(sessionID, promptWarnings(sessionID, w.prompt))
}

// promptWarnings maps the assembled prompt diagnostics onto the store's
// typed session group.
func promptWarnings(sessionID string, warnings []prompt.Warning) []protocol.Warning {
	if len(warnings) == 0 {
		return nil
	}
	out := make([]protocol.Warning, 0, len(warnings))
	for _, warning := range warnings {
		id := sessionID
		out = append(out, protocol.Warning{Source: promptSource, Kind: warning.Kind, Message: warning.Message, SessionId: &id})
	}
	return out
}

// protocolWarnings maps one transport attempt's returned diagnostics onto the
// store's typed session group.
func protocolWarnings(sessionID string, warnings []model.ProtocolWarning) []protocol.Warning {
	if len(warnings) == 0 {
		return nil
	}
	out := make([]protocol.Warning, 0, len(warnings))
	for _, warning := range warnings {
		id := sessionID
		out = append(out, protocol.Warning{Source: protocolSource, Kind: warning.Kind, Message: warning.Message, SessionId: &id})
	}
	return out
}

// preparation supplies the existing Harness preparation contract for every
// admission producer: root, queued delivery and Fork all bind through the one
// closure returned by bind, with only resource lifetime and the selected
// invocation context added to what Harness already validates. The snapshot
// pointer loads once per call, so a reload overlapping preparation affects the
// next capture, not the in-flight one; the returned opener keeps the same
// immutable snapshot while borrowed bindings and the call context stay inside
// the preparation guard.
type preparation struct {
	config *configurationService

	// identity and locations are the host-supplied prompt and diagnostic
	// inputs: the application persona text and the explicit global rules and
	// wire-debug locations. Their zero values supply no identity, no global
	// rules and no wire diagnostics, never an implicit fallback.
	identity     string
	locations    Locations
	background   *backgroundBridge
	passive      *observationAdapter
	newEvaluator func(PolicyInputs) (harness.PermissionEvaluator, error)

	// artifacts is the Melta's private per-Session artifact interval
	// registry. Every committed opener acquires its Session's token here
	// before any scope, plugin, model, tool or hook effect and releases it
	// last, after the scoped cleanup; nil leaves an isolated preparation
	// without artifact ownership.
	artifacts *artifactIntervals

	// subprocessEnv is the Melta's live subprocess-environment producer.
	// It is invoked at each process start, never at preparation; an error
	// fails the spawn before start.
	subprocessEnv func(context.Context) ([]string, error)
}

// newPreparation wires the binder to the published configuration, the
// selected locations, the host-supplied prompt identity, the background
// services bridge armed after harness.New returns, the Melta's passive
// publication adapter (nil in isolated preparation tests, dropping only
// passive presentation), the Melta's private artifact interval registry
// (nil without a Melta), the Melta's live child-environment producer, and
// the optional evaluator factory. One concrete preparation path serves every
// admission.
func newPreparation(config *configurationService, locations Locations, identity string, background *backgroundBridge, passive *observationAdapter, artifacts *artifactIntervals, subprocessEnv func(context.Context) ([]string, error), newEvaluator func(PolicyInputs) (harness.PermissionEvaluator, error)) *preparation {
	return &preparation{
		config:        config,
		identity:      identity,
		locations:     locations,
		background:    background,
		passive:       passive,
		artifacts:     artifacts,
		subprocessEnv: subprocessEnv,
		newEvaluator:  newEvaluator,
	}
}

// bind returns the exact two-argument callback type required by
// harness.Dependencies.Prepare. Selected unknown or unavailable Agent types
// and models report harness.ErrInvalid at admission, never a failed
// configuration publication; preparation errors and cancellation abort
// admission with no partial consequence published.
func (p *preparation) bind() func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
	return func(ctx context.Context, req harness.PreparationRequest) (prepared harness.PreparedExecution, resultErr error) {
		// Configuration and the credentials its catalog references are one
		// owner-coherent capture: the selected model, its transport secret,
		// and the revision all belong to the same ready pair.
		captured, err := p.config.capturePreparation(ctx)
		if err != nil {
			return harness.PreparedExecution{}, err
		}
		snapshot := captured.snapshot
		var releaseArtifacts func()
		if snapshot != nil {
			defer func() {
				if resultErr != nil {
					resultErr = errors.Join(resultErr, snapshot.resources.release())
					captured.releaseHeldNames()
				}
			}()
		}
		if snapshot == nil {
			return harness.PreparedExecution{}, fmt.Errorf("agent has no published configuration: %w", harness.ErrInvalid)
		}
		var inputs harness.AgentInputs
		var definition *harness.DefinitionRef
		name := "resolved inputs"
		chosen := req.Session.AgentSelection
		if chosen.Definition != nil {
			ref := *chosen.Definition
			inputs, err = resolveDefinition(ctx, snapshot, ref)
			if err != nil {
				return harness.PreparedExecution{}, err
			}
			definition = &ref
			name = ref.DefinitionID
		} else if chosen.Inputs != nil {
			inputs = chosen.Inputs.Owned()
		} else {
			return harness.PreparedExecution{}, fmt.Errorf("invalid agent selection: %w", harness.ErrInvalid)
		}
		if err := harness.ValidateAgentInputs(inputs); err != nil {
			return harness.PreparedExecution{}, fmt.Errorf("agent inputs: %v: %w", err, harness.ErrInvalid)
		}
		// A selected strategy's declared capability and lifetime are one
		// admission rule for both selection forms; the concrete open
		// implementation is resolved later under the execution guard.
		if inputs.Strategy.ID != "" {
			capability, ok := snapshot.composition.compiled.Capability(inputs.Strategy.ID)
			if !ok || capability.Kind != extension.KindStrategy {
				return harness.PreparedExecution{}, fmt.Errorf("unknown strategy %q: %w", inputs.Strategy.ID, harness.ErrInvalid)
			}
			if capability.Component.Lifetime.Rank() > extension.LifetimeOperation.Rank() {
				return harness.PreparedExecution{}, fmt.Errorf("strategy %q is shorter-lived than an Operation: %w", inputs.Strategy.ID, harness.ErrInvalid)
			}
		}
		// Every selected capability must exist in the compiled export table
		// before admission. A selected binding that belongs to a shorter
		// lifetime is still compiled here and correctly binds only when its
		// scope opens; only an ID that no declaration owns is rejected.
		for _, id := range inputs.Capabilities {
			if _, ok := snapshot.composition.exports[id]; !ok {
				return harness.PreparedExecution{}, fmt.Errorf("agent type %q selects undeclared capability %q: %w", name, id, harness.ErrInvalid)
			}
		}
		for _, id := range inputs.Tools {
			if _, ok := snapshot.composition.toolSpec(id); !ok {
				return harness.PreparedExecution{}, fmt.Errorf("selected tool %q is unavailable: %w", id, harness.ErrInvalid)
			}
		}
		if err := requireCatalogModel(snapshot, inputs.Model); err != nil {
			return harness.PreparedExecution{}, err
		}
		workspace := req.Session.Identity.Workspace
		evaluator, err := p.permissionEvaluator(snapshot, workspace)
		if err != nil {
			return harness.PreparedExecution{}, err
		}
		runtimeScope, workspaces := snapshot.resources.runtime, snapshot.resources.workspaces
		workspaceInfo := ScopeInfo{Kind: ScopeWorkspace, DataDir: runtimeScope.info.DataDir, Workspace: workspace, Revision: snapshot.resources.revisionString()}
		workspaceInfo.sections = snapshot.extensions
		workspaceInfo.declaredBindings = snapshot.declaredBindings
		workspaceScope, err := workspaces.get(ctx, workspaceInfo)
		if err != nil {
			return harness.PreparedExecution{}, err
		}
		selected := make([]string, 0, len(inputs.Capabilities)+len(inputs.Tools))
		selected = append(selected, inputs.Capabilities...)
		selected = append(selected, inputs.Tools...)
		sel := selection{inputs: inputs, definition: definition, scopes: []*scope{runtimeScope, workspaceScope}, selected: selected, invocation: Invocation{snapshot: snapshot}, credentials: captured.snapshot.credentials, permissions: evaluator, operationSecrets: captured.operationSecrets()}

		callCtx, release, err := workspaceScope.enter(ctx)
		if err != nil {
			return harness.PreparedExecution{}, err
		}
		defer release() // the one preparation guard covers the concrete preparation and every pure hook

		// The per-preparation collector is allocated owned: only a successful
		// preparation — validation, hooks and cancellation checks all passed —
		// publishes its values under the admitted Session identity.
		sel.warnings = &preparationWarnings{}
		capture, opener, err := p.concretePrepare(callCtx, req, sel)
		if err != nil {
			return harness.PreparedExecution{}, err
		}
		// The collected presentation rides the prepared opener: only the
		// committed admission's opener publishes it, so a failed fork or
		// admission never creates warnings for an unpublished Session.
		warnings := sel.warnings
		prepared = harness.PreparedExecution{
			Capture: capture,
			Open: func(openCtx context.Context, admission harness.OperationAdmission) (harness.Execution, error) {
				return p.open(openCtx, admission, workspace, runtimeScope, workspaceScope, inputs, definition, snapshot, sel.permissions, sel.operationSecrets, warnings, opener, &releaseArtifacts)
			},
		}
		prepared.Release = func() error {
			err := snapshot.resources.release()
			if releaseArtifacts != nil {
				releaseArtifacts()
			}
			captured.releaseHeldNames()
			return err
		}
		return prepared, nil
	}
}

// permissionEvaluator builds the preparation-time evaluator from the
// captured policy inputs. A nil factory selects the included resolver; a
// factory error, nil evaluator or empty evaluator is an invalid preparation.
func (p *preparation) permissionEvaluator(snapshot *configuration, workspace string) (harness.PermissionEvaluator, error) {
	if p.newEvaluator == nil {
		return snapshot.permissionPolicy(workspace), nil
	}
	built, err := p.newEvaluator(snapshot.policyInputs(workspace))
	if err != nil {
		return nil, fmt.Errorf("prepare permission evaluator: %w", err)
	}
	if built == nil {
		return nil, fmt.Errorf("prepare permission evaluator: nil evaluator: %w", harness.ErrInvalid)
	}
	return built, nil
}

// open turns the committed admission into the execution: it first acquires
// the Session's whole artifact interval — before any scope, plugin, model,
// tool or hook effect — then opens only the Operation scope and enters its
// guard. The returned Execution carries the invocation factory: every Engine
// call — the conversation run and each maintenance model request — opens its
// own Agent-tier scope over the same Operation, enters its guard, selects the
// Agent's capabilities over the four scopes, and hands the new selection with
// the actual committed admission and the Harness-issued request to this
// preparation's opener, so hooks' final prompt and tools can never be replaced
// by stale pre-hook values. The Operation guard covers opening and every
// nested invocation until the returned Close releases it and closes the
// Operation scope; each invocation's own Close releases its guard and closes
// its Agent scope before that; the prepared reservation releases the artifact
// token after the resource-set hold, and an opening failure releases it before returning. Only
// after the opening succeeds does the collected prompt presentation publish,
// so a failed fork or admission never creates warnings for an unpublished
// Session and a successful opener publishes before any progress.
func (p *preparation) open(ctx context.Context, admission harness.OperationAdmission, workspace string, runtimeScope, workspaceScope *scope, inputs harness.AgentInputs, definition *harness.DefinitionRef, snapshot *configuration, evaluator harness.PermissionEvaluator, operationSecrets map[string]map[string]string, warnings *preparationWarnings, opener openInvocation, artifactRelease *func()) (harness.Execution, error) {
	releaseArtifacts, err := p.artifacts.acquireExecution(ctx, sessionArtifactDir(runtimeScope.info.DataDir, admission.SessionID))
	if err != nil {
		return harness.Execution{}, err
	}
	artifactLease := &artifactCallLease{
		intervals: p.artifacts,
		key:       sessionArtifactDir(runtimeScope.info.DataDir, admission.SessionID),
		borrowed:  true,
		release:   releaseArtifacts,
	}
	base := ScopeInfo{DataDir: runtimeScope.info.DataDir, Workspace: workspace, SessionID: admission.SessionID, OperationID: admission.OperationID, Revision: snapshot.resources.revisionString(), artifactLease: artifactLease, declaredBindings: snapshot.declaredBindings, operationSecrets: operationSecrets}
	base.sections = snapshot.extensions
	operationInfo := base
	operationInfo.Kind = ScopeOperation
	operation, err := snapshot.composition.openScope(ctx, operationInfo, []*scope{runtimeScope, workspaceScope})
	if err != nil {
		releaseArtifacts()
		return harness.Execution{}, err
	}
	_, releaseOperation, err := operation.enter(ctx)
	if err != nil {
		err = errors.Join(err, operation.close())
		releaseArtifacts() // the opened scope unwind finishes before the ownership releases
		return harness.Execution{}, err
	}
	agentInfo := base
	agentInfo.Kind = ScopeAgent
	openInvocation := func(ctx context.Context, request harness.InvocationRequest) (harness.InvocationExecution, error) {
		agentInfo.InvocationID = request.ID
		agentInfo.Purpose = string(request.Purpose)
		agentScope, err := snapshot.composition.openScope(ctx, agentInfo, []*scope{runtimeScope, workspaceScope, operation})
		if err != nil {
			return harness.InvocationExecution{}, err
		}
		unwind := func(cause error, release func()) (harness.InvocationExecution, error) {
			if release != nil {
				release()
			}
			return harness.InvocationExecution{}, errors.Join(cause, agentScope.close())
		}
		callCtx, releaseAgent, err := agentScope.enter(ctx)
		if err != nil {
			return unwind(err, nil)
		}
		// The invocation view selects every capability the Agent declared —
		// its non-tool capability IDs and each declared tool name, the
		// standard tools included — over the freshly opened Agent tier.
		selected := make([]string, 0, len(inputs.Capabilities)+len(inputs.Tools))
		selected = append(selected, inputs.Capabilities...)
		selected = append(selected, inputs.Tools...)
		invocation, err := opener(callCtx, admission, selection{inputs: inputs, definition: definition, scopes: []*scope{runtimeScope, workspaceScope, operation, agentScope}, selected: selected, invocation: Invocation{snapshot: snapshot}, lease: artifactLease, operationSecrets: operationSecrets}, request)
		if err != nil {
			return unwind(err, releaseAgent)
		}
		invocation.Close = func() error {
			releaseAgent()
			return agentScope.close()
		}
		return invocation, nil
	}
	// The committed opener owns the collected presentation: the successful
	// execution publishes its Session's prompt group before any model or tool
	// progress.
	warnings.publish(p.passive, admission.SessionID)
	*artifactRelease = releaseArtifacts
	strategyBinding, err := resolveStrategyBinding(snapshot.composition, []*scope{runtimeScope, workspaceScope, operation}, inputs.Strategy)
	if err != nil {
		releaseOperation()
		err = errors.Join(err, operation.close())
		releaseArtifacts()
		return harness.Execution{}, err
	}
	return harness.Execution{
		OpenInvocation: openInvocation,
		Permissions:    evaluator,
		Strategy:       strategyBinding,
		Close: func() error {
			releaseOperation()
			err := operation.close()
			return err
		},
	}, nil
}

// defaultStrategy returns the captured composition's host-selected default
// strategy binding. The one copy is captured at construction and consumed for
// both the execution capture and strategy selection.
func defaultStrategy(snapshot *configuration) *harness.StrategyBinding {
	return snapshot.composition.defaultStrategy
}

// requireCatalogModel admits only a selected model that resolves in the
// captured catalog, and rejects a wholly
// empty selection with the retained "no model configured" diagnostic. A
// partial identity keeps the catalog path: it is not resolvable there either.
// The context-window requirement is the selected strategy's declared
// metadata, enforced by the Harness admission gate over the capture.
// Credentials and provider connection are resolved by the concrete
// preparation that follows this availability check.
func requireCatalogModel(snapshot *configuration, ref model.ModelRef) error {
	if ref.IsZero() {
		return fmt.Errorf("no model configured: %w", harness.ErrInvalid)
	}
	_, _, err := snapshot.catalog.Lookup(catalog.ModelRef{Provider: ref.Provider, Model: ref.Model})
	if err != nil {
		return fmt.Errorf("agent model %s is not available in the catalog: %w: %w", ref.String(), err, harness.ErrInvalid)
	}
	return nil
}

// concretePrepare is the one production preparation. It consumes only the captured snapshot's inputs — the
// catalog entry, the once-resolved credential, and the composed prompt/tool
// surface — and returns the durable capture plus the invocation factory
// carrying the two constructed transports and the prepared workspace. Later
// configuration reloads and environment changes never reach a prepared
// execution, and no transport input, secret, or warning is stored beyond the
// durable capture fields.
func (p *preparation) concretePrepare(ctx context.Context, req harness.PreparationRequest, sel selection) (harness.ExecutionCapture, openInvocation, error) {
	snapshot := sel.invocation.snapshot
	provider, entry, err := snapshot.catalog.Lookup(catalog.ModelRef{Provider: sel.inputs.Model.Provider, Model: sel.inputs.Model.Model})
	if err != nil {
		return harness.ExecutionCapture{}, nil, fmt.Errorf("agent model %s is not available in the catalog: %w: %w", sel.inputs.Model.String(), err, harness.ErrInvalid)
	}
	apiKey := ""
	if env := provider.Transport.APIKeyEnv; env != "" {
		apiKey = sel.credentials[env].Value
		if apiKey == "" {
			return harness.ExecutionCapture{}, nil, fmt.Errorf("%w: %s (for provider %q): %w", config.ErrMissingEnvVar, env, provider.ID, harness.ErrInvalid)
		}
	}
	transport, err := p.concreteTransport(provider, entry, sel.inputs.Model, apiKey)
	if err != nil {
		return harness.ExecutionCapture{}, nil, err
	}
	// The compact configuration is the top-level compaction section of the
	// same one-revision snapshot; it is independent of the selectable compact
	// Agent definition.
	compaction := snapshot.compactionValues
	// The effective compact model is one fallback chain ending at the
	// conversation model: the configured compaction model — when it names a
	// different model at all — must resolve in the catalog and build a
	// transport over a resolved credential; otherwise the conversation model
	// and its transport are kept. The recorded compact configuration is
	// always the final effective model's.
	compactRef := sel.inputs.Model
	compactTransport := transport
	compactCost := entry.Cost
	if !compaction.model.IsZero() && compaction.model != sel.inputs.Model {
		compactProvider, compactEntry, lookupErr := snapshot.catalog.Lookup(catalog.ModelRef{Provider: compaction.model.Provider, Model: compaction.model.Model})
		if lookupErr == nil {
			keyMissing := false
			compactKey := ""
			if env := compactProvider.Transport.APIKeyEnv; env != "" {
				compactKey = sel.credentials[env].Value
				keyMissing = compactKey == ""
			}
			if !keyMissing {
				built, buildErr := p.concreteTransport(compactProvider, compactEntry, compaction.model, compactKey)
				if buildErr == nil {
					compactRef = compaction.model
					compactTransport = built
					compactCost = compactEntry.Cost
				}
			}
		}
	}
	specs, err := snapshot.composition.toolDeclarations(sel.inputs.Tools)
	if err != nil {
		return harness.ExecutionCapture{}, nil, err
	}
	agentInputs := sel.inputs
	subprocessEnv := p.subprocessEnv
	// The baseline environment presentation reads the same child-environment
	// projection the command path consumes: a name the owner-wide held or
	// current capture excludes is absent from the prompt and every
	// preparation/request hook input, and an absent projection or value
	// renders the existing "unknown".
	shell := projectedShell(ctx, subprocessEnv)
	result, catalog, err := composeSurface(ctx, surfaceRequest{
		hostIdentity:   p.identity,
		globalRulesDir: p.locations.GlobalRulesDir,
		workspace:      req.Session.Identity.Workspace,
		sessionStart:   req.Session.Identity.CreatedAt, // a Fork's zero start samples the compose time once
		promptSize:     prompt.Mode(sel.inputs.AssemblyMode),
		promptBody:     sel.inputs.Instructions,
		invocation:     sel.invocation,
		model:          sel.inputs.Model,
		constraints:    harness.ToolConstraints{Readonly: sel.inputs.Constraints.Readonly, WriteDir: sel.inputs.Constraints.WriteDir},
		identity:       req.Session.Identity,
		agentInputs:    agentInputs,
		toolSpecs:      specs,
		shell:          shell,
	})
	if err != nil {
		return harness.ExecutionCapture{}, nil, err
	}
	// One local view owns the baseline prompt, the eligible catalog and the
	// current visible subset while the selected contributions apply their
	// closed consequences in selected capability order.
	view := newPreparationView(result.Assembly, result.Prompt, catalog)
	if err := runPreparationContributions(ctx, req.Session.Identity, sel, view); err != nil {
		return harness.ExecutionCapture{}, nil, err
	}
	// The assembled prompt's diagnostics are this preparation's presentation
	// contribution, collected privately and published by the binder only
	// after every preparation step succeeded.
	if sel.warnings != nil {
		sel.warnings.prompt = result.Warnings
	}
	capture := harness.ExecutionCapture{
		ConfigurationRevision: sel.invocation.Revision(),
		Model:                 sel.inputs.Model,
		ContextWindow:         entry.ContextWindow,
		OutputReserve:         outputReserve(entry.MaxOutputTokens),
		SystemPrompt:          view.prompt,
		Tools:                 ownToolDefinitions(view.catalog),
		InitialToolNames:      slices.Clone(view.visible),
		Capabilities:          slices.Clone(sel.inputs.Capabilities),
		Readonly:              sel.inputs.Constraints.Readonly,
		WriteDir:              sel.inputs.Constraints.WriteDir,
		Pricing:               capturedPricing(entry.Cost),
		Compact: harness.CompactCapture{
			Model:        compactRef,
			SystemPrompt: compaction.prompt,
			Pricing:      capturedPricing(compactCost),
		},
		Strategy: harness.StrategyCapture{
			ID:                    defaultStrategy(snapshot).ID,
			RequiresContextWindow: defaultStrategy(snapshot).RequiresContextWindow,
		},
		Implementations: implementationProvenance(snapshot.composition, sel),
	}
	if sel.inputs.Strategy.ID != "" {
		capability, _ := snapshot.composition.compiled.Capability(sel.inputs.Strategy.ID)
		capture.Strategy = harness.StrategyCapture{ID: capability.ID, RequiresContextWindow: capability.Strategy.RequiresContextWindow}
	}
	capture.Strategy.Settings = append(json.RawMessage(nil), sel.inputs.Strategy.Settings...)
	workspace := req.Session.Identity.Workspace
	return capture, p.concreteOpener(transport, compactTransport, req.Session.Identity, workspace, view.catalog, subprocessEnv), nil
}

// projectedShell reads SHELL from the same child-environment projection the
// command path consumes. An absent producer, absent snapshot, absent value or
// empty value renders the existing "unknown".
func projectedShell(ctx context.Context, env func(context.Context) ([]string, error)) string {
	if env == nil {
		return "unknown"
	}
	snapshot, err := env(ctx)
	if err != nil || snapshot == nil {
		return "unknown"
	}
	for _, entry := range snapshot {
		name, value, ok := strings.Cut(entry, "=")
		if ok && name == "SHELL" {
			if value == "" {
				return "unknown"
			}
			return value
		}
	}
	return "unknown"
}

// outputReserve applies the retained reserve rule: the catalog entry's
// MaxOutputTokens when positive, else the fixed 131072 reserve.
func outputReserve(maxOutputTokens int) int {
	if maxOutputTokens > 0 {
		return maxOutputTokens
	}
	return 131072
}

// capturedPricing copies one catalog entry's per-million USD rates into
// owned capture pricing: an absent cost means unknown pricing, and a
// non-finite catalog rate becomes an absent rate, never a preparation
// failure.
func capturedPricing(cost *catalog.Cost) *harness.ModelPricing {
	if cost == nil {
		return nil
	}
	return &harness.ModelPricing{
		Input:     finiteRate(cost.Input),
		Output:    finiteRate(cost.Output),
		CacheRead: finiteRate(cost.CacheRead),
	}
}

func finiteRate(v *float64) *float64 {
	if v == nil || math.IsNaN(*v) || math.IsInf(*v, 0) {
		return nil
	}
	copied := *v
	return &copied
}

// concreteTransport converts the captured catalog provider/model entry and
// the resolved key into the one fixed transport for the execution.
func (p *preparation) concreteTransport(provider *catalog.Provider, entry *catalog.Model, ref model.ModelRef, apiKey string) (*model.Transport, error) {
	resolved, err := p.resolveTransport(provider, entry, ref, apiKey)
	if err != nil {
		return nil, err
	}
	transport, err := model.NewTransport(resolved)
	if err != nil {
		return nil, fmt.Errorf("build model transport for %s: %w", ref.String(), err)
	}
	return transport, nil
}

// resolveTransport builds the transport's resolved input from the captured
// catalog provider/model entry and the resolved key, applying the retained
// conversion semantics exactly: the model entry's effective system role and
// streamed-usage flag as the catalog build already resolved them; sidecar
// extra-body layers re-encoded with raw numbers preserved; the same-provider
// source-family map from the provider's models; and the retained opt-in
// wire-debug directory. No transport option feeds this conversion; no timeout
// input or HTTP option exists.
func (p *preparation) resolveTransport(provider *catalog.Provider, entry *catalog.Model, ref model.ModelRef, apiKey string) (model.ResolvedTransport, error) {
	resolved := model.ResolvedTransport{
		Model:          ref,
		BaseURL:        provider.Transport.BaseURL,
		APIKey:         apiKey,
		Headers:        provider.Transport.Headers,
		WireSystemRole: string(entry.SystemRole), // the catalog build injects the effective role; Encode maps empty to "system" as backstop
		StreamedUsage:  entry.UsageInStream,
		WireDebugDir:   wireDebugDir(p.locations.WireDebugDir),
	}
	providerExtras, err := extraBody(provider.ExtraBody)
	if err != nil {
		return model.ResolvedTransport{}, fmt.Errorf("provider %s extra body: %w", provider.ID, err)
	}
	resolved.ProviderExtraBody = providerExtras
	modelExtras, err := extraBody(entry.ExtraBody)
	if err != nil {
		return model.ResolvedTransport{}, fmt.Errorf("model %s extra body: %w", entry.ID, err)
	}
	resolved.ModelExtraBody = modelExtras
	if meta := entry.ProtocolMetadata; meta != nil { // the built catalog's model metadata is the effective model-over-provider merge
		resolved.ProtocolFamily = meta.Family
		resolved.MustPreserve = meta.MustPreserve
		if len(meta.Drop) > 0 {
			resolved.Drop = make(map[string]bool, len(meta.Drop))
			for _, key := range meta.Drop {
				resolved.Drop[key] = true
			}
		}
	}
	resolved.SourceFamilies = make(map[model.ModelRef]string, len(provider.Models))
	for id, sibling := range provider.Models {
		if sibling == nil || sibling.ProtocolMetadata == nil || sibling.ProtocolMetadata.Family == "" {
			continue
		}
		resolved.SourceFamilies[model.ModelRef{Provider: provider.ID, Model: id}] = sibling.ProtocolMetadata.Family
	}
	return resolved, nil
}

// extraBody converts one catalog sidecar layer into the transport's extra
// layer by re-encoding each value verbatim: catalog numbers decode as
// json.Number, so raw number lexemes are preserved with no float coercion.
func extraBody(in map[string]any) (model.Extra, error) {
	if len(in) == 0 {
		return nil, nil
	}
	out := make(model.Extra, len(in))
	for key, value := range in {
		raw, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("value of %q: %w", key, err)
		}
		out[key] = raw
	}
	return out, nil
}

// wireDebugDir applies the selected location: an empty location disables
// diagnostics; otherwise raw wire artifacts dump under it, created once per
// preparation; a failed creation disables diagnostics, never execution.
func wireDebugDir(location string) string {
	if location == "" {
		return ""
	}
	if err := os.MkdirAll(location, 0o700); err != nil {
		return ""
	}
	return location
}

// concreteOpener returns the production invocation factory: per Engine call
// it binds every advertised tool name from that invocation's view over all
// four scopes, binds the selected argument hooks, owns one ToolContext
// carrying the prepare-time Workspace, the calling Operation's admitted-input
// identity, the captured Invocation, the validated hard constraints and a
// never-armed background object for normalization, and answers with the
// invocation whose model callback makes exactly one physical attempt over the
// transport its request purpose selects — the conversation transport for a
// conversation request and the effective compact transport for a maintenance
// request, the same construction over the one shared transport when the
// compact model is the conversation model. Each tool dispatch copies that
// context with a fresh background object, prepares, freezes the plan's
// permissions and wraps its executor, so the plugin holds no background
// authority outside Execute. The ToolContext's environment callback is the
// preparation's fixed wrapper over the Melta's live producer: it filters the
// captured provider and declared credential names from every snapshot it
// returns, never rereads the selected execution policy, and keeps the nil
// producer and nil snapshot fail-closed shapes unchanged. The shared
// advertisement gate rejects unknown names before dispatch; retry
// classification and scope disposal belong to the shared machinery.
func (p *preparation) concreteOpener(transport, compactTransport *model.Transport, session harness.SessionIdentity, workspace string, catalog []model.ToolDefinition, subprocessEnv func(context.Context) ([]string, error)) openInvocation {
	return func(_ context.Context, admission harness.OperationAdmission, sel selection, request harness.InvocationRequest) (harness.InvocationExecution, error) {
		table := make(map[string]Tool, len(catalog))
		for _, definition := range catalog {
			tool, err := sel.invocation.snapshot.composition.bindTool(sel, definition.Name)
			if err != nil {
				return harness.InvocationExecution{}, err
			}
			table[definition.Name] = tool
		}
		identity := callIdentity{session: session, operationID: admission.OperationID, invocationID: request.ID, purpose: string(request.Purpose), lease: sel.lease, operationSecrets: sel.operationSecrets}
		hooks := bindToolArgumentHooks(sel, identity)
		requestHooks := bindRequestHooks(sel, identity)
		outputHooks := bindToolOutputHooks(sel, identity)
		sessionID := admission.AdmittedEntry.SessionID
		tc := ToolContext{
			Session:       session,
			Model:         sel.inputs.Model,
			Workspace:     workspace,
			OperationID:   admission.OperationID,
			InvocationID:  request.ID,
			Purpose:       request.Purpose,
			AdmittedEntry: admission.AdmittedEntry,
			Invocation:    sel.invocation,
			Constraints:   harness.ToolConstraints{Readonly: sel.inputs.Constraints.Readonly, WriteDir: sel.inputs.Constraints.WriteDir},
			Background:    &boundBackground{bridge: p.background, sessionID: sessionID},
			SubprocessEnv: subprocessEnv,
		}
		attemptTransport := transport
		if request.Purpose == harness.InvocationPurposeMaintenance {
			attemptTransport = compactTransport
		}
		return harness.InvocationExecution{
			Model: func(ctx context.Context, req model.Request) (model.Stream, error) {
				stream, warnings, err := attemptTransport.Stream(ctx, req, nil) // runtime extras are a later phase's channel
				// The attempt's diagnostics are recorded under the admitted
				// Session identity even when the physical request failed after
				// encoding succeeded — a diagnostic is presentation only.
				p.passive.replaceProtocolWarnings(admission.SessionID, protocolWarnings(admission.SessionID, warnings))
				return stream, err
			},
			Tool: func(ctx context.Context, call model.ToolCall) harness.PreparedTool {
				callTC := tc
				background := &boundBackground{bridge: p.background, sessionID: sessionID}
				callTC.Background = background
				return background.bindPlan(table[call.Name].Prepare(ctx, callTC, call))
			},
			NormalizeTool: func(ctx context.Context, call model.ToolCall) (json.RawMessage, error) {
				return table[call.Name].Normalize(ctx, tc, call)
			},
			ToolHooks:       hooks,
			RequestHooks:    requestHooks,
			ToolOutputHooks: outputHooks,
		}, nil
	}
}

// preparationView is the one local owner of a preparation's mutable
// prompt/catalog/visibility state. It starts from the Core-assembled baseline
// inputs, the eligible catalog of every tool whose owner-declared description
// is available, and all of those names visible. Every contribution receives
// owned copies of the current state and may change it only through one closed
// consequence; the Core-owned capture is constructed from this view after the
// selected contributions finish.
type preparationView struct {
	assembly prompt.AssemblyInput
	prompt   string
	catalog  []model.ToolDefinition
	visible  []string
}

func newPreparationView(assembly prompt.AssemblyInput, prompt string, catalog []model.ToolDefinition) *preparationView {
	visible := make([]string, len(catalog))
	for i := range catalog {
		visible[i] = catalog[i].Name
	}
	return &preparationView{assembly: assembly, prompt: prompt, catalog: catalog, visible: visible}
}

// tools returns the current visible subset in its visible order.
func (v *preparationView) tools() []model.ToolDefinition {
	index := make(map[string]model.ToolDefinition, len(v.catalog))
	for _, definition := range v.catalog {
		index[definition.Name] = definition
	}
	tools := make([]model.ToolDefinition, 0, len(v.visible))
	for _, name := range v.visible {
		tools = append(tools, index[name])
	}
	return tools
}

// input returns one contribution's owned copy of the current view plus the
// supplying provider's own captured settings section.
func (v *preparationView) input(session harness.SessionIdentity, ref model.ModelRef, constraints harness.ToolConstraints, settings json.RawMessage) extension.PreparationInput {
	assembly := v.assembly
	assembly.Rules.Warnings = slices.Clone(v.assembly.Rules.Warnings)
	return extension.PreparationInput{
		Session:     session,
		Model:       ref,
		Constraints: constraints,
		Assembly:    assembly,
		Prompt:      v.prompt,
		Catalog:     ownToolDefinitions(v.catalog),
		Tools:       ownToolDefinitions(v.tools()),
		Settings:    append(json.RawMessage(nil), settings...),
	}
}

// apply accepts one contribution's consequence. Descriptions and selected names
// must belong to the original eligible catalog; selected names must be unique
// and keep their given order. A present empty Prompt or
// ToolNames removes the prompt or advertisement respectively.
func (v *preparationView) apply(id string, consequence extension.PreparationConsequence) error {
	if consequence.Prompt != nil {
		v.prompt = *consequence.Prompt
	}
	known := make(map[string]int, len(v.catalog))
	for i, definition := range v.catalog {
		known[definition.Name] = i
	}
	for name, description := range consequence.Descriptions {
		index, ok := known[name]
		if !ok {
			return fmt.Errorf("preparation hook %q described tool %q outside the original catalog: %w", id, name, harness.ErrInvalid)
		}
		v.catalog[index].Description = description
	}
	if consequence.ToolNames != nil {
		selected := make(map[string]bool, len(consequence.ToolNames))
		for _, name := range consequence.ToolNames {
			if _, ok := known[name]; !ok {
				return fmt.Errorf("preparation hook %q selected tool %q outside the original catalog: %w", id, name, harness.ErrInvalid)
			}
			if selected[name] {
				return fmt.Errorf("preparation hook %q selected duplicate tool %q: %w", id, name, harness.ErrInvalid)
			}
			selected[name] = true
		}
		v.visible = slices.Clone(consequence.ToolNames)
	}
	return nil
}

// ownToolDefinitions returns a deep copy of one tool-definition list: the
// slice, every definition, and its parameter bytes are independent of the
// Caller's storage.
func ownToolDefinitions(definitions []model.ToolDefinition) []model.ToolDefinition {
	if definitions == nil {
		return nil
	}
	owned := make([]model.ToolDefinition, len(definitions))
	for i, definition := range definitions {
		owned[i] = definition
		owned[i].Parameters = bytes.Clone(definition.Parameters)
	}
	return owned
}

// runPreparationContributions binds and directly invokes, in selected order
// inside the same preparation guard, every available long-lived selected
// preparation hook. Each contribution sees an owned copy of the current view
// and only the supplying provider's settings, and its accepted consequence is
// applied before the next runs. An invalid consequence, error or observed
// cancellation aborts admission before the next contribution runs.
func runPreparationContributions(ctx context.Context, session harness.SessionIdentity, sel selection, view *preparationView) error {
	constraints := harness.ToolConstraints{Readonly: sel.inputs.Constraints.Readonly, WriteDir: sel.inputs.Constraints.WriteDir}
	composition := sel.invocation.snapshot.composition
	for _, id := range sel.inputs.Capabilities {
		value, ok := selectedCapability(sel.scopes, id)
		if !ok || value.capability == nil || value.capability.Kind != extension.KindHook {
			continue
		}
		if _, ok := hookBoundary(value, extension.BoundaryPrepare); !ok {
			continue
		}
		if value.scope.unavailable() {
			return fmt.Errorf("preparation hook %q: %w", id, ErrClosed)
		}
		hook := value.implementation.Preparation
		if hook == nil {
			return fmt.Errorf("preparation hook %q has no implementation: %w", id, ErrComposition)
		}
		var settings json.RawMessage
		if value.capability.Declaration != nil {
			settings = sel.invocation.Config(value.capability.Declaration.Declaration.ID)
		}
		viewScope, call := composition.newView(value, ctx, callIdentity{session: session, revision: sel.invocation.Revision(), grant: nativeGrant{pure: true}})
		consequence, err := hook(viewScope.ctx, call, view.input(session, sel.inputs.Model, constraints, settings))
		viewScope.end()
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := view.apply(id, consequence); err != nil {
			return err
		}
	}
	return nil
}
