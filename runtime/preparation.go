package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/model"
)

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
// revision or capability IDs, and it may not add tool names outside the
// original capture. Hooks read their own settings through the Invocation,
// start no effects, and hold no resource: a hook provider must therefore be
// Runtime- or Workspace-scoped. Harness owns no hook fields and no hook
// dispatcher.
type PreparationHook interface {
	Prepare(context.Context, Invocation, harness.ExecutionCapture) (harness.ExecutionCapture, error)
}

// preparationHookType is the declared-type test one selected capability must
// satisfy to be bound and invoked as a preparation hook.
var preparationHookType = reflect.TypeFor[PreparationHook]()

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
	prepare     prepare
}

// newPreparation wires the binder to the published configuration, the
// composition with its constructed Runtime scope and Workspace registry, and
// the controlled preparation function.
func newPreparation(config *configurationService, c *composition, runtime *scope, workspaces *workspaceScopes, prepare prepare) *preparation {
	return &preparation{
		config:      config,
		composition: c,
		runtime:     runtime,
		workspaces:  workspaces,
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
		capture, opener, err := p.prepare(callCtx, req, input)
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
	bindings, err := selectCapabilities([]*scope{p.runtime, workspaceScope, operation, agentScope}, agent.Capabilities)
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
		Model: execution.Model,
		Tool:  execution.Tool,
		Close: func() error {
			release()
			return errors.Join(agentScope.close(), operation.close())
		},
	}, nil
}

// requireCatalogModel admits only a selected model that resolves in the
// captured catalog with a positive context window. Credentials and provider
// connection are not required for controlled effects; production readiness
// belongs to concrete preparation.
func requireCatalogModel(snapshot *configuration, ref model.ModelRef) error {
	_, entry, err := snapshot.catalog.Lookup(catalog.ModelRef{Provider: ref.Provider, Model: ref.Model})
	if err != nil {
		return fmt.Errorf("agent model %s is not available in the catalog: %w: %w", ref.String(), err, harness.ErrInvalid)
	}
	if entry.ContextWindow <= 0 {
		return fmt.Errorf("agent model %s has no positive context window: %w", ref.String(), harness.ErrInvalid)
	}
	return nil
}

// validateCaptureSelection checks the prepared capture's model and revision
// against the selected view and its capability names against the Agent's full
// selected names in the same order; nil and empty selections compare equal.
// Harness remains the final capture and admission validator.
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
// that only the system prompt and tool definitions changed: the identity
// stays intact and no tool name outside the original capture appears.
func validateHookedCapture(base, next harness.ExecutionCapture) error {
	if next.Model != base.Model {
		return fmt.Errorf("preparation hook changed the captured model: %w", harness.ErrInvalid)
	}
	if next.ConfigurationRevision != base.ConfigurationRevision {
		return fmt.Errorf("preparation hook changed the captured revision: %w", harness.ErrInvalid)
	}
	if !slices.Equal(next.Capabilities, base.Capabilities) {
		return fmt.Errorf("preparation hook changed the captured capability IDs: %w", harness.ErrInvalid)
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
