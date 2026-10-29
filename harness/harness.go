package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/MMinasyan/lightcode/model"
)

// JobStopper is the job-stop seam: the Harness cannot import plugin packages,
// so the interface is declared at its consumer. Nil is legal; StartJob rejects
// job admission without one (managed shutdown stops every Job).
type JobStopper interface {
	StopJob(sessionID, jobID string)
}

// Dependencies are the construction inputs of one Harness: the durable
// Storage, the single preparation callback shared by every admission
// producer, and the optional passive observer of committed invalidation and
// transient model/tool progress. The observer is synchronous, expected
// nonblocking, and grants no authority: a nil observer is a no-op and a
// failing observer never changes a transition or a settlement.
type Dependencies struct {
	Storage Storage
	Prepare func(context.Context, PreparationRequest) (PreparedExecution, error)
	Jobs    JobStopper
	Observe func(HarnessFact)
}

// PreparationSession is the revision-free owned Session view one preparation
// receives.
type PreparationSession struct {
	Identity  SessionIdentity
	AgentType string
}

// PreparationRequest is the input of one preparation call.
type PreparationRequest struct {
	Session     PreparationSession
	RequestKind RequestKind
}

// PreparedExecution is the outcome of one preparation: the durable capture,
// the one required opener that turns the committed admission into the
// execution the Harness runs, and the optional release of the reservation the
// successful preparation transferred to its caller. A preparation returns
// values, an opener and one reservation, never short-lived resources: failed
// admission discards the values and the Harness releases the reservation it
// was handed. Nil Release means the preparation reserved nothing. No returned
// function is durable.
type PreparedExecution struct {
	Capture ExecutionCapture
	Open    func(context.Context, OperationAdmission) (Execution, error)
	// Release disposes the reserved resources one successful Prepare
	// transferred to its caller. The Harness runs it exactly once: on every
	// path that discards the prepared execution, and after the opened
	// execution's Close on the path that adopts it. A release failure is a
	// cleanup failure retained through Wait; it never rewrites a committed
	// terminal.
	Release func() error
}

// HookMode declares one hook's participation contract at its boundary. A pure
// hook validates and coalesces its changed value in memory with no effect
// transaction; an effectful hook's execution is its own Harness-settled
// Operation effect with durable hook_result evidence.
type HookMode string

const (
	HookModePure      HookMode = "pure"
	HookModeEffectful HookMode = "effectful"
)

func validHookMode(mode HookMode) bool {
	return mode == HookModePure || mode == HookModeEffectful
}

// ExecutionEvidence describes one effect execution's knowledge independently
// of its status: not_started is proven unstarted work, observed is a valid
// terminal execution response (including cooperative interruption), and
// unknown is possible execution without a valid terminal response.
type ExecutionEvidence string

const (
	ExecutionNotStarted ExecutionEvidence = "not_started"
	ExecutionObserved   ExecutionEvidence = "observed"
	ExecutionUnknown    ExecutionEvidence = "unknown"
)

func validExecutionEvidence(evidence ExecutionEvidence) bool {
	switch evidence {
	case ExecutionNotStarted, ExecutionObserved, ExecutionUnknown:
		return true
	}
	return false
}

// EffectError carries one hook's declared execution knowledge through a
// binding-infrastructure failure to the Harness: the declared Evidence reaches
// the shared accepting validator unchanged. Unwrap exposes the underlying
// cause for classification.
type EffectError struct {
	Evidence ExecutionEvidence
	Err      error
}

func (e *EffectError) Error() string { return e.Err.Error() }
func (e *EffectError) Unwrap() error { return e.Err }

// ToolArgumentsHook is one argument hook: before a concrete tool call
// prepares, it may rewrite the call's raw argument bytes. Its successful
// replacement must be exactly one JSON object the execution's normalizer
// accepts; it can never replace the call identity, tool name, or another
// call's arguments. An effectful hook's execution is a Harness-settled
// Operation effect with durable hook_result evidence; a pure hook coalesces
// in memory. Run receives owned call data.
type ToolArgumentsHook struct {
	ID          string
	ExtensionID string
	Mode        HookMode
	Run         func(context.Context, model.ToolCall) (json.RawMessage, error)
}

// RequestHookInput is one request hook's owned input: the bound request
// identity, the frozen canonical conversation messages and canonical
// references they were projected from, the captured rendered prompt, and the
// captured eligible tool catalog. The CanonicalMessages are the round-frozen
// projection without any request contribution, CapturedPrompt the captured
// baseline the current Prompt evolved from, and Catalog the full eligible
// catalog behind whatever Tools are currently visible. Every mutable member is
// a deep-owned copy; the hook cannot reach the coordinator's state.
type RequestHookInput struct {
	RequestID         string
	CanonicalMessages []model.Message        // the round-frozen canonical view, contributions excluded
	Messages          []model.Message        // the effective conversation view accepted so far
	CheckpointEntryID string                 // the named current checkpoint, empty when uncompacted
	BoundaryEntryID   string                 // the canonical boundary of the consumed projection
	CapturedPrompt    string                 // the captured baseline prompt text
	Prompt            string                 // the effective prompt text
	Catalog           []model.ToolDefinition // the full captured eligible catalog
	Tools             []model.ToolDefinition // the currently visible tool view
}

// RequestConsequence is one request hook's closed contribution: absent members
// mean unchanged. A present Prompt replaces the complete prompt text (a
// present-empty Prompt removes the prompt); Messages are complete request-only
// messages appended to the conversation view; a present ToolNames replaces the
// visible tool view with those names selected from the captured catalog
// (present-empty advertises none).
type RequestConsequence struct {
	Prompt    *string
	Messages  []model.Message
	ToolNames []string
}

// RequestHook is one selected request-boundary hook: it runs after steering
// and before any model effect, receives the current effective request view,
// and contributes an optional complete prompt replacement, request-only
// messages, and tool visibility. Run receives deep-owned values.
type RequestHook struct {
	ID          string
	ExtensionID string
	Mode        HookMode
	Run         func(context.Context, RequestHookInput) (RequestConsequence, error)
}

// ToolOutputInput is one output hook's owned input: the settled tool outcome
// and the current model-facing content text. Outcome is an owned copy for
// every hook invocation — the hook can never reach the stored value — and
// Content is the producer's original content for the first hook and each
// preceding hook's return for every later one.
type ToolOutputInput struct {
	Outcome ToolOutcome
	Content string
}

// ToolOutputHook is one selected after-tool output hook: pure model-facing
// output shaping over one settled tool outcome. Run receives owned values and
// returns the shaped content text; a hook error or a return that fails the
// unchanged result-shape rule aborts the chain and the call settles with its
// original content. Shaping is text only: it can never change the settled
// status, evidence, usage or metadata.
type ToolOutputHook struct {
	ID          string
	ExtensionID string
	Run         func(context.Context, ToolOutputInput) (string, error)
}

// RetryPolicy decides one failed physical model attempt: it receives the
// attempt's failure and the 1-based number of the attempt that just failed
// (1 for the initial attempt), and returns the nonnegative delay before the
// next attempt plus whether to retry at all. A false result or a negative
// delay ends the effect with that failure; the Harness owns every wait.
type RetryPolicy func(error, int) (time.Duration, bool)

// InvocationPurpose names which Engine call one opened invocation serves: the
// conversation Agent run or one maintenance model request.
type InvocationPurpose string

const (
	InvocationPurposeConversation InvocationPurpose = "conversation"
	InvocationPurposeMaintenance  InvocationPurpose = "maintenance"
)

// InvocationRequest is the Harness-issued identity of one Engine call: a
// fresh per-call identity, the purpose it serves, and the model that call
// runs on. The opener is the request's owner.
type InvocationRequest struct {
	ID      string
	Purpose InvocationPurpose
	Model   model.ModelRef
}

// InvocationExecution is one opened Engine invocation: the physical
// model-request callback the Harness retries and assembles, the tool effect
// function, the required pure argument-normalization callback, the selected
// argument hooks, the selected output-shaping hooks, plus the cleanup of the
// external resources backing them — the existing callback contracts bound to
// that invocation. A successful Model, Tool and NormalizeTool must be
// non-nil, and every ToolHooks and ToolOutputHooks entry requires a
// non-empty unique ID and a non-nil Run. Close is optional; the opened
// invocation's resources are released exactly once after its effects settle
// and their result is consumed, before the owning execution closes.
type InvocationExecution struct {
	// Model is the one physical model-request callback. Each invocation of
	// it makes exactly one physical attempt and returns its accepted stream
	// or that attempt's failure; retry, assembly and settlement are
	// Harness-owned.
	Model func(context.Context, model.Request) (model.Stream, error)
	Tool  func(context.Context, model.ToolCall) PreparedTool
	// NormalizeTool is pure, terminating argument validation and defaulting
	// with no filesystem, service, permission or effect I/O. It receives an
	// owned call and returns one owned JSON object preserving unrelated
	// accepted fields; its error is a per-call validation failure.
	NormalizeTool func(model.ToolCall) (json.RawMessage, error)
	// ToolHooks are the selected argument hooks in configured order. Each
	// runs after the advertisement gate and before concrete preparation; the
	// first receives the original raw call arguments, every later one the
	// preceding committed value.
	ToolHooks []ToolArgumentsHook
	// ToolOutputHooks are the selected output-shaping hooks in configured
	// order. They run over one settled tool outcome before its result
	// commits: the first receives the producer's original content, every
	// later one the preceding hook's return. They are pure and can never
	// change the settled status, evidence, usage or metadata.
	ToolOutputHooks []ToolOutputHook
	// RequestHooks are the selected request-boundary hooks in configured
	// order. They run inside the Operation's request source, after steering
	// and before the automatic-maintenance orchestration and any model
	// effect.
	RequestHooks []RequestHook
	Close        func() error
}

// Execution is one opened execution: the required invocation opener the
// Harness drives once per Engine call, the Operation's retry classifier and
// permission evaluator, plus the cleanup of the Operation resources backing
// them. OpenInvocation must be non-nil; it opens the conversation run's
// invocation and a distinct maintenance invocation per strategy model
// request, each with its own opened resources and Close. A nil Retry selects
// the private standard retry classifier; a nil Permissions evaluator selects
// the built-in policy; Close is optional when the concrete execution needs no
// external disposal. The opener owns provisional cleanup on error.
type Execution struct {
	// OpenInvocation opens one Engine invocation's callback contracts over
	// the Harness-issued request. Successful opening transfers the invoked
	// resources to the Harness, which closes the invocation once after its
	// effects settle, before the owning Execution.Close.
	OpenInvocation func(context.Context, InvocationRequest) (InvocationExecution, error)
	// Retry classifies one failed physical attempt for the next-attempt
	// decision. Nil selects the private standard classifier.
	Retry RetryPolicy
	// Permissions is the permission evaluator deciding this Operation's
	// prepared calls, resolved from the same immutable configuration
	// revision its capture records. Nil selects the included built-in
	// policy (the zero PermissionPolicy).
	Permissions PermissionEvaluator
	// Strategy is the selected context strategy binding of this Operation,
	// resolved from the same immutable configuration revision its capture
	// records. Nil selects the included summary policy binding.
	Strategy *StrategyBinding
	Close    func() error
}

// ToolOutcome is one owned tool plan's complete outcome: the model-visible
// result returned to the Agent, plus optional raw-JSON metadata whose
// semantics belong to the producing tool. The Harness validates only
// well-formedness and the durable size bound and never interprets the value.
type ToolOutcome struct {
	Result   model.ToolResult
	Metadata json.RawMessage // optional, bounded, well-formed; semantics owned by the tool
}

// PreparedTool is one immutable prepared call: exactly one immediate outcome
// or executor, its declared permission/target pairs, and the canonical
// bindings permission evaluation and concrete I/O consume. Normalized
// arguments are Harness-supplied input, never a second value the preparer
// replaces. Execution and immediate-success plans require a nonempty list of
// nonempty permission/target pairs; immediate error, denied and interrupted
// outcomes need no successful target declaration.
type PreparedTool struct {
	Permissions        []PermissionRequest
	CanonicalWorkspace string
	CanonicalWriteDir  string
	Immediate          *ToolOutcome
	Execute            func(context.Context) ToolOutcome
}

// CreateSessionRequest is the input of one root Session creation.
type CreateSessionRequest struct {
	Workspace string
	AgentType string
}

// SubmitRequest is the input of one public Session message submission.
type SubmitRequest struct {
	SessionID   string
	OperationID string
	Origin      InputOrigin
	Content     []model.ContentPart
	Mode        MessageMode
}

// SubmitResult is the outcome of one public Submit. Operation is present for
// admitted/existing only; a buffered item is not a durable Operation yet.
type SubmitResult struct {
	Disposition SubmitDisposition
	Operation   *OperationRecord
}

// CompactRequest is the input of one public manual compaction: the
// caller-generated stable Operation identity supplies admission and
// idempotency.
type CompactRequest struct {
	SessionID   string
	OperationID string
}

// SweepPolicy is the explicit-time lifecycle thresholds of one sweep call: an
// open Session is archived when now-last_activity exceeds ArchiveAfter, and an
// archived Session is deleted when now-archived_at exceeds DeleteAfterArchive.
// A nonpositive threshold disables only its corresponding transition.
type SweepPolicy struct {
	ArchiveAfter       time.Duration
	DeleteAfterArchive time.Duration
}

// Harness is the public durable state machine over one Storage. It owns
// Session materialization, reads, root creation, Agent-type change, the shared
// admission producer, public Submit with its two process-local buffers, the
// Session lifecycle (reopen, archive, delete, and explicit-time sweep), and
// execution; it owns neither storage close nor Runtime shutdown policy.
type Harness struct {
	ctx  context.Context
	deps Dependencies

	mu       sync.Mutex
	sessions map[string]*coordinator

	// storageFailure retains the first storage-class failure that stopped
	// admitted work; Wait returns it after all process-local work has ended.
	storageFailure error

	// cleanupFailure retains the first execution-cleanup failure alongside
	// the first storage failure; Wait joins both after complete convergence.
	cleanupFailure error
}

// pendingMessage is one buffered Submit: a process-local FIFO item that is
// neither a durable Operation nor a Session entry while it waits, carries no
// idempotency, and is discarded on Harness-context loss. A delivery's
// selected head stays buffered until its one attempt resolves.
type pendingMessage struct {
	operationID string
	origin      InputOrigin
	content     []model.ContentPart
}

// activeExecution is the one in-flight execution of a coordinator: installed
// after the admission commit and released only after the terminal settlement
// and the post-terminal buffer drain have converged. Its execution context is
// a child of the Harness context; Interrupt cancels it and the retirement
// tail cancels it once after the drain.
type activeExecution struct {
	done    chan struct{}      // closed when the execution goroutine finishes
	execCtx context.Context    // the execution context driving the opener, the Agent run, and every effect attempt
	cancel  context.CancelFunc // cancels execCtx; idempotent, so a retiring predecessor's cancel is a harmless no-op
}

// coordinator is the one per-Session authority: the retained identity, the
// validated Session graph while one is resident, a sticky corruption marker,
// the single admission/fork reservation, the two process-local message FIFOs,
// and the optional active execution. The cell itself is stable for its
// Session's lifetime; the graph is resident only while a use holds it, and
// every remaining local field survives every graph release.
type coordinator struct {
	// sessionID is the durable identity, fixed when the cell is installed:
	// release cleanup and lookup never need the graph.
	sessionID string

	// The following fields belong to the Harness registry mutex.
	uses       int   // current uses; the graph may be cleared only at zero
	everLoaded bool  // a graph was validated for this cell at least once
	corru      error // sticky semantic corruption latch

	mu       sync.Mutex
	graph    *sessionGraph
	identity SessionIdentity
	// durableRevision is the last resident graph's Session register revision,
	// captured when that graph is released. An observation of a graphless
	// established cell answers with it; while a graph is resident the graph's
	// own current revision is the value.
	durableRevision int64

	reserved chan struct{} // non-nil while one reservation is held; closed on release

	steering []*pendingMessage // regular input waiting for the next model boundary
	queued   []*pendingMessage // queued input waiting for the next Agent turn end
	run      *activeExecution  // non-nil while one execution is in flight

	// pendingFacts holds the active execution's accumulated uncommitted
	// maintenance facts — incurred by procedures that settled at none —
	// until the next successful checkpoint or terminal publication accounts
	// them exactly once. Process-local only: never durable, never a
	// crash-survival guarantee.
	pendingFacts []maintenanceFact

	group             *backgroundGroup // nil until the first member
	bgState           bgState          // starts as bgOpen at every construction site; the zero value is never observed
	stop              *stopInterval
	pendingCompletion *launchInfo
	interruptOp       string

	gone bool // set by the post-commit deletion invalidation: every holder of the coordinator gets ErrNotFound from then on

	// localRev counts coordinator-local publications that do not adopt a newer
	// Session register: pending FIFO changes, background membership and group
	// state, and the reservation/run slots ExecutionBusy reads. Every increment
	// happens under mu inside the same critical section as the publication it
	// counts, so a snapshot taken under one hold observes one publication.
	localRev uint64
}

// New constructs one Harness over the given dependencies. It requires a live
// Harness context and non-nil dependencies, performs no I/O, and starts no
// goroutine.
func New(ctx context.Context, deps Dependencies) (*Harness, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, invalidInput("harness construction requires a live context")
	}
	if deps.Storage == nil || deps.Prepare == nil {
		return nil, invalidInput("harness construction requires non-nil storage and preparation dependencies")
	}
	return &Harness{ctx: ctx, deps: deps, sessions: map[string]*coordinator{}}, nil
}

// CreateSession creates one Workspace-bound root Session with a random
// identity and one sampled creation time for created_at and last_activity.
func (h *Harness) CreateSession(ctx context.Context, req CreateSessionRequest) (SessionRecord, error) {
	if err := validateWorkspace(req.Workspace); err != nil {
		return SessionRecord{}, invalidInput("workspace: %v", err)
	}
	if req.AgentType == "" {
		return SessionRecord{}, invalidInput("agent type must be non-empty")
	}
	sessionID, err := newHexID()
	if err != nil {
		return SessionRecord{}, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	now := time.Now().UTC()
	record := SessionRecord{
		Identity: SessionIdentity{SessionID: sessionID, Workspace: req.Workspace, CreatedAt: now},
		State: SessionState{
			Lifecycle:        LifecycleOpen,
			CurrentAgentType: req.AgentType,
			Usage:            UsageTotals{},
			LastActivity:     now,
		},
	}
	payload, err := encodeSessionRegister(record)
	if err != nil {
		return SessionRecord{}, err
	}
	if err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
		registered, err := tx.InsertRegister(RegisterDraft{Key: RegisterKey{SessionID: sessionID, Kind: RegisterSession}, Payload: payload})
		if err != nil {
			return err
		}
		record.Revision = registered.Revision
		return nil
	}); err != nil {
		return SessionRecord{}, err
	}
	// The committed creation installs the stable cell with its committed graph
	// and a call-owned pin; the release clears the graph and retains the cell.
	created := &coordinator{sessionID: sessionID, graph: &sessionGraph{Session: record}, bgState: bgOpen}
	created.identity = record.Identity
	created.everLoaded = true
	created.uses = 1
	h.mu.Lock()
	h.sessions[sessionID] = created
	h.mu.Unlock()
	defer h.releaseUse(created)
	h.observeInvalidation(created) // the durable creation is the first Session publication
	return ownSessionRecord(record), nil
}

// ReadOperation returns the materialized Operation record of one Operation of
// one Session. The shared unavailable check runs inside the same
// coordinator-mutex hold that copies the record, so a deletion or a corruption
// marker that committed after materialization resolves to ErrNotFound or the
// typed corruption error rather than a stale resident read.
func (h *Harness) ReadOperation(ctx context.Context, sessionID, operationID string) (OperationRecord, error) {
	if err := validateOperationIdentity(operationID, "operation id"); err != nil {
		return OperationRecord{}, invalidInput("operation id: %v", err)
	}
	var record OperationRecord
	if err := h.withSessionRead(ctx, sessionID, func(c *coordinator) error {
		rec, ok := c.graph.Operation(operationID)
		if !ok {
			return fmt.Errorf("%w: operation %q in session %q", ErrNotFound, operationID, sessionID)
		}
		record = ownOperationRecord(rec)
		return nil
	}); err != nil {
		return OperationRecord{}, err
	}
	return record, nil
}

// ChangeAgentType changes the current Agent type of an open Session. It
// preserves identity, usage, history, and last activity, and never alters an
// active Operation's admission capture.
func (h *Harness) ChangeAgentType(ctx context.Context, sessionID, agentType string) (SessionRecord, error) {
	if agentType == "" {
		return SessionRecord{}, invalidInput("agent type must be non-empty")
	}
	c, release, err := h.acquireCoordinator(ctx, sessionID)
	if err != nil {
		return SessionRecord{}, err
	}
	defer release()
	for {
		if err := c.waitIdle(ctx); err != nil {
			return SessionRecord{}, err
		}
		c.mu.Lock()
		if c.reserved != nil { // a reservation was taken between the wait and the lock
			c.mu.Unlock()
			continue
		}
		if c.gone { // a deletion committed while this call waited or relocked
			c.mu.Unlock()
			return SessionRecord{}, notFoundSession(sessionID)
		}
		if c.graph.Session.State.Lifecycle != LifecycleOpen {
			c.mu.Unlock()
			return SessionRecord{}, invalidInput("session %q is archived; the Agent type requires an open Session", sessionID)
		}
		expected := c.graph.Session.Revision
		var updated SessionRecord
		err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
			key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
			reg, err := tx.ReadRegister(key)
			if err != nil {
				return err
			}
			current, err := decodeSessionRegister(reg)
			if err != nil {
				return corruptSession(sessionID, "session register: %v", err)
			}
			if reg.Revision != expected {
				return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, expected, reg.Revision)
			}
			state := current.State
			state.CurrentAgentType = agentType
			changed := SessionRecord{Identity: current.Identity, State: state}
			payload, err := encodeSessionRegister(changed)
			if err != nil {
				return err
			}
			replaced, err := tx.ReplaceRegister(key, reg.Revision, payload)
			if err != nil {
				return err
			}
			changed.Revision = replaced.Revision
			updated = changed
			return nil
		})
		if err != nil {
			h.markCorrupt(sessionID, err)
			c.mu.Unlock()
			if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the resident view
				if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
					return SessionRecord{}, rerr
				}
			}
			return SessionRecord{}, err
		}
		c.graph.Session = updated
		result := ownSessionRecord(updated)
		c.mu.Unlock()
		h.observeInvalidation(c) // the durable Agent-type advance
		return result, nil
	}
}

// Submit submits one Session message. While the Session is idle, regular and
// queued input use normal admission; while an Operation is active, regular
// input enters the steering FIFO and queued input enters the queued FIFO. The
// buffers are process-local: a buffered item is not a durable Operation or
// Session entry, has no idempotency, and is discarded on Harness-context
// loss. Submit holds the Session's admission reservation across its whole
// routing decision: an existing same-Session/message Operation resolves
// before routing, a waiter routes by mode only after the winning admission —
// or its own enqueue — has settled, and an idle caller admits through the
// held reservation, installing the prepared execution before releasing it.
func (h *Harness) Submit(ctx context.Context, req SubmitRequest) (SubmitResult, error) {
	if err := h.ctx.Err(); err != nil { // cancellation has closed admission
		return SubmitResult{}, err
	}
	switch req.Mode {
	case MessageModeRegular, MessageModeQueued:
	default:
		return SubmitResult{}, invalidInput("message mode %q is not one of regular or queued", req.Mode)
	}
	content, err := validateSubmitInput(req.OperationID, req.Origin, req.Content)
	if err != nil {
		return SubmitResult{}, err
	}
	c, release, err := h.acquireCoordinator(ctx, req.SessionID)
	if err != nil {
		return SubmitResult{}, err
	}
	defer release()
	admissionRelease, err := h.reserve(ctx, c) // the reservation precedes every routing decision
	if err != nil {
		return SubmitResult{}, err
	}
	defer admissionRelease()
	c.mu.Lock()
	if c.gone { // a deletion committed after materialization: no buffer or admission on the absent Session
		c.mu.Unlock()
		return SubmitResult{}, notFoundSession(req.SessionID)
	}
	if rec, ok := c.graph.Operation(req.OperationID); ok { // existing resolves before routing
		if rec.Admission.RequestKind != RequestKindMessage { // a compact Operation's identity rejects message reuse
			c.mu.Unlock()
			return SubmitResult{}, invalidInput("operation %q already exists in session %q with a different request kind", req.OperationID, req.SessionID)
		}
		first := ownOperationRecord(rec)
		c.mu.Unlock()
		return SubmitResult{Disposition: DispositionExisting, Operation: &first}, nil
	}
	if c.graph.Session.State.Lifecycle != LifecycleOpen {
		c.mu.Unlock()
		return SubmitResult{}, invalidInput("session %q is archived; admission requires an open Session", req.SessionID)
	}
	if c.bgState == bgClosed { // a permanently closed background group rejects agent work beside the lifecycle gate
		c.mu.Unlock()
		return SubmitResult{}, invalidInput("session is closed to agent work")
	}
	if err := ctx.Err(); err != nil { // the routing gate: cancellation observed here publishes nothing
		c.mu.Unlock()
		return SubmitResult{}, err
	}
	if err := h.ctx.Err(); err != nil {
		c.mu.Unlock()
		return SubmitResult{}, err
	}
	if c.graph.Session.State.CurrentOperationID != "" || c.run != nil { // active: buffer by mode; the enqueue is the publication gate
		item := &pendingMessage{operationID: req.OperationID, origin: req.Origin, content: content}
		disposition := DispositionSteering
		if req.Mode == MessageModeQueued {
			c.queued = append(c.queued, item)
			disposition = DispositionQueued
		} else {
			c.steering = append(c.steering, item)
		}
		c.bumpLocalRevision() // the buffer enqueue is a coordinator-local publication
		c.mu.Unlock()
		h.observeInvalidation(c)
		return SubmitResult{Disposition: disposition}, nil
	}
	c.mu.Unlock()

	// the idle branch admits through the held reservation; the returned
	// execution installs before the deferred release runs
	rec, prepared, disposition, err := h.admitReserved(ctx, c, admissionRequest{
		SessionID:   req.SessionID,
		OperationID: req.OperationID,
		Kind:        RequestKindMessage,
		Origin:      req.Origin,
		Content:     content,
	})
	if err != nil {
		return SubmitResult{}, err
	}
	if prepared != nil { // install process-local execution and start Agent only after commit
		h.startExecution(c, rec.Admission.OperationID, *prepared)
	}
	return SubmitResult{Disposition: disposition, Operation: &rec}, nil
}

// Compact compacts one idle Session manually under a dedicated Operation: the
// caller-generated stable Operation ID supplies normal admission and
// idempotency without any applicable input entry or synthetic user message.
// The method follows the reserved admission shape, never Submit's: an existing same-Session
// Operation of the compact kind resolves before the idle check, without
// running preparation or compaction again; otherwise the Session must be
// truly idle — no current Operation, no installed run, and both the steering
// and queued buffers empty — checked before and again under the admission
// reservation, so Compact cannot overtake a pending drain in the
// terminal-to-retirement window or work admitted while it waited. Everything
// outside this one valid set is rejected: Compact never queues or steers and
// never enters either buffer. On success the reserved body prepares and
// publishes the compact kind and the execution installs before the
// reservation releases; a Submit arriving afterward follows its ordinary
// active routing, and manual compaction does not drain or consume that
// pending message.
func (h *Harness) Compact(ctx context.Context, req CompactRequest) (OperationRecord, error) {
	if err := validateOperationIdentity(req.OperationID, "operation id"); err != nil {
		return OperationRecord{}, invalidInput("operation id: %v", err)
	}
	c, release, err := h.acquireCoordinator(ctx, req.SessionID)
	if err != nil {
		return OperationRecord{}, err
	}
	defer release()
	// resolve-or-guard runs before the reservation and again under it: the
	// existing same-kind resolution precedes the idle guard on both passes.
	resolve := func() (OperationRecord, bool, error) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.gone { // a deletion committed after materialization
			return OperationRecord{}, false, notFoundSession(req.SessionID)
		}
		if rec, ok := c.graph.Operation(req.OperationID); ok {
			if rec.Admission.RequestKind != RequestKindCompact { // a different request kind's use of the identity is invalid
				return OperationRecord{}, false, invalidInput("operation %q already exists in session %q with a different request kind", req.OperationID, req.SessionID)
			}
			return ownOperationRecord(rec), true, nil
		}
		if c.graph.Session.State.CurrentOperationID != "" || c.run != nil || len(c.steering) > 0 || len(c.queued) > 0 {
			return OperationRecord{}, false, invalidInput("session %q is not idle; admission requires an idle Session", req.SessionID)
		}
		return OperationRecord{}, false, nil
	}
	rec, existing, err := resolve()
	if err != nil || existing {
		return rec, err
	}
	admissionRelease, err := h.reserve(ctx, c)
	if err != nil {
		return OperationRecord{}, err
	}
	defer admissionRelease()
	rec, existing, err = resolve() // the re-evaluation under the reservation
	if err != nil || existing {
		return rec, err
	}
	admitted, prepared, _, err := h.admitReserved(ctx, c, admissionRequest{
		SessionID:   req.SessionID,
		OperationID: req.OperationID,
		Kind:        RequestKindCompact,
	})
	if err != nil {
		return OperationRecord{}, err
	}
	if prepared != nil { // install the execution before the deferred release runs
		h.startExecution(c, admitted.Admission.OperationID, *prepared)
	}
	return admitted, nil
}

// ReopenSession reopens one archived Session: it changes the lifecycle to
// open, clears the archived time, and advances last activity to the reopen
// time, preserving history, usage, context, lineage, and Agent type.
// Reopening an already open Session is a no-write success.
func (h *Harness) ReopenSession(ctx context.Context, sessionID string) (SessionRecord, error) {
	c, release, err := h.acquireCoordinator(ctx, sessionID)
	if err != nil {
		return SessionRecord{}, err
	}
	defer release()
	for {
		if err := c.waitIdle(ctx); err != nil {
			return SessionRecord{}, err
		}
		c.mu.Lock()
		if c.reserved != nil { // a reservation was taken between the wait and the lock
			c.mu.Unlock()
			continue
		}
		if c.gone { // a deletion committed while this call waited or relocked
			c.mu.Unlock()
			return SessionRecord{}, notFoundSession(sessionID)
		}
		if c.graph.Session.State.Lifecycle != LifecycleArchived { // reopening open is a no-write success
			result := ownSessionRecord(c.graph.Session)
			c.mu.Unlock()
			return result, nil
		}
		expected := c.graph.Session.Revision
		now := time.Now().UTC() // the reopen time advances last activity
		var updated SessionRecord
		err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
			key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
			reg, err := tx.ReadRegister(key)
			if err != nil {
				return err
			}
			current, err := decodeSessionRegister(reg)
			if err != nil {
				return corruptSession(sessionID, "session register: %v", err)
			}
			if reg.Revision != expected {
				return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, expected, reg.Revision)
			}
			state := current.State
			state.Lifecycle = LifecycleOpen
			state.ArchivedAt = nil
			state.LastActivity = now
			changed := SessionRecord{Identity: current.Identity, State: state}
			payload, err := encodeSessionRegister(changed)
			if err != nil {
				return err
			}
			replaced, err := tx.ReplaceRegister(key, reg.Revision, payload)
			if err != nil {
				return err
			}
			changed.Revision = replaced.Revision
			updated = changed
			return nil
		})
		if err != nil {
			h.markCorrupt(sessionID, err)
			c.mu.Unlock()
			if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the resident view
				if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
					return SessionRecord{}, rerr
				}
			}
			return SessionRecord{}, err
		}
		c.graph.Session = updated
		result := ownSessionRecord(updated)
		c.mu.Unlock()
		h.observeInvalidation(c) // the durable reopen advance
		return result, nil
	}
}

// ArchiveSession archives one open idle Session: it changes the lifecycle to
// archived with one sampled UTC time, leaves last activity unchanged, and
// only ever reaches an idle archived Session — a Session that holds any busy
// state is rejected. Archiving an already archived Session is a no-write
// success.
func (h *Harness) ArchiveSession(ctx context.Context, sessionID string) (SessionRecord, error) {
	c, release, err := h.acquireCoordinator(ctx, sessionID)
	if err != nil {
		return SessionRecord{}, err
	}
	defer release()
	for {
		if err := c.waitIdle(ctx); err != nil {
			return SessionRecord{}, err
		}
		c.mu.Lock()
		if c.reserved != nil { // a reservation was taken between the wait and the lock
			c.mu.Unlock()
			continue
		}
		if c.gone { // a deletion committed while this call waited or relocked
			c.mu.Unlock()
			return SessionRecord{}, notFoundSession(sessionID)
		}
		if !c.quiescentLocked() { // the shared lifecycle gate: no transition crosses busy state
			c.mu.Unlock()
			return SessionRecord{}, invalidInput("session %q is busy; archiving requires a quiescent Session", sessionID)
		}
		if c.graph.Session.State.Lifecycle == LifecycleArchived { // archiving archived is a no-write success
			result := ownSessionRecord(c.graph.Session)
			c.mu.Unlock()
			return result, nil
		}
		expected := c.graph.Session.Revision
		now := time.Now().UTC() // the one sampled archive time
		var updated SessionRecord
		err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
			key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
			reg, err := tx.ReadRegister(key)
			if err != nil {
				return err
			}
			current, err := decodeSessionRegister(reg)
			if err != nil {
				return corruptSession(sessionID, "session register: %v", err)
			}
			if reg.Revision != expected {
				return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, expected, reg.Revision)
			}
			state := current.State
			stamped := now
			state.Lifecycle = LifecycleArchived
			state.ArchivedAt = &stamped
			changed := SessionRecord{Identity: current.Identity, State: state}
			payload, err := encodeSessionRegister(changed)
			if err != nil {
				return err
			}
			replaced, err := tx.ReplaceRegister(key, reg.Revision, payload)
			if err != nil {
				return err
			}
			changed.Revision = replaced.Revision
			updated = changed
			return nil
		})
		if err != nil {
			h.markCorrupt(sessionID, err)
			c.mu.Unlock()
			if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the resident view
				if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
					return SessionRecord{}, rerr
				}
			}
			return SessionRecord{}, err
		}
		c.graph.Session = updated
		result := ownSessionRecord(updated)
		c.mu.Unlock()
		h.observeInvalidation(c) // the durable archive advance
		return result, nil
	}
}

// DeleteSession deletes one archived Session: the same transaction that
// validates its state performs the storage deletion. After the commit it uses
// the post-commit coordinator invalidation: callers already holding that
// coordinator and later materialization both return ErrNotFound. Deleting an
// open or absent Session is rejected without a write.
func (h *Harness) DeleteSession(ctx context.Context, sessionID string) error {
	c, release, err := h.acquireCoordinator(ctx, sessionID)
	if err != nil {
		return err
	}
	defer release()
	for {
		if err := c.waitIdle(ctx); err != nil {
			return err
		}
		c.mu.Lock()
		if c.reserved != nil { // a reservation was taken between the wait and the lock
			c.mu.Unlock()
			continue
		}
		if c.gone { // a deletion committed while this call waited or relocked
			c.mu.Unlock()
			return notFoundSession(sessionID)
		}
		if c.graph.Session.State.Lifecycle != LifecycleArchived {
			c.mu.Unlock()
			return invalidInput("session %q is open; deletion requires an archived Session", sessionID)
		}
		expected := c.graph.Session.Revision
		err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
			key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
			reg, err := tx.ReadRegister(key)
			if err != nil {
				return err
			}
			if reg.Revision != expected {
				return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, sessionID, expected, reg.Revision)
			}
			return tx.DeleteSession(sessionID)
		})
		if err != nil {
			h.markCorrupt(sessionID, err)
			c.mu.Unlock()
			if errors.Is(err, errRevisionRace) { // a foreign writer changed the durable state under the resident view
				if rerr := h.rematerialize(ctx, c, sessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
					return rerr
				}
			}
			return err
		}
		c.invalidate() // post-commit, under the same coordinator ownership: buffers cleared and absence marked before registry removal
		c.mu.Unlock()
		h.removeCoordinator(sessionID)
		return nil
	}
}

// SweepSession is the explicit-time per-Session lifecycle transition. It
// never waits: a reserved
// candidate takes the same no-transition outcome as running, buffered, or
// live-background work, so one busy Session never makes a caller wait on
// unrelated execution. Under the coordinator mutex held across its own
// transaction and post-commit adoption it re-reads the current Session
// register and applies at most one transition against the explicit sweep
// time: archive for an open idle Session past its threshold, deletion for an
// archived Session past its threshold. A gone candidate reports the
// not-found class and a corrupt one the corruption class, both left
// unchanged. Deletion uses the same post-commit coordinator invalidation as
// DeleteSession; archiving adopts the committed record under the same hold.
// It reports whether the deletion transition committed: the flag is true only
// after the deleting transaction and its post-commit adoption succeeded.
func (h *Harness) SweepSession(ctx context.Context, sessionID string, policy SweepPolicy, now time.Time) (bool, error) {
	if now.IsZero() {
		return false, invalidInput("sweep time must not be zero")
	}
	c, release, err := h.acquireCoordinator(ctx, sessionID)
	if err != nil {
		return false, err
	}
	defer release()
	return h.sweepOne(ctx, c, sessionID, policy, now)
}

// sweepOne handles one Sweep Session through the coordinator: without
// waiting, it checks the coordinator under its mutex — held across the whole
// transaction and post-commit adoption — and applies at most one transition
// against the explicit sweep time: archive for an open idle Session past its
// threshold, deletion for an archived Session past its threshold. A reserved
// or quiescence-failing Session (running, buffered, or live-background) takes
// the same no-transition outcome, so one busy Session never makes a caller
// wait on unrelated execution; a gone candidate reports the not-found class
// and a corrupt register the corruption class, both left unchanged. Deletion
// uses the same post-commit coordinator invalidation as DeleteSession;
// archiving adopts the committed record under the same hold. It reports
// whether the deletion transition committed: the flag is true only after the
// deleting transaction and its post-commit adoption succeeded.
func (h *Harness) sweepOne(ctx context.Context, c *coordinator, sessionID string, policy SweepPolicy, now time.Time) (bool, error) {
	c.mu.Lock()
	if c.reserved != nil { // a reserved candidate skips like every other busy state: the pass never waits on admission work
		c.mu.Unlock()
		return false, nil
	}
	if c.gone { // a deletion committed before this transition
		c.mu.Unlock()
		return false, notFoundSession(sessionID)
	}
	if !c.quiescentLocked() { // running, buffered, or live background work: left unchanged
		c.mu.Unlock()
		return false, nil
	}
	var (
		archived   SessionRecord
		didArchive bool
		deleted    bool
	)
	err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
		key := RegisterKey{SessionID: sessionID, Kind: RegisterSession}
		reg, err := tx.ReadRegister(key)
		if err != nil {
			return err
		}
		current, err := decodeSessionRegister(reg)
		if err != nil {
			return corruptSession(sessionID, "session register: %v", err)
		}
		switch current.State.Lifecycle {
		case LifecycleOpen:
			if policy.ArchiveAfter <= 0 || current.State.CurrentOperationID != "" {
				return nil // a disabled threshold or a running Session is left unchanged
			}
			if now.Sub(current.State.LastActivity) <= policy.ArchiveAfter {
				return nil
			}
			state := current.State
			stamped := now // the sweep time stamps the archive
			state.Lifecycle = LifecycleArchived
			state.ArchivedAt = &stamped
			changed := SessionRecord{Identity: current.Identity, State: state}
			payload, err := encodeSessionRegister(changed)
			if err != nil {
				return err
			}
			replaced, err := tx.ReplaceRegister(key, reg.Revision, payload)
			if err != nil {
				return err
			}
			changed.Revision = replaced.Revision
			archived, didArchive = changed, true
		case LifecycleArchived:
			if policy.DeleteAfterArchive <= 0 {
				return nil
			}
			if now.Sub(*current.State.ArchivedAt) <= policy.DeleteAfterArchive {
				return nil
			}
			if err := tx.DeleteSession(sessionID); err != nil {
				return err
			}
			deleted = true
		}
		return nil
	})
	if err != nil {
		h.markCorrupt(sessionID, err)
		c.mu.Unlock()
		return false, err
	}
	if deleted { // the same post-commit coordinator invalidation as DeleteSession, under this hold
		c.invalidate()
		c.mu.Unlock()
		h.removeCoordinator(sessionID)
		return true, nil
	}
	if didArchive {
		c.graph.Session = archived
	}
	c.mu.Unlock()
	if didArchive {
		h.observeInvalidation(c) // the durable sweep archive advance
	}
	return false, nil
}

// quiescentLocked applies the one lifecycle gate under the caller-held
// coordinator mutex: a Session is quiescent only when it carries no durable
// active Operation, no installed execution run, no process-local steering or
// queued input, and no live owned background work. ArchiveSession rejects a
// Session outside this set with the invalid-input class and the sweep skips
// it; a durable terminal alone is not retirement. Each entry point retains
// its own reservation check beside this predicate.
func (c *coordinator) quiescentLocked() bool {
	return c.graph.Session.State.CurrentOperationID == "" && c.run == nil && len(c.steering) == 0 && len(c.queued) == 0 && !liveBackground(c)
}

// bumpLocalRevision advances the coordinator-local revision under the
// caller-held coordinator mutex. It counts one publication: a publication
// combining several changes in one hold advances the counter exactly once,
// and a publication that adopts a newer Session register is instead covered
// by the register's durable revision.
func (c *coordinator) bumpLocalRevision() {
	c.localRev++
}

// discardBuffers clears both process-local FIFOs under the caller-held
// coordinator mutex, reporting whether the discard changed any buffered item
// and advanced the local revision. Archive and sweep never touch the buffers:
// the shared quiescent predicate only lets them commit against empty buffers,
// and the deletion invalidation clears an absent coordinator, so none of those
// transitions goes through here.
func (c *coordinator) discardBuffers() bool {
	changed := len(c.steering) > 0 || len(c.queued) > 0
	if changed {
		c.localRev++
	}
	c.steering, c.queued = nil, nil
	return changed
}

// dropBufferedLocked removes one delivery's previously selected pending item
// from the head of whichever buffer still holds it, reporting whether it
// removed anything. The caller holds the coordinator mutex. Only the exact
// selected item matches: a concurrently discarded buffer's new head is never
// removed.
func (c *coordinator) dropBufferedLocked(item *pendingMessage) bool {
	if item == nil {
		return false
	}
	if len(c.steering) > 0 && c.steering[0] == item {
		c.steering = c.steering[1:]
		return true
	}
	if len(c.queued) > 0 && c.queued[0] == item {
		c.queued = c.queued[1:]
		return true
	}
	return false
}

// invalidate marks one coordinator absent under the caller-held coordinator
// mutex: it clears the process-local buffers and sets the gone flag in the
// same critical section as the deletion commit's adoption, so no holder can
// observe a committed deletion without the absence. The caller removes the
// registry entry through removeCoordinator after releasing the mutex. From
// then on, callers already holding the coordinator and later materialization
// both return ErrNotFound.
func (c *coordinator) invalidate() {
	c.steering, c.queued = nil, nil
	c.gone = true
}

// removeCoordinator drops one invalidated coordinator after its canonical
// deletion commit: the registry entry goes, so no later materialization
// reaches it. A caller already holding the coordinator keeps it, and its gone
// flag, until its uses release; the graph pointer is left alone, because the
// cell is no longer reachable through the registry and dies with its last
// holder.
func (h *Harness) removeCoordinator(sessionID string) {
	h.mu.Lock()
	delete(h.sessions, sessionID)
	h.mu.Unlock()
}

// isCorruption reports whether one error is a persisted semantic violation of
// a Session, which lifecycle transitions leave unchanged rather than acting on.
func isCorruption(err error) bool {
	var corrupt *CorruptionError
	return errors.As(err, &corrupt)
}

// notFoundSession is the absent-Session error: the landed not-found class for
// one deleted or unknown Session identity.
func notFoundSession(sessionID string) error {
	return fmt.Errorf("%w: session %q not found", ErrNotFound, sessionID)
}

// acquireCoordinator acquires one Session's coordinator with one use held and
// its validated graph installed. The cell and the use are claimed under the
// registry mutex, which is released before the coordinator mutex is taken:
// the two are never nested. A missing cell is installed empty before its first
// load, so concurrent acquirers share one stable cell, and a missing graph is
// validated and published under that cell's mutex, so one load serves them
// all. A persisted semantic violation latches on the stable cell and makes the
// Session unavailable in this Harness instance; other Sessions remain usable.
// A deleted coordinator stays absent to every holder: it resolves to the
// not-found class instead of a stale resident read. The returned release drops
// the use, clears the graph at the final release, and must be called after the
// caller leaves the coordinator mutex for the last time.
func (h *Harness) acquireCoordinator(ctx context.Context, sessionID string) (*coordinator, func(), error) {
	c := h.claimCell(sessionID)
	if err := h.loadGraph(ctx, c, sessionID, false); err != nil {
		h.releaseUse(c)
		return nil, nil, err
	}
	return c, func() { h.releaseUse(c) }, nil
}

// retainGraph extends the use its caller already holds on one coordinator, for
// work that outlives that caller: execution retention through execute,
// terminal settlement, disposal, the post-terminal drain, and child
// completion. The claim increments the use count under the registry mutex
// without inspecting the graph — the caller's own use already pins the cell —
// and the returned release is the ordinary use release.
func (h *Harness) retainGraph(c *coordinator) func() {
	h.mu.Lock()
	c.uses++
	h.mu.Unlock()
	return func() { h.releaseUse(c) }
}

// claimCell returns one Session's stable coordinator with one use claimed and
// the registry mutex released.
func (h *Harness) claimCell(sessionID string) *coordinator {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.sessions[sessionID]
	if c == nil {
		c = &coordinator{sessionID: sessionID, bgState: bgOpen}
		h.sessions[sessionID] = c
	}
	c.uses++
	return c
}

// releaseUse drops one use. The final release is the single graph owner: it
// runs under the coordinator mutex and then the registry mutex — the existing
// allowed direction, never the reverse — captures the resident graph's durable
// Session revision into the cell's retained value, and clears the graph. Every
// graph, whatever the caller kind, is cleared at its final use release; a
// graph with a positive use count is never cleared. The stable cell, its
// retained identity and revision, its sticky latch, and every local field
// stay. A never-loaded latch-free cell leaves the registry, and a cell removed
// by canonical deletion is left alone: its graph dies with its last holder.
func (h *Harness) releaseUse(c *coordinator) {
	c.mu.Lock()
	h.mu.Lock()
	c.uses--
	if c.uses > 0 {
		h.mu.Unlock()
		c.mu.Unlock()
		return
	}
	if h.sessions[c.sessionID] != c {
		h.mu.Unlock()
		c.mu.Unlock()
		return // canonically deleted: the registry entry is already gone
	}
	if c.graph != nil {
		c.durableRevision = c.graph.Session.Revision
		c.graph = nil
	}
	if !c.everLoaded && c.corru == nil { // a failed first lookup leaves no cell behind
		delete(h.sessions, c.sessionID)
	}
	h.mu.Unlock()
	c.mu.Unlock()
}

// loadGraph validates one Session's durable graph and publishes it under the
// coordinator mutex: the cold load and the stale-view refresh share this
// validation and publication. The availability check runs before the
// validation and again before the validated graph is adopted, so a deletion or
// a corruption latch that landed meanwhile refuses the load instead of being
// overwritten; a persisted semantic violation latches on the stable cell,
// while storage and context failures leave it retryable. A cold load of an
// already-published graph keeps that graph; only a refresh replaces a present
// one. Storage I/O uses the supplied context.
func (h *Harness) loadGraph(ctx context.Context, c *coordinator, sessionID string, replace bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := h.unavailableLocked(c, sessionID); err != nil {
		return err
	}
	if !replace && c.graph != nil {
		return nil
	}
	graph, err := validateSessionGraph(ctx, h.deps.Storage, sessionID)
	if err != nil {
		h.markCorrupt(sessionID, err)
		return err
	}
	if err := h.unavailableLocked(c, sessionID); err != nil {
		return err
	}
	c.graph = graph
	c.identity = graph.Session.Identity
	h.mu.Lock()
	c.everLoaded = true
	h.mu.Unlock()
	return nil
}

// markCorrupt makes one Session unavailable in this Harness instance when a
// persisted semantic violation is detected for it: the latch sticks on the
// Session's stable cell, so no later read, load, or refresh clears it.
func (h *Harness) markCorrupt(sessionID string, err error) {
	var corrupt *CorruptionError
	if !errors.As(err, &corrupt) {
		return
	}
	h.mu.Lock()
	if c := h.sessions[sessionID]; c != nil && c.corru == nil {
		c.corru = err
	}
	h.mu.Unlock()
}

// reserve takes the Session's single admission reservation, waiting while
// another reservation is held, subject to the caller's context. The returned
// release resolves it exactly once. The reservation is a coordinator-local
// publication and emits its invalidation after the coordinator mutex is
// released; the reservation channel is not a state mutex, so ordinary reads
// proceed while it is held.
func (h *Harness) reserve(ctx context.Context, c *coordinator) (func(), error) {
	for {
		c.mu.Lock()
		if c.reserved == nil {
			c.reserved = make(chan struct{})
			c.bumpLocalRevision() // the reservation is an ExecutionBusy publication
			c.mu.Unlock()
			h.observeInvalidation(c)
			return func() { h.releaseReservation(c) }, nil
		}
		held := c.reserved
		c.mu.Unlock()
		select {
		case <-held:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func (h *Harness) releaseReservation(c *coordinator) {
	c.mu.Lock()
	released := false
	if c.reserved != nil {
		close(c.reserved)
		c.reserved = nil
		c.bumpLocalRevision() // the release is an ExecutionBusy publication
		released = true
	}
	c.mu.Unlock()
	if released {
		h.observeInvalidation(c)
	}
}

// waitIdle waits until no admission reservation is held. A caller that
// transitions Session state must re-check idleness after acquiring the
// coordinator mutex, which blocks new reservations.
func (c *coordinator) waitIdle(ctx context.Context) error {
	for {
		c.mu.Lock()
		if c.reserved == nil {
			c.mu.Unlock()
			return nil
		}
		held := c.reserved
		c.mu.Unlock()
		select {
		case <-held:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// admissionRequest is the input of the private normal admission producer.
type admissionRequest struct {
	SessionID   string
	OperationID string
	Kind        RequestKind
	Origin      InputOrigin
	Content     []model.ContentPart
	// buffered identifies a drain's head for removal during adoption; nil
	// leaves the FIFOs unchanged for ordinary admissions.
	buffered *pendingMessage
}

// errAdmissionExisting aborts the admission transaction once the conflicting
// same-Session Operation has been read; it never escapes the reserved admission body.
var errAdmissionExisting = errors.New("harness: operation already admitted in this session")

// errRevisionRace marks the revision-guard conflict of an admission,
// agent-type, or effect transaction: durable state changed under the
// coordinator's validated view. It keeps the landed conflict class reachable.
var errRevisionRace = fmt.Errorf("harness: session changed concurrently: %w", ErrConflict)

// validateSubmitInput validates one submitted message's identity, origin, and
// content, returning the owned content copy every downstream use consumes.
func validateSubmitInput(operationID string, origin InputOrigin, content []model.ContentPart) ([]model.ContentPart, error) {
	if err := validateOperationIdentity(operationID, "operation id"); err != nil {
		return nil, invalidInput("operation id: %v", err)
	}
	switch origin {
	case InputOriginUser, InputOriginRuntime, InputOriginPlugin:
	default:
		return nil, invalidInput("input origin %q is not one of user, runtime or plugin", origin)
	}
	owned := make([]model.ContentPart, 0, len(content))
	for i, part := range content {
		validated, err := model.NewContentPart(part) // the validated owned copy feeds every downstream use
		if err != nil {
			return nil, invalidInput("content[%d]: %v", i, err)
		}
		owned = append(owned, validated)
	}
	return owned, nil
}

// admitReserved is the reserved admission body: the re-checked existing,
// lifecycle, and idle guards, the outside-lock preparation, and the
// transactional publication. The caller holds the Session's admission
// reservation and releases it only after the returned execution, when present,
// is installed. A prepared execution returns only for a newly admitted
// Operation; an existing resolution carries none.
func (h *Harness) admitReserved(ctx context.Context, c *coordinator, req admissionRequest) (OperationRecord, *PreparedExecution, SubmitDisposition, error) {
	c.mu.Lock()
	if c.gone { // a deletion committed while the reservation holder was outside the lock: never prepare for an absent Session
		c.mu.Unlock()
		return OperationRecord{}, nil, "", notFoundSession(req.SessionID)
	}
	if rec, ok := c.graph.Operation(req.OperationID); ok { // the prior reservation holder admitted it
		c.mu.Unlock()
		if rec.Admission.RequestKind != req.Kind { // a different request kind's use of the identity is invalid
			return OperationRecord{}, nil, "", invalidInput("operation %q already exists in session %q with a different request kind", req.OperationID, req.SessionID)
		}
		return ownOperationRecord(rec), nil, DispositionExisting, nil
	}
	if c.graph.Session.State.Lifecycle != LifecycleOpen {
		c.mu.Unlock()
		return OperationRecord{}, nil, "", invalidInput("session %q is archived; admission requires an open Session", req.SessionID)
	}
	if c.bgState == bgClosed { // a permanently closed background group rejects agent work beside the lifecycle gate
		c.mu.Unlock()
		return OperationRecord{}, nil, "", invalidInput("session is closed to agent work")
	}
	if c.graph.Session.State.CurrentOperationID != "" {
		running := c.graph.Session.State.CurrentOperationID
		c.mu.Unlock()
		return OperationRecord{}, nil, "", invalidInput("session %q already runs operation %q; admission requires an idle Session", req.SessionID, running)
	}
	view := c.graph.Session // the state the reservation resolved on
	c.mu.Unlock()

	prepared, capture, prepCtx, cleanup, err := h.prepareExecution(ctx, PreparationSession{
		Identity:  view.Identity,
		AgentType: view.State.CurrentAgentType,
	}, req.Kind)
	if err != nil {
		return OperationRecord{}, nil, "", err
	}
	defer cleanup() // the combined context stays live until publication returns

	// the transaction runs on the combined preparation context: either
	// cancellation before commit aborts it, and a committed publication wins
	// later cancellation
	record, existing, err := h.publishAdmission(prepCtx, c, view, capture, req)
	if err != nil {
		h.releasePrepared(&prepared) // the publication failed: this admission discards the prepared execution with its reservation
		return OperationRecord{}, nil, "", publicationError(ctx, err)
	}
	if existing {
		h.releasePrepared(&prepared) // the identity resolved to another admission's Operation: nothing here adopts
		return ownOperationRecord(record), nil, DispositionExisting, nil
	}
	return ownOperationRecord(record), &prepared, DispositionAdmitted, nil
}

// prepareExecution is the shared outside-lock preparation and validation of
// one admission producer, used by normal admission and Fork alike: it merges
// the caller context with the Harness context, invokes the single preparation
// callback, requires a non-nil opener and a valid capture, and returns the
// prepared execution with its owned durable capture plus the combined context
// and its cleanup. A preparation failure returns with the combined context
// already cleaned up; on success the caller keeps it live through publication
// and runs the cleanup when publication returns. A returned reservation is
// released on every discard — a nil opener, an invalid capture, or
// cancellation observed after preparation — so only an adopted prepared
// execution ever carries one. Caller or Harness cancellation before or after
// preparation publishes nothing.
func (h *Harness) prepareExecution(ctx context.Context, session PreparationSession, kind RequestKind) (PreparedExecution, ExecutionCapture, context.Context, context.CancelFunc, error) {
	prepCtx, cancel := context.WithCancel(h.ctx)
	stop := context.AfterFunc(ctx, cancel)
	cleanup := func() {
		stop()
		cancel()
	}
	if err := ctx.Err(); err != nil { // the combined preparation context is already done: never invoke Prepare
		cleanup()
		return PreparedExecution{}, ExecutionCapture{}, nil, nil, err
	}
	if err := h.ctx.Err(); err != nil {
		cleanup()
		return PreparedExecution{}, ExecutionCapture{}, nil, nil, h.ctx.Err()
	}
	prepared, prepErr := h.deps.Prepare(prepCtx, PreparationRequest{
		Session:     session,
		RequestKind: kind,
	})
	if prepErr != nil {
		cleanup()
		return PreparedExecution{}, ExecutionCapture{}, nil, nil, prepErr
	}
	if prepared.Open == nil {
		cleanup()
		h.releasePrepared(&prepared)
		return PreparedExecution{}, ExecutionCapture{}, nil, nil, invalidInput("prepared execution requires a non-nil opener")
	}
	if err := validateExecutionCapture(prepared.Capture); err != nil {
		cleanup()
		h.releasePrepared(&prepared)
		return PreparedExecution{}, ExecutionCapture{}, nil, nil, invalidInput("prepared execution capture: %v", err)
	}
	capture := ownCapture(prepared.Capture)

	if err := ctx.Err(); err != nil { // caller cancellation wins when both contexts are done
		cleanup()
		h.releasePrepared(&prepared)
		return PreparedExecution{}, ExecutionCapture{}, nil, nil, err
	}
	if err := h.ctx.Err(); err != nil {
		cleanup()
		h.releasePrepared(&prepared)
		return PreparedExecution{}, ExecutionCapture{}, nil, nil, h.ctx.Err()
	}
	return prepared, capture, prepCtx, cleanup, nil
}

// releasePrepared runs one preparation reservation's release exactly once and
// retains its failure through the cleanup channel the Harness keeps for Wait,
// never rewriting a committed terminal. A nil Release means the preparation
// reserved nothing. Clearing the copied Release enforces once-only ownership
// within one holder.
func (h *Harness) releasePrepared(prepared *PreparedExecution) {
	if prepared == nil || prepared.Release == nil {
		return
	}
	release := prepared.Release
	prepared.Release = nil
	h.recordCleanupFailure(release())
}

// publicationError keeps the caller-facing cancellation class at the
// publication boundary: the combined preparation context is canceled through
// its AfterFunc hop, so a store abort that observed a caller deadline
// expiring during publication surfaces as context.Canceled even though the
// caller's own Err is context.DeadlineExceeded. The adapter restores that
// deadline class; nil, success, and every other error pass through
// unchanged.
func publicationError(caller context.Context, err error) error {
	if err != nil && errors.Is(err, context.Canceled) && caller.Err() == context.DeadlineExceeded {
		return caller.Err()
	}
	return err
}

// publishAdmission runs the in-transaction admission producer: re-read the
// exact Session revision, insert the input entry and the running Operation,
// and set the Session current Operation. A concurrent winner of the same
// identity resolves to the first Operation; a foreign revision change
// resolves to the existing Operation, a violated admission precondition, or
// a conflict; an insertion conflict without a same-Session owner is
// cross-Session reuse.
func (h *Harness) publishAdmission(ctx context.Context, c *coordinator, view SessionRecord, capture ExecutionCapture, req admissionRequest) (OperationRecord, bool, error) {
	var (
		published  OperationRecord
		newSession SessionRecord
		newEntry   graphEntry
		existing   bool
		foreign    bool
	)
	err := h.deps.Storage.Transact(ctx, func(tx Transaction) error {
		sessionKey := RegisterKey{SessionID: req.SessionID, Kind: RegisterSession}
		reg, err := tx.ReadRegister(sessionKey)
		if err != nil {
			return err
		}
		current, err := decodeSessionRegister(reg)
		if err != nil {
			return corruptSession(req.SessionID, "session register: %v", err)
		}
		if reg.Revision != view.Revision {
			foreign = true
			rec, found, err := readExistingOperation(tx, req.SessionID, req.OperationID)
			if err != nil {
				return err
			}
			if found {
				if rec.Admission.RequestKind != req.Kind { // a different request kind's use of the identity is invalid
					return invalidInput("operation %q already exists in session %q with a different request kind", req.OperationID, req.SessionID)
				}
				published, existing = rec, true
				return errAdmissionExisting
			}
			// a violated semantic precondition outranks the conflict class
			if current.State.Lifecycle != LifecycleOpen {
				return invalidInput("session %q is archived; admission requires an open Session", req.SessionID)
			}
			if current.State.CurrentOperationID != "" {
				return invalidInput("session %q already runs operation %q; admission requires an idle Session", req.SessionID, current.State.CurrentOperationID)
			}
			return fmt.Errorf("%w: session %q revision %d changed concurrently to %d", errRevisionRace, req.SessionID, view.Revision, reg.Revision)
		}
		record, committed, entry, perr := produceAdmission(tx, current, capture, req)
		if perr != nil {
			if errors.Is(perr, ErrConflict) { // first-writer resolution: the same-Session Operation read decides
				rec, found, rerr := readExistingOperation(tx, req.SessionID, req.OperationID)
				if rerr != nil {
					return rerr
				}
				if found {
					foreign = true                             // the conflicting identity is durable: refresh the stale view on any rejection
					if rec.Admission.RequestKind != req.Kind { // a different request kind's use of the identity is invalid
						return invalidInput("operation %q already exists in session %q with a different request kind", req.OperationID, req.SessionID)
					}
					published, existing = rec, true
					return errAdmissionExisting
				}
				return invalidInput("operation %q already exists in another session", req.OperationID)
			}
			return perr
		}
		published, newSession, newEntry = record, committed, entry
		return nil
	})
	if existing {
		if foreign { // a foreign writer changed the durable state under the resident view
			if rerr := h.rematerialize(ctx, c, req.SessionID); rerr != nil {
				return OperationRecord{}, false, rerr
			}
		}
		return published, true, nil
	}
	if err != nil {
		h.markCorrupt(req.SessionID, err)
		if foreign { // the transaction proved the resident view stale; refresh it before the error returns
			if rerr := h.rematerialize(ctx, c, req.SessionID); rerr != nil { // a discovered corruption or storage failure is the current truth
				return OperationRecord{}, false, rerr
			}
		}
		return OperationRecord{}, false, err
	}
	c.mu.Lock()
	if newEntry.Envelope.ID != "" { // a compact admission publishes no input entry
		c.graph.Entries = append(c.graph.Entries, newEntry)
	}
	c.graph.Operations = append(c.graph.Operations, published)
	c.graph.Session = newSession
	// The durable advance also publishes removal of the selected FIFO head.
	c.dropBufferedLocked(req.buffered)
	c.mu.Unlock()
	h.observeInvalidation(c) // the durable admission advance
	return published, false, nil
}

// produceAdmission is the shared in-transaction destination admission
// producer, used by normal admission, compact admission, and Fork alike: it
// inserts the input entry and the running Operation register for one
// destination Session record held at its current revision, and sets the
// Session current Operation. A compact admission inserts no input entry and
// samples one UTC time for the admission, start, and last-activity stamps.
// First-writer resolution stays with the caller: an insertion conflict
// returns the raw storage conflict for the caller to resolve — the
// same-Session Operation read for normal admission, the exact idempotency
// lookup for Fork.
func produceAdmission(tx Transaction, session SessionRecord, capture ExecutionCapture, req admissionRequest) (OperationRecord, SessionRecord, graphEntry, error) {
	if req.Kind == RequestKindCompact {
		now := time.Now().UTC()
		record := OperationRecord{
			Admission: OperationAdmission{
				SessionID:   req.SessionID,
				OperationID: req.OperationID,
				RequestKind: req.Kind,
				AgentType:   session.State.CurrentAgentType,
				Execution:   capture,
				AdmittedAt:  now,
			},
			State: OperationCurrentState{
				Status:           OperationRunning,
				StartedAt:        now,
				PendingToolCalls: []PendingToolCall{},
				Usage:            UsageTotals{},
			},
		}
		opPayload, err := encodeOperationRegister(record)
		if err != nil {
			return OperationRecord{}, SessionRecord{}, graphEntry{}, err
		}
		registered, err := tx.InsertRegister(RegisterDraft{
			Key:     RegisterKey{SessionID: req.SessionID, Kind: RegisterOperation, OperationID: req.OperationID},
			Payload: opPayload,
		})
		if err != nil {
			return OperationRecord{}, SessionRecord{}, graphEntry{}, err
		}
		record.Revision = registered.Revision
		state := session.State
		state.CurrentOperationID = req.OperationID
		state.LastActivity = now
		committed := SessionRecord{Identity: session.Identity, State: state}
		sessionPayload, err := encodeSessionRegister(committed)
		if err != nil {
			return OperationRecord{}, SessionRecord{}, graphEntry{}, err
		}
		replaced, err := tx.ReplaceRegister(RegisterKey{SessionID: req.SessionID, Kind: RegisterSession}, session.Revision, sessionPayload)
		if err != nil {
			return OperationRecord{}, SessionRecord{}, graphEntry{}, err
		}
		committed.Revision = replaced.Revision
		return record, committed, graphEntry{}, nil
	}
	entryID, err := newHexID()
	if err != nil {
		return OperationRecord{}, SessionRecord{}, graphEntry{}, fmt.Errorf("%w: %v", ErrStorage, err)
	}
	input := inputEntry{
		SessionID:   req.SessionID,
		EntryID:     entryID,
		OperationID: req.OperationID,
		Origin:      req.Origin,
		Content:     req.Content,
	}
	payload, err := encodeInputEntry(input)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, graphEntry{}, err
	}
	inserted, err := tx.InsertEntry(EntryDraft{
		SessionID:   req.SessionID,
		ID:          entryID,
		OperationID: req.OperationID,
		Kind:        EntryInput,
		Payload:     payload,
	})
	if err != nil {
		return OperationRecord{}, SessionRecord{}, graphEntry{}, err
	}
	record := OperationRecord{
		Admission: OperationAdmission{
			SessionID:     req.SessionID,
			OperationID:   req.OperationID,
			RequestKind:   req.Kind,
			AdmittedEntry: EntryRef{SessionID: req.SessionID, EntryID: entryID},
			AgentType:     session.State.CurrentAgentType,
			Execution:     capture,
			AdmittedAt:    inserted.CommittedAt,
		},
		State: OperationCurrentState{
			Status:           OperationRunning,
			StartedAt:        inserted.CommittedAt,
			PendingToolCalls: []PendingToolCall{},
			Usage:            UsageTotals{},
		},
	}
	opPayload, err := encodeOperationRegister(record)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, graphEntry{}, err
	}
	registered, err := tx.InsertRegister(RegisterDraft{
		Key:     RegisterKey{SessionID: req.SessionID, Kind: RegisterOperation, OperationID: req.OperationID},
		Payload: opPayload,
	})
	if err != nil {
		return OperationRecord{}, SessionRecord{}, graphEntry{}, err
	}
	record.Revision = registered.Revision
	state := session.State
	state.CurrentOperationID = req.OperationID
	state.LastActivity = inserted.CommittedAt
	committed := SessionRecord{Identity: session.Identity, State: state}
	sessionPayload, err := encodeSessionRegister(committed)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, graphEntry{}, err
	}
	replaced, err := tx.ReplaceRegister(RegisterKey{SessionID: req.SessionID, Kind: RegisterSession}, session.Revision, sessionPayload)
	if err != nil {
		return OperationRecord{}, SessionRecord{}, graphEntry{}, err
	}
	committed.Revision = replaced.Revision
	return record, committed, graphEntry{Envelope: inserted, Input: &input}, nil
}

// rematerialize refreshes the coordinator's validated view after a foreign
// writer changed the durable state under it: the owned cell revalidates and
// republishes its graph under the coordinator mutex, with the caller's use
// still held, exactly like a cold load.
func (h *Harness) rematerialize(ctx context.Context, c *coordinator, sessionID string) error {
	return h.loadGraph(ctx, c, sessionID, true)
}

// startExecution installs the coordinator's active execution with its own
// execution context — a child of the Harness context — and starts the Agent
// composition on it after the admission commit. The execution holds its own
// graph use from here through execute, terminal settlement, disposal, and the
// post-terminal drain, so the caller's use never has to outlive its call.
// Interrupt cancels that context; the execution slot releases, and the
// post-terminal buffer drain completes, before the execution's done channel
// closes. After execute, the storage failure latch, and the drain have
// returned, the tail clears the run slot only when it is still this run — a
// drain-installed successor owns the slot and a distinct context — then
// cancels the execution context, runs the child completion settlement for this
// operation, drops its use, and closes done.
func (h *Harness) startExecution(c *coordinator, operationID string, prepared PreparedExecution) {
	release := h.retainGraph(c)
	execCtx, cancel := context.WithCancel(h.ctx)
	run := &activeExecution{done: make(chan struct{}), execCtx: execCtx, cancel: cancel}
	c.mu.Lock()
	c.run = run
	c.bumpLocalRevision() // the run slot's installation is an ExecutionBusy publication
	c.mu.Unlock()
	h.observeInvalidation(c)
	go func() {
		res, closeInvocation, closeExecution, err := h.execute(c, operationID, prepared, run.execCtx)
		_ = h.finishExecution(c, run, operationID, prepared, res, err, closeInvocation, closeExecution)
		retired := false
		c.mu.Lock()
		if c.run == run { // a buffered drain may have installed the next execution already
			c.run = nil
			c.bumpLocalRevision()
			retired = true
		}
		c.mu.Unlock()
		if retired {
			h.observeInvalidation(c)
		}
		cancel() // the context dies only after the drain: a drain-installed successor owns a distinct context
		h.childCompletionSettled(c, operationID)
		release()
		close(run.done)
	}()
}

// drainBuffers delivers steering before queued input after terminal commit.
// A selected head stays pending until admission or final failure/duplicate.
// Successful admission starts the successor; failure advances to the next
// head. Harness loss discards both FIFOs. An empty scan retires only this run
// under the same hold, never a successor's slot. Its sole production caller,
// finishExecution, holds the one reservation across this body: the successor
// installs before that release.
func (h *Harness) drainBuffers(c *coordinator, run *activeExecution) {
	c.mu.Lock()
	sessionID := c.graph.Session.Identity.SessionID
	c.mu.Unlock()
	for {
		c.mu.Lock()
		if h.ctx.Err() != nil { // Harness loss discards both buffers
			discarded := c.discardBuffers()
			c.mu.Unlock()
			if discarded {
				h.observeInvalidation(c)
			}
			return
		}
		if c.graph.Session.State.CurrentOperationID != "" { // the drain publishes only after terminal commit
			c.mu.Unlock()
			return
		}
		var item *pendingMessage
		switch {
		case len(c.steering) > 0:
			item = c.steering[0] // peek: the head stays pending while its delivery is unresolved
		case len(c.queued) > 0:
			item = c.queued[0]
		default: // the FIFO scan is empty: retire this run's slot in the same critical section
			retired := false
			if c.run == run { // never clear a replacement run
				c.run = nil
				c.bumpLocalRevision()
				retired = true
			}
			c.mu.Unlock()
			if retired {
				h.observeInvalidation(c)
			}
			return
		}
		c.mu.Unlock()
		rec, prepared, disposition, err := h.admitReserved(h.ctx, c, admissionRequest{
			SessionID:   sessionID,
			OperationID: item.operationID,
			Kind:        RequestKindMessage,
			Origin:      item.origin,
			Content:     item.content,
			buffered:    item,
		})
		if err != nil || disposition != DispositionAdmitted || prepared == nil {
			// one failed delivery attempt is final for the item: the selected
			// head leaves its buffer as the attempt's final outcome; an
			// admission that does not adopt its prepared execution already
			// released it
			c.mu.Lock()
			dropped := c.dropBufferedLocked(item)
			if dropped {
				c.bumpLocalRevision()
			}
			c.mu.Unlock()
			if dropped {
				h.observeInvalidation(c)
			}
			continue // the next buffered message proceeds
		}
		h.startExecution(c, rec.Admission.OperationID, *prepared)
		return
	}
}

// recordStorageFailure retains the first storage-class failure or marked
// publication failure that stopped admitted work — whatever the cause's own
// identity, Is/As preserved through the wrapper; later failures never replace
// it.
func (h *Harness) recordStorageFailure(err error) {
	if err == nil || (!errors.Is(err, ErrStorage) && !isPublicationFailure(err)) {
		return
	}
	h.mu.Lock()
	if h.storageFailure == nil {
		h.storageFailure = err
	}
	h.mu.Unlock()
}

// recordCleanupFailure retains the first non-nil execution Close failure
// alongside the first storage failure; later failures never replace it. A
// cleanup failure never rewrites the Operation's terminal settlement.
func (h *Harness) recordCleanupFailure(err error) {
	if err == nil {
		return
	}
	h.mu.Lock()
	if h.cleanupFailure == nil {
		h.cleanupFailure = err
	}
	h.mu.Unlock()
}

// snapshotCoordinators copies the registry's coordinator pointers under h.mu
// and returns them with h.mu released: the caller inspects each pointer under
// its own c.mu, so no lock is ever held across the other. A coordinator
// removed from the registry after the copy is still safe to inspect: its
// reservation and run slots only ever hold toward convergence.
func (h *Harness) snapshotCoordinators() []*coordinator {
	h.mu.Lock()
	cs := make([]*coordinator, 0, len(h.sessions))
	for _, c := range h.sessions {
		cs = append(cs, c)
	}
	h.mu.Unlock()
	return cs
}

// snapshotEstablishedCoordinators copies the registry's established cells
// under h.mu and returns them with h.mu released: an established cell is one
// whose Session graph was validated at least once, so it owns the local state
// a stop can join, whether or not a graph is resident right now. A failed
// never-loaded lookup owns nothing. The caller drives each stop body directly,
// with the cell's retained identity and its local group, run, and reservation
// handles; no graph and no storage read is involved, so an idle corpus is
// never loaded to be classified.
func (h *Harness) snapshotEstablishedCoordinators() []*coordinator {
	h.mu.Lock()
	cs := make([]*coordinator, 0, len(h.sessions))
	for _, c := range h.sessions {
		if c.everLoaded {
			cs = append(cs, c)
		}
	}
	h.mu.Unlock()
	return cs
}

// Wait joins the Harness after its context is canceled: it returns after
// cancellation has closed admission and every in-flight preparation,
// execution, and required settlement has converged. Its own context cancels
// only the wait. If admitted work stopped because required storage
// publication failed, or an execution's resource cleanup failed, it returns
// the errors.Join of the first retained storage failure and the first
// retained cleanup failure after all process-local work has ended; neither
// retained value is ever replaced by a later failure. Both scans copy the
// coordinator pointers under h.mu, release it, and inspect each under its
// own c.mu — no h.mu -> c.mu nesting, because a transition may hold c.mu
// while taking h.mu (markCorrupt) concurrently.
func (h *Harness) Wait(ctx context.Context) error {
	select {
	case <-h.ctx.Done():
	case <-ctx.Done():
		return ctx.Err()
	}
	for {
		var waits []chan struct{}
		for _, c := range h.snapshotCoordinators() {
			c.mu.Lock()
			if c.reserved != nil {
				waits = append(waits, c.reserved)
			}
			if c.run != nil {
				waits = append(waits, c.run.done)
			}
			c.mu.Unlock()
		}
		for _, done := range waits {
			select {
			case <-done:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		busy := false // a waited reservation may have published a new execution: re-scan before converging
		for _, c := range h.snapshotCoordinators() {
			c.mu.Lock()
			if c.reserved != nil || c.run != nil {
				busy = true
			}
			discarded := c.discardBuffers() // Harness loss discards both buffers on every coordinator
			c.mu.Unlock()
			if discarded {
				h.observeInvalidation(c)
			}
		}
		if !busy {
			h.mu.Lock()
			storage, cleanup := h.storageFailure, h.cleanupFailure
			h.mu.Unlock()
			return errors.Join(storage, cleanup)
		}
	}
}

// readExistingOperation reads one same-Session Operation register once inside
// a transaction, reporting whether it exists.
func readExistingOperation(tx Transaction, sessionID, operationID string) (OperationRecord, bool, error) {
	reg, err := tx.ReadRegister(RegisterKey{SessionID: sessionID, Kind: RegisterOperation, OperationID: operationID})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return OperationRecord{}, false, nil
		}
		return OperationRecord{}, false, err
	}
	rec, err := decodeOperationRegister(reg)
	if err != nil {
		return OperationRecord{}, false, corruptSession(sessionID, "operation register %q: %v", operationID, err)
	}
	return rec, true, nil
}

// newHexID returns one durable identifier: 32 lowercase hexadecimal
// characters from 16 cryptographically random bytes.
func newHexID() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// ownCapture returns an independent owned copy of one execution capture. A
// zero-length tool list or capability selection normalizes to nil so no
// caller backing is retained, even with spare capacity.
func ownCapture(c ExecutionCapture) ExecutionCapture {
	out := c
	if len(c.Tools) == 0 {
		out.Tools = nil
	} else {
		out.Tools = make([]model.ToolDefinition, len(c.Tools))
		for i, tool := range c.Tools {
			out.Tools[i] = tool
			out.Tools[i].Parameters = model.CloneRaw(tool.Parameters)
		}
	}
	if len(c.Capabilities) == 0 {
		out.Capabilities = nil
	} else {
		out.Capabilities = append([]string(nil), c.Capabilities...)
	}
	out.Pricing = c.Pricing.Clone()
	out.Compact.Pricing = c.Compact.Pricing.Clone()
	out.Strategy.Settings = model.CloneRaw(c.Strategy.Settings)
	return out
}

// ownUsageTotals returns an independent owned copy of one totals value.
func ownUsageTotals(t UsageTotals) UsageTotals {
	if len(t.ByModel) == 0 {
		return UsageTotals{}
	}
	out := UsageTotals{ByModel: make([]ModelUsage, len(t.ByModel))}
	copy(out.ByModel, t.ByModel)
	return out
}

// ownSessionRecord returns an independent owned copy of one Session record.
func ownSessionRecord(rec SessionRecord) SessionRecord {
	out := rec
	if rec.State.ArchivedAt != nil {
		stamped := *rec.State.ArchivedAt
		out.State.ArchivedAt = &stamped
	}
	out.State.Usage = ownUsageTotals(rec.State.Usage)
	return out
}

// ownOperationRecord returns an independent owned copy of one Operation record.
func ownOperationRecord(rec OperationRecord) OperationRecord {
	out := rec
	out.Admission.Execution = ownCapture(rec.Admission.Execution)
	out.State.Usage = ownUsageTotals(rec.State.Usage)
	out.State.PendingToolCalls = make([]PendingToolCall, len(rec.State.PendingToolCalls))
	copy(out.State.PendingToolCalls, rec.State.PendingToolCalls)
	if rec.State.ActiveEffect != nil {
		out.State.ActiveEffect = ownActiveEffect(rec.State.ActiveEffect)
	}
	if rec.State.Request != nil {
		request := *rec.State.Request
		if rec.State.Request.HookResults != nil {
			request.HookResults = append([]EntryRef(nil), rec.State.Request.HookResults...)
		}
		out.State.Request = &request
	}
	if rec.State.SettledAt != nil {
		stamped := *rec.State.SettledAt
		out.State.SettledAt = &stamped
	}
	if rec.State.Terminal != nil {
		terminal := *rec.State.Terminal
		out.State.Terminal = &terminal
	}
	return out
}

// ownActiveEffect returns an independent owned copy of one active effect:
// nested evidence, model pointee, raw selection bytes, and provenance lists
// are deep-copied so a public read can never alias the coordinator's state.
func ownActiveEffect(effect *ActiveEffect) *ActiveEffect {
	out := *effect
	if effect.Model != nil {
		modelRef := *effect.Model
		out.Model = &modelRef
	}
	if effect.Evidence != nil {
		out.Evidence = copyRequestEvidencePointer(effect.Evidence)
	}
	out.FinalArguments = model.CloneRaw(effect.FinalArguments)
	if effect.ContributionIDs != nil {
		out.ContributionIDs = append([]HookIdentity(nil), effect.ContributionIDs...)
	}
	return &out
}
