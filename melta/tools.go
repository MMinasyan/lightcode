package melta

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/model"
)

// ToolDescription is one static tool description: the model-visible
// definition and whether the tool is available under the hard constraints
// (never a permission filter).
type ToolDescription struct {
	Definition model.ToolDefinition
	Available  bool
}

// BackgroundServices is the baseline background-work surface one standard tool
// call carries: job start and completion delivery over the Melta's public
// Harness methods. Extension child launch is a declared native host service,
// never a Go callback on this interface.
type BackgroundServices interface {
	StartJob(jobID string, spawn func(ctx context.Context, deliver func(content string) error) error) error
}

// backgroundBridge is the one-field holder of the Melta's Harness. It is
// created before harness.New, threaded into the preparation, and armed with
// the constructed Harness before the owner is published. No synchronization:
// harness.New performs no I/O and the first Prepare requires the admission
// gate, so no Caller can observe the unarmed bridge.
type backgroundBridge struct {
	h *harness.Harness
}

// boundBackground is one tool call's background authority. The Melta owns
// the calling Session, the plan's permission pairs frozen when Prepare
// returns, the derived context, expiry and join; a plugin receives no
// authority outside the plan's Execute.
type boundBackground struct {
	bridge    *backgroundBridge
	sessionID string

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	closed  bool
	entries sync.WaitGroup
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

// arm derives this call's context from the Execute context and returns it:
// the Melta passes that same derived context into the original Execute, so
// the tool and background admission observe one origin. A fresh unarmed object
// is armed exactly once by the Melta's Execute wrapper.
func (b *boundBackground) arm(ctx context.Context) context.Context {
	derived, cancel := context.WithCancel(ctx)
	b.mu.Lock()
	b.ctx, b.cancel = derived, cancel
	b.mu.Unlock()
	return derived
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

// bindPlan binds this standard tool call's background authority to its
// executor. A plan without an executor is returned unchanged and the object
// never arms. Extension tools bind their own authorized call context in the
// native adapter and never pass through this baseline path.
func (b *boundBackground) bindPlan(plan harness.PreparedTool) harness.PreparedTool {
	if plan.Execute == nil {
		return plan
	}
	execute := plan.Execute
	plan.Execute = func(ctx context.Context) harness.ToolOutcome {
		ctx = b.arm(ctx)
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
	Session       harness.SessionIdentity
	Model         model.ModelRef
	Workspace     string
	OperationID   string
	InvocationID  string
	Purpose       harness.InvocationPurpose
	AdmittedEntry harness.EntryRef
	Invocation    Invocation
	Constraints   harness.ToolConstraints
	Background    BackgroundServices

	// SubprocessEnv is invoked at each concrete process start — foreground
	// Execute and the admitted Jobs start handoff alike — and returns the
	// exact environment slice that process runs with: the live ambient
	// environment with the Melta's live credential exclusion applied (the
	// current revision's provider names, together with the preparation-time
	// captured names). It is that live exclusion service only, never a
	// promise to reread the selected execution policy: the model, prompt,
	// tools, permissions and configuration revision stay the admission's
	// frozen capture. A nil callback or an error fails the cooperative
	// command before any process starts; there is no ambient fallback.
	SubprocessEnv func(context.Context) ([]string, error)
}

// Tool is one concrete tool capability: pure terminating argument
// normalization plus preparation of one authorized call. Both members
// receive the same ToolContext; normalization supplies the immutable call
// facts preparation consumes.
type Tool interface {
	Normalize(context.Context, ToolContext, model.ToolCall) (json.RawMessage, error)
	Prepare(context.Context, ToolContext, model.ToolCall) harness.PreparedTool
}
