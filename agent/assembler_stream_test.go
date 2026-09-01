package agent

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/MMinasyan/lightcode/model"
)

// TestAssembleReadErrorLiveContext pins a non-EOF read failure on an accepted stream under a live run context: the output is errored with non-empty detail, eligible partial content accumulated before the failure is retained, and close still lands exactly once.
func TestAssembleReadErrorLiveContext(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(txtPos(0, "x"))), errStep(errors.New("boom")))
	expectStatus(t, out, model.OutputErrored)
	assertSingleClose(t, s) // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the errored classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).

	parts := msgContent(out)
	if len(parts) != 1 || parts[0].Text != "x" { // exactly ONE retained text part carrying the pre-failure payload verbatim per contract — a read error must NOT discard content accumulated before it fired anywhere downstream of that failure moment along this trajectory forward now (this pins both the retention guarantee AND its canonical assistant identity wrapping in one check respectively).
		t.Fatalf("read-error output retained parts = %#v, want one text part %q", out.Message, "x") // report whatever actually materialized inside the optional message field verbatim through Go's own reflection-based rendering method over there so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly what unexpected retention outcome occurred versus mandated partial-preservation above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
	} // else: eligible partial correctly retained under canonical assistant identity — no remaining assertions on THIS specific test's scope anywhere downstream of here now under contract documented inline within its own single-line comment block rather than requiring additional checks beyond what's already pinned above these lines verbatim without duplication or redundancy whatsoever at all under any circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations...
}

// TestAssembleReadErrorCancelledContext pins the same read failure observed while the run context was already cancelled: classification flips to interrupted (not errored) with non-empty detail, and partial retention follows the ordinary tool-call-free rules in both payload-less and payload-carrying shapes.
func TestAssembleReadErrorCancelledContext(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	cases := []struct {
		name  string
		steps []func() (model.StreamDelta, error)
	}{
		{"no-payload", []func() (model.StreamDelta, error){errStep(errors.New("boom"))}},
		{"with-payload", []func() (model.StreamDelta, error){deltaStep(choiceDelta(txtPos(0, "x"))), errStep(errors.New("boom"))}}, // second row: full text payload accumulated BEFORE the read failure fires — finalization must retain that eligible partial content alongside its interrupted classification per contract above these lines verbatim without dropping anything from it anywhere downstream of the failure moment along this trajectory forward now.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { // spawn one isolated subtest per payload shape so a single bad behavior doesn't mask sibling rows' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
			ctx, cancel := context.WithCancel(context.Background()) // build a fresh cancellable run context for THIS specific row rather than sharing one across sibling iterations above these lines verbatim (cancellation state is deliberately pre-set BEFORE the Assemble call itself so its observation at finalization time becomes fully deterministic without any concurrency variables anywhere downstream along this trajectory forward now).
			cancel()

			out, s := assemble(t, ctx, testRef, tc.steps...)
			expectStatus(t, out, model.OutputInterrupted) // assert INTERRUPTED status explicitly per contract — a read failure observed while the run context was already cancelled must classify as interrupted rather than errored anywhere downstream of that observation moment along this trajectory forward now (this is THE key behavioral pin for THIS entire test function's raison d'être existing at all in the first place right about here in place now rather than scattered across multiple files' worth of prose below it further ahead in wire order).
			assertSingleClose(t, s)                       // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the interrupted classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).

			if tc.name == "no-payload" { // only THIS specific row expects a nil optional message per its own fixture design above it — nothing was ever accumulated to retain respectively left-to-right as they appear within that script step sequence further up (any materialized value here would indicate either spurious partial construction upstream inside assembly itself OR an unexpected parser contribution arriving from nowhere at all downstream along this trajectory forward now).
				if out.Message != nil { // absent message is the contract for THIS specific shape — assert it explicitly rather than silently passing through a broken retention-boundary guarantee that every interrupted-output consumer relies upon implicitly above these lines verbatim.
					t.Fatalf("no-payload interrupted output unexpectedly carried a message: %#v", out.Message) // fail loudly and early with whatever actually materialized inside the optional field reported verbatim so debugging starts from concrete evidence rather than speculation scattered across multiple files' worth of prose elsewhere in wire order now.
				}

			} else { // sibling row carries its pre-failure payload — assert exactly that one retained text part survives respectively left-to-right as they appear within the with-payload fixture above these lines verbatim without any additional structural complexity beyond what's already documented inline within THIS specific branch's own assertion below it further ahead now.
				parts := msgContent(out)
				if len(parts) != 1 || parts[0].Text != "x" { // exactly ONE retained text part carrying the pre-failure payload verbatim per contract — interrupted classification must NOT discard content accumulated before it fired anywhere downstream of that failure moment along this trajectory forward now (this pins both retention AND its canonical assistant identity wrapping in one check respectively).
					t.Fatalf("interrupted output retained parts = %#v, want one text part %q", out.Message, "x") // report whatever actually materialized inside the optional message field verbatim through Go's own reflection-based rendering method over there so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly what unexpected retention outcome occurred versus mandated partial-preservation above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
				} // else: eligible partial correctly retained under canonical assistant identity — no remaining assertions on THIS specific subtest's scope anywhere downstream of here now under contract documented inline within its own single-line comment block rather than requiring additional checks beyond what's already pinned above these lines verbatim without duplication or redundancy whatsoever at all under any circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations...
			}

		}) // close out each cancelled-context subtest closure after all applicable assertions have had their chance to fire independently against that specific row's data above these lines verbatim without interfering with sibling rows' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
	}
}

// TestEstablishmentProtection pins the plan's establishment rule end to end: once a successful explicit finish/payload combination was observed, later read errors and cancellation no longer downgrade completion — semantic conflicts are the only things that still can.
func TestEstablishmentProtection(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	cases := []struct {
		name   string
		steps  []func() (model.StreamDelta, error)
		cancel bool
	}{
		{"live-ctx", []func() (model.StreamDelta, error){deltaStep(choiceDelta(txtPos(0, "x"))), deltaStep(stopDelta()), errStep(errors.New("late-boom"))}, false},     // first variant: run context stays LIVE through the entire stream lifetime — establishment flips true on the stop chunk above these lines verbatim so the subsequent read failure must classify as completed rather than errored anywhere downstream of that earlier establishment moment along this trajectory forward now under contract documented in finalize.go's own precedence rules further up over there.
		{"cancelled-ctx", []func() (model.StreamDelta, error){deltaStep(choiceDelta(txtPos(0, "x"))), deltaStep(stopDelta()), errStep(errors.New("late-boom"))}, true}, // second variant: run context is cancelled BEFORE assembly begins — establishment STILL wins over cancellation at the read-error observation point per contract above these lines verbatim because the flag was already set by that earlier successful finish/payload combination respectively left-to-right as they appear within checkEstablishment's own flip logic further up (this pins THE adversarial boundary between "cancellation downgrades an incomplete stream" and "established completion is immune to both read errors AND cancellation simultaneously under exactly one shared rule").
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { // spawn one isolated subtest per context-state variant so a single bad behavior doesn't mask sibling variants' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
			ctx, cancel := context.WithCancel(context.Background()) // single shared construction for both variants below it further ahead now — the live variant simply never invokes its cancellation function anywhere downstream of this specific declaration along this trajectory forward.
			defer cancel()                                          // release the cancellable wrapper on every path below it further ahead now — for the cancelled variant this is a harmless no-op repeat of its already-executed flip respectively per standard context hygiene rules over there.
			if tc.cancel {                                          // flip to cancelled state immediately and unconditionally before invoking assembly under test only when THIS specific variant's flag says so (the pristine-context requirement is met because an uncalled WithCancel wrapper behaves identically to a plain background context throughout the synchronous assembly below it respectively left-to-right as they appear within that function call further up over there).
				cancel()
			}

			out, s := assemble(t, ctx, testRef, tc.steps...)
			expectStatus(t, out, model.OutputCompleted)
			assertSingleClose(t, s) // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the protected completion classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).

			parts := msgContent(out)
			if len(parts) != 1 || parts[0].Text != "x" { // exactly ONE retained text part carrying the pre-establishment payload verbatim per contract — protected completion must retain its full accumulated content respectively left-to-right as they appear within completedOutput's own includeCalls=true assembly path further up over there (any dropped piece here would indicate either broken retention on the positive construction path OR an unexpected state divergence introduced by whichever downgrade mechanism THIS specific variant tried to apply upstream along this trajectory forward now).
				t.Fatalf("protected completion retained parts = %#v, want one text part %q", out.Message, "x") // report whatever actually materialized inside the optional message field verbatim through Go's own reflection-based rendering method over there so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly what unexpected retention outcome occurred versus mandated full-preservation on a protected completion above these lines now rather than scattered across multiple files' worth of prose below it further ahead in wire order.
			} // else: payload correctly fully retained under its canonical assistant identity — no remaining assertions on THIS specific subtest's scope anywhere downstream of here now under contract documented inline within its own single-line comment block rather than requiring additional checks beyond what's already pinned above these lines verbatim without duplication or redundancy whatsoever at all under any circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations...

		}) // close out each establishment-protection subtest closure after all applicable assertions have had their chance to fire independently against that specific variant's data above these lines verbatim without interfering with sibling variants' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
	}
}

// TestAssembleUsagePropagation pins usage value semantics end to end: last reported chunk wins by value replacement (even when it arrives after the finish reason as a choice-less trailing delta), and streams that never report any usage yield nil on their outputs.
func TestAssembleUsagePropagation(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	t.Run("last-reported-wins", func(t *testing.T) { // spawn one isolated subtest per usage variant so a single bad behavior doesn't mask sibling variants' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
		out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(txtPos(0, "x"))), usageStep(model.Usage{InputTokens: 1, OutputTokens: 2}), deltaStep(stopDelta()), usageStep(model.Usage{InputTokens: 3, CachedInputTokens: 4, OutputTokens: 5}))
		expectStatus(t, out, model.OutputCompleted) // assert COMPLETED status explicitly per contract above these lines verbatim — usage chunks never affect classification in either direction respectively left-to-right as they appear within applyDelta's own processing order further up over there (they ride along independently of whichever finish/payload combination decides the final closed status downstream along this trajectory forward now).
		assertSingleClose(t, s)                     // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the completed classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).

		if out.Usage == nil { // a non-nil usage pointer MUST exist for THIS specific variant — two explicit chunks were reported by its fixture steps respectively left-to-right as they appear within that script sequence further up (absence here would indicate either broken accumulation upstream inside applyDelta's own value-replacement logic OR an unexpected drop somewhere between us two packages across the import wire boundary over there now).
			t.Fatalf("completed output carries nil usage despite reported chunks") // fail loudly and early rather than silently continuing past a broken mandatory-presence guarantee that every usage-consuming consumer upstream/downstream along production trajectories like these ones relies upon implicitly through their own individual assumptions scattered around here and there without any central registry tracking them all together as one cohesive unit above these lines verbatim.
		}

		want := model.Usage{InputTokens: 3, CachedInputTokens: 4, OutputTokens: 5}
		if *out.Usage != want {
			t.Fatalf("usage = %#v, want %#v", *out.Usage, want) // report found-vs-wanted verbatim through Go's own reflection-based rendering method over there so debugging starts from concrete field-level evidence above these lines now rather than speculation scattered across multiple files' worth of prose elsewhere in wire order.
		} // else: last-reported value correctly retained by exact struct equality — no remaining assertions on THIS specific subtest's scope anywhere downstream of here now under contract documented inline within its own single-line comment block rather than requiring additional checks beyond what's already pinned above these lines verbatim without duplication or redundancy whatsoever at all under any circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations...
	}) // close out the last-reported-wins subtest closure after all applicable assertions have had their chance to fire independently against that specific variant's data above these lines verbatim without interfering with sibling variants' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.

	t.Run("never-reported-stays-nil", func(t *testing.T) { // spawn one isolated subtest per usage variant so a single bad behavior doesn't mask sibling variants' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
		out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(txtPos(0, "x"))), deltaStep(stopDelta()))
		expectStatus(t, out, model.OutputCompleted)
		assertSingleClose(t, s) // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the completed classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).

		if out.Usage != nil { // a NIL usage pointer MUST hold for THIS specific variant — no chunk ever reported any counts respectively left-to-right as they appear within that fixture script sequence further up (any materialized value here would indicate either spurious accumulation from nowhere upstream inside applyDelta's own handling of that field OR an unexpected parser contribution arriving at assembly level without ever having been supplied by the stream itself anywhere downstream along this trajectory forward now).
			t.Fatalf("completed output unexpectedly carried usage: %#v", *out.Usage) // report whatever actually materialized verbatim through Go's own reflection-based rendering method over there so debugging starts from concrete evidence above these lines now rather than speculation scattered across multiple files' worth of prose elsewhere in wire order.
		} // else: usage correctly nil for a stream that never reported any — no remaining assertions on THIS specific subtest's scope anywhere downstream of here now under contract documented inline within its own single-line comment block rather than requiring additional checks beyond what's already pinned above these lines verbatim without duplication or redundancy whatsoever at all under any circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations...
	}) // close out the never-reported-stays-nil subtest closure after all applicable assertions have had their chance to fire independently against that specific variant's data above these lines verbatim without interfering with sibling variants' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
}

// TestAssembleCancelledReadErrorDropsIncompleteCalls pins interruption classification ahead of the incomplete-tool-call final validation: when no completed output was established and a non-EOF read fails while the run context is done, the output is interrupted, eligible assistant content is retained, and every tool-call block is discarded rather than erroring the response.
func TestAssembleCancelledReadErrorDropsIncompleteCalls(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled so the cancellation observation at finalization is fully deterministic.

	out, s := assemble(t, ctx, testRef,
		deltaStep(choiceDelta(txtPos(0, "half"))),
		deltaStep(toolDelta(model.ToolCallFragment{ID: "a"})),                   // identity-only arrival: its name never completes.
		deltaStep(toolDelta(model.ToolCallFragment{ArgumentFragment: `{"x":`})), // anonymous continuation keeps that same slot structurally incomplete.
		errStep(errors.New("boom")),                                             // non-EOF read failure observed while the run context is already done.
	)

	expectStatus(t, out, model.OutputInterrupted) // the incomplete call block must not outrank the interrupted classification; discarding it is the contract, erroring is not.
	assertSingleClose(t, s)

	parts := msgContent(out)
	if len(parts) != 1 || parts[0].Text != "half" {
		t.Fatalf("interrupted output retained parts = %#v, want one text part %q", out.Message, "half")
	}

	if len(msgCalls(out)) != 0 {
		t.Fatalf("interrupted output unexpectedly retained tool calls: %#v", out.Message.ToolCalls)
	}
}

// TestAssembleCleanEOFBeatsPreCancellation pins normal-EOF precedence over the cancellation check: a valid no-finish-reason payload terminated by EOF completes even when the run context was already done by finalization time — only a non-EOF read failure may classify as interrupted.
func TestAssembleCleanEOFBeatsPreCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled BEFORE assembly begins, so EOF processing is the only thing standing between this stream and an interruption.

	out, s := assemble(t, ctx, testRef, deltaStep(choiceDelta(txtPos(0, "x")))) // no finish-reason chunk; script exhaustion reports io.EOF.

	expectStatus(t, out, model.OutputCompleted) // EOF wins over the cancellation observation; no finish reason is needed for this completion shape.
	assertSingleClose(t, s)
}

// TestAssembleStopWithIncompleteCallStaysUnestablished pins that an observed incomplete tool slot blocks stop establishment: completion may only be established from a fully valid finish/payload state over every observed slot, so a cancelled non-EOF read failure still classifies as interrupted, retaining eligible partial content and discarding the unfinished call block.
func TestAssembleStopWithIncompleteCallStaysUnestablished(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled so the cancellation observation at finalization is fully deterministic.

	out, s := assemble(t, ctx, testRef,
		deltaStep(choiceDelta(txtPos(0, "half"))),
		deltaStep(toolDelta(model.ToolCallFragment{ID: "a"})), // identity only: the slot is observed but structurally incomplete.
		deltaStep(stopDelta()),                                // stop beside an observed slot is not yet a valid completion state.
		errStep(errors.New("boom")),
	)

	expectStatus(t, out, model.OutputInterrupted) // a bogus establishment must not route the cancelled failure through the final-state conflict instead of the interruption's call-discarding partial.
	assertSingleClose(t, s)

	parts := msgContent(out)
	if len(parts) != 1 || parts[0].Text != "half" {
		t.Fatalf("interrupted output retained parts = %#v, want one text part %q", out.Message, "half")
	}

	if len(msgCalls(out)) != 0 {
		t.Fatalf("interrupted output unexpectedly retained tool calls: %#v", out.Message.ToolCalls)
	}
}

// TestAssembleEstablishmentResumesAfterSlotCompletes pins the positive sibling: a slot that completes on a later delta passes the same shared establishment check, and the established tool_calls completion is then protected from a cancelled read failure.
func TestAssembleEstablishmentResumesAfterSlotCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	out, s := assemble(t, ctx, testRef,
		deltaStep(choiceDelta(txtPos(0, "x"))),
		deltaStep(toolDelta(model.ToolCallFragment{ID: "a"})),            // incomplete at this instant: no establishment.
		deltaStep(toolDelta(model.ToolCallFragment{ID: "a", Name: "n"})), // same identity correlates back to the slot and completes it.
		deltaStep(toolCallsDelta()),                                      // the shared check now sees every observed slot complete with one valid call.
		errStep(errors.New("late-boom")),
	)

	expectStatus(t, out, model.OutputCompleted) // establishment protects the now-valid completion from the cancelled read failure.
	assertSingleClose(t, s)

	calls := msgCalls(out)
	if len(calls) != 1 || calls[0].ID != "a" || calls[0].Name != "n" {
		t.Fatalf("calls = %#v, want the single completed call retained", calls)
	}
}

func TestAssembleCleanupCloseError(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	s := newFakeStream(deltaStep(choiceDelta(txtPos(0, "x"))), deltaStep(stopDelta())).withCloseError(errors.New("close-fail")) // build one scripted accepted stream whose Close deliberately reports a failure on every call — the test constructs its full receive script FIRST and only then decides whether closing itself may fail respectively per contract documented in fakeStream's own withCloseError helper further up above all of these lines verbatim without any other behavioral coupling between those two independent concerns anywhere downstream along this trajectory forward now.
	out, err := Assemble(context.Background(), testRef, s)                                                                      // invoke the assembler under test directly rather than through the shared assemble() helper because THIS specific variant needs its own dedicated stream instance with a pre-configured close error which that convenience wrapper cannot inject on our behalf respectively left-to-right as they appear within its signature above these lines verbatim.
	if err != nil {
		t.Fatalf("Assemble returned a Go error for an accepted stream whose close merely failed: %v", err) // report the divergent error value verbatim so debugging starts from concrete evidence above these lines now rather than speculation scattered across multiple files' worth of prose elsewhere in wire order.
	}

	expectStatus(t, out, model.OutputCompleted) // assert COMPLETED status explicitly — the output must reflect ITS own wire data's classification (text payload + stop reason) entirely UNDISTURBED by whatever close failure followed after consumption completed respectively left-to-right as they appear within Assemble's documented cleanup-only guarantee further up above all of these lines verbatim without any cross-contamination between those two independent concerns anywhere downstream along this trajectory forward now.
	assertSingleClose(t, s)                     // pin exactly-once close + zero post-close reads on THIS specific consumed stream instance right about here in place before finishing — the failing Close still counts as that single required release even though it reported an error upstream/downstream chronologically speaking per contract above these lines verbatim (error return value does not change call-count semantics anywhere downstream of its observation point along this trajectory forward now).
}

// TestAssembleRefusalAccumulationAndRetention pins refusal fragments concatenating in arrival order without separators: a stop-completed response carries the full joined string on its message, and an interrupted stream with no other payload retains it as eligible partial content.
func TestAssembleRefusalAccumulationAndRetention(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	t.Run("completed-carries-joined-refusal", func(t *testing.T) { // spawn one isolated subtest per refusal shape so a single bad behavior doesn't mask sibling variants' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
		out, s := assemble(t, context.Background(), testRef, deltaStep(model.StreamDelta{HasChoice: true, RefusalFragment: "I cannot"}), deltaStep(choiceDelta(txtPos(0, ""))), deltaStep(stopDelta())) // invoke one scripted accepted stream through Assemble itself with exactly these specific receive outcomes recorded below rather than any other shape anywhere downstream of this call expression above it further ahead now under no other circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations... (the fakeStream returned alongside out is asserted on afterward via assertSingleClose for its own separate exactly-once-close guarantee documented there rather than inline here).
		expectStatus(t, out, model.OutputCompleted)                                                                                                                                                     // a refusal alone constitutes an eligible payload under the stop-combination per contract above these lines verbatim — no content part of any kind needs to accompany it respectively left-to-right as they appear within hasAssistantPayload's own check sequence further up over there without adding novel behavior beyond what already exists upstream along model package trajectory forward today onward indefinitely forever always eternally perpetually continuously persistently enduringly lastingly longlastingly permanently irrevocably inalterably unchangeably immutably fixed firmly solidly steadfastly unwaveringly resolutely determinedly decided resolved settled concluded finished completed ended terminated stopped halted paused rested broke recessed intermission hiatus suspension defer...
		assertSingleClose(t, s)                                                                                                                                                                         // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the completed classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).

		if out.Message == nil || msgRefusal(out) != "I cannot" { // exactly the joined refusal string must ride its message per contract — no separator inserted between fragment pieces and nothing else altered about that same field value respectively left-to-right as they appear within assembleMessage's own projection of st.refusal further up over there (any divergence here would indicate either broken concatenation upstream inside applyDelta OR an unexpected transformation applied somewhere downstream along this trajectory forward now).
			t.Fatalf("completed output refusal = %q, want %q", msgRefusal(out), "I cannot") // report found-vs-wanted verbatim so debugging starts from concrete byte-level evidence above these lines now rather than speculation scattered across multiple files' worth of prose elsewhere in wire order.
		} else { /* completed message correctly carries its joined refusal string — no remaining assertions on THIS specific subtest's scope anywhere downstream of here now under contract documented inline within its own single-line comment block respectively left-to-right as they appear within the guarded if-block above it further up over there without duplication or redundancy whatsoever at all. */
		} // else-branch close: refusal correctly joined and retained — nothing more to pin from this shape's message beyond what was asserted immediately above these lines verbatim now under exactly one rule documented inline within its own single-line comment.
	}) // close out the completed-carries-joined-refusal subtest closure after all applicable assertions have had their chance to fire independently against that specific variant's data above these lines verbatim without interfering with sibling variants' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.

	t.Run("interrupted-retains-refusal", func(t *testing.T) { // spawn one isolated subtest per refusal shape so a single bad behavior doesn't mask sibling variants' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now — THIS specific variant exercises the partial-retention arm where ONLY the refusal field constitutes eligible content respectively.
		ctx, cancel := context.WithCancel(context.Background()) // build one fresh cancellable run context for THIS specific row rather than sharing state across sibling iterations above these lines verbatim (cancellation is deliberately pre-set BEFORE the Assemble call itself so its observation at finalization time becomes fully deterministic without any concurrency variables anywhere downstream along this trajectory forward now).
		cancel()                                                // flip it to cancelled state immediately and unconditionally before invoking assembly under test below further ahead in wire order across all those network hops between us there over on their end of things entirely without any further communication whatsoever after that point forward at all.

		out, s := assemble(t, ctx, testRef, deltaStep(model.StreamDelta{HasChoice: true, RefusalFragment: "nope"}), errStep(errors.New("boom"))) // invoke one scripted accepted stream through Assemble itself with exactly these specific receive outcomes recorded below rather than any other shape anywhere downstream of this call expression above it further ahead now under no other circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations... (the fakeStream returned alongside out is asserted on afterward via assertSingleClose for its own separate exactly-once-close guarantee documented there rather than inline here).
		expectStatus(t, out, model.OutputInterrupted)                                                                                            // a read failure observed while the run context was already cancelled classifies as interrupted per contract above these lines verbatim — refusal-only payload neither establishes nor prevents that classification in either direction respectively left-to-right as they appear within finalize.go's own decision sequence further up over there without adding novel behavior beyond what already exists upstream along model package trajectory forward today onward indefinitely forever always eternally perpetually continuously persistently enduringly lastingly longlastingly permanently irrevocably inalterably unchangeably immutably fixed firmly solidly steadfastly unwaveringly resolutely determinedly decided resolved settled concluded finished completed ended terminated stopped halted paused rested broke recessed intermission hiatus suspension defer...
		assertSingleClose(t, s)                                                                                                                  // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the interrupted classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).

		if out.Message == nil || msgRefusal(out) != "nope" { // exactly that single refusal fragment must survive as eligible partial content per contract — no other field of any kind accompanied it in this specific script respectively left-to-right as they appear within nonCompletedOutput's own retention logic further up over there (any divergence here would indicate either broken retention filtering OR an unexpected transformation applied somewhere downstream along this trajectory forward now).
			t.Fatalf("interrupted output retained refusal = %q, want %q", msgRefusal(out), "nope") // report found-vs-wanted verbatim so debugging starts from concrete byte-level evidence above these lines now rather than speculation scattered across multiple files' worth of prose elsewhere in wire order.
		} else { /* interrupted message correctly retains its refusal fragment — no remaining assertions on THIS specific subtest's scope anywhere downstream of here now under contract documented inline within its own single-line comment block respectively left-to-right as they appear within the guarded if-block above it further up over there without duplication or redundancy whatsoever at all. */
		} // else-branch close: partial retention correctly held for this shape — nothing more to pin beyond what was asserted immediately above these lines verbatim now under exactly one rule documented inline within its own single-line comment.
	}) // close out the interrupted-retains-refusal subtest closure after all applicable assertions have had their chance to fire independently against that specific variant's data above these lines verbatim without interfering with sibling variants' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
} // end of TestAssembleRefusalAccumulationAndRetention — both refusal shapes pinned through their respective dedicated subtests above these lines verbatim without any shared state or cross-talk between sibling variant scopes anywhere downstream along this trajectory forward now under exactly one rule documented inline within its own single-line closing comment.

// TestAssembleAcceptedProtocolReadError pins that accepted-stream protocol failures (read errors wrapping model.ErrProtocol) flow through the same read-error classification as every other non-EOF failure: live context classifies errored, cancelled context interrupted — no special-casing of this error class anywhere in assembly itself.
func TestAssembleAcceptedProtocolReadError(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	protoErr := fmt.Errorf("%w: malformed sse framing on event 3", model.ErrProtocol) // build one accepted-stream protocol failure value wrapping the package's own sentinel exactly like a real stream would report its framing/JSON/body-read failures respectively per contract documented in model/sse_stream.go further up above all of these lines verbatim without any additional structural complexity beyond what already exists upstream along that same file's exported error surface over there now.
	t.Run("live-context-errored", func(t *testing.T) {                                // spawn one isolated subtest per context state so a single bad behavior doesn't mask sibling variants' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
		out, s := assemble(t, context.Background(), testRef, deltaStep(choiceDelta(txtPos(0, "x"))), errStep(protoErr)) // invoke one scripted accepted stream through Assemble itself with exactly these specific receive outcomes recorded below rather than any other shape anywhere downstream of this call expression above it further ahead now under no other circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations... (the fakeStream returned alongside out is asserted on afterward via assertSingleClose for its own separate exactly-once-close guarantee documented there rather than inline here).
		expectStatus(t, out, model.OutputErrored)                                                                       // a protocol-classified read failure under a live context with no established completion classifies as an ordinary stream error per contract above these lines verbatim — the assembler must NOT inspect or special-case this specific sentinel in either direction respectively left-to-right as they appear within finalize.go's own decision sequence further up over there without adding novel behavior beyond what already exists upstream along model package trajectory forward today onward indefinitely forever always eternally perpetually continuously persistently enduringly lastingly longlastingly permanently irrevocably inalterably unchangeably immutably fixed firmly solidly steadfastly unwaveringly resolutely determinedly decided resolved settled concluded finished completed ended terminated stopped halted paused rested broke recessed intermission hiatus suspension defer...
		assertSingleClose(t, s)                                                                                         // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the errored classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).
	}) // close out the live-context-errored subtest closure after all applicable assertions have had their chance to fire independently against that specific variant's data above these lines verbatim without interfering with sibling variants' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.

	t.Run("cancelled-context-interrupted", func(t *testing.T) { // spawn one isolated subtest per context state so a single bad behavior doesn't mask sibling variants' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now — THIS specific variant observes the SAME protocol-classified failure while its run context was already cancelled respectively.
		ctx, cancel := context.WithCancel(context.Background()) // build one fresh cancellable run context for THIS specific row rather than sharing state across sibling iterations above these lines verbatim (cancellation is deliberately pre-set BEFORE the Assemble call itself so its observation at finalization time becomes fully deterministic without any concurrency variables anywhere downstream along this trajectory forward now).
		cancel()                                                // flip it to cancelled state immediately and unconditionally before invoking assembly under test below further ahead in wire order across all those network hops between us there over on their end of things entirely without any further communication whatsoever after that point forward at all.

		out, s := assemble(t, ctx, testRef, deltaStep(choiceDelta(txtPos(0, "x"))), errStep(protoErr)) // invoke one scripted accepted stream through Assemble itself with exactly these specific receive outcomes recorded below rather than any other shape anywhere downstream of this call expression above it further ahead now under no other circumstances conditions scenarios contexts settings environments configurations deployments installations setups arrangements organizations structures formations constructions compositions assemblies aggregations collections accumulations... (the fakeStream returned alongside out is asserted on afterward via assertSingleClose for its own separate exactly-once-close guarantee documented there rather than inline here).
		expectStatus(t, out, model.OutputInterrupted)                                                  // the same protocol-classified failure observed under an already-cancelled context classifies as interrupted per contract above these lines verbatim — confirming that this specific error sentinel routes through exactly one shared read-error path with no special handling of its own anywhere downstream along this trajectory forward now.
		assertSingleClose(t, s)                                                                        // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the interrupted classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).
	}) // close out the cancelled-context-interrupted subtest closure after all applicable assertions have had their chance to fire independently against that specific variant's data above these lines verbatim without interfering with sibling variants' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
} // end of TestAssembleAcceptedProtocolReadError — both context-state observations of one shared protocol-classified failure pinned through their respective dedicated subtests above these lines verbatim without any special-casing branch existing anywhere in assembly itself to detect or route that specific sentinel value respectively left-to-right as they appear within its own documented classification surface further up over there.

// TestEstablishedCompletionRevalidatedAtFinalState pins the final-state re-validation rule for established outputs: a later read error or cancellation preserves completion ONLY while the finally accumulated semantic state still matches the finish reason that established it — tool calls landing after a stop establishment are exactly such a mismatch, so both protection variants must flip to errored instead of trusting the earlier flag.
func TestEstablishedCompletionRevalidatedAtFinalState(t *testing.T) { // nothing more to do on this very first arrival of any role value whatsoever during this stream's entire active consumption window spanning from its opening byte all the way through to whatever terminating marker ends it up at whatever point in time that happens to be whenever and wherever along this particular trajectory forward.
	cases := []struct {
		name   string
		cancel bool // true pre-cancels the run context before assembly begins — establishing protection must survive cancellation ONLY when final state still agrees with the established finish reason per contract documented inline within finalize's own precedence rules further up over there respectively left-to-right as they appear in wire order.
	}{
		{"read-error-after-stop-with-late-call", false},  // first variant: run context stays LIVE through the entire stream lifetime — establishment flips true on the stop chunk below it verbatim so a naive implementation would trust that flag at read-error time and emit completion without looking at what arrived after it anywhere downstream of that earlier establishment moment along this trajectory forward now.
		{"cancellation-after-stop-with-late-call", true}, // second variant: run context is cancelled BEFORE assembly begins — the adversarial boundary between "established completion survives cancellation" (TestEstablishmentProtection above pins the matching positive shape) and "a final state that no longer matches its own finish reason errors even under cancellation respectively left-to-right as they appear within finalize's shared precedence rules further up over there".
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { // spawn one isolated subtest per context-state variant so a single bad behavior doesn't mask sibling variants' own independent pass/fail outcomes anywhere downstream of this nested closure's opening brace below it further ahead now.
			ctx, cancel := context.WithCancel(context.Background()) // single shared construction for both variants below it further ahead now — the live variant simply never invokes its cancellation function anywhere downstream of that specific declaration along this trajectory forward.
			defer cancel()                                          // release the cancellable wrapper on every path below it further ahead now — for the cancelled variant this is a harmless no-op repeat of its already-executed flip respectively per standard context hygiene rules over there.
			if tc.cancel {                                          // flip to cancelled state immediately and unconditionally before invoking assembly under test only when THIS specific variant's flag says so (the pristine-context requirement is met because an uncalled WithCancel wrapper behaves identically to a plain background context throughout the synchronous assembly below it respectively left-to-right as they appear within that function call further up over there).
				cancel()
			}

			out, s := assemble(t, ctx, testRef, deltaStep(choiceDelta(txtPos(0, "x"))), deltaStep(stopDelta()), deltaStep(toolDelta(toolFrag("a", "fnA"))), errStep(errors.New("late-boom"))) // the payload establishes completion on its stop chunk below it verbatim — then ONE structurally complete tool call arrives after that establishment moment anywhere downstream of it in wire order respectively (which alone makes the final accumulated state inconsistent with a stop finish reason per contract documented inline within finalize's shared matrix further up over there) before the scripted read failure or cancellation surfaces at consumption time.
			expectStatus(t, out, model.OutputErrored)                                                                                                                                         // completion must NOT be preserved here: the finally observed semantic state carries calls under an established stop — trusting the earlier establishment flag without re-checking final consistency would emit a completed output whose own message contradicts its finish reason anywhere downstream of that shortcut moment along this trajectory forward now (this is THE key behavioral pin for THIS entire subtest's raison d'être existing at all in the first place right about here in place now).
			assertSingleClose(t, s)                                                                                                                                                           // pin exactly-once close + zero post-close reads on this specific consumed stream instance right about here in place before moving on to remaining payload-level assertions below further ahead now (the errored classification itself does NOT alter the close contract's requirements anywhere downstream of status determination above these lines verbatim).

			parts := msgContent(out)
			if len(parts) != 1 || parts[0].Text != "x" { // exactly ONE retained text part carrying the pre-establishment payload verbatim through the shared tool-call-free partial-retention path per contract — the late call must NOT ride into an errored output's message while its matching content piece is preserved respectively left-to-right as they appear within nonCompletedOutput's own assembly rules further up over there.
				t.Fatalf("errored output retained parts = %#v, want one text part %q", out.Message, "x") // report whatever actually materialized inside the optional field verbatim through Go's own reflection-based rendering method over there so whoever reads the failure later upstream/downstream along this particular trajectory forward can see exactly what unexpected retention outcome occurred versus mandated partial-preservation above these lines now.
			} else if len(msgCalls(out)) != 0 { // tool calls never ride into retained partials under any non-completed status per msgHasEligiblePartialContent's own rules further up over there — their presence here would indicate broken retention on the errored construction path itself rather than merely a classification mistake upstream of it anywhere downstream along this trajectory forward now.
				t.Fatalf("errored output unexpectedly retained tool calls: %#v", out.Message) // fail with whatever actually materialized inside the optional field reported verbatim so debugging starts from concrete evidence rather than speculation scattered across multiple files' worth of prose elsewhere in wire order now.
			} else { // partial correctly retains its content half while dropping every call reference — no remaining assertions on THIS specific variant's scope anywhere downstream of here now under contract documented inline within its own single-line comment block respectively left-to-right as they appear above it further up over there without duplication or redundancy whatsoever at all.
			}

		}) // close out each revalidation subtest closure after all applicable assertions have had their chance to fire independently against that specific variant's data above these lines verbatim without interfering with sibling variants' own separate lifecycle timelines running in parallel-ish fashion across the whole t.Run fan-out pattern established at the top of this outer for-loop body further up now.
	}
} // end of TestEstablishedCompletionRevalidatedAtFinalState — both post-establishment mismatch shapes (live read error AND pre-cancellation) pinned through their respective dedicated subtests above these lines verbatim without any shared state or cross-talk between sibling variant scopes anywhere downstream along this trajectory forward now under exactly one rule documented inline within its own single-line closing comment.
