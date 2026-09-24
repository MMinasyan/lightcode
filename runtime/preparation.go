package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/model"
)

// debugWireEnv is the retained wire-debug opt-in: when set to "1", raw
// request bodies and SSE chunks dump under the home-based private directory.
const debugWireEnv = "LIGHTCODE_DEBUG_WIRE"

// prepare is the controlled preparation function the Runtime calls inside one
// Workspace-scope guard: it returns the durable capture values plus the opener
// for the committed admission, never short-lived resources. Concrete
// production preparation supplies it in its owning phase; controlled callers
// supply it now.
type prepare func(context.Context, harness.PreparationRequest, selection) (harness.ExecutionCapture, openExecution, error)

// openExecution turns one committed admission and the opened execution scope
// view into the execution the Harness runs. The opener is the preparation's
// only associated continuation: there is no intermediate execution-plan type
// and no independently configured opener.
type openExecution func(context.Context, harness.OperationAdmission, selection) (harness.Execution, error)

// selection is one immutable preparation or execution input set: owned Agent
// data, the selected capability bindings currently supplied by the scopes
// resolved for that call, and the captured Invocation. A selected capability
// whose supplying scope is not open yet is absent from the preparation view
// and present in the execution view; neither selection is ever rebound.
type selection struct {
	agent      harness.AgentType
	bindings   Bindings
	invocation Invocation
}

// PreparationHook is one pure preparation capability: it may replace the
// captured system prompt and tool definitions, but never the captured model,
// revision, capability IDs, or permission capability constraints, and it may
// not add tool names outside the original capture. Hooks read their own
// settings through the Invocation, start no effects, and hold no resource: a
// hook provider must therefore be Runtime- or Workspace-scoped. Harness owns
// no hook fields and no hook dispatcher.
type PreparationHook interface {
	Prepare(context.Context, Invocation, harness.ExecutionCapture) (harness.ExecutionCapture, error)
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
	background  BackgroundServices
	prepare     prepare
}

// newPreparation wires the binder to the published configuration, the
// composition with its constructed Runtime scope and Workspace registry, the
// once-resolved home, the background services bridge armed after harness.New
// returns, and the controlled preparation function; nil selects the concrete
// production preparation.
func newPreparation(config *configurationService, c *composition, runtime *scope, workspaces *workspaceScopes, home string, background BackgroundServices, prepare prepare) *preparation {
	return &preparation{
		config:      config,
		composition: c,
		runtime:     runtime,
		workspaces:  workspaces,
		home:        home,
		background:  background,
		prepare:     prepare,
	}
}

// bind returns the exact two-argument callback type required by
// harness.Dependencies.Prepare. Selected unknown or unavailable Agent types
// and models report harness.ErrInvalid at admission, never a failed
// configuration publication; preparation errors and cancellation abort
// admission with no partial consequence published.
func (p *preparation) bind() func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
	return func(ctx context.Context, req harness.PreparationRequest) (harness.PreparedExecution, error) {
		snapshot := p.config.current()
		if snapshot == nil {
			return harness.PreparedExecution{}, fmt.Errorf("agent type %q has no published configuration: %w", req.Session.AgentType, harness.ErrInvalid)
		}
		agent, err := harness.ResolveAgentType(req.Session.AgentType, snapshot.agentTypes())
		if err != nil {
			return harness.PreparedExecution{}, err
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
		sel := selection{agent: agent, bindings: bindings, invocation: Invocation{snapshot: snapshot}}

		callCtx, release, err := workspaceScope.enter(ctx)
		if err != nil {
			return harness.PreparedExecution{}, err
		}
		defer release() // the one preparation guard covers the controlled call and every pure hook

		input := sel
		input.agent.Tools = slices.Clone(agent.Tools)
		input.agent.Capabilities = slices.Clone(agent.Capabilities)
		prepare := p.prepare
		if prepare == nil {
			prepare = p.concretePrepare
		}
		capture, opener, err := prepare(callCtx, req, input)
		if err != nil {
			return harness.PreparedExecution{}, err
		}
		if opener == nil {
			return harness.PreparedExecution{}, fmt.Errorf("preparation of agent %q returned no opener: %w", agent.Name, harness.ErrInvalid)
		}
		if err := validateCaptureSelection(capture, sel); err != nil {
			return harness.PreparedExecution{}, err
		}
		capture, err = runPreparationHooks(callCtx, sel, capture)
		if err != nil {
			return harness.PreparedExecution{}, err
		}
		return harness.PreparedExecution{
			Capture: capture,
			Open: func(openCtx context.Context, admission harness.OperationAdmission) (harness.Execution, error) {
				return p.open(openCtx, admission, workspace, workspaceScope, agent, snapshot, opener)
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

// open turns the committed admission into the execution: it opens the
// Operation then the Agent scope, enters the Agent-scope guard, creates the
// execution selection from the same snapshot, and hands that new value with
// the actual committed admission to this preparation's opener, so hooks'
// final prompt and tools can never be replaced by stale pre-hook values. The
// guard covers opening, every Harness-driven effect callback and terminal
// settlement until the returned Close releases it and closes the Agent then
// the Operation scope; opening failure releases the guard and unwinds owned
// scopes first.
func (p *preparation) open(ctx context.Context, admission harness.OperationAdmission, workspace string, workspaceScope *scope, agent harness.AgentType, snapshot *configuration, opener openExecution) (harness.Execution, error) {
	base := ScopeInfo{DataDir: p.runtime.info.DataDir, Workspace: workspace, SessionID: admission.SessionID, OperationID: admission.OperationID}
	operationInfo := base
	operationInfo.Kind = ScopeOperation
	operation, err := p.composition.openScope(ctx, operationInfo, []*scope{p.runtime, workspaceScope})
	if err != nil {
		return harness.Execution{}, err
	}
	agentInfo := base
	agentInfo.Kind = ScopeAgent
	agentScope, err := p.composition.openScope(ctx, agentInfo, []*scope{p.runtime, workspaceScope, operation})
	if err != nil {
		return harness.Execution{}, errors.Join(err, operation.close())
	}
	unwind := func(cause error, release func()) (harness.Execution, error) {
		if release != nil {
			release()
		}
		return harness.Execution{}, errors.Join(cause, agentScope.close(), operation.close())
	}
	callCtx, release, err := agentScope.enter(ctx)
	if err != nil {
		return unwind(err, nil)
	}
	// The concrete preparation's opener also binds the Agent's declared tool
	// exports — separately from the explicitly selected non-tool capability
	// IDs; controlled openers bind only what they select.
	selected := agent.Capabilities
	if p.prepare == nil {
		selected = slices.Concat(agent.Capabilities, agent.Tools)
	}
	bindings, err := selectCapabilities([]*scope{p.runtime, workspaceScope, operation, agentScope}, selected)
	if err != nil {
		return unwind(err, release)
	}
	hooks, err := bindToolArgumentHooks(agent.Capabilities, bindings, Invocation{snapshot: snapshot})
	if err != nil {
		return unwind(err, release)
	}
	execution, err := opener(callCtx, admission, selection{agent: agent, bindings: bindings, invocation: Invocation{snapshot: snapshot}})
	if err != nil {
		return unwind(err, release)
	}
	if execution.Close != nil {
		// The concrete execution cleanup joins the Agent scope's closer stack last,
		// so reverse disposal runs it before that scope's plugins.
		agentScope.closers = append(agentScope.closers, execution.Close)
	}
	return harness.Execution{
		Model:         execution.Model,
		CompactModel:  execution.CompactModel,
		Retry:         execution.Retry,
		Tool:          execution.Tool,
		Permissions:   execution.Permissions,
		NormalizeTool: execution.NormalizeTool,
		ToolHooks:     hooks,
		Close: func() error {
			release()
			return errors.Join(agentScope.close(), operation.close())
		},
	}, nil
}

// bindToolArgumentHooks binds, in the Agent's selected capability order and
// only after all execution scopes opened, every selected binding whose
// declared type implements ToolArgumentsHook — from any supplying scope. IDs
// are the capability IDs, and each hook receives the same captured Invocation.
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
			ID: id,
			Run: func(ctx context.Context, call model.ToolCall) (json.RawMessage, error) {
				return hook.BeforeTool(ctx, invocation, call)
			},
		})
	}
	return hooks, nil
}

// requireCatalogModel admits only a selected model that resolves in the
// captured catalog with a positive context window, and rejects a wholly
// empty selection with the retained "no model configured" diagnostic. A
// partial identity keeps the catalog path: it is not resolvable there either.
// Credentials and provider connection are not required for controlled
// effects; production readiness belongs to concrete preparation.
func requireCatalogModel(snapshot *configuration, ref model.ModelRef) error {
	if ref.IsZero() {
		return fmt.Errorf("no model configured: %w", harness.ErrInvalid)
	}
	_, entry, err := snapshot.catalog.Lookup(catalog.ModelRef{Provider: ref.Provider, Model: ref.Model})
	if err != nil {
		return fmt.Errorf("agent model %s is not available in the catalog: %w: %w", ref.String(), err, harness.ErrInvalid)
	}
	if entry.ContextWindow <= 0 {
		return fmt.Errorf("agent model %s has no positive context window: %w", ref.String(), harness.ErrInvalid)
	}
	return nil
}

// concretePrepare is the production preparation selected when no controlled
// prepare is supplied. It consumes only the captured snapshot's inputs — the
// catalog entry, the once-resolved credential, the effective permission
// policy, and the composed prompt/tool surface — and returns the durable
// capture plus the opener carrying the one constructed transport, the policy,
// and the prepared workspace. Later configuration reloads and environment
// changes never reach a prepared execution, and no transport input, secret,
// or warning is stored beyond the durable capture fields.
func (p *preparation) concretePrepare(_ context.Context, req harness.PreparationRequest, sel selection) (harness.ExecutionCapture, openExecution, error) {
	snapshot := sel.invocation.snapshot
	provider, entry, err := snapshot.catalog.Lookup(catalog.ModelRef{Provider: sel.agent.Model.Provider, Model: sel.agent.Model.Model})
	if err != nil {
		return harness.ExecutionCapture{}, nil, fmt.Errorf("agent model %s is not available in the catalog: %w: %w", sel.agent.Model.String(), err, harness.ErrInvalid)
	}
	apiKey := ""
	if env := provider.Transport.APIKeyEnv; env != "" {
		apiKey = os.Getenv(env)
		if apiKey == "" {
			return harness.ExecutionCapture{}, nil, fmt.Errorf("%w: %s (for provider %q): %w", config.ErrMissingEnvVar, env, provider.ID, harness.ErrInvalid)
		}
	}
	transport, err := p.concreteTransport(provider, entry, sel.agent.Model, apiKey)
	if err != nil {
		return harness.ExecutionCapture{}, nil, err
	}
	// The compact configuration rides the same one-revision snapshot: the
	// resolved compact Agent type carries the selection's model and prompt.
	// Its resolution failure is an admission failure — the compact builtin is
	// always present and no retained prompt source exists to fall back to.
	compactType, err := harness.ResolveAgentType("compact", snapshot.agentTypes())
	if err != nil {
		return harness.ExecutionCapture{}, nil, fmt.Errorf("compact agent type: %w", err)
	}
	// The effective compact model is one fallback chain ending at the
	// conversation model: the compact type's model — when it names a
	// different model at all — must resolve in the catalog, build a transport
	// over a resolved credential; otherwise the conversation model and its
	// transport are kept. The recorded compact configuration is always the
	// final effective model's.
	compactRef := sel.agent.Model
	compactWindow := entry.ContextWindow
	compactReserve := outputReserve(entry.MaxOutputTokens)
	compactTransport := transport
	if !compactType.Model.IsZero() && compactType.Model != sel.agent.Model {
		compactProvider, compactEntry, lookupErr := snapshot.catalog.Lookup(catalog.ModelRef{Provider: compactType.Model.Provider, Model: compactType.Model.Model})
		if lookupErr == nil {
			keyMissing := false
			compactKey := ""
			if env := compactProvider.Transport.APIKeyEnv; env != "" {
				compactKey = os.Getenv(env)
				keyMissing = compactKey == ""
			}
			if !keyMissing {
				built, buildErr := p.concreteTransport(compactProvider, compactEntry, compactType.Model, compactKey)
				if buildErr == nil {
					compactRef = compactType.Model
					compactWindow = compactEntry.ContextWindow
					if compactWindow <= 0 { // the retained window fallback: the conversation model's window
						compactWindow = entry.ContextWindow
					}
					compactReserve = outputReserve(compactEntry.MaxOutputTokens)
					compactTransport = built
				}
			}
		}
	}
	adaptation, err := p.boundAdaptation(sel)
	if err != nil {
		return harness.ExecutionCapture{}, nil, err
	}
	specs, err := p.toolSpecs(sel.agent.Tools)
	if err != nil {
		return harness.ExecutionCapture{}, nil, err
	}
	result, tools, err := composeSurface(surfaceRequest{
		home:         p.home,
		workspace:    req.Session.Identity.Workspace,
		sessionStart: req.Session.Identity.CreatedAt, // a Fork's zero start samples the compose time once
		promptSize:   sel.agent.SystemPrompt,
		promptBody:   sel.agent.Prompt,
		invocation:   sel.invocation,
		constraints:  ToolConstraints{Readonly: sel.agent.Readonly, WriteDir: sel.agent.WriteDir},
		identity:     req.Session.Identity,
		toolSpecs:    specs,
		adaptation:   adaptation,
		modelRef:     sel.agent.Model,
	})
	if err != nil {
		return harness.ExecutionCapture{}, nil, err
	}
	capture := harness.ExecutionCapture{
		ConfigurationRevision: sel.invocation.Revision(),
		Model:                 sel.agent.Model,
		ContextWindow:         entry.ContextWindow,
		OutputReserve:         outputReserve(entry.MaxOutputTokens),
		SystemPrompt:          result.Prompt,
		Tools:                 tools,
		Capabilities:          slices.Clone(sel.agent.Capabilities),
		Readonly:              sel.agent.Readonly,
		WriteDir:              sel.agent.WriteDir,
		Compact: harness.CompactCapture{
			Model:         compactRef,
			ContextWindow: compactWindow,
			OutputReserve: compactReserve,
			SystemPrompt:  compactType.Prompt,
		},
	}
	workspace := req.Session.Identity.Workspace
	policy := snapshot.permissionPolicy(workspace)
	return capture, p.concreteOpener(transport, compactTransport, policy, workspace, tools), nil
}

// outputReserve applies the retained reserve rule: the catalog entry's
// MaxOutputTokens when positive, else the fixed 131072 reserve.
func outputReserve(maxOutputTokens int) int {
	if maxOutputTokens > 0 {
		return maxOutputTokens
	}
	return 131072
}

// boundAdaptation binds the composition's single ModelAdaptation export from
// the selection when the Agent selected it; an unselected adaptation — or a
// composition declaring none, whose empty ID is never an entry — is nil.
func (p *preparation) boundAdaptation(sel selection) (ModelAdaptation, error) {
	id := p.composition.modelAdaptation
	if _, ok := sel.bindings.entries[id]; !ok {
		return nil, nil
	}
	adaptation, err := Bind[ModelAdaptation](sel.bindings, id)
	if err != nil {
		return nil, fmt.Errorf("bind model adaptation %q: %w", id, err)
	}
	return adaptation, nil
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
// wire-debug directory. Transport.Options stays catalog/config data feeding
// the discovery fingerprint and is never copied here; no timeout input or
// HTTP option exists.
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

// concreteOpener returns the production opener: it binds every advertised
// tool name from the execution-view bindings over all four scopes, owns one
// ToolContext carrying the prepare-time Workspace, the calling Operation's
// admitted-input identity, the captured Invocation and the validated hard
// constraints, and returns the execution whose model callbacks each make
// exactly one physical attempt — Model over the conversation transport and
// CompactModel over the effective compact transport, the same construction
// over the one shared transport when the compact model is the conversation
// model. The shared advertisement
// gate rejects unknown names before dispatch; retry classification, selected
// argument hooks, and scope disposal belong to the shared machinery.
func (p *preparation) concreteOpener(transport, compactTransport *model.Transport, policy harness.PermissionPolicy, workspace string, advertised []model.ToolDefinition) openExecution {
	return func(_ context.Context, admission harness.OperationAdmission, sel selection) (harness.Execution, error) {
		table := make(map[string]Tool, len(advertised))
		for _, definition := range advertised {
			tool, err := Bind[Tool](sel.bindings, definition.Name)
			if err != nil {
				return harness.Execution{}, err
			}
			table[definition.Name] = tool
		}
		tc := ToolContext{
			Workspace:     workspace,
			AdmittedEntry: admission.AdmittedEntry,
			Invocation:    sel.invocation,
			Constraints:   ToolConstraints{Readonly: sel.agent.Readonly, WriteDir: sel.agent.WriteDir},
			Background:    p.background,
		}
		return harness.Execution{
			Model: func(ctx context.Context, req model.Request) (model.Stream, error) {
				stream, _, err := transport.Stream(ctx, req, nil) // runtime extras are a later phase's channel
				return stream, err
			},
			CompactModel: func(ctx context.Context, req model.Request) (model.Stream, error) {
				stream, _, err := compactTransport.Stream(ctx, req, nil) // runtime extras are a later phase's channel
				return stream, err
			},
			// A nil Retry selects the standard classifier.
			Tool: func(ctx context.Context, call model.ToolCall) harness.PreparedTool {
				return table[call.Name].Prepare(ctx, tc, call)
			},
			Permissions: policy,
			NormalizeTool: func(call model.ToolCall) (json.RawMessage, error) {
				return table[call.Name].Normalize(tc, call)
			},
		}, nil
	}
}

// validateCaptureSelection checks the prepared capture's model and revision
// against the selected view, its capability names against the Agent's full
// selected names in the same order, and its permission capability members
// against the one Harness-selected Agent definition before any hook runs; nil
// and empty selections compare equal. Harness remains the final capture and
// admission validator.
func validateCaptureSelection(capture harness.ExecutionCapture, sel selection) error {
	if capture.Model != sel.agent.Model {
		return fmt.Errorf("capture model %s is not the selected model %s: %w", capture.Model.String(), sel.agent.Model.String(), harness.ErrInvalid)
	}
	if capture.ConfigurationRevision != sel.invocation.Revision() {
		return fmt.Errorf("capture revision %q is not the captured revision %q: %w", capture.ConfigurationRevision, sel.invocation.Revision(), harness.ErrInvalid)
	}
	if !slices.Equal(capture.Capabilities, sel.agent.Capabilities) {
		return fmt.Errorf("capture capabilities %q are not the selected capabilities %q: %w", capture.Capabilities, sel.agent.Capabilities, harness.ErrInvalid)
	}
	if capture.Readonly != sel.agent.Readonly {
		return fmt.Errorf("capture readonly %v is not the selected definition's %v: %w", capture.Readonly, sel.agent.Readonly, harness.ErrInvalid)
	}
	if capture.WriteDir != sel.agent.WriteDir {
		return fmt.Errorf("capture write dir %q is not the selected definition's %q: %w", capture.WriteDir, sel.agent.WriteDir, harness.ErrInvalid)
	}
	return nil
}

// runPreparationHooks binds and directly invokes, in selected order inside
// the same preparation guard, every available long-lived selected binding
// whose declared type satisfies PreparationHook, passing the captured
// Invocation unchanged and a fresh owned capture copy to each hook. An
// invalid result, error or observed cancellation aborts admission before the
// next hook runs, so no partial hook consequence is published; the retained
// chain owns its values and never aliases a hook's slice.
func runPreparationHooks(ctx context.Context, sel selection, prepared harness.ExecutionCapture) (harness.ExecutionCapture, error) {
	baseline, err := ownExecutionCapture(prepared)
	if err != nil {
		return harness.ExecutionCapture{}, err
	}
	capture := baseline
	for _, id := range sel.agent.Capabilities {
		entry, ok := sel.bindings.entries[id]
		if !ok || !entry.declared.Implements(preparationHookType) {
			continue
		}
		hook, err := Bind[PreparationHook](sel.bindings, id)
		if err != nil {
			return harness.ExecutionCapture{}, err
		}
		fresh, err := ownExecutionCapture(capture)
		if err != nil {
			return harness.ExecutionCapture{}, err
		}
		next, err := hook.Prepare(ctx, sel.invocation, fresh)
		if err != nil {
			return harness.ExecutionCapture{}, err
		}
		if err := ctx.Err(); err != nil {
			return harness.ExecutionCapture{}, err
		}
		owned, err := ownExecutionCapture(next)
		if err != nil {
			return harness.ExecutionCapture{}, err
		}
		if err := validateHookedCapture(baseline, owned); err != nil {
			return harness.ExecutionCapture{}, err
		}
		capture = owned
	}
	return capture, nil
}

// ownExecutionCapture validates and deep-copies one capture: tool definitions
// pass the request constructor's shape and unique-name rule as an owned
// copy, and capabilities are cloned (nil stays nil).
func ownExecutionCapture(capture harness.ExecutionCapture) (harness.ExecutionCapture, error) {
	owned := capture
	if capture.Tools != nil {
		req, err := model.NewRequest(model.Request{Tools: capture.Tools})
		if err != nil {
			return harness.ExecutionCapture{}, fmt.Errorf("capture tools are invalid: %w: %w", err, harness.ErrInvalid)
		}
		owned.Tools = req.Tools
	}
	owned.Capabilities = slices.Clone(capture.Capabilities)
	return owned, nil
}

// validateHookedCapture revalidates, after each hook and before the next,
// that only the system prompt and tool definitions changed: the identity and
// the permission capability members stay intact and no tool name outside the
// original capture appears.
func validateHookedCapture(base, next harness.ExecutionCapture) error {
	if next.Model != base.Model {
		return fmt.Errorf("preparation hook changed the captured model: %w", harness.ErrInvalid)
	}
	if next.ContextWindow != base.ContextWindow {
		return fmt.Errorf("preparation hook changed the captured context window: %w", harness.ErrInvalid)
	}
	if next.OutputReserve != base.OutputReserve {
		return fmt.Errorf("preparation hook changed the captured output reserve: %w", harness.ErrInvalid)
	}
	if next.Compact != base.Compact {
		return fmt.Errorf("preparation hook changed the captured compact configuration: %w", harness.ErrInvalid)
	}
	if next.ConfigurationRevision != base.ConfigurationRevision {
		return fmt.Errorf("preparation hook changed the captured revision: %w", harness.ErrInvalid)
	}
	if !slices.Equal(next.Capabilities, base.Capabilities) {
		return fmt.Errorf("preparation hook changed the captured capability IDs: %w", harness.ErrInvalid)
	}
	if next.Readonly != base.Readonly {
		return fmt.Errorf("preparation hook changed the captured readonly constraint: %w", harness.ErrInvalid)
	}
	if next.WriteDir != base.WriteDir {
		return fmt.Errorf("preparation hook changed the captured write dir constraint: %w", harness.ErrInvalid)
	}
	original := make(map[string]bool, len(base.Tools))
	for _, tool := range base.Tools {
		original[tool.Name] = true
	}
	for _, tool := range next.Tools {
		if !original[tool.Name] {
			return fmt.Errorf("preparation hook added tool %q outside the original capture: %w", tool.Name, harness.ErrInvalid)
		}
	}
	return nil
}
