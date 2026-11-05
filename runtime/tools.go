package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sync"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
)

// toolType is the declared-type identity of the public Tool contract: a
// Provides declaration whose type is exactly this interface is a tool
// declaration, its ID is the model-visible tool name, and its description
// function is mandatory.
var toolType = reflect.TypeFor[Tool]()

// ToolConstraints carries the selected Agent's hard capability constraints:
// only the readonly/write-dir boundary. Description receives the resolved
// Agent definition's values and execution the values from its validated
// capture; tools supply neither and cannot loosen them.
type ToolConstraints struct {
	Readonly bool
	WriteDir string
}

// ToolDescription is one static tool description: the model-visible
// definition and whether the tool is available under the hard constraints
// (never a permission filter).
type ToolDescription struct {
	Definition model.ToolDefinition
	Available  bool
}

// ChildRequest has no parent field: the parent is the calling Session.
type ChildRequest struct {
	AgentType     string
	Content       []model.ContentPart
	OperationID   string
	MaxConcurrent int
	OutputLimit   int
}

// BackgroundServices is the background-work surface one prepared call carries:
// child launch, job start, and completion delivery over the Runtime's public
// Harness methods. The Runtime's implementation is the bound object the
// production opener creates per tool call; the zero ToolContext carries none.
type BackgroundServices interface {
	LaunchChild(req ChildRequest) (childSessionID string, err error)
	StartJob(jobID string, spawn func(ctx context.Context, deliver func(content string) error) error) error
}

// backgroundBridge is the one-field holder of the Runtime's Harness. It is
// created before harness.New, threaded into the preparation, and armed with
// the constructed Harness before the owner is published. No synchronization:
// harness.New performs no I/O and the first Prepare requires the admission
// gate, so no caller can observe the unarmed bridge.
type backgroundBridge struct {
	h *harness.Harness
}

// boundBackground is one tool call's background authority. The Runtime owns
// the calling Session, the plan's permission pairs frozen when Prepare
// returns, the derived context, expiry and join; a plugin receives no
// authority outside the plan's Execute.
type boundBackground struct {
	bridge    *backgroundBridge
	sessionID string

	mu          sync.Mutex
	permissions []harness.PermissionRequest
	ctx         context.Context
	cancel      context.CancelFunc
	closed      bool
	entries     sync.WaitGroup
}

// enter admits one call only while the object is armed, not closed, and its
// derived context is live. The entry is counted under the same mutex close
// uses to refuse new entries, so a Wait never races an uncounted Add.
func (b *boundBackground) enter() (context.Context, func(), error) {
	b.mu.Lock()
	if b.closed || b.ctx == nil || b.ctx.Err() != nil {
		b.mu.Unlock()
		return nil, nil, fmt.Errorf("background services: %w", ErrClosed)
	}
	b.entries.Add(1)
	ctx := b.ctx
	b.mu.Unlock()
	return ctx, b.entries.Done, nil
}

// arm derives this call's context from the Execute context. A fresh unarmed
// object is armed exactly once by the Runtime's Execute wrapper.
func (b *boundBackground) arm(ctx context.Context) {
	derived, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	b.ctx, b.cancel = derived, cancel
	b.mu.Unlock()
}

// close cancels the derived context before joining entered calls, so an
// entered call waiting on that context cannot block its own join, and refuses
// every later entry.
func (b *boundBackground) close() {
	b.mu.Lock()
	b.closed = true
	cancel := b.cancel
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	b.entries.Wait()
}

// permits reports whether the frozen pairs carry the requested child-launch
// permission.
func (b *boundBackground) permits(target string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Contains(b.permissions, harness.PermissionRequest{Permission: harness.PermissionChildLaunch, Target: target})
}

// LaunchChild launches one child Session under the bound Session and the
// frozen permission pairs. A target the plan did not declare is refused with
// the same ErrClosed failure as every other closed-authority call.
func (b *boundBackground) LaunchChild(req ChildRequest) (string, error) {
	ctx, done, err := b.enter()
	if err != nil {
		return "", err
	}
	defer done()
	if !b.permits(req.AgentType) {
		return "", fmt.Errorf("background services: child target %q: %w", req.AgentType, ErrClosed)
	}
	result, err := b.bridge.h.LaunchChildSession(ctx, harness.LaunchChildRequest{
		ParentSessionID: b.sessionID,
		AgentType:       req.AgentType,
		Content:         req.Content,
		OperationID:     req.OperationID,
		MaxConcurrent:   req.MaxConcurrent,
		OutputLimit:     req.OutputLimit,
	})
	if err != nil {
		return "", err
	}
	return result.ChildSessionID, nil
}

// StartJob starts one background job under the bound Session. The Harness
// stays the validation owner; a nil spawn is forwarded as nil. A successful
// start's deliver closure outlives the tool call: delivery is the job's
// completion obligation and is neither gated nor joined.
func (b *boundBackground) StartJob(jobID string, spawn func(ctx context.Context, deliver func(content string) error) error) error {
	ctx, done, err := b.enter()
	if err != nil {
		return err
	}
	defer done()
	var forwarded func(context.Context, string) error
	if spawn != nil {
		forwarded = func(spawnCtx context.Context, completionID string) error {
			return spawn(spawnCtx, func(content string) error {
				return b.bridge.h.DeliverBackgroundCompletion(context.Background(), b.sessionID, completionID, content)
			})
		}
	}
	return b.bridge.h.StartJob(ctx, b.sessionID, jobID, forwarded)
}

// bindPlan freezes one prepared plan's permissions and binds this call's
// background authority to its executor. The frozen clone replaces the plan's
// pairs, so the Harness evaluates exactly the pairs the object enforces and
// the producer holds no reference to them. A plan without an executor is
// returned unchanged and the object never arms.
func (b *boundBackground) bindPlan(plan harness.PreparedTool) harness.PreparedTool {
	permissions := slices.Clone(plan.Permissions)
	plan.Permissions = permissions
	b.mu.Lock()
	b.permissions = permissions
	b.mu.Unlock()
	if plan.Execute == nil {
		return plan
	}
	execute := plan.Execute
	plan.Execute = func(ctx context.Context) harness.ToolOutcome {
		b.arm(ctx)
		defer b.close()
		outcome := execute(ctx)
		outcome.Metadata = bytes.Clone(outcome.Metadata)
		return outcome
	}
	return plan
}

// ToolContext carries what one tool call needs beyond the call itself: the
// Workspace, the calling Operation's admitted-input identity, the captured
// Invocation, the selected Agent's hard constraints, this call's bound
// background authority, and the call-time subprocess environment producer. It
// exposes neither the complete admission register nor file/command-specific
// numeric fields.
type ToolContext struct {
	Workspace     string
	AdmittedEntry harness.EntryRef
	Invocation    Invocation
	Constraints   ToolConstraints
	Background    BackgroundServices

	// SubprocessEnv is invoked at each concrete process start — foreground
	// Execute and the admitted Jobs start handoff alike — and returns the
	// exact environment slice that process runs with: the live ambient
	// environment with the Runtime's live credential exclusion applied (the
	// current managed-key set and provider credential names, together with
	// the preparation-time captured names). It is that live exclusion
	// service only, never a promise to reread the selected execution policy:
	// the model, prompt, tools, permissions and configuration revision stay
	// the admission's frozen capture. A nil callback fails the cooperative
	// command; there is no ambient fallback.
	SubprocessEnv func() []string
}

// Tool is one concrete tool capability: pure terminating argument
// normalization plus preparation of one authorized call. Both members
// receive the same ToolContext; normalization supplies the immutable call
// facts preparation consumes.
type Tool interface {
	Normalize(ToolContext, model.ToolCall) (json.RawMessage, error)
	Prepare(context.Context, ToolContext, model.ToolCall) harness.PreparedTool
}

// ToolSpec is the Provides declaration for the public Tool contract: it
// records the tool ID, the Tool interface type, and the pure description
// function in one CapabilitySpec. The recorded description validates and
// copies the returned definition and enforces the ID/name match when invoked;
// no execution instance is constructed to obtain the declaration's metadata.
// A nil description function records a declaration lacking one, which
// composition rejects.
func ToolSpec(id string, describe func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error)) CapabilitySpec {
	if describe == nil {
		return CapabilitySpec{id: id, typ: toolType}
	}
	return CapabilitySpec{
		id:  id,
		typ: toolType,
		describe: func(invocation Invocation, constraints ToolConstraints, identity harness.SessionIdentity) (ToolDescription, error) {
			description, err := describe(invocation, constraints, identity)
			if err != nil {
				return ToolDescription{}, err
			}
			definition, err := model.NewToolDefinition(description.Definition)
			if err != nil {
				return ToolDescription{}, fmt.Errorf("tool %q: %w", id, err)
			}
			if definition.Name != id {
				return ToolDescription{}, fmt.Errorf("tool %q: description declares name %q", id, definition.Name)
			}
			description.Definition = definition
			return description, nil
		},
	}
}
