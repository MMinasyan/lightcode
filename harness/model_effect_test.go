// Contract coverage for the single Harness model-effect path over the fixed
// physical transport: real httptest requests drive model.Transport as the
// Execution.Model callback, zero-delay policies and cancellation prove the
// attempt loop, and the private standard classifier is pinned directly. Every
// settlement here is Harness-derived from the accepted stream; no test
// supplies one.
package harness

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// transportAttempt binds the fixed model.Transport to the Execution.Model
// physical-request shape: exactly one physical attempt per invocation. The
// protocol-warning return is dropped until its Phase 7 observation consumer
// exists.
func transportAttempt(tr *model.Transport) func(context.Context, model.Request) (model.Stream, error) {
	return func(ctx context.Context, req model.Request) (model.Stream, error) {
		stream, _, err := tr.Stream(ctx, req, nil)
		return stream, err
	}
}

// scriptedResponses is the fixed response script of one httptest chat server:
// each request consumes the next entry, and the last entry repeats when the
// script is exhausted. A non-200 status answers the plain body text; a 200
// streams the events as SSE data lines followed by [DONE].
type scriptedResponses struct {
	status int
	events []string
}

// sseChatServer runs the script over one httptest server and reports every
// request through the returned atomic counter and buffered channel.
func sseChatServer(t *testing.T, script ...scriptedResponses) (*httptest.Server, *atomic.Int64, <-chan struct{}) {
	t.Helper()
	var hits atomic.Int64
	hits.Store(0)
	requests := make(chan struct{}, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		select {
		case requests <- struct{}{}:
		default:
		}
		response := script[len(script)-1]
		if int(n) <= len(script) {
			response = script[n-1]
		}
		if response.status != http.StatusOK {
			http.Error(w, "provider says no", response.status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range response.events {
			fmt.Fprintf(w, "data: %s\n\n", event)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, requests
}

// transportExecution builds the effect fixtures' execution whose Model is the
// real transport bound to the given server.
func transportExecution(srv *httptest.Server, retry RetryPolicy) Execution {
	tr, err := model.NewTransport(model.ResolvedTransport{Model: testModelRef(), BaseURL: srv.URL})
	if err != nil {
		panic(fmt.Sprintf("transport construction failed for a valid resolved input: %v", err))
	}
	exec := effectExecution(transportAttempt(tr), nil)
	exec.Retry = retry
	return exec
}

// retrySpy is the fixtures' captured retry policy: up to limit retries of any
// failure with zero delay, recording every observed failure and 1-based
// attempt number.
type retrySpy struct {
	limit    int
	seen     []error
	attempts []int
}

func (s *retrySpy) retry(cause error, failed int) (time.Duration, bool) {
	s.seen = append(s.seen, cause)
	s.attempts = append(s.attempts, failed)
	if failed <= s.limit {
		return 0, true
	}
	return 0, false
}

// statusErrorOf asserts the failure is the transport's 429 status error.
func statusErrorOf(t *testing.T, cause error) *model.HTTPStatusError {
	t.Helper()
	var status *model.HTTPStatusError
	testassert.Require(t, errors.As(cause, &status) && status.StatusCode == http.StatusTooManyRequests, "attempt failure %v, want the transport's 429 status error", cause)
	return status
}

// requireIntentAndResultCounts asserts the durable shape of one real settled
// turn: exactly one terminal settlement and, when the accepted output carries a
// payload, exactly one assistant entry consuming the intent; the intent is
// cleared.
func requireIntentAndResultCounts(t *testing.T, h *Harness, store *graphStorage, sessionID string, assistant bool) {
	t.Helper()
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	assistants, settlements := 0, 0
	for _, entry := range graph.Entries {
		if entry.Assistant != nil {
			assistants++
		}
		if entry.Settlement != nil {
			settlements++
		}
	}
	if assistant {
		testassert.Require(t, assistants == 1 && settlements == 1, "committed %d assistants and %d settlements, want the one payload entry beside the terminal", assistants, settlements)
	} else {
		testassert.Require(t, assistants == 0 && settlements == 1, "committed %d assistants and %d settlements, want the one terminal without a payload entry", assistants, settlements)
	}
	rec, err := h.ReadOperation(context.Background(), sessionID, testOpID)
	testassert.NoError(t, err)
	testassert.Require(t, rec.State.Terminal != nil && rec.State.ActiveEffect == nil, "settled run = %+v, want a terminal and no active intent", rec.State)
}

// TestModelEffectRetriesTransportFailures proves the retry row over the real
// transport: two 429 responses retry under the captured zero-delay policy with
// 1-based attempt numbers, the accepted stream settles ready through exactly
// one assembly, and one intent/result pair commits.
func TestModelEffectRetriesTransportFailures(t *testing.T) {
	srv, hits, _ := sseChatServer(t,
		scriptedResponses{status: http.StatusTooManyRequests},
		scriptedResponses{status: http.StatusTooManyRequests},
		scriptedResponses{status: http.StatusOK, events: []string{ // one completed text turn reporting usage
			`{"choices":[{"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2}}`,
		}},
	)
	spy := &retrySpy{limit: 2}
	h, store, c, sessionID := newEffectHarness(t, nil)
	exec := transportExecution(srv, spy.retry)
	testassert.NoError(t, executeDirect(h, c, context.Background(), testOpID, preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	})))
	if hits.Load() != 3 {
		t.Fatalf("physical requests = %d, want the two retries plus the accepted attempt", hits.Load())
	}
	testassert.Require(t, len(spy.attempts) == 2 && spy.attempts[0] == 1 && spy.attempts[1] == 2, "attempt numbers = %v, want the 1-based failed attempts [1 2]", spy.attempts)
	statusErrorOf(t, spy.seen[0])
	statusErrorOf(t, spy.seen[1])
	requireIntentAndResultCounts(t, h, store, sessionID, true)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	var assistant *assistantEntry
	for i := range graph.Entries {
		if graph.Entries[i].Assistant != nil {
			assistant = graph.Entries[i].Assistant
		}
	}
	testassert.Require(t, assistant != nil && assistant.Status == model.OutputCompleted && assistant.Content[0].Text == "done", "assistant entry = %+v, want the accepted turn's completed payload", assistant)
}

// TestModelEffectExhaustedRetriesSettleFailure proves the exhaustion row: an
// always-failing provider with a bounded policy settles the committed failure
// settlement with the transport's diagnostic, returned with a nil error, and
// the durable terminal carries exactly one settlement entry.
func TestModelEffectExhaustedRetriesSettleFailure(t *testing.T) {
	srv, hits, _ := sseChatServer(t, scriptedResponses{status: http.StatusTooManyRequests})
	spy := &retrySpy{limit: 2}
	h, store, c, sessionID := newEffectHarness(t, nil)
	exec := transportExecution(srv, spy.retry)
	testassert.NoError(t, executeDirect(h, c, context.Background(), testOpID, preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	})))
	if hits.Load() != 3 {
		t.Fatalf("physical requests = %d, want the initial attempt plus both retries", hits.Load())
	}
	testassert.Require(t, len(spy.attempts) == 3 && spy.attempts[2] == 3, "attempt numbers = %v, want the policy consulted through attempt 3", spy.attempts)
	requireIntentAndResultCounts(t, h, store, sessionID, false)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	settlement := settlementEntryOf(t, graph)
	testassert.Require(t, settlement.Status == OperationFailure && strings.Contains(settlement.Detail, "429"), "settlement = %+v, want the committed outputless failure with its 429 diagnostic", settlement)
}

// TestModelEffectConnectionFailureRetries proves the connection-failure row:
// a transport failure wrapping net.OpError is retryable under the captured
// policy, and an unretryable classification stops with the failure settlement.
func TestModelEffectConnectionFailureRetries(t *testing.T) {
	srv, _, _ := sseChatServer(t, scriptedResponses{status: http.StatusOK})
	srv.Close() // every dial now fails with a connection error wrapping net.OpError

	t.Run("retryable connection failure exhausts through the policy", func(t *testing.T) {
		spy := &retrySpy{limit: 1}
		h, store, c, sessionID := newEffectHarness(t, nil)
		exec := transportExecution(srv, spy.retry)
		testassert.NoError(t, executeDirect(h, c, context.Background(), testOpID, preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
			return exec, nil
		})))
		testassert.Require(t, len(spy.attempts) == 2 && spy.attempts[0] == 1 && spy.attempts[1] == 2, "attempt numbers = %v, want the one allowed retry", spy.attempts)
		var op *net.OpError
		testassert.Require(t, errors.As(spy.seen[0], &op), "attempt failure %v, want the transport's wrapped net.OpError", spy.seen[0])
		requireIntentAndResultCounts(t, h, store, sessionID, false)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		testassert.Require(t, settlementEntryOf(t, graph).Status == OperationFailure, "settlement = %+v, want the connection failure terminal", settlementEntryOf(t, graph))
	})

	t.Run("unretryable connection failure stops at the first attempt", func(t *testing.T) {
		spy := &retrySpy{limit: 0}
		h, store, c, sessionID := newEffectHarness(t, nil)
		exec := transportExecution(srv, spy.retry)
		testassert.NoError(t, executeDirect(h, c, context.Background(), testOpID, preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
			return exec, nil
		})))
		testassert.Require(t, len(spy.attempts) == 1, "attempt numbers = %v, want the false classification to stop at attempt 1", spy.attempts)
		requireIntentAndResultCounts(t, h, store, sessionID, false)
	})

	t.Run("a negative delay also means no retry", func(t *testing.T) {
		attempts := 0
		policy := func(error, int) (time.Duration, bool) {
			attempts++
			return -time.Second, true
		}
		h, store, c, sessionID := newEffectHarness(t, nil)
		exec := transportExecution(srv, policy)
		testassert.NoError(t, executeDirect(h, c, context.Background(), testOpID, preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
			return exec, nil
		})))
		testassert.Equal(t, attempts, 1)
		requireIntentAndResultCounts(t, h, store, sessionID, false)
	})
}

// TestModelEffectAttemptFailureUnderCancellationSettlesInterruption proves
// the pre-acceptance cancellation priority: an attempt failure observed under
// a done execution context settles the committed interruption regardless of
// the attempt error's shape or the retry policy's answer — no output, no
// assembly, and no second attempt.
func TestModelEffectAttemptFailureUnderCancellationSettlesInterruption(t *testing.T) {
	policyCalls := 0
	policy := func(error, int) (time.Duration, bool) {
		policyCalls++
		return 0, true // the supplied policy would retry every failure
	}
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	modelFn := func(context.Context, model.Request) (model.Stream, error) {
		attempts++
		cancel()                                   // the execution context dies during the attempt
		return nil, errors.New("provider dropped") // not cancellation-shaped
	}
	h, store, c, sessionID := newEffectHarness(t, nil)
	exec := effectExecution(modelFn, nil)
	exec.Retry = policy
	testassert.NoError(t, executeDirect(h, c, ctx, testOpID, preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	})))
	testassert.Require(t, attempts == 1 && policyCalls == 0, "attempts = %d policy calls = %d, want the done context to settle before any classification or second attempt", attempts, policyCalls)
	requireIntentAndResultCounts(t, h, store, sessionID, false)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	settlement := settlementEntryOf(t, graph)
	testassert.Require(t, settlement.Status == OperationInterruption && settlement.Detail == executionInterruptedDetail, "settlement = %+v, want the fixed interruption terminal", settlement)
}

// TestModelEffectNilRetryCancelsDuringStandardBackoff proves nil Retry selects
// the private standard classifier and the Harness owns the wait: the first
// 429 enters the standard 2s backoff, observed execution cancellation during
// the backoff settles the committed interruption with no further request and
// no assembly.
func TestModelEffectNilRetryCancelsDuringStandardBackoff(t *testing.T) {
	srv, hits, requests := sseChatServer(t, scriptedResponses{status: http.StatusTooManyRequests})
	h, store, c, sessionID := newEffectHarness(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-requests // the first attempt failed into the standard backoff
		time.Sleep(50 * time.Millisecond)
		cancel() // observed execution cancellation during the Harness-owned wait
	}()
	exec := transportExecution(srv, nil)
	started := time.Now()
	testassert.NoError(t, executeDirect(h, c, ctx, testOpID, preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	})))
	elapsed := time.Since(started)
	testassert.Require(t, elapsed < 2*time.Second, "cancellation during the 2s backoff returned after %v, want the wait interrupted promptly", elapsed)
	if hits.Load() != 1 {
		t.Fatalf("physical requests = %d, want no second attempt after the cancellation", hits.Load())
	}
	requireIntentAndResultCounts(t, h, store, sessionID, false)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	settlement := settlementEntryOf(t, graph)
	testassert.Require(t, settlement.Status == OperationInterruption && settlement.Detail == executionInterruptedDetail, "settlement = %+v, want the fixed interruption terminal", settlement)
}

// TestModelEffectStandardClassifierRows pins the private standard classifier:
// HTTP 429/5xx, errors wrapping net.OpError and pre-response
// io.EOF/io.ErrUnexpectedEOF permit retries 1..3 at 2s/4s/8s; every other
// failure stops.
func TestModelEffectStandardClassifierRows(t *testing.T) {
	retryable := []error{
		&model.HTTPStatusError{StatusCode: 429, StatusText: "429"},
		&model.HTTPStatusError{StatusCode: 500, StatusText: "500"},
		&model.HTTPStatusError{StatusCode: 503, StatusText: "503"},
		&net.OpError{Op: "dial"},
		fmt.Errorf("post: %w", &net.OpError{Op: "read"}),
		io.EOF,
		io.ErrUnexpectedEOF,
		fmt.Errorf("wrapped: %w", io.ErrUnexpectedEOF),
	}
	stopping := []error{
		&model.HTTPStatusError{StatusCode: 400, StatusText: "400"},
		&model.HTTPStatusError{StatusCode: 401, StatusText: "401"},
		&model.HTTPStatusError{StatusCode: 404, StatusText: "404"},
		&model.HTTPStatusError{StatusCode: 600, StatusText: "600"}, // beyond 5xx: the retryable status class is 429 plus 500..599, not every code >= 500
		errors.New("plain failure"),
		context.Canceled,
	}
	for _, cause := range retryable {
		for failed, want := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second} {
			delay, again := standardRetryPolicy(cause, failed+1)
			testassert.Require(t, again && delay == want, "standardRetryPolicy(%v, %d) = (%v, %v), want (%v, true)", cause, failed+1, delay, again, want)
		}
		{
			_, again := standardRetryPolicy(cause, 4)
			testassert.Require(t, !again, "standardRetryPolicy(%v, 4) retries, want the 1..3 bound to stop", cause)
		}
	}
	for _, cause := range stopping {
		{
			delay, again := standardRetryPolicy(cause, 1)
			testassert.Require(t, !again && delay == 0, "standardRetryPolicy(%v, 1) = (%v, %v), want no retry", cause, delay, again)
		}
	}
}

// TestModelEffectAcceptedStreamNeverRetries proves the acceptance boundary:
// an accepted stream whose read fails mid-body never re-enters physical
// retry — exactly one request — and its real classification stands: the
// retained partial commits with the fixed continuation signal and the next
// projection continues from the committed history.
func TestModelEffectAcceptedStreamNeverRetries(t *testing.T) {
	srv, hits, _ := sseChatServer(t, scriptedResponses{status: http.StatusOK})
	srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) > 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","content":"after"},"finish_reason":"stop"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"delta":{"role":"assistant","content":"partial"}}]}`+"\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // the partial event reaches the client before the sever
		}
		panic(http.ErrAbortHandler) // sever the accepted connection mid-body: a read failure, never a new request
	})
	spy := &retrySpy{limit: 5}
	h, store, c, sessionID := newEffectHarness(t, nil)
	exec := transportExecution(srv, spy.retry)
	testassert.NoError(t, executeDirect(h, c, context.Background(), testOpID, preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	})))
	testassert.Equal(t, len(spy.attempts), 0)
	graph, err := validateFixture(t, store, sessionID)
	testassert.NoError(t, err)
	var assistant *assistantEntry
	var signal *signalEntry
	for i := range graph.Entries {
		if graph.Entries[i].Assistant != nil {
			assistant = graph.Entries[i].Assistant
		}
		if graph.Entries[i].Signal != nil {
			signal = graph.Entries[i].Signal
		}
	}
	testassert.Require(t, assistant != nil && assistant.Status == model.OutputCompleted && assistant.Content[0].Text == "after", "last assistant entry = %+v, want the next round's completed turn", assistant)
	testassert.Require(t, signal != nil && signal.Signal == SignalModelFailureContinuation, "signal entry = %+v, want the fixed continuation signal", signal)
	graphCount, gerr := validateFixture(t, store, sessionID)
	testassert.NoError(t, gerr)
	assistants, settlements := 0, 0
	for _, entry := range graphCount.Entries {
		if entry.Assistant != nil {
			assistants++
		}
		if entry.Settlement != nil {
			settlements++
		}
	}
	testassert.Require(t, assistants == 2 && settlements == 1, "committed %d assistants and %d settlements, want the partial and the next turn beside the terminal", assistants, settlements)
	// Partial continuation: the next context projection reads the committed
	// partial from history ahead of the next round's turn.
	msgs, err := fixtureProjection(h, c)(context.Background())
	testassert.NoError(t, err)
	testassert.Require(t, len(msgs) >= 4 && msgs[2].Role == model.RoleAssistant && msgs[2].TextContent() == "partial", "projected partial = %+v, want the committed errored assistant", msgs)
}

// TestModelEffectOneAssemblyAndClose pins the ownership boundary on an
// accepted stream: the Harness invokes assembly exactly once and the stream
// closes exactly once, on every classification.
func TestModelEffectOneAssemblyAndClose(t *testing.T) {
	stream := completedTurnStream()
	h, store, c, sessionID := newEffectHarness(t, nil)
	exec := effectExecution(func(context.Context, model.Request) (model.Stream, error) {
		return stream, nil
	}, nil)
	testassert.NoError(t, executeDirect(h, c, context.Background(), testOpID, preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
		return exec, nil
	})))
	testassert.Equal(t, stream.closes, 1)
	requireIntentAndResultCounts(t, h, store, sessionID, true)
	assistant := compositionAssistant(t, store, sessionID)
	testassert.Require(t, assistant.Status == model.OutputCompleted && assistant.Content[0].Text == "done", "assistant entry = %+v, want the single assembled payload", assistant)
}

// TestModelEffectInvalidAttemptOutcomeSettlesBoundaryFailure proves the
// physical-request boundary: a callback returning both a stream and an error
// closes the supplied stream exactly once and settles the boundary failure,
// and one returning neither does the same without a close.
func TestModelEffectInvalidAttemptOutcomeSettlesBoundaryFailure(t *testing.T) {
	t.Run("both a stream and an error", func(t *testing.T) {
		stream := streamOf()
		h, store, c, sessionID := newEffectHarness(t, nil)
		exec := effectExecution(func(context.Context, model.Request) (model.Stream, error) {
			return stream, errors.New("ambiguous outcome")
		}, nil)
		err := executeDirect(h, c, context.Background(), testOpID, preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
			return exec, nil
		}))
		testassert.Require(t, err != nil, "run = nil, want the physical-request boundary failure")
		testassert.Equal(t, stream.closes, 1)
		requireIntentAndResultCounts(t, h, store, sessionID, false)
		graph, err := validateFixture(t, store, sessionID)
		testassert.NoError(t, err)
		testassert.Require(t, settlementEntryOf(t, graph).Status == OperationFailure, "settlement = %+v, want the boundary failure terminal", settlementEntryOf(t, graph))
	})

	t.Run("neither a stream nor an error", func(t *testing.T) {
		h, store, c, sessionID := newEffectHarness(t, nil)
		exec := effectExecution(func(context.Context, model.Request) (model.Stream, error) {
			return nil, nil
		}, nil)
		err := executeDirect(h, c, context.Background(), testOpID, preparedExecutionFor(testCapture(), func(context.Context, OperationAdmission) (Execution, error) {
			return exec, nil
		}))
		testassert.Require(t, err != nil, "run = nil, want the physical-request boundary failure")
		requireIntentAndResultCounts(t, h, store, sessionID, false)
	})
}
