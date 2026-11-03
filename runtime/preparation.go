package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/prompt"
	"github.com/MMinasyan/lightcode/protocol"
)

// debugWireEnv is the retained wire-debug opt-in: when set to "1", raw
// request bodies and SSE chunks dump under the home-based private directory.
const debugWireEnv = "LIGHTCODE_DEBUG_WIRE"

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
	agent      harness.AgentType
	bindings   Bindings
	invocation Invocation

	// credentials is the preparation's one captured credential observation
	// set, sampled with the same capture as the Invocation's snapshot: the
	// concrete preparation resolves its transport secrets from it, and no
	// value leaves the prepared opener or the durable capture.
	credentials map[string]config.EnvValue

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

// PreparationInput is one selected preparation contribution's complete owned
// input: the admission identity and hard constraints, the captured baseline
// assembly inputs, the effective prompt and eligible tool catalog accepted so
// far, the current visible tool subset, and only the contributing provider's
// own captured settings section. It carries no authority: the model,
// constraints, catalog membership and assembly inputs are copies, and the
// contribution's only output is a closed consequence.
type PreparationInput struct {
	Session     harness.SessionIdentity
	Model       model.ModelRef
	Constraints ToolConstraints
	Assembly    prompt.AssemblyInput
	Prompt      string
	Catalog     []model.ToolDefinition
	Tools       []model.ToolDefinition
	Settings    json.RawMessage
}

// PreparationConsequence is one contribution's complete accepted effect.
// Absent Prompt and ToolNames change nothing; a present empty Prompt removes
// the prompt and a present empty ToolNames advertises no tool. Description
// keys and selected names must belong to the original eligible catalog;
// selected names must be unique and are applied in catalog order. Description
// text may be empty. Parameters, identity and eligibility remain the tool
// owner's metadata.
type PreparationConsequence struct {
	Prompt       *string
	Descriptions map[string]string
	ToolNames    []string
}

// PreparationHook is one pure preparation capability: it may replace the
// effective system prompt, tool descriptions and advertised subset, but never
// the captured model, revision, capability IDs, or permission capability
// constraints, and it may not add tool names outside the original eligible
// catalog. Hooks read only their own captured settings through the input,
// start no effects, and hold no resource: a hook provider must therefore be
// Runtime- or Workspace-scoped. Harness owns no hook fields and no hook
// dispatcher.
type PreparationHook interface {
	Prepare(context.Context, PreparationInput) (PreparationConsequence, error)
}

// preparationHookType is the declared-type test one selected capability must
// satisfy to be bound and invoked as a preparation hook.
var preparationHookType = reflect.TypeFor[PreparationHook]()

// ToolArgumentsHook is one effectful argument-repair capability: before a
// concrete tool call prepares, it may rewrite the call's raw argument bytes.
// Its successful replacement must be exactly one JSON object the execution's
// normalizer accepts; it can never replace the call identity, tool name, or
// another call's arguments. Each execution is a Harness-settled Operation
// effect with durable evidence; the hook itself receives owned call data and
// the same captured Invocation, and any of the four scopes may supply one.
type ToolArgumentsHook interface {
	BeforeTool(context.Context, Invocation, model.ToolCall) (json.RawMessage, error)
}

// toolArgumentsHookType is the declared-type test one selected capability
// must satisfy to be bound and invoked as an argument hook.
var toolArgumentsHookType = reflect.TypeFor[ToolArgumentsHook]()

// preparation supplies the existing Harness preparation contract for every
// admission producer: root, queued delivery and Fork all bind through the one
// closure returned by bind, with only resource lifetime and the selected
// invocation context added to what Harness already validates. The snapshot
// pointer loads once per call, so a reload overlapping preparation affects the
// next capture, not the in-flight one; the returned opener keeps the same
// immutable snapshot while borrowed bindings and the call context stay inside
// the preparation guard.
type preparation struct {
	config      *configurationService
	composition *composition
	runtime     *scope
	workspaces  *workspaceScopes
	home        string

	// identity and globalRulesDir are the host-supplied prompt inputs: the
	// application persona text and the explicit global rules directory. Their
	// zero values supply no identity and no global rules, never an implicit
	// fallback.
	identity       string
	globalRulesDir string
	background     BackgroundServices
	passive        *observationAdapter

	// artifacts is the Runtime's private per-Session artifact interval
	// registry. Every committed opener acquires its Session's token here
	// before any scope, plugin, model, tool or hook effect and releases it
	// last, after the scoped cleanup; nil leaves an isolated preparation
	// without artifact ownership.
	artifacts *artifactIntervals

	// subprocessEnv is the Runtime's live subprocess-environment producer —
	// the managed-environment snapshot with the current provider credential
	// names excluded — supplied only when the retained manager exists; nil
	// leaves every prepared command without an environment producer and the
	// cooperative command fails. It is invoked at each process start, never
	// at preparation.
	subprocessEnv func() []string
}

// newPreparation wires the binder to the published configuration, the
// composition with its constructed Runtime scope and Workspace registry, the
// once-resolved home, the host-supplied prompt identity and global rules
// directory, the background services bridge armed after harness.New
// returns, the Runtime's passive publication adapter (nil in isolated
// preparation tests, dropping only passive presentation), the Runtime's
// private artifact interval registry (nil without a Runtime), and the
// Runtime's live managed-environment producer (nil without a retained
// manager). One concrete preparation path serves every admission.
func newPreparation(config *configurationService, c *composition, runtime *scope, workspaces *workspaceScopes, home, identity, globalRulesDir string, background BackgroundServices, passive *observationAdapter, artifacts *artifactIntervals, subprocessEnv func() []string) *preparation {
	return &preparation{
		config:         config,
		composition:    c,
		runtime:        runtime,
		workspaces:     workspaces,
		home:           home,
		identity:       identity,
		globalRulesDir: globalRulesDir,
		background:     background,
		passive:        passive,
		artifacts:      artifacts,
		subprocessEnv:  subprocessEnv,
	}
}

// bind returns the exact two-argument callback type required by
// harness.Dependencies.Prepare. Selected unknown or unavailable Agent types
// and models report harness.ErrInvalid at admission, never a failed
// configuration publication; preparation errors and cancellation abort
// admission with no partial consequence published.
func (p *preparation) bind() func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
	return func(ctx context.Context, req harness.PreparationRequest) (harness.PreparedExecution, error) {
		// Configuration and the credentials its catalog references are one
		// owner-coherent capture: the selected model, its transport secret,
		// and the revision all belong to the same ready pair.
		captured, err := p.config.capture(ctx)
		if err != nil {
			return harness.PreparedExecution{}, err
		}
		snapshot := captured.snapshot
		if snapshot == nil {
			return harness.PreparedExecution{}, fmt.Errorf("agent type %q has no published configuration: %w", req.Session.AgentType, harness.ErrInvalid)
		}
		agent, err := harness.ResolveAgentType(req.Session.AgentType, snapshot.agentTypes())
		if err != nil {
			return harness.PreparedExecution{}, err
		}
		// Every selected capability must exist in the compiled export table
		// before admission. A selected binding that belongs to a shorter
		// lifetime is still compiled here and correctly binds only when its
		// scope opens; only an ID that no declaration owns is rejected.
		for _, id := range agent.Capabilities {
			if _, ok := p.composition.exports[id]; !ok {
				return harness.PreparedExecution{}, fmt.Errorf("agent type %q selects undeclared capability %q: %w", agent.Name, id, harness.ErrInvalid)
			}
		}
		if err := requireCatalogModel(snapshot, agent.Model); err != nil {
			return harness.PreparedExecution{}, err
		}
		workspace := req.Session.Identity.Workspace
		workspaceScope, err := p.workspaces.get(ctx, ScopeInfo{Kind: ScopeWorkspace, DataDir: p.runtime.info.DataDir, Workspace: workspace})
		if err != nil {
			return harness.PreparedExecution{}, err
		}
		bindings := p.preparationBindings(workspaceScope, agent.Capabilities)
		sel := selection{agent: agent, bindings: bindings, invocation: Invocation{snapshot: snapshot}, credentials: captured.credentials}

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
		return harness.PreparedExecution{
			Capture: capture,
			Open: func(openCtx context.Context, admission harness.OperationAdmission) (harness.Execution, error) {
				return p.open(openCtx, admission, workspace, workspaceScope, agent, snapshot, warnings, opener)
			},
		}, nil
	}
}

// preparationBindings resolves the preparation view: the Agent's selected
// capabilities currently supplied by the Runtime or Workspace scope. A
// selected capability provided only by an Operation or Agent scope is valid
// but not constructed yet, so it stays absent until Open rather than rejecting
// the admission.
func (p *preparation) preparationBindings(workspaceScope *scope, selected []string) Bindings {
	entries := make(map[string]bindingEntry, len(selected))
	for _, id := range selected {
		for _, source := range []*scope{p.runtime, workspaceScope} {
			if entry, ok := source.bindings.entries[id]; ok {
				entries[id] = entry
				break
			}
		}
	}
	return Bindings{entries: entries}
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
// its Agent scope before that; the artifact token releases last, after all
// scoped cleanup, and an opening failure releases it before returning. Only
// after the opening succeeds does the collected prompt presentation publish,
// so a failed fork or admission never creates warnings for an unpublished
// Session and a successful opener publishes before any progress.
func (p *preparation) open(ctx context.Context, admission harness.OperationAdmission, workspace string, workspaceScope *scope, agent harness.AgentType, snapshot *configuration, warnings *preparationWarnings, opener openInvocation) (harness.Execution, error) {
	releaseArtifacts, err := p.artifacts.acquireExecution(ctx, sessionArtifactDir(p.runtime.info.DataDir, admission.SessionID))
	if err != nil {
		return harness.Execution{}, err
	}
	base := ScopeInfo{DataDir: p.runtime.info.DataDir, Workspace: workspace, SessionID: admission.SessionID, OperationID: admission.OperationID}
	operationInfo := base
	operationInfo.Kind = ScopeOperation
	operation, err := p.composition.openScope(ctx, operationInfo, []*scope{p.runtime, workspaceScope})
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
		agentScope, err := p.composition.openScope(ctx, agentInfo, []*scope{p.runtime, workspaceScope, operation})
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
		selected := make([]string, 0, len(agent.Capabilities)+len(agent.Tools))
		selected = append(selected, agent.Capabilities...)
		selected = append(selected, agent.Tools...)
		bindings, err := selectCapabilities([]*scope{p.runtime, workspaceScope, operation, agentScope}, selected)
		if err != nil {
			return unwind(err, releaseAgent)
		}
		invocation, err := opener(callCtx, admission, selection{agent: agent, bindings: bindings, invocation: Invocation{snapshot: snapshot}}, request)
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
	return harness.Execution{
		OpenInvocation: openInvocation,
		Permissions:    snapshot.permissionPolicy(workspace),
		Strategy:       productionStrategy,
		Close: func() error {
			releaseOperation()
			err := operation.close()
			releaseArtifacts() // the token releases last, after the scoped cleanup
			return err
		},
	}, nil
}

// bindToolArgumentHooks binds, in the Agent's selected capability order and
// only after all execution scopes opened, every selected binding whose
// declared type implements ToolArgumentsHook — from any supplying scope. IDs
// are the capability IDs, each hook's extension identity is the supplying
// plugin's, and every Runtime-declared argument hook participates as an
// effectful boundary. Each hook receives the same captured Invocation.
func bindToolArgumentHooks(selected []string, bindings Bindings, invocation Invocation) ([]harness.ToolArgumentsHook, error) {
	var hooks []harness.ToolArgumentsHook
	for _, id := range selected {
		entry, ok := bindings.entries[id]
		if !ok || !entry.declared.Implements(toolArgumentsHookType) {
			continue
		}
		hook, err := Bind[ToolArgumentsHook](bindings, id)
		if err != nil {
			return nil, err
		}
		hooks = append(hooks, harness.ToolArgumentsHook{
			ID:          id,
			ExtensionID: entry.plugin,
			Mode:        harness.HookModeEffectful,
			Run: func(ctx context.Context, call model.ToolCall) (json.RawMessage, error) {
				return hook.BeforeTool(ctx, invocation, call)
			},
		})
	}
	return hooks, nil
}

// productionStrategy is the one context strategy binding this production
// preparation supplies: the included summary policy. Its identity and
// declared requirement are resolved capture inputs; the implementation is the
// owned execution resource the opened Execution carries.
var productionStrategy = harness.IncludedStrategyBinding()

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
	provider, entry, err := snapshot.catalog.Lookup(catalog.ModelRef{Provider: sel.agent.Model.Provider, Model: sel.agent.Model.Model})
	if err != nil {
		return harness.ExecutionCapture{}, nil, fmt.Errorf("agent model %s is not available in the catalog: %w: %w", sel.agent.Model.String(), err, harness.ErrInvalid)
	}
	apiKey := ""
	if env := provider.Transport.APIKeyEnv; env != "" {
		apiKey = sel.credentials[env].Value
		if apiKey == "" {
			return harness.ExecutionCapture{}, nil, fmt.Errorf("%w: %s (for provider %q): %w", config.ErrMissingEnvVar, env, provider.ID, harness.ErrInvalid)
		}
	}
	transport, err := p.concreteTransport(provider, entry, sel.agent.Model, apiKey)
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
	compactRef := sel.agent.Model
	compactTransport := transport
	compactCost := entry.Cost
	if !compaction.model.IsZero() && compaction.model != sel.agent.Model {
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
	specs, err := p.toolSpecs(sel.agent.Tools)
	if err != nil {
		return harness.ExecutionCapture{}, nil, err
	}
	result, catalog, err := composeSurface(surfaceRequest{
		hostIdentity:   p.identity,
		globalRulesDir: p.globalRulesDir,
		workspace:      req.Session.Identity.Workspace,
		sessionStart:   req.Session.Identity.CreatedAt, // a Fork's zero start samples the compose time once
		promptSize:     prompt.Mode(sel.agent.SystemPrompt),
		promptBody:     sel.agent.Prompt,
		invocation:     sel.invocation,
		constraints:    ToolConstraints{Readonly: sel.agent.Readonly, WriteDir: sel.agent.WriteDir},
		identity:       req.Session.Identity,
		toolSpecs:      specs,
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
	tools := view.tools()
	capture := harness.ExecutionCapture{
		ConfigurationRevision: sel.invocation.Revision(),
		Model:                 sel.agent.Model,
		ContextWindow:         entry.ContextWindow,
		OutputReserve:         outputReserve(entry.MaxOutputTokens),
		SystemPrompt:          view.prompt,
		Tools:                 tools,
		Capabilities:          slices.Clone(sel.agent.Capabilities),
		Readonly:              sel.agent.Readonly,
		WriteDir:              sel.agent.WriteDir,
		Pricing:               capturedPricing(entry.Cost),
		Compact: harness.CompactCapture{
			Model:        compactRef,
			SystemPrompt: compaction.prompt,
			Pricing:      capturedPricing(compactCost),
		},
		Strategy: harness.StrategyCapture{
			ID:                    productionStrategy.ID,
			RequiresContextWindow: productionStrategy.RequiresContextWindow,
		},
	}
	workspace := req.Session.Identity.Workspace
	// The prepared opener carries only the preparation-time credential
	// names, never their values: the ToolContext's fixed callback filters
	// those names from the live producer's snapshot, so a binding renamed or
	// removed after this preparation still cannot reach a command child.
	credentialNames := make([]string, 0, len(sel.credentials))
	for name := range sel.credentials {
		credentialNames = append(credentialNames, name)
	}
	return capture, p.concreteOpener(transport, compactTransport, workspace, tools, credentialNames), nil
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

// toolSpecs resolves the Agent's declared tool names, deduplicated by first
// occurrence, to the composition's declarations in that order. The agent
// parser intersects tool lists with the composed tool universe, so a miss
// cannot occur in a published selection.
func (p *preparation) toolSpecs(names []string) ([]CapabilitySpec, error) {
	specs := make([]CapabilitySpec, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		spec, ok := p.composition.toolSpec(name)
		if !ok {
			return nil, fmt.Errorf("tool %q has no composed declaration: %w", name, ErrComposition)
		}
		specs = append(specs, spec)
	}
	return specs, nil
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
		WireDebugDir:   wireDebugDir(p.home),
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

// wireDebugDir applies the retained opt-in: with the environment variable set
// to "1", raw wire artifacts dump under the home-based private directory,
// created once per preparation; a failed creation disables diagnostics, never
// execution.
func wireDebugDir(home string) string {
	if os.Getenv(debugWireEnv) != "1" {
		return ""
	}
	dir := filepath.Join(home, ".lightcode", "debug", "wire")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return dir
}

// concreteOpener returns the production invocation factory: per Engine call
// it binds every advertised tool name from that invocation's view over all
// four scopes, binds the selected argument hooks, owns one ToolContext
// carrying the prepare-time Workspace, the calling Operation's admitted-input
// identity, the captured Invocation and the validated hard constraints, and
// answers with the invocation whose model callback makes exactly one physical
// attempt over the transport its request purpose selects — the conversation
// transport for a conversation request and the effective compact transport for
// a maintenance request, the same construction over the one shared transport
// when the compact model is the conversation model. The ToolContext's
// environment callback is the preparation's fixed wrapper over the Runtime's
// live producer: it filters the captured credential names from every snapshot
// it returns, never rereads the selected execution policy, and keeps the nil
// producer and nil snapshot fail-closed shapes unchanged. The shared
// advertisement gate rejects unknown names before dispatch; retry
// classification and scope disposal belong to the shared machinery.
func (p *preparation) concreteOpener(transport, compactTransport *model.Transport, workspace string, advertised []model.ToolDefinition, credentialNames []string) openInvocation {
	return func(_ context.Context, admission harness.OperationAdmission, sel selection, request harness.InvocationRequest) (harness.InvocationExecution, error) {
		table := make(map[string]Tool, len(advertised))
		for _, definition := range advertised {
			tool, err := Bind[Tool](sel.bindings, definition.Name)
			if err != nil {
				return harness.InvocationExecution{}, err
			}
			table[definition.Name] = tool
		}
		hooks, err := bindToolArgumentHooks(sel.agent.Capabilities, sel.bindings, sel.invocation)
		if err != nil {
			return harness.InvocationExecution{}, err
		}
		subprocessEnv := p.subprocessEnv
		if subprocessEnv != nil {
			base := subprocessEnv
			subprocessEnv = func() []string {
				env := base()
				if env == nil {
					return nil
				}
				return config.EnvWithoutKeys(env, credentialNames)
			}
		}
		tc := ToolContext{
			Workspace:     workspace,
			AdmittedEntry: admission.AdmittedEntry,
			Invocation:    sel.invocation,
			Constraints:   ToolConstraints{Readonly: sel.agent.Readonly, WriteDir: sel.agent.WriteDir},
			Background:    p.background,
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
				return table[call.Name].Prepare(ctx, tc, call)
			},
			NormalizeTool: func(call model.ToolCall) (json.RawMessage, error) {
				return table[call.Name].Normalize(tc, call)
			},
			ToolHooks: hooks,
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

// tools returns the current visible subset in catalog order.
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
func (v *preparationView) input(session harness.SessionIdentity, ref model.ModelRef, constraints ToolConstraints, settings json.RawMessage) PreparationInput {
	assembly := v.assembly
	assembly.Rules.Warnings = slices.Clone(v.assembly.Rules.Warnings)
	return PreparationInput{
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
// and are canonicalized into catalog order. A present empty Prompt or
// ToolNames removes the prompt or advertisement respectively.
func (v *preparationView) apply(id string, consequence PreparationConsequence) error {
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
		visible := make([]string, 0, len(selected))
		for _, definition := range v.catalog {
			if selected[definition.Name] {
				visible = append(visible, definition.Name)
			}
		}
		v.visible = visible
	}
	return nil
}

// ownToolDefinitions returns a deep copy of one tool-definition list: the
// slice, every definition, and its parameter bytes are independent of the
// caller's storage.
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
// binding whose declared type satisfies PreparationHook. Each contribution
// sees an owned copy of the current view and only the supplying provider's
// settings, and its accepted consequence is applied before the next runs. An
// invalid consequence, error or observed cancellation aborts admission before
// the next contribution runs, so no partial consequence is published.
func runPreparationContributions(ctx context.Context, session harness.SessionIdentity, sel selection, view *preparationView) error {
	constraints := ToolConstraints{Readonly: sel.agent.Readonly, WriteDir: sel.agent.WriteDir}
	for _, id := range sel.agent.Capabilities {
		entry, ok := sel.bindings.entries[id]
		if !ok || !entry.declared.Implements(preparationHookType) {
			continue
		}
		hook, err := Bind[PreparationHook](sel.bindings, id)
		if err != nil {
			return err
		}
		var settings json.RawMessage
		if entry.plugin != "" {
			settings = sel.invocation.Config(entry.plugin)
		}
		consequence, err := hook.Prepare(ctx, view.input(session, sel.agent.Model, constraints, settings))
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
