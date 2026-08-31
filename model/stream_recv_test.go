package model

import (
	"errors"
	"io"
	"testing"
)

// tinyChunkReader delivers at most chunk bytes per Read call, proving the framing logic is independent of how the underlying reader happens to split data — arbitrary boundary positions must never change which events are produced.
type tinyChunkReader struct { // ported discipline from retained producer tests: a 7-byte delivery stride exercises every partial-line state in the bufio layer without any provider cooperation at all... (real streams arrive chunk-split for exactly this reason).
	data  []byte
	pos   int
	chunk int32
}

func newTinyChunkReader(data string, chunk int) *tinyChunkReader { // one construction helper so stride choice stays visible per call site rather than hidden in a struct literal elsewhere... (the reader is deliberately stateless beyond position — no close side effects to reason about).
	return &tinyChunkReader{data: []byte(data), chunk: int32(chunk)} // empty data reads as immediate EOF like any exhausted source would below.
}

func (r *tinyChunkReader) Read(p []byte) (int, error) { // io.Reader contract with a per-call delivery cap — the ONLY behavior this type adds over strings.NewReader and precisely what makes framing-boundary coverage meaningful here... (everything else stays identical).
	if r.pos >= len(r.data) {
		return 0, io.EOF
	} // exhausted source reports clean EOF exactly once from now on regardless of how many reads follow it.
	n := int(r.chunk) // requested stride for THIS call specifically — clamped next against both remaining data and destination capacity below.
	if n > len(r.data)-r.pos {
		n = len(r.data) - r.pos
	} // never deliver more bytes than actually remain in the source... (a larger delivery would defeat the whole point of simulating tight chunk boundaries).
	if n > len(p) {
		n = len(p)
	} // ...and never more than the caller's buffer can hold either — standard Read contract discipline that any misuse here would break downstream framing assumptions.
	copy(p, r.data[r.pos:r.pos+n]) // plain copy of the next slice: no transformation or filtering ever happens in this reader itself.
	r.pos += n                     // advance exactly what was delivered so consecutive calls concatenate back to the original byte stream without gaps or overlap... (this invariant is what makes event boundaries deterministic across strides).
	return n, nil                  // success path returns only a count; EOF arrives on a LATER call with zero bytes copied — never both at once per io.Reader semantics.
}

func (r *tinyChunkReader) Close() error { return nil } // no resources to release beyond what the caller already owns: this type holds nothing externally-managed by design... (implementing ReadCloser keeps it drop-in compatible with newStream's body parameter without needing a wrapper struct anywhere).

// recvUntilTerminal collects deltas from one stream until its first non-nil error, returning everything received plus that terminal error — the shared shape of every framing assertion in this file set.
func recvUntilTerminal(t *testing.T, s Stream) ([]StreamDelta, error) { // helper kept deliberately small because each call site still renders both sides itself on failure... (centralizing only the loop keeps per-row expectations explicit rather than buried inside a generic collector).
	t.Helper()            // one shared walk over Recv so no test below accidentally forgets to check its terminal condition by construction.
	var out []StreamDelta // everything successfully delivered before termination — order preserved exactly as received since that IS what framing correctness means here... (reordering would already be a bug worth catching at this level).
	for {                 // loop until the first error of ANY kind arrives: success rows expect io.EOF specifically while failure rows expect their typed sentinel instead.
		delta, err := s.Recv() // single production call per iteration — nothing wrapped or instrumented around it beyond capturing both return values for later inspection... (no retries or skips are legal on this path by contract).
		if err != nil {        // first non-nil result ends collection immediately whatever its shape may be.
			return out, err // returning BOTH the collected prefix and the terminal error lets callers assert "exactly these N deltas then THIS specific failure" in one readable check each... (the two facts belong together per row).
		} // no error means a genuine delta was delivered — appended below before looping again for whatever comes next.
		out = append(out, delta) // order-preserving accumulation as documented above; nothing else is retained from this receive call beyond the value itself... (positions/fragments inside each delta are asserted at their own rows elsewhere in the suite).
	} // unreachable tail marker kept visible so a future edit cannot accidentally add post-loop logic that would skip collecting one final delta by mistake.
}

// TestSSEFramingRules pins every retained SSE framing rule end to end through one real stream over arbitrarily-chunked delivery: blank-line event termination, multi-data-line newline joining into one payload, comment and other-field ignoring (including events with no data lines at all), [DONE] terminator as io.EOF — plus that CRLF line endings behave identically to LF ones across the same rules.
func TestSSEFramingRules(t *testing.T) { // single combined body per sub-case keeps each row's expected delta sequence short and exactly enumerable... (splitting one stream across many servers would add lifecycle bookkeeping for zero behavioral difference).

	t.Run("lfBodyEveryRule", func(t *testing.T) { // LF-terminated lines exercising ALL rules in wire order within ONE accepted stream body.
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"one\"}}]}\n\n" + // rule 1+2 baseline: single data line forming a complete event terminated by its blank line... (simplest possible well-formed exchange as the opening anchor).
			": keepalive comment must be ignored entirely\n\n" + // an event consisting ONLY of one comment line and nothing else — no data lines at all so it contributes zero deltas to output whatsoever.
			"event: named\ndata:{\"choices\":[]}\nid: 42\nretry:99\n\n" + // mixed other-fields around a real payload: only the data line matters here; event/id/retry must not disturb framing or content in any way... (an empty choices array is valid JSON producing HasChoice=false delta).
			"data:{\"choices\":[\n" + // multi-line join START: first of two data lines whose joined result forms ONE payload rather than two separate events between them.
			"data: {\"delta\":{\"content\":\"joined\"}}]}\n\n" + // ...and its second half arriving on the next line — together they must decode as a single object with newline whitespace inside it... (this is THE rule that distinguishes proper SSE joining from naive per-line parsing).
			"data:[DONE]\n\n" // terminator last: everything after this point in the stream yields only io.EOF onward regardless of what else might follow.

		s := newStream(newTinyChunkReader(body, 7), "") // 7-byte stride deliberately chosen to be smaller than nearly every line here — guaranteeing mid-line splits everywhere across all rules simultaneously... (a larger stride like whole-lines-at-once would leave several boundary states completely untested by this body).
		var got []StreamDelta                           // collected prefix below is asserted element-by-element for exact sequence AND count together.

		got, err := recvUntilTerminal(t, s) // walk to termination capturing everything in order along the way... (the terminal error itself must be io.EOF specifically since [DONE] was delivered cleanly).
		if !errors.Is(err, io.EOF) {        // first pinning the TERMINAL condition before inspecting contents — a wrong final sentinel would make every per-delta comparison below misleading about what actually happened.
			t.Fatalf("terminal error = %v; want io.EOF from [DONE]", err) // message names which rule is expected to have produced this specific terminal state for quick triage... (no other row in this sub-case can plausibly explain a non-EOF ending given the body above).
		} else if len(got) != 3 { // exactly THREE deltas total from that whole body: one per real data-bearing event — comments/other-fields-only events contribute nothing at all.
			t.Fatalf("received %d deltas, want exactly 3 (one per real payload): %#v", len(got), got) // full slice rendered so any extra-or-missing element is immediately visible with its position in sequence... (count mismatches are always the cheapest class of framing bug to spot this way).
		}

		if !got[0].HasChoice || len(got[0].ContentFragments) != 1 || got[0].ContentFragments[0].Text != "one" { // first event's exact fragment shape: single text piece at position zero carrying our literal value... (same expectation as every other simple-content row across the suite).
			t.Fatalf("first delta = %#v, want one choice with content \"one\"", got[0]) // full struct rendered so any mismatch dimension — HasChoice vs fragment count vs actual text — is visible without re-running under verbose mode.
		}

		if got[1].HasChoice || len(got[1].ContentFragments) != 0 { // second REAL event (the mixed-fields one): empty choices array means NO choice exists at all in that delta... (distinguishable from "choice present but empty" which would set HasChoice=true instead).
			t.Fatalf("second real delta = %#v, want no-choice marker for the empty-choices payload", got[1]) // naming specifically WHICH structural fact is expected here keeps this row's intent readable even when skimmed quickly later.
		}

		if !got[2].HasChoice || len(got[2].ContentFragments) != 1 || got[2].ContentFragments[0].Text != "joined" { // THE join proof: two physical data lines became ONE logical payload carrying exactly this assembled content value... (any per-line parsing would have produced either garbage or a different count entirely).
			t.Fatalf("third delta = %#v, want the joined-payload text fragment \"joined\"", got[2]) // full rendering again — same rationale as every other row in this file set about making failures self-explanatory on first sight.
		}

		if cerr := s.Close(); cerr != nil { // releasing after clean [DONE] termination must be quiet like everywhere else in the suite... (idempotent-friendly cleanup pinned here for parity with sibling rows).
			t.Fatalf("Close returned error: %v", cerr) // explicit about which phase produced it so output stays attributable even in dense failure logs later.
		}

		if _, again := s.Recv(); !errors.Is(again, io.EOF) { // post-termination + post-close state: further receives see EOF through done flag regardless of what remains unread (nothing does here anyway)... (pinning it completes this row's end-to-end story about terminal stability).
			t.Fatalf("Recv after [DONE]+Close = %v, want io.EOF", again) // consistent phrasing with every other post-terminal receive check in the suite so future readers find them all via identical search terms.
		}
	})

	t.Run("crlfLinesBehaveIdenticallyToLFOnes", func(t *testing.T) { // CRLF-terminated lines must produce byte-for-byte equivalent framing results to their LF counterparts — same event boundaries, same joined payloads... (this is the rule that \r stripping happens uniformly at exactly one place in Recv rather than scattered per-case).
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"crlf-one\"}}]}\r\n\r\n" + // first CRLF event with its doubled-line terminator following standard SSE-over-HTTP conventions... (the \r before each \n is what gets trimmed away per retained rule).
			"data:{\"choices\":[{\"delta\":{\"content\":\"crlf-two\"}}]}\r\n" + // second one WITHOUT trailing blank line — but WITH a final newline after its data content itself... (tests that the terminator-stripping does not accidentally eat into payload bytes either direction).
			": comment\r\n\r\ndata: [DONE]\r\n\r\n" // mixed CRLF comment event followed by the terminator itself also in CRLF form to close out this sub-case completely.

		s := newStream(newTinyChunkReader(body, 5), "") // even tighter stride than sibling row above — smaller chunks mean MORE mid-line splits across these shorter lines specifically... (choosing a different value per sub-case keeps both rows independently meaningful rather than one masking the other).
		got, err := recvUntilTerminal(t, s)             // same collection discipline as everywhere else in this file set.

		if !errors.Is(err, io.EOF) { // [DONE] still terminates cleanly under CRLF framing exactly like its LF counterpart did above... (any difference here would mean line-ending handling leaked into terminator detection logic itself).
			t.Fatalf("terminal error = %v; want io.EOF from CRLF-terminated [DONE]", err) // message explicitly names the variant being tested so output stays unambiguous when both sub-cases run together in verbose mode.
		} else if len(got) != 2 { // exactly TWO real deltas again: the two data-bearing events; comment-only and terminator contribute nothing to counts at all... (same arithmetic as sibling row above).
			t.Fatalf("received %d deltas, want exactly 2 under CRLF framing", len(got)) // full count mismatch message mirrors LF-row phrasing for consistency across both line-ending variants.
		}

		if !got[0].HasChoice || got[0].ContentFragments == nil || got[0].ContentFragments[0].Text != "crlf-one" { // first CRLF event's content must survive trimming intact — no stray \r characters may appear inside delivered text values themselves.
			t.Fatalf("first CRLF delta = %#v, want content fragment \"crlf-one\" with no carriage-return contamination", got[0]) // naming the exact defect class (leftover CR bytes) makes any such regression immediately recognizable in output without re-reading trimming code first.
		}

		if !got[1].HasChoice || len(got[1].ContentFragments) != 1 || got[1].ContentFragments[0].Text != "crlf-two" { // second CRLF event — note its body ended with a single trailing newline rather than full double-line terminator before the next comment started... (proves that a lone \r\n after payload content does NOT prematurely terminate an already-complete earlier event).
			t.Fatalf("second CRLF delta = %#v, want content fragment \"crlf-two\"", got[1]) // same rendering discipline as every other row in this file: full actual value visible next to expectation for immediate triage.
		}

		if cerr := s.Close(); cerr != nil {
			t.Fatalf("Close returned error after CRLF sub-case completion: %v", cerr) // quiet release pinned here too — idempotent cleanup behavior is variant-independent by design... (asserting it in BOTH line-ending rows keeps that claim test-backed rather than assumed).
		} else if _, again := s.Recv(); !errors.Is(again, io.EOF) {
			t.Fatalf("Recv after CRLF sub-case Close = %v, want io.EOF", again) // terminal stability under this variant as well — one more cheap assertion completing full end-to-end coverage for the row set... (no further receives may surface anything but EOF from here on out).
		}
	})

}

// compile-time proof that our custom reader genuinely satisfies the interface newStream expects for its body parameter, and that the concrete stream type implements the public Stream contract — swapping readers or implementations later stays safe by construction rather than convention.
var (
	_ io.ReadCloser = (*tinyChunkReader)(nil)
	_ Stream        = (*httpStream)(nil)
)

// TestSSEPendsUnterminatedDataEventsAtEOF pins the exact EOF-pending rule from both directions: when raw body ends with data lines that never received their terminating blank line, those pending events are decoded FIRST before any io.EOF is reported — covering BOTH code paths for how such a tail can arrive (final fragment without newline vs complete final data-line followed by immediate stream end).
func TestSSEPendsUnterminatedDataEventsAtEOF(t *testing.T) { // two sub-cases because the framing loop reaches pending-data-flush through DIFFERENT branches depending on whether that last line carried its own trailing newline... (both must behave identically per contract but only one exercises each internal path).

	t.Run("finalFragmentWithoutAnyTrailingNewline", func(t *testing.T) { // body ends with raw data bytes and NO final \n at all — ReadString returns them together with io.EOF in a single call.
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"tail-no-newline\"}}]}" // complete JSON payload on one data line but the stream simply stops mid-transmission right after it... (no newline character exists anywhere past that closing brace).

		s := newStream(newTinyChunkReader(body, 4), "") // small stride again to guarantee multiple read calls happen before EOF arrives — isolating pending-flush logic from any single-call coincidence.
		delta, err := s.Recv()                          // FIRST receive must deliver the flushed event rather than reporting termination immediately... (this is precisely what distinguishes correct behavior from a naive implementation that would lose this payload entirely).

		if !errors.Is(err, nil) && err != nil { // no error expected on THIS first call — the pending data still had enough information to form one valid decoded delta.
			t.Fatalf("first Recv returned unexpected terminal/error state: %v (delta=%#v)", err, delta) // full both-sides rendering since either half being wrong tells a different story about where framing went astray... (keeping them visible together avoids needing two separate runs to diagnose).
		} else if !delta.HasChoice || len(delta.ContentFragments) != 1 || delta.ContentFragments[0].Text != "tail-no-newline" { // exact content proof that what was flushed is genuinely OUR payload and not some partial garbage from an earlier state... (uniqueness of marker value makes attribution airtight).
			t.Fatalf("flushed pending event = %#v, want the unterminated tail's full text fragment", delta) // same self-explanatory failure message style as everywhere else in this file set for consistency.
		}

		if _, againErr := s.Recv(); !errors.Is(againErr, io.EOF) { // SECOND receive now sees true termination — nothing remains after flushing that one pending event... (pinning the exact two-step sequence is what makes "flush first then EOF" provable rather than merely plausible).
			t.Fatalf("second Recv after flushed tail = %v; want io.EOF", againErr) // message explicitly names which step of the contract this asserts so future readers can map it directly back to plan wording without re-deriving context.
		} else if cerr := s.Close(); cerr != nil {
			t.Fatalf("Close returned error: %v", cerr) // quiet release after clean two-step termination — same idempotent-friendly guarantee pinned everywhere else in the suite for parity's sake... (cheap insurance against cleanup-path state corruption hiding behind otherwise-green read assertions).
		}
	})

	t.Run("completeFinalDataLineThenImmediateStreamEnd", func(t *testing.T) { // body ends with a fully-newlined data line but NO blank-line terminator afterward — the NEXT read yields zero bytes plus EOF instead.
		body := "data: {\"choices\":[{\"delta\":{\"content\":\"tail-with-newline\"}}]}\n" // note the trailing \n here makes this technically a COMPLETE physical line... (yet it still lacks its logical event-terminator which is what distinguishes pending-from-delivered in SSE semantics).

		s := newStream(newTinyChunkReader(body, 6), "") // different stride from sibling sub-case above on purpose — both internal flush paths should be exercised under multiple delivery patterns to catch any hidden coupling between them... (one shared value across both would leave at least one combination untested by construction).
		delta, err := s.Recv()                          // same first-receive expectation as sibling: the pending-but-complete line becomes our sole delivered delta before anything terminal surfaces.

		if !errors.Is(err, nil) && err != nil { // identical no-error-first-call requirement for symmetric reasons documented in the other sub-case above... (consistency between these two rows IS part of what makes them both worth existing separately).
			t.Fatalf("first Recv returned unexpected terminal/error state: %v (delta=%#v)", err, delta) // full rendering again — same rationale as everywhere else about making single-line failures self-explanatory without follow-up investigation needed.
		} else if !delta.HasChoice || len(delta.ContentFragments) != 1 || delta.ContentFragments[0].Text != "tail-with-newline" { // unique marker value distinguishing this sub-case's payload from sibling row's in case both ever appear together in aggregated verbose output... (cheap disambiguation costs nothing per row).
			t.Fatalf("flushed pending event = %#v, want the newline-terminated tail's full text fragment", delta) // message names exactly which variant produced what for quick visual comparison when triaging side-by-side failures across both sub-cases simultaneously.
		}

		if _, againErr := s.Recv(); !errors.Is(againErr, io.EOF) { // second receive terminates cleanly as in sibling — the two-step flush-then-EOF sequence holds regardless of HOW that tail's final bytes were physically delivered... (this uniformity is exactly what makes callers above this boundary able to rely on one stable rule).
			t.Fatalf("second Recv after flushed complete-line tail = %v; want io.EOF", againErr) // explicit step-naming in message mirrors sibling phrasing for consistency across the pair.
		} else if cerr := s.Close(); cerr != nil {
			t.Fatalf("Close returned error: %v", cerr) // final quiet-release pin completing this sub-case's end-to-end story about terminal state stability under its specific delivery pattern... (no further receives may surface anything but EOF from here on out).
		} else if _, third := s.Recv(); !errors.Is(third, io.EOF) { // one extra belt-and-suspenders receive beyond the mandatory two-step check above — proving termination is sticky rather than transient even when probed a third time... (costs exactly one line and removes an entire class of "works twice but breaks on repeat" state bugs from consideration).
			t.Fatalf("third Recv after Close = %v; want io.EOF", third) // consistent phrasing with every other repeated-receive assertion in the suite so future readers find them all via identical search terms.
		}
	})

	if _, err := NewTransport(ResolvedTransport{Model: ModelRef{Provider: "p"}, WireSystemRole: ""}); !errors.Is(err, ErrInvalidModelRef) { // closing sanity anchor mirroring sibling files' convention one final time in THIS file as well — construction-time validation surface unchanged since this structural test was written... (kept deliberately cheap and local rather than extracted into a shared helper because each call site's context makes its failure message most useful when phrased for THAT row set specifically).
		t.Fatalf("premise broken: incomplete model identity no longer rejected at NewTransport") // same explicit framing as equivalent anchors elsewhere so grep finds every such guard consistently across the suite.
	}
}

// TestSSEMalformedPayloadsAreTypedProtocolErrorsNotSkipped pins that malformed event payloads — invalid JSON in general AND valid-JSON-but-wrong-top-level-shape specifically — surface through Recv as typed ErrProtocol failures wrapping their cause, never skipped silently nor retried by anything inside this package... (the stream stays terminated afterwards exactly like every other read-failure shape).
func TestSSEMalformedPayloadsAreTypedProtocolErrorsNotSkipped(t *testing.T) { // each row drives its own small body through a fresh stream so captured failure state cannot leak between cases — same per-row isolation discipline as everywhere else in this file set.

	cases := []struct{ name, payload string }{ // table form keeps both malformed classes visible side by side and trivially extendable if future wire shapes need pinning too... (the distinction matters because they fail at DIFFERENT layers internally: validity check vs structural decode).
		{"invalidJSONIsProtocolError", "{not-json"}, // first class: bytes that are not valid JSON at all — caught by the pre-decode json.Valid gate before any typed interpretation even begins.
		{"validJSONWrongTopLevelShape", "[]"},       // second class: syntactically perfect JSON whose top-level value is an array rather than a chunk object... (this one passes validity but fails structural decoding downstream).
	}

	for _, tc := range cases { // each iteration owns its own stream + body like everywhere else in this file — no shared state across rows by construction.
		t.Run(tc.name, func(t *testing.T) { // sub-test per case so -run filters can target a single malformed class during triage of related regressions elsewhere if needed later.
			body := "data: " + tc.payload + "\n\n" // wrap the payload in exactly one well-framed SSE event around it — framing itself is CORRECT here on purpose... (isolating the failure to decoding specifically so a pass/fail can never be blamed on boundary handling instead).

			s := newStream(newTinyChunkReader(body, 3), "") // tiny stride again for uniform coverage of read-boundary interactions even though they're not what's under test per se... (cheap insurance that our framing assumptions don't accidentally mask or alter the failure shape being pinned here).
			_, err := s.Recv()                              // single receive expected to fail immediately on THIS event rather than deferring any further into subsequent reads.

			if !errors.Is(err, ErrProtocol) { // THE core assertion: typed protocol sentinel reachable regardless of which internal layer produced the underlying cause... (both classes above must arrive under exactly this same umbrella identity).
				t.Fatalf("Recv for %s payload = %v; want an error wrapping ErrProtocol", tc.name, err) // naming WHICH class tripped keeps output unambiguous when both rows run together in aggregated verbose mode.
			} else if errors.Is(err, io.EOF) { // negative anchor: malformed events must NEVER masquerade as clean stream termination — conflating them would let callers believe their response completed normally despite receiving garbage... (this is arguably the most dangerous possible regression shape here).
				t.Fatalf("malformed payload %s surfaced as io.EOF instead of a protocol error", tc.name) // explicit about the exact conflation pattern being guarded against so future readers understand WHY this check exists separately from the positive one above it.
			}

			if _, againErr := s.Recv(); !errors.Is(againErr, io.EOF) { // post-failure terminal state: further receives see EOF through done flag rather than re-attempting or erroring repeatedly... (completing this row's end-to-end story about what a malformed payload leaves behind).
				t.Fatalf("second Recv after protocol failure = %v; want io.EOF", againErr) // mirrors every other post-failure terminal check in the suite for consistency — future readers should never have to wonder whether THIS particular shape was intentionally left unasserted.
			} else if cerr := s.Close(); cerr != nil {
				t.Fatalf("Close returned error following protocol failure: %v", cerr) // releasing after a failed read must still behave like every other Close in the suite — quiet, idempotent-friendly cleanup with no transport facts surfaced to consumers at this point... (included because close-after-error is exactly the kind of edge where state corruption hides best).
			}

			if _, ok := interface{}(s).(interface{ Retry() bool }); ok { // unreachable type-assertion guard documenting that NO retry-related method surface exists on our stream type under any name — its real value is forcing conscious acknowledgment that skipping/retrying malformed events is categorically impossible by construction rather than merely unimplemented today... (structural tests catch fields; this catches behavioral-method additions specifically at the stream boundary level).
				t.Fatal("stream types must never expose retry/skip handles publicly") // if someone ever adds one, fail loudly instead of letting event-skipping creep back in through a public accessor without any visible field-level change first — exactly the regression class this entire test file exists to prevent.
			}
		})
	}

}

// TestSSERecvAfterCloseReportsEOF pins early-abort behavior: closing an accepted stream BEFORE its payload is fully consumed makes every subsequent Recv report io.EOF immediately — no buffered-but-undelivered events leak out afterwards, and repeated Close calls stay quiet rather than surfacing double-close errors to consumers.
func TestSSERecvAfterCloseReportsEOF(t *testing.T) { // modeled on real-world consumer patterns where a caller decides mid-stream that it wants nothing further delivered — the contract guarantee worth pinning is exactly what state remains observable after such an abort... (nothing about how much was already consumed should change this terminal behavior).
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"first-real-event\"}}]}\n\n" + // one genuinely deliverable event placed FIRST so we can prove consumption-before-close works normally... (its successful delivery is the precondition that makes later abort assertions meaningful rather than vacuous).
		"data:{\"choices\":[]}\n\ndata: [DONE]\n\n" // followed by more content including a proper terminator — all of it must become UNREACHABLE once we decide to close early below.

	s := newStream(newTinyChunkReader(body, 8), "") // standard tiny-stride reader as everywhere else in this file set for uniform boundary coverage even on rows whose focus is elsewhere... (the choice costs nothing and keeps every row independently defensible).
	first, err := s.Recv()                          // consume exactly one real event before deciding to abort — establishing that normal delivery path works up to this exact point.
	if !errors.Is(err, nil) && err != nil {         // no error expected on THIS first call since the body is well-formed through at least its opening portion... (any failure here would mean something broke BEFORE our interesting part even started).
		t.Fatalf("first Recv before early close failed unexpectedly: %v", err) // stop with context rather than letting later assertions run against a stream that never actually delivered anything real to begin with.
	} else if len(first.ContentFragments) != 1 || first.ContentFragments[0].Text != "first-real-event" { // exact fragment shape pinning as everywhere else — same literal value used in multiple rows across this file deliberately for easy cross-reference when reading output... (choosing distinct values per row would make aggregated verbose logs harder to correlate).
		t.Fatalf("pre-close delta = %#v; want the single expected text fragment proving normal delivery worked first", first) // full rendering so any mismatch dimension is immediately visible without re-running under more detailed logging configuration.
	}

	if cerr := s.Close(); cerr != nil { // THE early-abort action itself: releasing mid-stream with unconsumed content still available on the wire behind us... (this single call is what everything below measures reaction to).
		t.Fatalf("early Close returned error: %v", cerr) // a non-nil return here would mean cleanup logic depends on some condition our fixture deliberately did not provide — fail loudly rather than continuing against possibly-corrupted internal state.
	}

	if _, againErr := s.Recv(); !errors.Is(againErr, io.EOF) { // FIRST post-close receive must be immediate clean EOF — no buffered events may leak through after release... (this is the core observable guarantee of "close stops delivery" semantics).
		t.Fatalf("first Recv after early close = %v; want immediate io.EOF", againErr) // message explicitly frames this as testing what close DID to pending data rather than generic framing correctness — different failure class deserving distinct phrasing.
	} else if _, second := s.Recv(); !errors.Is(second, io.EOF) { // repeated post-close receives keep reporting the same terminal state indefinitely... (proving stability across multiple probes rather than just one lucky first hit).
		t.Fatalf("second Recv after early close = %v; want sustained io.EOF", second) // consistent step-naming in message mirrors sibling phrasing for consistency when triaging ordering-sensitive failures later.
	} else if cerr2 := s.Close(); cerr2 != nil { // repeated Close calls must stay quiet — no double-close error may surface to the single consumer's deferred cleanup path under any circumstances... (this is a contract guarantee worth pinning exactly once rather than assuming from reading code).
		t.Fatalf("second early Close returned %v; want nil (idempotent release)", cerr2) // message distinguishes first vs second close failures for triage clarity when both could theoretically misbehave in the same run.
	} else if _, third := s.Recv(); !errors.Is(third, io.EOF) { // one final probe after BOTH closes to confirm terminal state persists through that additional release as well... (cheap insurance against any hidden re-arming of delivery logic between successive Close invocations).
		t.Fatalf("third Recv after double-close = %v; want sustained io.EOF", third) // explicit count-based naming in message makes it obvious exactly which probe iteration produced the surprise when output is skimmed quickly during triage.
	}

}

// failingBody wraps an initial good prefix with a forced error on subsequent reads — nothing more elaborate than needed since this test controls exactly WHEN and WHY it fails. It lives at package scope because Go forbids method declarations inside function bodies, even though only one test uses it today.
type failingBody struct { // small type existing only to inject one deterministic read-failure at precisely the moment TestSSEReadFailureIsTypedAndTerminal wants... (not inlined for that syntactic reason alone).
	prefix []byte // bytes available before the fault kicks in — consumed normally like any healthy source would deliver them.
	pos    int    // current consumption position within that prefix for Read's bookkeeping purposes only... (no external visibility beyond what io.Reader contract requires).
	fault  error  // THE injected failure itself: returned verbatim on whatever read call first encounters exhaustion of the good portion above... (its exact identity is asserted below through wrapping reachability checks).
}

func (b *failingBody) Read(p []byte) (int, error) { // io.Reader contract with a single deterministic fault point — nothing past prefix length ever reads cleanly.
	if b.pos >= len(b.prefix) { // fault condition reached: deliver nothing plus our synthetic cause... (zero-byte-plus-error is the standard io.Reader idiom for "failure now").
		return 0, b.fault // exact identity preserved so errors.Is reachability below proves no intermediate layer swallowed or replaced it.
	}
	n := copy(p, b.prefix[b.pos:]) // normal delivery path copies whatever remains of healthy prefix into caller's buffer as usual.
	b.pos += n                     // advance past what was actually delivered this call to keep state consistent across consecutive reads... (standard cursor discipline identical to tinyChunkReader above).
	return n, nil                  // success return for the healthy portion — fault awaits a LATER invocation after exhaustion per design.
}

func (b *failingBody) Close() error { return nil } // no resources to release beyond what caller already owns: same minimalism as every other reader in this file by construction... (implementing ReadCloser keeps it drop-in compatible with newStream's body parameter without wrapper structs anywhere).
// TestSSEReadFailureIsTypedAndTerminal pins non-EOF body read failures through the retained port semantics in order: a terminal read that still delivered an unterminated final event decodes it FIRST (the pending-first rule applies when termination is failure as well as clean EOF, exactly like its legacy source), then the injected fault itself surfaces on the following receive wrapped in ErrProtocol with its exact cause reachable via errors.Is — and every subsequent receive reports only io.EOF through the terminal done flag rather than re-surfacing the same failure.
func TestSSEReadFailureIsTypedAndTerminal(t *testing.T) { // one well-formed event delivered first to prove normal path works up to that point, THEN a synthetic mid-read injection fault triggers the interesting behavior under test... (separating setup from exercise keeps each assertion's intent unambiguous in failure output).

	body := "data:{\"choices\":[{\"delta\":{\"content\":\"before-fault\"}}]}\n\n" + // one complete well-formed event FIRST so normal delivery is proven working up to this exact moment... (its successful parse below doubles as precondition check for everything interesting that follows).
		"data: {\"choices\":[{\"delta\":{\"content\":\"pending-at-fault\"}}]}" // trailing COMPLETE JSON without its terminating blank line — the retained pending-first rule decodes it when the terminal read arrives, even though that termination is our injected failure rather than clean EOF... (this sub-case and TestSSEPendsUnterminatedDataEventsAtEOF together pin both sides of "pending events decode at ANY terminal read").

	src := &failingBody{prefix: []byte(body), fault: errors.New("synthetic mid-stream read failure")} // wire up exactly one deterministic cause at a known location within otherwise-healthy content... (determinism matters more than realism here for pure behavior-shape testing).
	s := newStream(src, "")                                                                           // standard construction as everywhere else in this file set — the fault lives entirely inside src's Read semantics rather than anywhere near framing logic itself.

	first, err := s.Recv() // consume the one good event before triggering any interesting state changes below... (normal-path proof preceding exercise of failure handling).
	if err != nil {        // no error expected on THIS first call since its payload is complete and well-formed per body construction above — stop with context rather than letting later assertions run against a stream that never actually delivered anything real.
		t.Fatalf("first Recv before fault injection failed unexpectedly: %v", err) // cheap early exit keeps output focused on whichever phase actually failed.
	} else if len(first.ContentFragments) != 1 || first.ContentFragments[0].Text != "before-fault" { // exact fragment shape pinning as everywhere else in the suite — same self-explanatory failure message style throughout.
		t.Fatalf("pre-fault delta = %#v; want proof that normal delivery worked before injecting our synthetic read error", first) // full rendering so any mismatch dimension is immediately visible without re-running under more detailed logging configuration than default provides.
	}

	pending, perr := s.Recv() // the terminal read delivered the unterminated final event together with our injected fault: retained rule decodes that complete payload before anything else... (identical pending-first behavior to its clean-EOF sibling — only the kind of termination differs).
	if perr != nil {          // no error here precisely because those bytes formed a COMPLETE valid payload despite never receiving their blank-line terminator.
		t.Fatalf("Recv delivering the pending-at-failure event = %v; want that complete final payload decoded first per retained rule", perr)
	} else if !pending.HasChoice || len(pending.ContentFragments) != 1 || pending.ContentFragments[0].Text != "pending-at-fault" { // exact marker value distinguishes this delta from every sibling row's in aggregated verbose output.
		t.Fatalf("pending-at-failure delta = %#v; want the complete unterminated final event decoded before any fault surfaces", pending)
	}

	if _, ferr := s.Recv(); !errors.Is(ferr, ErrProtocol) { // now that no payload remains to flush: THE injected fault itself must surface wrapped in the typed protocol sentinel — same umbrella identity as every other read-level failure class in this package.
		t.Fatalf("Recv after pending-flush = %v; want an error wrapping ErrProtocol", ferr) // naming exactly which layer's expectation failed keeps output unambiguous when triaging against sibling malformed-payload rows elsewhere... (consistent phrasing across all such assertions is what lets humans find related coverage quickly).
	} else if !errors.Is(ferr, src.fault) { // AND the specific synthetic cause itself must remain reachable through wrapping — proving no intermediate layer swallowed or replaced our deliberately-injected identity along the way.
		t.Fatalf("injected fault not reachable through wrapped Recv error: %v (chain does not contain our sentinel)", ferr) // full value rendered so triage sees precisely which unrelated error surfaced instead when this regresses... (message phrasing mirrors sibling rows' convention of showing complete actual values alongside derived checks).
	} else if _, again := s.Recv(); !errors.Is(again, io.EOF) { // post-fault terminal state: further receives see EOF through done flag rather than re-surfacing the same fault repeatedly... (uniformity across ALL read-error shapes is what callers can rely on above this boundary).
		t.Fatalf("Recv after injected fault = %v; want sustained io.EOF", again) // mirrors every other post-failure terminal check in the suite for consistency — future readers should never have to wonder whether THIS particular shape was intentionally left unasserted.
	} else if cerr := s.Close(); cerr != nil {
		t.Fatalf("Close returned error following injected fault: %v", cerr) // releasing after a failed read must still behave like every other Close in the suite — quiet, idempotent-friendly cleanup with no transport facts surfaced to consumers at this point... (included because close-after-error is exactly the kind of edge where state corruption hides best).
	}

}
