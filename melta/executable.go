package melta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extension/worker"
	"github.com/MMinasyan/lightcode/harness"
)

// executableExtension is one installed code selection: its declaration and the
// executable entry point that serves it. Package installation supplies it until
// the package loader owns that lifecycle.
type executableExtension struct {
	Declaration extension.Declaration
	EntryPoint  string
}

const (
	// executableQueuedFrames bounds the one outbound frame queue.
	executableQueuedFrames = 256

	// executableLiveCalls bounds requests and hostcalls awaiting completion.
	executableLiveCalls = 64

	// executableReadiness bounds activation readiness.
	executableReadiness = 30 * time.Second
)

// executableShutdownGrace is the cooperative cancellation/stop window before
// the process group is terminated. The production value is five seconds; tests
// may shorten it to exercise the same deadline path.
var executableShutdownGrace = 5 * time.Second

// executableCall is one host-issued call context's nested-call entry point.
type executableCall func(context.Context, string, string, json.RawMessage) (extension.Result, error)

// executableProcess is one spawned executable's protocol streams and process
// lifetime. Wait observes only the leader; the group is killed separately.
type executableProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.Reader
	Wait() error
	Kill() error
}

// executableParentCloser optionally closes the host-side pipe ends once the
// activation terminates.
type executableParentCloser interface {
	CloseParent() error
}

// spawnExecutable is the process construction seam. Tests replace it with an
// in-process adapter or controlled barrier process.
var spawnExecutable = func(_ context.Context, path string, environment []string) (executableProcess, error) {
	return spawnExecutableProcess(path, environment)
}

// executableSupervisor owns the one live activation per executable extension.
// Resource sets retain an activation while they use it; the last release
// stops it. A failed activation remains failed while any set retains it and is
// never restarted.
type executableSupervisor struct {
	environment func(context.Context, []string) ([]string, error)

	mu          sync.Mutex
	activations map[string]*executableActivation
}

func newExecutableSupervisor(environment func(context.Context, []string) ([]string, error)) *executableSupervisor {
	return &executableSupervisor{environment: environment, activations: make(map[string]*executableActivation)}
}

// acquire retains the executable's activation, starting it when no set holds
// one. A failed live activation reports its failure instead of restarting.
func (s *executableSupervisor) acquire(ctx context.Context, executable executableExtension, excluded []string) error {
	id := executable.Declaration.ID
	s.mu.Lock()
	activation, ok := s.activations[id]
	if ok {
		activation.refs++
		s.mu.Unlock()
		<-activation.ready
		activation.mu.Lock()
		failed := activation.failed
		activation.mu.Unlock()
		return failed
	}
	activation = newExecutableActivation(executable, s.environment)
	activation.refs = 1
	s.activations[id] = activation
	s.mu.Unlock()
	return activation.start(ctx, excluded)
}

// release drops one set's hold and joins the termination after the last one.
func (s *executableSupervisor) release(id string) {
	s.mu.Lock()
	activation := s.activations[id]
	if activation == nil {
		s.mu.Unlock()
		return
	}
	activation.refs--
	last := activation.refs == 0
	if last {
		delete(s.activations, id)
	}
	s.mu.Unlock()
	if last {
		activation.stop()
	}
}

// live returns the usable activation for one executable.
func (s *executableSupervisor) live(id string) (*executableActivation, error) {
	s.mu.Lock()
	activation := s.activations[id]
	s.mu.Unlock()
	if activation == nil {
		return nil, fmt.Errorf("executable extension %q has no activation: %w", id, extension.ErrClosed)
	}
	activation.mu.Lock()
	failed, closed := activation.failed, activation.closed
	activation.mu.Unlock()
	if failed != nil {
		return nil, failed
	}
	if closed {
		return nil, extension.ErrClosed
	}
	return activation, nil
}

// outboundFrame is one queued frame. request names the pending request whose
// delivery knowledge is recorded when the frame is successfully written.
type outboundFrame struct {
	encoded []byte
	request *pendingRequest
}

// pendingRequest is one request awaiting its terminal. Its outcome is stored
// once, under mu, before any waiter can observe completion.
type pendingRequest struct {
	id       string
	token    string
	validate func(json.RawMessage) (any, error)
	done     chan struct{}

	mu        sync.Mutex
	claimed   bool
	sent      bool
	cancelled bool
	cancelErr error
	outcome   *requestOutcome
}

// requestOutcome is one request's terminal result or failure, together with the
// delivery and host-cancellation knowledge the caller needs for settlement.
type requestOutcome struct {
	value         any
	failure       *worker.Error
	err           error
	evidence      harness.ExecutionEvidence
	hostCancelled bool
}

// executableActivation is one supervised worker process and its protocol state.
type executableActivation struct {
	declaration extension.Declaration
	entryPoint  string
	environment func(context.Context, []string) ([]string, error)
	process     executableProcess
	writer      *worker.Writer

	out        chan outboundFrame
	stopc      chan struct{}
	done       chan struct{}
	terminated chan struct{}
	ready      chan struct{}
	waitDone   chan struct{}
	writerDone chan struct{}
	readerDone chan struct{}

	cooperativeOnce sync.Once

	mu            sync.Mutex
	refs          int
	next          uint64
	live          int
	failed        error
	closed        bool
	termStarted   bool
	writerStopped bool
	pending       map[string]*pendingRequest
	tokens        map[string]executableCall
	hostcalls     map[string]context.CancelFunc
}

func newExecutableActivation(executable executableExtension, environment func(context.Context, []string) ([]string, error)) *executableActivation {
	return &executableActivation{
		declaration: executable.Declaration,
		entryPoint:  executable.EntryPoint,
		environment: environment,
		out:         make(chan outboundFrame, executableQueuedFrames),
		done:        make(chan struct{}),
		terminated:  make(chan struct{}),
		ready:       make(chan struct{}),
		stopc:       make(chan struct{}),
		waitDone:    make(chan struct{}),
		writerDone:  make(chan struct{}),
		readerDone:  make(chan struct{}),
		pending:     make(map[string]*pendingRequest),
		tokens:      make(map[string]executableCall),
		hostcalls:   make(map[string]context.CancelFunc),
	}
}

// start spawns the process and completes the exact-version activation
// handshake before any instance opens.
func (a *executableActivation) start(ctx context.Context, excluded []string) (err error) {
	defer close(a.ready)
	a.mu.Lock()
	closed, failed := a.closed, a.failed
	a.mu.Unlock()
	if closed {
		if failed == nil {
			failed = extension.ErrClosed
		}
		return failed
	}
	environment, err := a.environment(ctx, excluded)
	if err != nil {
		a.startTermination(err, false)
		return err
	}
	process, err := spawnExecutable(ctx, a.entryPoint, environment)
	if err != nil {
		a.startTermination(err, false)
		return err
	}
	a.mu.Lock()
	if a.closed {
		failed = a.failed
		a.mu.Unlock()
		_ = process.Kill()
		if failed == nil {
			failed = extension.ErrClosed
		}
		return failed
	}
	a.process = process
	a.writer = worker.NewWriter(process.Stdin())
	a.mu.Unlock()
	go a.writeLoop()
	go a.waitLoop()
	go a.readLoop(worker.NewReader(process.Stdout()))

	activateCtx, cancel := context.WithTimeout(ctx, executableReadiness)
	defer cancel()
	activated := make(chan requestOutcome, 1)
	go func() {
		activated <- a.request(activateCtx, worker.MethodActivate, func(string) (any, error) {
			return worker.ActivateParams{ContractVersion: extension.ContractVersion}, nil
		}, nil, a.activateResponse)
	}()
	var outcome requestOutcome
	select {
	case outcome = <-activated:
	case <-activateCtx.Done():
		a.startTermination(activateCtx.Err(), false)
		outcome = <-activated
	}
	if outcome.err != nil || outcome.failure != nil {
		return a.activationError(outcome)
	}
	return nil
}

// activationError returns the failure observed by a request.
func (a *executableActivation) activationError(outcome requestOutcome) error {
	if outcome.err != nil {
		return outcome.err
	}
	if outcome.failure != nil {
		return outcome.failure
	}
	a.mu.Lock()
	failed := a.failed
	a.mu.Unlock()
	return failed
}

// activateResponse validates the activation declaration.
func (a *executableActivation) activateResponse(raw json.RawMessage) (any, error) {
	var result worker.ActivateResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("activation result: %w", err)
	}
	wantDeclaration, err := json.Marshal(worker.NewDeclaration(a.declaration))
	if err != nil {
		return nil, err
	}
	gotDeclaration, err := json.Marshal(result.Declaration)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(gotDeclaration, wantDeclaration) {
		return nil, errors.New("activation declaration differs from the supplied declaration")
	}
	return result, nil
}

// request sends one host request and waits for its validated terminal. build
// receives the host-issued token when call is non-nil; only that token grants a
// hostcall.
func (a *executableActivation) request(ctx context.Context, method string, build func(token string) (any, error), call executableCall, decode func(json.RawMessage) (any, error)) requestOutcome {
	if ctx == nil {
		ctx = context.Background()
	}
	a.mu.Lock()
	if a.closed {
		failed := a.failed
		a.mu.Unlock()
		if failed == nil {
			failed = extension.ErrClosed
		}
		return requestOutcome{err: failed, evidence: harness.ExecutionNotStarted}
	}
	if a.live >= executableLiveCalls {
		a.mu.Unlock()
		return requestOutcome{err: &worker.Error{Class: worker.ClassUnavailable, Message: "executable extension has too many live calls"}, evidence: harness.ExecutionNotStarted}
	}
	id := "h" + strconv.FormatUint(a.next, 10)
	a.next++
	token := ""
	if call != nil {
		minted, err := newInstanceID()
		if err != nil {
			a.mu.Unlock()
			return requestOutcome{err: err, evidence: harness.ExecutionNotStarted}
		}
		token = minted
		a.tokens[token] = call
	}
	request := &pendingRequest{id: id, token: token, validate: decode, done: make(chan struct{})}
	a.pending[id] = request
	a.live++
	a.mu.Unlock()

	params, err := build(token)
	if err != nil {
		a.settle(request, requestOutcome{err: err, evidence: harness.ExecutionNotStarted})
		return requestOutcome{err: err, evidence: harness.ExecutionNotStarted}
	}
	encodedParams, err := json.Marshal(params)
	if err != nil {
		a.settle(request, requestOutcome{err: err, evidence: harness.ExecutionNotStarted})
		return requestOutcome{err: err, evidence: harness.ExecutionNotStarted}
	}
	encoded, err := (worker.Frame{Kind: worker.KindRequest, ID: id, Method: method, Params: encodedParams}).Encode()
	if err != nil {
		a.settle(request, requestOutcome{err: err, evidence: harness.ExecutionNotStarted})
		return requestOutcome{err: err, evidence: harness.ExecutionNotStarted}
	}
	if err := a.enqueueRequest(outboundFrame{encoded: encoded, request: request}); err != nil {
		outcome := requestOutcome{err: err, evidence: harness.ExecutionNotStarted}
		a.settle(request, outcome)
		return outcome
	}

	ctxDone := ctx.Done()
	var cancelTimer *time.Timer
	var cancelC <-chan time.Time
	defer func() {
		if cancelTimer != nil {
			cancelTimer.Stop()
		}
	}()
	for {
		select {
		case <-request.done:
			outcome, _ := request.stored()
			return outcome
		case <-a.done:
			if outcome, ok := request.stored(); ok {
				return outcome
			}
			a.mu.Lock()
			failed := a.failed
			a.mu.Unlock()
			if failed == nil {
				failed = extension.ErrClosed
			}
			return requestOutcome{err: failed, evidence: harness.ExecutionNotStarted}
		case <-ctxDone:
			ctxDone = nil
			if outcome, ok := request.stored(); ok {
				return outcome
			}
			if request.cancelUnclaimed(ctx.Err()) {
				a.settle(request, requestOutcome{err: ctx.Err(), evidence: harness.ExecutionNotStarted, hostCancelled: true})
				outcome, _ := request.stored()
				return outcome
			}
			a.enqueueCancel(id)
			if cancelTimer == nil {
				cancelTimer = time.NewTimer(executableShutdownGrace)
				cancelC = cancelTimer.C
			}
		case <-cancelC:
			cancelC = nil
			if _, ok := request.stored(); ok {
				continue
			}
			a.startTermination(fmt.Errorf("request %s cancelled: %w", id, ctx.Err()), false)
		}
	}
}

// enqueueRequest queues one request frame; a full or closed queue loses only
// this call.
func (a *executableActivation) enqueueRequest(item outboundFrame) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return extension.ErrClosed
	}
	select {
	case a.out <- item:
		return nil
	default:
		return &worker.Error{Class: worker.ClassUnavailable, Message: "executable extension frame queue is full"}
	}
}

// enqueueTerminal queues one cancel or hostcall response frame, including
// during the cooperative stop window. Terminal frames are never written
// synchronously by the reader.
func (a *executableActivation) enqueueTerminal(encoded []byte) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.writerStopped {
		return extension.ErrClosed
	}
	select {
	case a.out <- outboundFrame{encoded: encoded}:
		return nil
	default:
		return &worker.Error{Class: worker.ClassUnavailable, Message: "executable extension frame queue is full"}
	}
}

// enqueueCancel queues one best-effort cancellation frame.
func (a *executableActivation) enqueueCancel(id string) {
	encoded, err := (worker.Frame{Kind: worker.KindCancel, ID: id}).Encode()
	if err != nil {
		return
	}
	_ = a.enqueueTerminal(encoded)
}

// cancelUnclaimed settles a request cancelled before the writer claimed it.
// It returns false when the writer already claimed the frame or an outcome
// exists; then the request waits for its terminal or the grace window.
func (p *pendingRequest) cancelUnclaimed(err error) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.outcome != nil {
		return false
	}
	if p.claimed {
		p.cancelErr = err
		return false
	}
	p.cancelled = true
	p.cancelErr = err
	return true
}

func (p *pendingRequest) store(outcome requestOutcome) bool {
	p.mu.Lock()
	if p.outcome != nil {
		p.mu.Unlock()
		return false
	}
	p.outcome = &outcome
	p.mu.Unlock()
	close(p.done)
	return true
}

func (p *pendingRequest) stored() (requestOutcome, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.outcome == nil {
		return requestOutcome{}, false
	}
	return *p.outcome, true
}

// settle stores one request's outcome exactly once and releases its token and
// live-call slot.
func (a *executableActivation) settle(request *pendingRequest, outcome requestOutcome) {
	if !request.store(outcome) {
		return
	}
	a.mu.Lock()
	delete(a.pending, request.id)
	if request.token != "" {
		delete(a.tokens, request.token)
	}
	a.live--
	a.mu.Unlock()
}

// abortOutcome settles one remaining request after termination; only a frame
// that was successfully written may have executed.
func (a *executableActivation) abortOutcome(request *pendingRequest, cause error) requestOutcome {
	request.mu.Lock()
	sent := request.sent
	cancelErr := request.cancelErr
	request.mu.Unlock()
	evidence := harness.ExecutionNotStarted
	if sent {
		evidence = harness.ExecutionUnknown
	}
	if cancelErr != nil {
		return requestOutcome{err: cancelErr, evidence: evidence, hostCancelled: true}
	}
	if cause == nil {
		cause = extension.ErrClosed
	}
	return requestOutcome{err: cause, evidence: evidence}
}

// writeLoop serializes every outbound frame and records successful writes.
func (a *executableActivation) writeLoop() {
	defer close(a.writerDone)
	for {
		select {
		case <-a.stopc:
			return
		case item := <-a.out:
			if item.request != nil {
				request := item.request
				request.mu.Lock()
				if request.outcome != nil || request.cancelled {
					cancelErr := request.cancelErr
					request.mu.Unlock()
					if cancelErr == nil {
						cancelErr = context.Canceled
					}
					a.settle(request, requestOutcome{err: cancelErr, evidence: harness.ExecutionNotStarted, hostCancelled: true})
					continue
				}
				// The writer claims the frame and records its transmission
				// attempt atomically; cancellation after this point waits.
				request.claimed = true
				request.sent = true
				request.mu.Unlock()
			}
			if err := a.writer.WriteEncoded(item.encoded); err != nil {
				a.startTermination(fmt.Errorf("worker write: %w", err), false)
				return
			}
		}
	}
}

// waitLoop reaps the leader independently of reading. Leader exit without a
// started termination is an activation failure.
func (a *executableActivation) waitLoop() {
	err := a.process.Wait()
	close(a.waitDone)
	a.mu.Lock()
	started := a.termStarted
	a.mu.Unlock()
	if started {
		return
	}
	if err != nil {
		a.startTermination(fmt.Errorf("worker exited: %w", err), false)
		return
	}
	a.startTermination(extension.ErrClosed, false)
}

// readLoop decodes peer frames until EOF. The reader drains terminals even
// while the writer is blocked.
func (a *executableActivation) readLoop(reader *worker.Reader) {
	defer close(a.readerDone)
	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				a.startTermination(fmt.Errorf("worker read: %w", err), false)
			} else {
				a.startTermination(extension.ErrClosed, false)
			}
			return
		}
		if err := a.handleFrame(frame); err != nil {
			a.startTermination(err, false)
			return
		}
	}
}

// handleFrame routes one peer frame; any invalid routing fails the activation.
func (a *executableActivation) handleFrame(frame worker.Frame) error {
	switch frame.Kind {
	case worker.KindRequest:
		if frame.Method != worker.MethodHostcall {
			return fmt.Errorf("worker: request method %q is not a hostcall", frame.Method)
		}
		return a.hostcall(frame)
	case worker.KindResponse:
		return a.response(frame)
	case worker.KindCancel:
		a.cancelHostcall(frame.ID)
		return nil
	case worker.KindProgress:
		return nil
	default:
		return fmt.Errorf("worker: unknown frame kind %q", frame.Kind)
	}
}

// response validates one terminal and stores its outcome. Any invalid terminal
// fails the activation; a validated result is never overwritten.
func (a *executableActivation) response(frame worker.Frame) error {
	if frame.ID == "" {
		return errors.New("worker: response without an id")
	}
	a.mu.Lock()
	request := a.pending[frame.ID]
	a.mu.Unlock()
	if request == nil {
		return fmt.Errorf("worker: response to unknown request %q", frame.ID)
	}
	if (frame.Result == nil) == (frame.Error == nil) {
		return fmt.Errorf("worker: response %q must carry exactly one of result or error", frame.ID)
	}
	if frame.Error != nil {
		if !frame.Error.Class.Valid() {
			return fmt.Errorf("worker: response %q carries unknown error class %q", frame.ID, frame.Error.Class)
		}
		request.mu.Lock()
		hostCancelled := request.cancelErr != nil
		request.mu.Unlock()
		a.settle(request, requestOutcome{failure: frame.Error, evidence: harness.ExecutionUnknown, hostCancelled: hostCancelled})
		return nil
	}
	value, err := request.validate(frame.Result)
	if err != nil {
		return fmt.Errorf("worker: response %q: %w", frame.ID, err)
	}
	a.settle(request, requestOutcome{value: value, evidence: harness.ExecutionObserved})
	return nil
}

// hostcall resolves one worker callback through its token and queues its
// response through the bounded writer.
func (a *executableActivation) hostcall(frame worker.Frame) error {
	if frame.ID == "" {
		return errors.New("worker: hostcall without an id")
	}
	var params worker.HostcallParams
	if err := json.Unmarshal(frame.Params, &params); err != nil {
		return fmt.Errorf("worker: hostcall params: %w", err)
	}
	if params.Context.Token == "" {
		return errors.New("worker: hostcall without a context token")
	}
	if params.Binding == "" || params.Method == "" {
		return errors.New("worker: hostcall without a binding or method")
	}
	a.mu.Lock()
	call := a.tokens[params.Context.Token]
	if call == nil {
		a.mu.Unlock()
		a.respondError(frame.ID, &worker.Error{Class: worker.ClassClosed, Message: "hostcall token is closed"})
		return nil
	}
	if len(a.hostcalls) >= executableLiveCalls {
		a.mu.Unlock()
		return fmt.Errorf("worker: more than %d live hostcalls", executableLiveCalls)
	}
	if _, duplicate := a.hostcalls[frame.ID]; duplicate {
		a.mu.Unlock()
		return fmt.Errorf("worker: duplicate hostcall id %q", frame.ID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	a.hostcalls[frame.ID] = cancel
	a.mu.Unlock()

	go func() {
		result, err := call(ctx, params.Binding, params.Method, append(json.RawMessage(nil), params.Arguments...))
		a.mu.Lock()
		delete(a.hostcalls, frame.ID)
		a.mu.Unlock()
		cancel()
		if err != nil {
			a.respondError(frame.ID, executableFailure(err))
			return
		}
		a.respondResult(frame.ID, result)
	}()
	return nil
}

// respondResult queues one hostcall success response.
func (a *executableActivation) respondResult(id string, result extension.Result) {
	encoded, err := worker.EncodeServiceResult(result)
	if err != nil {
		a.respondError(id, executableFailure(err))
		return
	}
	a.respondFrame(worker.Frame{Kind: worker.KindResponse, ID: id, Result: encoded})
}

// executableFailure renders one host failure with the same closed class both
// sides use: melta.ErrorCode is the one classifier, and worker classes are the
// public error codes.
func executableFailure(err error) *worker.Error {
	return &worker.Error{Class: worker.Class(ErrorCode(err)), Message: err.Error()}
}

func (a *executableActivation) respondError(id string, failure *worker.Error) {
	a.respondFrame(worker.Frame{Kind: worker.KindResponse, ID: id, Error: failure})
}

func (a *executableActivation) respondFrame(frame worker.Frame) {
	encoded, err := frame.Encode()
	if errors.Is(err, worker.ErrFrameTooLarge) {
		encoded, err = (worker.Frame{Kind: worker.KindResponse, ID: frame.ID, Error: &worker.Error{Class: worker.ClassInternal, Message: "response exceeds the frame limit"}}).Encode()
	}
	if err != nil {
		a.startTermination(err, false)
		return
	}
	if err := a.enqueueTerminal(encoded); err != nil {
		a.startTermination(err, false)
	}
}

func (a *executableActivation) cancelHostcall(id string) {
	a.mu.Lock()
	cancel := a.hostcalls[id]
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// startTermination closes admission and starts the one termination procedure.
// It never blocks the reader or writer initiating it.
func (a *executableActivation) startTermination(cause error, cooperative bool) {
	a.mu.Lock()
	if a.termStarted {
		a.mu.Unlock()
		return
	}
	a.termStarted = true
	a.closed = true
	if cause == nil {
		cause = extension.ErrClosed
	}
	a.failed = cause
	a.mu.Unlock()
	go a.terminateRun(cause, cooperative)
}

// terminateRun is the one termination procedure: stop cooperatively when
// requested, always kill the process group, wait for the leader and the reader
// to finish, then settle every remaining request.
func (a *executableActivation) terminateRun(cause error, cooperative bool) {
	a.mu.Lock()
	process := a.process
	a.mu.Unlock()
	if process == nil {
		a.finishTermination(cause)
		return
	}
	if cooperative {
		a.beginCooperative()
		select {
		case <-a.waitDone:
		case <-time.After(executableShutdownGrace):
		}
	}
	a.killProcess()
	<-a.waitDone
	<-a.readerDone
	a.finishTermination(cause)
}

// beginCooperative closes stdin once, allowing the worker to finish its
// admitted work before the grace window expires.
func (a *executableActivation) beginCooperative() {
	a.cooperativeOnce.Do(func() {
		a.mu.Lock()
		process := a.process
		a.mu.Unlock()
		if process != nil {
			_ = process.Stdin().Close()
		}
	})
}

// killProcess terminates the process group, including descendants after the
// leader has exited.
func (a *executableActivation) killProcess() {
	a.mu.Lock()
	process := a.process
	a.mu.Unlock()
	if process != nil {
		_ = process.Kill()
	}
}

// finishTermination settles every remaining request with its delivery
// knowledge and cause, then releases waiters only after the group is gone.
func (a *executableActivation) finishTermination(cause error) {
	a.mu.Lock()
	a.writerStopped = true
	pending := make([]*pendingRequest, 0, len(a.pending))
	for _, request := range a.pending {
		pending = append(pending, request)
	}
	a.tokens = make(map[string]executableCall)
	hostcalls := a.hostcalls
	a.hostcalls = make(map[string]context.CancelFunc)
	a.mu.Unlock()
	for _, cancel := range hostcalls {
		cancel()
	}
	for _, request := range pending {
		a.settle(request, a.abortOutcome(request, cause))
	}
	close(a.done)
	close(a.stopc)
	a.mu.Lock()
	writerStarted := a.writer != nil
	process := a.process
	a.mu.Unlock()
	if writerStarted {
		select {
		case <-a.writerDone:
		case <-time.After(executableShutdownGrace):
		}
	}
	if closer, ok := process.(executableParentCloser); ok {
		_ = closer.CloseParent()
	}
	close(a.terminated)
}

// stop starts or joins the one cooperative termination procedure.
func (a *executableActivation) stop() {
	a.startTermination(extension.ErrClosed, true)
	<-a.terminated
}

// spawnExecutableProcess starts one entry-point process with the supplied
// secret-excluded environment, owned process group, and private pipes so Wait
// is independent of reading.
func spawnExecutableProcess(path string, environment []string) (executableProcess, error) {
	if environment == nil {
		return nil, errors.New("executable environment producer returned no environment")
	}
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
		return nil, err
	}
	command := exec.Command(path) // #nosec G204 -- the entry point is the operator-selected package executable.
	command.Env = environment
	command.Stderr = os.Stderr
	command.SysProcAttr = executableProcAttr()
	command.Stdin = stdinReader
	command.Stdout = stdoutWriter
	if err := command.Start(); err != nil {
		_ = stdinReader.Close()
		_ = stdinWriter.Close()
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		return nil, err
	}
	_ = stdinReader.Close()
	_ = stdoutWriter.Close()
	return &osExecutableProcess{command: command, stdin: stdinWriter, stdout: stdoutReader}, nil
}

type osExecutableProcess struct {
	command   *exec.Cmd
	stdin     io.WriteCloser
	stdout    io.Reader
	closeOnce sync.Once
}

func (p *osExecutableProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *osExecutableProcess) Stdout() io.Reader     { return p.stdout }
func (p *osExecutableProcess) Wait() error           { return p.command.Wait() }
func (p *osExecutableProcess) Kill() error           { return killExecutableProcess(p.command) }

// CloseParent closes the host-side stdin/stdout ends after termination.
func (p *osExecutableProcess) CloseParent() error {
	p.closeOnce.Do(func() {
		_ = p.stdin.Close()
		if closer, ok := p.stdout.(io.Closer); ok {
			_ = closer.Close()
		}
	})
	return nil
}
