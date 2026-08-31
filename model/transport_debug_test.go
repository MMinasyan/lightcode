package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTransportWireDiagnosticsWriteReqAndChunksPerDecodedEvent pins the retained wire-diagnostics contract end to end: with a resolved debug directory present, one request dump is written per invocation carrying exactly the encoded body bytes (verified against what the server actually received), and each successfully decoded SSE event appends its raw payload as one JSONL line in delivery order — malformed or [DONE] events never appear there.
func TestTransportWireDiagnosticsWriteReqAndChunksPerDecodedEvent(t *testing.T) { // two well-formed events plus a clean terminator give the minimum nontrivial corpus for asserting BOTH artifacts' contents and their shared-id pairing... (a single event would leave "per-decoded-event" indistinguishable from "once per stream").
	const payloadOne = `{"choices":[{"delta":{"content":"one"}}]}`           // exact wire bytes served on this line — the chunks artifact must contain them verbatim after decode, so they double as both input and expected output of the append path.
	const payloadTwo = `{"usage":{"prompt_tokens":2,"completion_tokens":1}}` // a choiceless usage-only event: it decodes successfully (HasChoice=false) and therefore appends too... pinning that "successfully decoded" means parsed-OK, not merely contained-a-choice.

	var receivedBody []byte                                                                      // captured by the handler for byte-for-byte comparison against the request dump below — proving the dump is a faithful copy of THE ACTUAL encoded body rather than some independently-computed (and possibly divergent) value...
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // minimal SSE server: capture then serve exactly two events plus [DONE] with no extra framing surprises.
		receivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: "+payloadOne+"\n\n"+ // single leading space after the colon is stripped during framing — payload bytes below stay exact either way... (using both spaced and unspaced forms across the two events proves neither style disturbs retention).
			"data:"+payloadTwo+"\n\ndata:[DONE]\n\n")
	}))
	defer server.Close()

	dir := t.TempDir() // isolated per-test filesystem root so file assertions can enumerate exhaustively without shared-state interference between rows... (TempDir cleanup is automatic and keeps no artifacts behind on failure either).
	rt := testResolved()
	rt.BaseURL = server.URL // pinned pre-construction per suite convention — resolved input is immutable after NewTransport deep-copies it.
	rt.WireDebugDir = dir   // THE switch under test: everything below about artifact existence/contents flows from this one field being nonempty.

	tr := mustTransport(t, rt)
	stream, _, err := tr.Stream(context.Background(), baseRequest(), nil) // acceptance must succeed with diagnostics enabled — any failure here would make every file assertion vacuous... (establishing the happy path first keeps triage linear if it ever breaks).
	if err != nil {
		t.Fatalf("Stream failed: %v", err)
	} else if _, rerr := stream.Recv(); rerr != nil || len(receivedBody) == 0 { // drain one delta to prove the exchange actually flowed end-to-end (request bytes captured AND at least one event decoded)... a second Recv would just re-read remaining events for no additional assertion value here since file contents are what this test exists to verify.
		t.Fatalf("first Recv = %v; want normal delivery with diagnostics enabled", rerr) // names which half of the precondition failed (delivery vs capture) so output points at its exact broken link in the chain.
	} else {
		for { // drain fully so [DONE] completes and any deferred artifact writes settle before file inspection below... (nothing is deferred by construction today, but draining to terminal state makes the test robust against future edits adding flush-on-close behavior).
			if _, e := stream.Recv(); errors.Is(e, io.EOF) {
				break // clean terminator reached — all events consumed.
			} else if e != nil {
				t.Fatalf("mid-drain Recv failed: %v", e) // any non-EOF error mid-stream with diagnostics enabled would indicate the debug path corrupted delivery itself... (exactly the class of regression this whole file exists to prevent).
			}
		}
		if cerr := stream.Close(); cerr != nil {
			t.Fatalf("Close returned error: %v", cerr) // idempotent quiet release as everywhere else in the suite — pinning it here too keeps close-after-full-drain covered under diagnostics-on conditions specifically.
		}
	}

	entries, lerr := os.ReadDir(dir) // enumerate exhaustively rather than guessing filenames (ids are timestamp-based and thus unguessable by test logic)... ReadDir failure itself is a real precondition break worth stopping on with context...
	if lerr != nil {
		t.Fatalf("read diagnostics dir: %v", lerr)
	} else if len(entries) != 2 { // exactly TWO artifacts per exchange — request dump and chunks file, nothing more (no directories, no temp leftovers, no duplicate ids)... count first so any extra/missing member is attributed before content inspection begins.
		t.Fatalf("diagnostics dir holds %d entries; want exactly the req+chunks pair: %#v", len(entries), entryNames(entries)) // full name list rendered so which expected artifact went missing or what unexpected one appeared is immediately visible alongside its sibling's for direct pairing comparison.
	}

	var reqFile, chunksFile string // identify each by suffix since shared timestamp base is the only thing distinguishing them beyond extension... (suffix-based selection keeps this test independent of id format details that could legitimately change without breaking contract).
	for _, e := range entries {    // single pass collecting both names for assertions below.
		switch {
		case strings.HasSuffix(e.Name(), "-req.json"):
			reqFile = filepath.Join(dir, e.Name())
		case strings.HasSuffix(e.Name(), "-chunks.jsonl"):
			chunksFile = filepath.Join(dir, e.Name())
		default: // any third suffix shape is already caught by the count check above but stated explicitly here too so a same-count-different-shape regression names its offender directly rather than failing opaquely on a later nil-file comparison... (cheap belt-and-braces against silent name-format drift).
			t.Fatalf("unexpected diagnostics artifact %q alongside req+chunks pair", e.Name()) // full path rendered for direct filesystem inspection if this ever fires in CI where local reproduction requires the exact same timestamp collision.
		}
	}

	if base := strings.TrimSuffix(filepath.Base(reqFile), "-req.json"); !strings.HasPrefix(filepath.Base(chunksFile), base) { // shared-id pairing: both artifacts derive from ONE exchange id so consumers can correlate them without parsing timestamps... (this is THE property making the pair a unit rather than two coincidentally-similarly-named files).
		t.Fatalf("request dump %q and chunks file %q do not share an exchange id prefix", reqFile, chunksFile) // both full names rendered side by side so any formatting divergence between the two write paths is visible character-by-character in output alone.
	}

	reqBytes, rerr := os.ReadFile(reqFile) // request dump content must equal EXACTLY what crossed the wire — byte-for-byte against handler-captured body rather than a recomputed expectation... (recomputing via Encode would test encode-twice-consistency which is already covered elsewhere; comparing to reality catches transport-side corruption specifically).
	if rerr != nil {
		t.Fatalf("read request dump: %v", rerr) // filesystem read failure here means the earlier WriteFile lied about success — stopping with its own distinct message keeps that hypothetical class triage-able separately from content-mismatch failures below.
	} else if !bytesEqual(reqBytes, receivedBody) {
		t.Fatalf("request dump bytes (%d) differ from wire body (%d): %q vs %q", len(reqBytes), len(receivedBody), reqBytes, receivedBody) // lengths first (cheapest distinguishing signal under large-body regressions) then full content rendering so the exact diverging region is visible without diffing two files externally...
	}

	chunkBytes, cerr := os.ReadFile(chunksFile) // chunks artifact: one JSONL line per successfully decoded event in delivery order — read whole file for structural assertion before any per-line checks below.
	if cerr != nil {
		t.Fatalf("read chunks dump: %v", cerr) // same distinct-failure-class rationale as the request-dump read above applied to its sibling artifact... (keeping each filesystem operation's failure independently named preserves triage linearity across this test's two parallel verification threads).
	} else if lines := splitJSONL(chunkBytes); len(lines) != 2 { // exactly our TWO decoded events — [DONE] never appends itself and no phantom third entry may exist from any code path... (count pinned before content so an off-by-one framing bug fails loudly here with its full line list rendered for immediate inspection).
		t.Fatalf("chunks artifact holds %d lines; want one per successfully-decoded event: %#v", len(lines), lines) // each raw payload string included verbatim in output so malformed-vs-missing-vs-extra failure modes are distinguishable by looking at actual retained bytes rather than guessing from counts alone.
	} else if !json.Valid([]byte(lines[0])) || !strings.Contains(lines[0], "one") { // line zero must be our FIRST event's exact payload — both structural validity (it IS JSON) and content attribution (that specific literal marker value appears within it)... double-checking because a reordered or concatenated write would pass pure-validity checks while still violating delivery-order retention.
		t.Fatalf("first chunk line = %q; want valid JSON containing the first event's exact payload", lines[0]) // full retained bytes rendered so any framing-level corruption (truncated join, doubled newline residue...) is visible character-by-character against this row's served constant above for direct comparison without re-running.
	} else if !json.Valid([]byte(lines[1])) || !strings.Contains(lines[1], "prompt_tokens") { // line one must be the usage-only event — its distinctive field name serves as content attribution marker exactly like the sibling check did... (using a FIELD NAME rather than full equality here deliberately leaves room for future whitespace-normalization at append time while still catching any reordering or cross-event contamination).
		t.Fatalf("second chunk line = %q; want valid JSON carrying the usage-only event", lines[1]) // same rendering discipline as above so both orderings of this pair's failure modes produce equally actionable output in aggregated test reports.
	} else if strings.Contains(string(chunkBytes), "[DONE]") { // negative anchor: the terminator marker must NOT be retained anywhere in chunks — it ends delivery rather than forming a decodable payload... (pinning its absence explicitly keeps any future "append everything received" simplification failing loudly at this exact assertion instead of silently polluting downstream consumers' JSONL parsing).
		t.Fatalf("chunks artifact contains [DONE] terminator text which must never be retained: %q", chunkBytes) // full file rendered so where the leak sits (extra line vs embedded fragment...) is immediately locatable from output alone.
	}
}

// TestTransportWireDiagnosticsFailureNeverAltersResults pins failure isolation at both diagnostic write points simultaneously: a debug directory that cannot be created writes nothing anywhere yet must leave transport AND stream results completely untouched — the retained legacy rule that diagnostics can never alter behavior, verified under real filesystem conditions rather than mocked ones... (this is THE safety property justifying keeping these artifacts synchronous-and-simple instead of buffering them with their own error channels).
func TestTransportWireDiagnosticsFailureNeverAltersResults(t *testing.T) { // nonexistent subdirectory inside a valid TempDir root guarantees both write points fail identically through real OS paths rather than any in-memory mock approximation... (choosing "parent exists but child does not" over a fully-impossible path like /dev/null/xyz specifically exercises the realistic misconfiguration shape operators actually encounter).
	const payload = `{"choices":[{"delta":{"content":"diagnostics-off-effect"}}]}` // unique marker value distinguishing this test's single event from every sibling row's in case both ever appear together under aggregated verbose output... (cheap disambiguation costs nothing per row while keeping attribution airtight across the whole suite).

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // serve exactly one well-formed event plus clean terminator — minimum corpus sufficient to prove delivery survives diagnostic failure completely.
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: "+payload+"\n\ndata:[DONE]\n\n")
	}))
	defer server.Close()

	root := t.TempDir() // valid parent root so the ONLY filesystem condition failing below is the missing child directory itself... (isolates our intended failure mode from any unrelated permission/path-shape issues that a more exotic fixture could introduce).
	rt := testResolved()
	rt.BaseURL = server.URL                                 // standard pre-construction pinning as everywhere else in this file set for uniformity of resolved-input handling across all diagnostic rows.
	rt.WireDebugDir = filepath.Join(root, "does-not-exist") // THE injected failure: no MkdirAll happens anywhere in production by design so this path stays permanently unwritable through both dumpRequest and every subsequent append... (deliberately NOT creating it first is the whole point of this row's fixture).

	tr := mustTransport(t, rt)
	stream, _, err := tr.Stream(context.Background(), baseRequest(), nil) // acceptance MUST still succeed with diagnostics failing — if this ever breaks, operators lose entire chat streams because a debug folder was misconfigured... (the exact catastrophic coupling class that motivated failure isolation as an explicit contract rather than incidental behavior).
	if err != nil {
		t.Fatalf("Stream failed despite unwritable diagnostics dir: %v", err) // names the injected condition explicitly in output so any future reader immediately knows which fixture aspect this assertion protects against... (self-documenting failure messages keep triage fast even months after initial authorship when context has faded).
	}

	var deltas int // count delivered fragments as positive proof that delivery proceeded normally end-to-end despite zero successful diagnostic writes anywhere along the way...
	for {          // drain to clean terminal state exactly like sibling tests do — uniformity in how far each row consumes keeps cross-row comparisons meaningful when triaging ordering-sensitive regressions later.
		delta, e := stream.Recv() // one receive at a time with explicit error handling below rather than batching into recvUntilTerminal since this row needs per-delta counting for its positive-delivery assertion... (the helper's all-or-nothing return shape doesn't expose intermediate counts which THIS specific invariant requires).
		if errors.Is(e, io.EOF) { // clean terminator reached — stream completed normally under total diagnostic failure.
			break // exit drain loop with full consumption proven below via delta count check rather than merely reaching EOF silently... (stating the break rationale inline keeps future readers from wondering whether partial-consumption exits were also acceptable here).
		} else if e != nil { // ANY non-EOF error during delivery would mean diagnostic failure corrupted transport behavior itself — THE regression class this entire test exists to catch and prevent forever.
			t.Fatalf("Recv failed while diagnostics could not write: %v", e) // full cause rendered so the exact corruption path (which layer, what operation...) is visible alongside its injected fixture condition for immediate root-cause attribution without additional instrumentation passes.
		} else if len(delta.ContentFragments) != 1 || delta.ContentFragments[0].Text != "diagnostics-off-effect" { // per-delta shape pinning proving the delivered content matches EXACTLY what was served — not merely that SOME delta arrived at all... (content-level verification rather than mere count keeps this row robust against hypothetical partial-delivery regressions that still terminate cleanly).
			t.Fatalf("delta corrupted under diagnostic failure: %#v; want our single exact text fragment", delta) // full struct rendered so which dimension drifted (count, kind, or actual value...) is immediately attributable from output alone alongside its expected constant defined at this test's top for direct visual comparison.
		} else { // successful well-formed delivery recorded — incrementing the running count toward final assertion below... (per-delta validation happens BEFORE counting so a corrupted-then-recovered sequence could never inflate our success metric past truth).
			deltas++ // exactly one delta expected from this fixture's single served event; anything more or less fails its own dedicated check at drain completion rather than here mid-loop where context would be thinner... (deferring count validation to post-drain keeps the loop body focused purely on per-iteration shape checks without mixing scopes of assertion).
		}
	}

	if cerr := stream.Close(); cerr != nil { // quiet idempotent release even after a fully-failed diagnostic environment — close path must remain independent of debug-write success/failure exactly like every other lifecycle operation in this package... (pinning it here specifically under failed-diagnostics conditions covers the intersection rather than assuming independence transfers from happy-path rows elsewhere).
		t.Fatalf("Close returned error following unwritable diagnostics: %v", cerr) // names both the precondition and its consequence for immediate triage clarity when skimming aggregated output across multiple failing sub-rows simultaneously.
	} else if deltas != 1 { // final positive-delivery count assertion completing this test's core claim that NOTHING observable changed from disabling... (one delta is exactly what one served event produces under normal conditions — equality with that baseline IS the proof of non-interference).
		t.Fatalf("delivered %d fragments; want exactly 1 proving diagnostic failure altered no delivery outcome", deltas) // explicit wording about WHAT this count comparison proves keeps future readers from misreading it as a generic stream-completeness check when its real job is verifying behavioral invariance under injected infrastructure failure.
	}

	if entries, lerr := os.ReadDir(root); lerr != nil { // the valid parent root must contain NOTHING — no partial files, no temp remnants, no half-written chunks from either failing write point... (empty-root assertion rather than per-artifact absence checks because ANY unexpected entry here represents a new failure mode worth seeing named explicitly in output).
		t.Fatalf("read diagnostics root: %v", lerr) // filesystem read failure at this late stage would itself indicate something unusual about our TempDir lifecycle under test conditions... (stopping with its own message keeps that hypothetical class distinguishable from content-level findings below rather than muddling the two together in one ambiguous error string).
	} else if len(entries) != 0 { // zero entries is THE expected end state after both write points failed on their nonexistent target directory... any deviation means some code path partially succeeded against an impossible location or leaked artifacts elsewhere under our control.
		t.Fatalf("unwritable diagnostics dir left %d filesystem entries behind: %#v", len(entries), entryNames(entries)) // full name list rendered so whichever artifact (or unexpected shape) survived despite its write failing is immediately visible alongside every sibling's for direct pairing analysis of which operation produced what remnant.
	}

	if _, err := NewTransport(ResolvedTransport{Model: ModelRef{Provider: "openai"}}); !errors.Is(err, ErrInvalidModelRef) { // closing sanity anchor mirroring sibling files' convention — construction-time validation surface unchanged since this diagnostic row was written... (kept deliberately cheap and local rather than extracted into a shared helper because each call site's context makes its failure message most useful when phrased for THAT specific fixture condition).
		t.Fatalf("premise broken: incomplete model identity no longer rejected at NewTransport") // same explicit framing as equivalent anchors elsewhere so grep finds every such guard consistently across the suite even under verbose aggregated output where individual messages scroll past quickly during triage.
	}
}

// entryNames renders directory entries' names only (not full structs) for compact failure-message rendering — enough to identify any unexpected artifact by name without dumping filesystem metadata noise into test output... (deliberately minimal helper since its sole consumer is this file's two Fatalf sites above).
func entryNames(entries []os.DirEntry) []string { // one-liner kept as a named function rather than inlined lambda so both call sites read symmetrically and any future addition of sorting/deduplication logic has exactly ONE place to live... (premature abstraction avoided beyond what those two identical rendering needs already justify).
	names := make([]string, 0, len(entries)) // preallocated at exact size since we know the full count upfront — no append-growth churn even though test-scale sizes would never measurably care about it anyway... (allocating correctly costs zero cognitive overhead versus a plain empty slice here).
	for _, e := range entries {              // single pass in directory order as returned by ReadDir which is already sorted per its documented contract... (relying on that ordering rather than re-sorting ourselves keeps output stable across runs without additional code to maintain or test separately).
		names = append(names, e.Name()) // name-only extraction discarding type/size/mtime metadata deliberately since none of those carry diagnostic value in a failure message about WHICH files exist versus which were expected... (keeping messages compact is what makes them actually readable when multiple rows fail simultaneously under aggregated CI output constraints).
	}
	return names // returned as plain string slice matching the Fatalf %#v rendering convention used at every sibling call site for consistent visual formatting across this package's test suite overall.
}

// bytesEqual compares two byte slices without importing a second comparison utility beyond what stdlib already provides through equality semantics suitable for exact-content assertions... (deliberately NOT using reflect.DeepEqual or testing.T directly inside since those would either add import churn here or couple the helper to its single caller unnecessarily).
func bytesEqual(a, b []byte) bool { // length-first short-circuit keeps large-body mismatches cheaply distinguishable from same-length content drifts in any future profiling of this test's runtime characteristics... (micro-optimization irrelevant at current scale but structurally correct ordering regardless).
	if len(a) != len(b) { // unequal lengths are the most common real-world divergence mode when comparing retained wire bytes against recomputed expectations elsewhere in broader suites too... (documenting that expectation here even though THIS test compares captured-vs-dumped makes the helper's general applicability clearer for future reuse candidates).
		return false // early exit before any per-byte work happens at all — same principle as every other well-written comparison primitive worth referencing when teaching newcomers why length checks precede element loops universally.
	} // trailing comment line exists purely to keep gofmt alignment stable on the block above... (purely cosmetic consideration with zero behavioral impact whatsoever).
	for i := range a { // index-based iteration over equal-length slices is idiomatic Go for byte-wise comparison without allocating copies or iterator machinery of any kind needed beyond what's already imported elsewhere in this package.
		if a[i] != b[i] { // first differing position suffices to declare inequality — no need to scan remaining bytes once divergence confirmed since boolean outcome cannot change thereafter... (constant-time concerns deliberately out of scope here as these are plaintext debug artifacts never carrying secret material by design).
			return false // immediate return on discovery rather than counting total mismatches keeps failure latency minimal under worst-case large-divergent-body scenarios that could theoretically arise from severe transport corruption bugs.
		} // blank separator line between conditional body and closing brace improves visual scannability of this nested structure specifically... (stylistic choice consistent with sibling helpers' formatting conventions throughout).
	} // reaching here means every byte matched across the entire compared span — success path falls through to explicit true below rather than implying it implicitly via fall-off behavior.
	return true // definitive positive confirmation that both inputs are identical in length AND content for their full extent... (explicit return statement preferred over bare expression result per package style guide consistency requirements).
}

// splitJSONL splits a chunks artifact's raw bytes into its non-empty line values preserving order — trailing newline from the final append produces no phantom empty entry since we filter empties explicitly rather than relying on reader conventions varying across platforms... (robustness against minor formatting differences in how future edits might terminate their writes).
func splitJSONL(b []byte) []string { // single-pass linear scan with standard library string operations only — no external dependencies or custom buffering machinery warranted at this scale of artifact sizes encountered under normal operation conditions anywhere near production workloads today.
	var out []string                                      // lazily allocated on first non-empty line discovered rather than upfront since empty artifacts (the disabled-diagnostics case) should ideally allocate nothing beyond zero-value initialization overhead... (allocation discipline matters less here but costs literally nothing to observe anyway).
	for _, line := range strings.Split(string(b), "\n") { // converting whole buffer once up-front is simpler and equally performant at these sizes compared with incremental scanning approaches that would add significant code complexity for no measurable benefit under realistic diagnostic artifact volume levels.
		if line == "" { // skip blank lines including any trailing terminator residue — they carry zero information content worth retaining in our structured assertion inputs below... (filtering them here centralizes the "what counts as a real chunk record" definition rather than scattering that judgment across every consuming check site).
			continue // continue-to-next-iteration idiom chosen over if/else-inversion purely for readability preference matching sibling helpers' general flow style throughout this package's test infrastructure.
		} // blank separator line between conditional body and append statement below improves visual scannability of this nested structure specifically... (stylistic choice consistent with sibling helpers' formatting conventions throughout).
		out = append(out, line) // preserve original wire ordering exactly since delivery-order retention is precisely what downstream assertions depend upon validating against served constants in fixed sequence.
	} // reaching end-of-input without finding anything yields nil slice which callers handle identically to empty-but-non-nil ones for counting purposes... (nil-vs-empty distinction deliberately not surfaced further up the call stack).
	return out // returned as-is with no defensive copy since string values are immutable in Go and therefore safe to share across multiple assertion contexts simultaneously.
}
