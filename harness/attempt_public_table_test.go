// Public-surface shared attempt-table coverage: one table drives the real
// conversation and compact caller effects through the real Agent/Harness
// pipeline over the real memory and SQLite storage implementations. The
// keyed and keyless physical-request rows run the fixed model.Transport
// against a test-local endpoint; no new transport seam exists. The
// invalid-outcome-under-cancellation rows and their failure sibling prove the
// boundary failure outranks the observed cancellation while a real attempt
// failure under the same cancellation interrupts.
package harness_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/model"
)

// attemptTableFixture wires one Harness over the given store whose prepared
// execution supplies distinct conversation and compact physical callbacks and
// the given retry policy. Its cleanup is registered at construction: it
// cancels the Harness and joins Wait before the store's and server's own
// cleanups run (later-registered cleanups run first).
type attemptTableFixture struct {
	h *harness.Harness
}

func newAttemptTableFixture(t *testing.T, store harness.Storage, conversationFn, compactFn func(context.Context, model.Request) (model.Stream, error), retry harness.RetryPolicy) *attemptTableFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h, err := harness.New(ctx, harness.Dependencies{Storage: store, Prepare: func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
		return harness.PreparedExecution{
			Capture: publicCapture(),
			Open: func(context.Context, harness.OperationAdmission) (harness.Execution, error) {
				return harness.Execution{
					Model:         conversationFn,
					CompactModel:  compactFn,
					NormalizeTool: publicNormalize,
					Tool: func(_ context.Context, call model.ToolCall) harness.PreparedTool {
						return harness.PreparedTool{Immediate: &harness.ToolOutcome{Result: model.ToolResult{CallID: call.ID, Status: model.ResultError, Content: "no tools"}}}
					},
					Retry: retry,
				}, nil
			},
		}, nil
	}})
	if err != nil {
		cancel()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		if err := h.Wait(context.Background()); err != nil {
			t.Errorf("harness retirement: %v", err)
		}
	})
	return &attemptTableFixture{h: h}
}

// attemptTableCallbacks pairs one row's physical callbacks for the effect
// under test and the idle conversation callback the compact rows seed their
// history with.
func attemptTablePair(fn func(context.Context, model.Request) (model.Stream, error), conversation bool) (conversationFn, compactFn func(context.Context, model.Request) (model.Stream, error)) {
	if conversation {
		return fn, fn
	}
	return func(context.Context, model.Request) (model.Stream, error) {
		return attemptTableTurnStream(), nil
	}, fn
}

// attemptTableStream is one fake accepted model stream that counts Close so
// the table can pin exactly-once closure of a supplied stream.
type attemptTableStream struct {
	deltas []model.StreamDelta
	i      int
	closes int
}

func (s *attemptTableStream) Recv() (model.StreamDelta, error) {
	if s.i >= len(s.deltas) {
		return model.StreamDelta{}, io.EOF
	}
	d := s.deltas[s.i]
	s.i++
	return d, nil
}

func (s *attemptTableStream) Close() error { s.closes++; return nil }

// attemptTableTurnStream assembles to the completed turn "done" with usage.
func attemptTableTurnStream() *attemptTableStream {
	return &attemptTableStream{deltas: []model.StreamDelta{
		{HasChoice: true, Role: "assistant", ContentFragments: []model.ContentFragment{{Position: 0, Kind: model.PartText, Text: "done"}}, FinishReason: "stop"},
		{Usage: &model.Usage{InputTokens: 3, OutputTokens: 2}},
	}}
}

// attemptTableHTTPServer answers every request with one completed-turn SSE
// body and records the Authorization header and request count.
func attemptTableHTTPServer(t *testing.T) (*httptest.Server, *atomic.Value, *atomic.Int64) {
	t.Helper()
	var auth atomic.Value
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &auth, &hits
}

// attemptTableStatusServer answers every request with the given HTTP status
// and reports the request count and one signal per request.
func attemptTableStatusServer(t *testing.T, status int) (*httptest.Server, *atomic.Int64, <-chan struct{}) {
	t.Helper()
	var hits atomic.Int64
	requests := make(chan struct{}, 16)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case requests <- struct{}{}:
		default:
		}
		http.Error(w, "provider says no", status)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, requests
}

// attemptTableTransport binds the fixed model.Transport with the given
// credential to the physical-request shape.
func attemptTableTransport(t *testing.T, srv *httptest.Server, apiKey string) func(context.Context, model.Request) (model.Stream, error) {
	t.Helper()
	tr, err := model.NewTransport(model.ResolvedTransport{Model: publicModelRef, BaseURL: srv.URL, APIKey: apiKey})
	if err != nil {
		t.Fatalf("transport construction failed for a valid resolved input: %v", err)
	}
	return func(ctx context.Context, req model.Request) (model.Stream, error) {
		stream, _, err := tr.Stream(ctx, req, nil)
		return stream, err
	}
}

// attemptTableRunConversation drives one conversation Operation through the
// real pipeline and waits for its terminal settlement.
func attemptTableRunConversation(t *testing.T, f *attemptTableFixture, session, operation string) harness.OperationRecord {
	t.Helper()
	res, err := submit(t, f.h, session, operation, harness.MessageModeRegular, "hello")
	if err != nil || res.Disposition != harness.DispositionAdmitted {
		t.Fatalf("submit = %+v err %v, want admitted", res, err)
	}
	return awaitTerminal(t, f.h, session, operation)
}

// attemptTableSeedHistory runs one completed conversation turn so the Session
// carries compactable history and is idle again.
func attemptTableSeedHistory(t *testing.T, f *attemptTableFixture, session string) {
	t.Helper()
	if rec := attemptTableRunConversation(t, f, session, "op-seed"); rec.State.Status != harness.OperationSuccess {
		t.Fatalf("seed turn state = %+v, want success", rec.State)
	}
}

// attemptTableAdmitCompact admits one manual compact Operation, retrying the
// transient idle guard, without waiting for its settlement.
func attemptTableAdmitCompact(t *testing.T, f *attemptTableFixture, session, operation string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := f.h.Compact(context.Background(), harness.CompactRequest{SessionID: session, OperationID: operation}); err == nil {
			return
		} else if !strings.Contains(err.Error(), "is not idle; admission requires an idle Session") {
			t.Fatalf("Compact %s: %v", operation, err)
		} else if time.Now().After(deadline) {
			t.Fatalf("Compact %s never admitted within the wait bound", operation)
		}
		time.Sleep(time.Millisecond)
	}
}

// attemptTableRunCompact admits one manual compact Operation and waits for
// its terminal settlement.
func attemptTableRunCompact(t *testing.T, f *attemptTableFixture, session, operation string) harness.OperationRecord {
	t.Helper()
	attemptTableAdmitCompact(t, f, session, operation)
	return awaitTerminal(t, f.h, session, operation)
}

// attemptTableRequireTerminal asserts the operation's terminal status and
// detail.
func attemptTableRequireTerminal(t *testing.T, rec harness.OperationRecord, want harness.OperationState, detailContains string) {
	t.Helper()
	if rec.State.Status != want {
		t.Fatalf("operation state = %+v, want %s", rec.State, want)
	}
	if rec.State.Terminal == nil || !strings.Contains(rec.State.Terminal.Detail, detailContains) {
		t.Fatalf("operation terminal = %+v, want detail containing %q", rec.State.Terminal, detailContains)
	}
}

// parkedAttempt returns the park function one physical callback calls before
// it returns its outcome — the callback returns only after its own execution
// context is actually canceled — and the arrival signal the test waits on
// before interrupting.
func parkedAttempt() (park func(context.Context), arrived <-chan struct{}) {
	arrivedCh := make(chan struct{})
	park = func(ctx context.Context) {
		close(arrivedCh)
		<-ctx.Done() // the outcome returns only after the execution context is canceled
	}
	return park, arrivedCh
}

// interruptParkedAttempt waits until the physical callback parked, then
// interrupts the Session's running Operation: the execution context is
// certainly canceled before the callback returns its outcome.
func interruptParkedAttempt(t *testing.T, f *attemptTableFixture, session string, arrived <-chan struct{}) {
	t.Helper()
	<-arrived
	if err := f.h.Interrupt(context.Background(), session); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
}

// TestPublicSharedAttemptOrchestrationTable runs the one shared physical
// attempt table over both real caller effects and both real storage
// implementations: keyed/keyless physical requests, default-retry backoff
// cancellation, custom retry exhaustion, negative delay, the invalid-outcome
// ordering under cancellation against its failure sibling, and the
// one-intent-across-attempts ownership axis.
func TestPublicSharedAttemptOrchestrationTable(t *testing.T) {
	for _, storeKind := range []struct {
		name string
		open func(t *testing.T) harness.Storage
	}{
		{"memory", func(t *testing.T) harness.Storage { return storage.NewMemory() }},
		{"sqlite", func(t *testing.T) harness.Storage {
			store, err := storage.OpenSQLite(filepath.Join(t.TempDir(), "lightcode.db"))
			if err != nil {
				t.Fatalf("OpenSQLite: %v", err)
			}
			t.Cleanup(func() { store.Close() })
			return store
		}},
	} {
		for _, conversation := range []bool{true, false} {
			effectName := "conversation"
			if !conversation {
				effectName = "compact"
			}
			for _, row := range publicAttemptTableRows {
				row := row
				t.Run(storeKind.name+"/"+effectName+"/"+row.name, func(t *testing.T) {
					row.run(t, storeKind.open(t), conversation)
				})
			}
		}
	}
}

// publicAttemptTableRows are the shared rows; every row builds its own
// fixture over the given real store, drives the requested caller effect
// through the real pipeline once, and asserts the terminal settlement and
// durable shape.
var publicAttemptTableRows = []struct {
	name string
	run  func(t *testing.T, store harness.Storage, conversation bool)
}{
	{
		name: "keyed http accepted on the first attempt",
		run: func(t *testing.T, store harness.Storage, conversation bool) {
			srv, auth, hits := attemptTableHTTPServer(t)
			fn := attemptTableTransport(t, srv, "table-key")
			conversationFn, compactFn := attemptTablePair(fn, conversation)
			f := newAttemptTableFixture(t, store, conversationFn, compactFn, nil)
			session := createSession(t, f.h)
			if conversation {
				rec := attemptTableRunConversation(t, f, session, "op-1")
				attemptTableRequireTerminal(t, rec, harness.OperationSuccess, "")
				kinds := entryKindsByOperation(t, store, session, "op-1")
				if !slices.Contains(kinds, harness.EntryAssistant) {
					t.Fatalf("operation entries = %v, want the accepted turn's assistant entry", kinds)
				}
			} else {
				attemptTableSeedHistory(t, f, session)
				rec := attemptTableRunCompact(t, f, session, "c-1")
				attemptTableRequireTerminal(t, rec, harness.OperationSuccess, "")
				kinds := entryKindsByOperation(t, store, session, "c-1")
				if len(kinds) != 2 || kinds[0] != harness.EntryCompaction || kinds[1] != harness.EntryOperationSettlement {
					t.Fatalf("compact operation entries = %v, want the compaction entry and its settlement", kinds)
				}
			}
			if hits.Load() != 1 {
				t.Fatalf("physical requests = %d, want the one accepted attempt", hits.Load())
			}
			if got := auth.Load().(string); got != "Bearer table-key" {
				t.Fatalf("Authorization header = %q, want the keyed transport's credential", got)
			}
		},
	},
	{
		name: "keyless http accepted on the first attempt",
		run: func(t *testing.T, store harness.Storage, conversation bool) {
			srv, auth, hits := attemptTableHTTPServer(t)
			fn := attemptTableTransport(t, srv, "")
			conversationFn, compactFn := attemptTablePair(fn, conversation)
			f := newAttemptTableFixture(t, store, conversationFn, compactFn, nil)
			session := createSession(t, f.h)
			if conversation {
				rec := attemptTableRunConversation(t, f, session, "op-1")
				attemptTableRequireTerminal(t, rec, harness.OperationSuccess, "")
			} else {
				attemptTableSeedHistory(t, f, session)
				rec := attemptTableRunCompact(t, f, session, "c-1")
				attemptTableRequireTerminal(t, rec, harness.OperationSuccess, "")
			}
			if hits.Load() != 1 {
				t.Fatalf("physical requests = %d, want the one accepted attempt", hits.Load())
			}
			if got := auth.Load().(string); got != "" {
				t.Fatalf("Authorization header = %q, want none from the keyless transport", got)
			}
		},
	},
	{
		name: "default retry cancels during the standard backoff",
		run: func(t *testing.T, store harness.Storage, conversation bool) {
			srv, hits, requests := attemptTableStatusServer(t, http.StatusTooManyRequests)
			fn := attemptTableTransport(t, srv, "")
			conversationFn, compactFn := attemptTablePair(fn, conversation)
			f := newAttemptTableFixture(t, store, conversationFn, compactFn, nil)
			session := createSession(t, f.h)
			operation := "op-1"
			if conversation {
				res, err := submit(t, f.h, session, operation, harness.MessageModeRegular, "hello")
				if err != nil || res.Disposition != harness.DispositionAdmitted {
					t.Fatalf("submit = %+v err %v, want admitted", res, err)
				}
			} else {
				attemptTableSeedHistory(t, f, session)
				operation = "c-1"
				attemptTableAdmitCompact(t, f, session, operation)
			}
			<-requests // the first attempt failed into the standard backoff
			if err := f.h.Interrupt(context.Background(), session); err != nil {
				t.Fatalf("Interrupt: %v", err)
			}
			rec := awaitTerminal(t, f.h, session, operation)
			attemptTableRequireTerminal(t, rec, harness.OperationInterruption, "agent interrupted")
			if hits.Load() != 1 {
				t.Fatalf("physical requests = %d, want no second attempt after the cancellation", hits.Load())
			}
		},
	},
	{
		name: "custom retry exhaustion settles the committed failure",
		run: func(t *testing.T, store harness.Storage, conversation bool) {
			var requests, policyCalls atomic.Int64
			fn := func(context.Context, model.Request) (model.Stream, error) {
				requests.Add(1)
				return nil, errors.New("provider dropped")
			}
			retry := func(error, int) (time.Duration, bool) {
				n := policyCalls.Add(1)
				return 0, n <= 2
			}
			conversationFn, compactFn := attemptTablePair(fn, conversation)
			f := newAttemptTableFixture(t, store, conversationFn, compactFn, retry)
			session := createSession(t, f.h)
			operation := "op-1"
			if conversation {
				rec := attemptTableRunConversation(t, f, session, operation)
				attemptTableRequireTerminal(t, rec, harness.OperationFailure, "provider dropped")
				kinds := entryKindsByOperation(t, store, session, operation)
				if slices.Contains(kinds, harness.EntryAssistant) || !slices.Contains(kinds, harness.EntryOperationSettlement) {
					t.Fatalf("operation entries = %v, want the settlement entry and no assistant", kinds)
				}
			} else {
				attemptTableSeedHistory(t, f, session)
				operation = "c-1"
				rec := attemptTableRunCompact(t, f, session, operation)
				attemptTableRequireTerminal(t, rec, harness.OperationFailure, "provider dropped")
			}
			if requests.Load() != 3 || policyCalls.Load() != 3 {
				t.Fatalf("requests = %d policy calls = %d, want the policy consulted through attempt 3", requests.Load(), policyCalls.Load())
			}
		},
	},
	{
		name: "negative delay stops without retry",
		run: func(t *testing.T, store harness.Storage, conversation bool) {
			var requests, policyCalls atomic.Int64
			fn := func(context.Context, model.Request) (model.Stream, error) {
				requests.Add(1)
				return nil, errors.New("provider dropped")
			}
			retry := func(error, int) (time.Duration, bool) {
				policyCalls.Add(1)
				return -time.Second, true
			}
			conversationFn, compactFn := attemptTablePair(fn, conversation)
			f := newAttemptTableFixture(t, store, conversationFn, compactFn, retry)
			session := createSession(t, f.h)
			operation := "op-1"
			if conversation {
				rec := attemptTableRunConversation(t, f, session, operation)
				attemptTableRequireTerminal(t, rec, harness.OperationFailure, "provider dropped")
			} else {
				attemptTableSeedHistory(t, f, session)
				operation = "c-1"
				rec := attemptTableRunCompact(t, f, session, operation)
				attemptTableRequireTerminal(t, rec, harness.OperationFailure, "provider dropped")
			}
			if requests.Load() != 1 || policyCalls.Load() != 1 {
				t.Fatalf("requests = %d policy calls = %d, want the negative delay to stop at the first failure", requests.Load(), policyCalls.Load())
			}
		},
	},
	{
		name: "failure under cancellation settles the interruption",
		run: func(t *testing.T, store harness.Storage, conversation bool) {
			park, arrived := parkedAttempt()
			fn := func(ctx context.Context, _ model.Request) (model.Stream, error) {
				park(ctx)
				return nil, errors.New("provider dropped")
			}
			conversationFn, compactFn := attemptTablePair(fn, conversation)
			f := newAttemptTableFixture(t, store, conversationFn, compactFn, nil)
			session := createSession(t, f.h)
			operation := "op-1"
			if conversation {
				if _, err := submit(t, f.h, session, operation, harness.MessageModeRegular, "hello"); err != nil {
					t.Fatalf("submit: %v", err)
				}
			} else {
				attemptTableSeedHistory(t, f, session)
				operation = "c-1"
				attemptTableAdmitCompact(t, f, session, operation)
			}
			interruptParkedAttempt(t, f, session, arrived)
			rec := awaitTerminal(t, f.h, session, operation)
			attemptTableRequireTerminal(t, rec, harness.OperationInterruption, "agent interrupted")
		},
	},
	{
		name: "invalid both-outcome under cancellation settles the boundary failure",
		run: func(t *testing.T, store harness.Storage, conversation bool) {
			park, arrived := parkedAttempt()
			stream := &attemptTableStream{}
			fn := func(ctx context.Context, _ model.Request) (model.Stream, error) {
				park(ctx)
				return stream, errors.New("ambiguous outcome")
			}
			conversationFn, compactFn := attemptTablePair(fn, conversation)
			f := newAttemptTableFixture(t, store, conversationFn, compactFn, nil)
			session := createSession(t, f.h)
			operation := "op-1"
			if conversation {
				if _, err := submit(t, f.h, session, operation, harness.MessageModeRegular, "hello"); err != nil {
					t.Fatalf("submit: %v", err)
				}
			} else {
				attemptTableSeedHistory(t, f, session)
				operation = "c-1"
				attemptTableAdmitCompact(t, f, session, operation)
			}
			interruptParkedAttempt(t, f, session, arrived)
			rec := awaitTerminal(t, f.h, session, operation)
			// The invalid outcome outranks the observed cancellation: the
			// nearest sibling of failure-under-cancellation settles the
			// boundary failure, not the interruption.
			attemptTableRequireTerminal(t, rec, harness.OperationFailure, "neither exactly one stream nor one error")
			if stream.closes != 1 {
				t.Fatalf("supplied stream closes = %d, want exactly one before the boundary failure", stream.closes)
			}
		},
	},
	{
		name: "invalid neither-outcome under cancellation settles the boundary failure",
		run: func(t *testing.T, store harness.Storage, conversation bool) {
			park, arrived := parkedAttempt()
			fn := func(ctx context.Context, _ model.Request) (model.Stream, error) {
				park(ctx)
				return nil, nil
			}
			conversationFn, compactFn := attemptTablePair(fn, conversation)
			f := newAttemptTableFixture(t, store, conversationFn, compactFn, nil)
			session := createSession(t, f.h)
			operation := "op-1"
			if conversation {
				if _, err := submit(t, f.h, session, operation, harness.MessageModeRegular, "hello"); err != nil {
					t.Fatalf("submit: %v", err)
				}
			} else {
				attemptTableSeedHistory(t, f, session)
				operation = "c-1"
				attemptTableAdmitCompact(t, f, session, operation)
			}
			interruptParkedAttempt(t, f, session, arrived)
			rec := awaitTerminal(t, f.h, session, operation)
			attemptTableRequireTerminal(t, rec, harness.OperationFailure, "neither exactly one stream nor one error")
		},
	},
	{
		name: "one intent across attempts and one accepted assembly",
		run: func(t *testing.T, store harness.Storage, conversation bool) {
			var requests, policyCalls atomic.Int64
			var intentIDs []string
			var intentMu sync.Mutex
			var fh *harness.Harness
			var fsession, foperation string
			stream := attemptTableTurnStream()
			fn := func(context.Context, model.Request) (model.Stream, error) {
				requests.Add(1)
				rec, err := fh.ReadOperation(context.Background(), fsession, foperation)
				if err != nil {
					return nil, err
				}
				if rec.State.ActiveEffect == nil {
					return nil, errors.New("no model effect intent is durable while the attempts run")
				}
				intentMu.Lock()
				intentIDs = append(intentIDs, rec.State.ActiveEffect.ResultEntryID)
				n := len(intentIDs)
				intentMu.Unlock()
				if n <= 3 {
					return nil, errors.New("provider dropped")
				}
				return stream, nil
			}
			retry := func(error, int) (time.Duration, bool) {
				n := policyCalls.Add(1)
				return 0, n <= 3
			}
			conversationFn, compactFn := attemptTablePair(fn, conversation)
			f := newAttemptTableFixture(t, store, conversationFn, compactFn, retry)
			fh = f.h
			session := createSession(t, f.h)
			fsession = session
			operation := "op-1"
			if !conversation {
				operation = "c-1"
			}
			foperation = operation
			if conversation {
				rec := attemptTableRunConversation(t, f, session, operation)
				attemptTableRequireTerminal(t, rec, harness.OperationSuccess, "")
			} else {
				attemptTableSeedHistory(t, f, session)
				rec := attemptTableRunCompact(t, f, session, operation)
				attemptTableRequireTerminal(t, rec, harness.OperationSuccess, "")
			}
			intentMu.Lock()
			defer intentMu.Unlock()
			if len(intentIDs) != 4 || intentIDs[0] == "" {
				t.Fatalf("attempt-intent identities %v, want one durable intent observed by every attempt", intentIDs)
			}
			for i, id := range intentIDs {
				if id != intentIDs[0] {
					t.Fatalf("attempt %d observed intent identity %q, want the one intent %q committed before the first attempt", i, id, intentIDs[0])
				}
			}
			if requests.Load() != 4 || policyCalls.Load() != 3 {
				t.Fatalf("requests = %d policy calls = %d, want three retries and the accepted fourth attempt", requests.Load(), policyCalls.Load())
			}
			if stream.closes != 1 {
				t.Fatalf("accepted stream closes = %d, want exactly once", stream.closes)
			}
			if kinds := entryKindsByOperation(t, store, session, operation); conversation && !slices.Contains(kinds, harness.EntryAssistant) {
				t.Fatalf("operation entries = %v, want the accepted turn's assistant entry", kinds)
			} else if !conversation && !slices.Contains(kinds, harness.EntryCompaction) {
				t.Fatalf("compact operation entries = %v, want the compaction entry", kinds)
			}
		},
	},
}
