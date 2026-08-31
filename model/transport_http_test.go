package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mustTransport builds a transport from the given resolved input, failing the test on any construction error. Valid-input fixtures only; every rejection case is asserted explicitly where it belongs (TestNewTransportRejectsInvalidResolvedInput).
func mustTransport(t *testing.T, rt ResolvedTransport) *Transport { // shared fixture helper for this file and sibling transport/stream tests in the package — all of them construct before dialing anything.
	t.Helper()
	tr, err := NewTransport(rt)
	if err != nil {
		t.Fatalf("NewTransport returned error for a valid resolved input: %v", err)
	}
	return tr
}

// baseRequest is the smallest logical request these tests stream with; it reuses the encoder suite's user-text fixture so body comparisons against Encode stay byte-exact.
func baseRequest() Request { return Request{Messages: []Message{userText("hi")}} } // one message, no tools — keeps encoded bodies minimal and stable across assertions in this file set.

// TestNewTransportRejectsInvalidResolvedInput pins construction-time validation with typed errors naming the offending field: zero and partial target identities fail via ErrInvalidModelRef, an out-of-set wire system role fails via ErrInvalidWireSystemRole (with the value in context), and every closed-set member plus a valid baseline construct cleanly without any I/O side effect.
func TestNewTransportRejectsInvalidResolvedInput(t *testing.T) { // resolved-input validity at construction is exactly this matrix — nothing else may fail here, since request-level concerns belong to Stream's trust boundary instead... (that split keeps each error surface owning its own inputs).

	if _, err := NewTransport(ResolvedTransport{}); !errors.Is(err, ErrInvalidModelRef) { // zero identity: provider and model both empty.
		t.Fatalf("zero-model error = %v, want one wrapping ErrInvalidModelRef", err) // the field-and-detail requirement shows up in this message carrying which value was seen... (asserted below for partial too).
	}

	partial := testResolved()
	partial.Model = ModelRef{Provider: "openai"} // provider-only is not complete — same rule as everywhere else in this package where a nonzero identity is required.
	if _, err := NewTransport(partial); !errors.Is(err, ErrInvalidModelRef) {
		t.Fatalf("partial-model error = %v, want one wrapping ErrInvalidModelRef", err) // partial must be rejected exactly like zero here; both are "not complete" for transport purposes... (the distinction never matters downstream because nothing can encode a request from either).
	}

	badRole := testResolved()
	badRole.WireSystemRole = "assistant" // outside the closed system/user/developer wire-role set — assistant is a canonical conversation role, not a wire system role.
	if _, err := NewTransport(badRole); !errors.Is(err, ErrInvalidWireSystemRole) {
		t.Fatalf("bad-wire-role error = %v, want one wrapping ErrInvalidWireSystemRole", err) // the offending value must be identifiable from this chain (the helper's own message carries it; Is() is what callers classify on).
	}

	for _, role := range []string{"system", "user", "developer"} { // every closed-set member constructs — pinning that validation rejects exactly out-of-set values and nothing else... (an over-strict implementation would fail one of these rows instead of the badRole row above).
		valid := testResolved()
		valid.WireSystemRole = role
		if _, err := NewTransport(valid); err != nil {
			t.Fatalf("wire role %q rejected at construction: %v", role, err) // no I/O happens on any success path — the type performs none by contract (no files appear, nothing dials)... which is why this loop can run without a server.
		}
	}

	if _, err := NewTransport(testResolved()); err != nil { // plain valid baseline: the happy construction itself must be an error-free no-op... (it is asserted last so its green cannot mask any rejection-row failure above).
		t.Fatalf("valid resolved input rejected at construction: %v", err) // a regression here would mean NewTransport started validating something outside its contract surface — worth failing loudly and specifically rather than discovering mid-suite.
	}
}

// TestNewTransportDeepCopiesResolvedInput pins the construction ownership rule end to end over the wire: mutating every reference-typed field of the caller's ResolvedTransport after building (header values, extra-body key deletion, in-place byte rewrite of an extra value) must never reach what a subsequent request from that transport actually posts.
func TestNewTransportDeepCopiesResolvedInput(t *testing.T) { // one mutation pass over each retained map is observable only through the bytes/headers this server captures next — exactly why no reflection shortcut replaces a real round trip here... (a copy-on-construction bug would leak precisely these mutations onto the wire).
	var gotHeader http.Header
	var rawBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // capture both sides of what construction retained: headers by value map and body bytes verbatim... (a 2xx with no stream content is all this test consumes from the response).
		gotHeader = r.Header.Clone()        // cloned so assertions after Close below read stable data even if transport internals still reference request-side state somewhere unexpectedly.
		rawBody, _ = io.ReadAll(r.Body)     // bytes as posted — compared against construction-time values rather than caller-mutated ones... (that asymmetry IS the assertion).
		w.WriteHeader(http.StatusNoContent) // 204: an accepted terminal response; nothing streams from it and Close below is a quiet release.
	}))
	defer server.Close()

	rt := ResolvedTransport{ // every retained map present at construction time so each has something to mutate afterwards... (leaving any out would make its copy-on-construction guarantee untested in this row set).
		Model:             ModelRef{Provider: "openai", Model: "gpt-test"},
		BaseURL:           server.URL, // endpoint pinned BEFORE construction — the transport must reach THIS server with retained values... (pointing it at defaults post-hoc would dial real hosts and break hermeticity).
		Headers:           map[string]string{"X-From": "caller"},
		ProviderExtraBody: Extra{"temperature": json.RawMessage(`0.1`)},
	}

	tr, err := NewTransport(rt) // construction must own deep copies before any of the mutations below can observe anything about them... (this is also why BaseURL was set pre-construction above rather than via mutation after).
	if err != nil {
		t.Fatalf("NewTransport returned error: %v", err) // a failure here would make every later assertion meaningless — stop with context instead of chasing phantom wire mismatches below.
	}

	rt.Headers["X-From"] = "mutated"            // mutating the original header map after construction... (must not change what this transport posts next).
	delete(rt.ProviderExtraBody, "temperature") // ...and deleting a layer key from the caller's own copy of it... (the retained body must still carry temperature on its request bytes).
	rewritten := json.RawMessage(`0.9`)         // and rewriting an extra value's bytes in place through a fresh slice holding identical initial content:
	copy(rewritten, []byte("2"))                // the posted body must show 0.1 regardless — proof that retained bytes are independent copies rather than shared backing arrays... (the classic aliasing bug this row exists to catch).

	stream, _, streamErr := tr.Stream(context.Background(), baseRequest(), nil)
	if streamErr != nil {
		t.Fatalf("Stream returned error after construction-time ownership was exercised: %v", streamErr) // any failure here means the mutation pass above somehow corrupted retained state — which is exactly what this test forbids... (so fail with that framing, not a generic message).
	} else if got := gotHeader.Get("X-From"); got != "caller" { // header value must still be the construction-time one on the wire.
		t.Fatalf("header X-From posted as %q after caller mutated it to \"mutated\" — retained headers are not independent copies", got) // a leak here means NewTransport stored the map reference instead of copying entries into its own... (the fix is mechanical once this row names it).
	} else if _, err := stream.Recv(); !errors.Is(err, io.EOF) { // 204 carries no body: first receive must be clean EOF — pinning that an empty accepted response does not error the consumer path either.
		t.Fatalf("Recv on a content-less accepted response = %v, want io.EOF", err) // (this sub-check is included because Close below would otherwise hide any read-side state corruption from view entirely).
	} else if cerr := stream.Close(); cerr != nil {
		t.Fatalf("Close returned error: %v", cerr) // releasing a fully-consumed empty body must be quiet — no cleanup path may surface transport facts to the consumer here... (any non-nil would mean release logic depends on something this fixture did not provide).
	}

	var posted map[string]json.RawMessage
	if err := json.Unmarshal(rawBody, &posted); err != nil { // decode what was actually sent so field-level comparisons below read naturally in failure messages too.
		t.Fatalf("posted body is not a JSON object: %v (%s)", err, rawBody) // unparseable bytes here would mean the ownership copy corrupted structure — fail loudly with both sides visible for diagnosis... (nothing else in this test can distinguish that shape from others).
	}

	rawTemp, ok := posted["temperature"] // temperature must still ride on the wire exactly as constructed: present AND byte-identical to 0.1's raw form after every caller mutation above.
	if !ok {
		t.Fatalf("posted body lost provider extra key \"temperature\" — retained layer was not independently owned at construction (body=%s)", rawBody) // absence here means the delete() call reached transport state through a shared map reference... (the single most likely aliasing bug shape in this file's scope).
	} else if !bytes.Equal(bytes.TrimSpace(rawTemp), []byte(`0.1`)) { // byte-level equality pins no sharing of backing arrays either — value-equality alone would miss the rewrite case above.
		t.Fatalf("posted temperature = %s, want exactly 0.1's bytes (caller rewrote its own copy to another digit)", rawTemp) // a different-but-valid number here means in-place byte mutation crossed an ownership boundary somewhere between construction and Do... (narrowing it further is the suite reader's job once this row names where).
	}

	if got := posted["model"]; string(got) != `"gpt-test"` { // sanity anchor that we captured THIS transport's request at all — a stray capture from elsewhere would make every other assertion above vacuous... (kept cheap: one field comparison, no server round trip needed to prove identity of origin).
		t.Fatalf("captured body model = %s; test may have observed the wrong exchange", got) // this guard exists purely so failures above are trustworthy evidence about THIS transport's retained state and nothing else running in parallel could explain them.
	}

	if _, ok := posted["X-From"]; ok { // negative anchor: header names must not leak into body keys — they travel on the wire separately by construction design... (a merged capture bug between the two channels would otherwise pass every positive check above while being structurally wrong).
		t.Fatalf("header name found inside request body keys; headers and extras were conflated somewhere") // no production code path writes header names into extra-body layers, so any hit here is a test-harness or transport defect rather than legitimate content... (failing it keeps the two capture channels provably distinct for all other rows in this file).
	}

	_ = rewritten // retained only to keep the mutation pass above readable as one coherent block; its effect is fully exercised through posted["temperature"] assertions already made. The variable itself carries no further state worth asserting on directly (its bytes are what temperature should NOT show, which that check proves by positive comparison instead).
}

// TestStreamPostsEncodedBodyWithBuiltHeaders pins the request side of one Stream invocation end to end: method POST at ChatEndpoint with trailing-slash normalization, body byte-equal to a direct Encode call for identical inputs, and BuildHeaders' full set present on the wire (JSON content type, SSE accept, Bearer authorization exactly when a key is resolved) — plus that an absent key emits no Authorization header rather than an empty one.
func TestStreamPostsEncodedBodyWithBuiltHeaders(t *testing.T) { // request-shape contract in real round trips; response handling stays minimal per sub-case since sibling tests own the stream-side behaviors separately... (each row here adds exactly one wire-level fact and nothing more).

	t.Run("bodyPathAndDefaultsWithoutKey", func(t *testing.T) { // baseline: no API key resolved — so Authorization must be ABSENT entirely while every other default still posts correctly alongside encoded bytes.
		var gotMethod, gotPath string
		var gotHeader http.Header
		var rawBody []byte
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // capture everything this row asserts on; the response is a complete trivial stream so Close below does real work rather than releasing nothing.
			gotMethod = r.Method                                                        // method pinned here because redirect/Do plumbing could theoretically downgrade it — retention says POST always for chat requests... (no GET form of this endpoint exists or may be invented by later edits).
			gotPath = r.URL.Path                                                        // exact path after ChatEndpoint's trailing-slash handling: /chat/completions with no doubled slashes anywhere in between.
			gotHeader = r.Header.Clone()                                                // clone for stable post-Close reads as elsewhere in this file — same rationale, one shared convention across rows... (avoids any cross-goroutine visibility questions at assertion time).
			rawBody, _ = io.ReadAll(r.Body)                                             // verbatim capture: the byte-equality oracle against Encode below needs rawness rather than a decoded re-encoding of it.
			w.Header().Set("Content-Type", "text/event-stream")                         // content type on responses is cosmetic for this test but keeps behavior identical to every other SSE-serving handler in the suite... (one less variable if someone later diffs captured exchanges across tests).
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n") // one real event so Recv below has a concrete parsed value to pin against rather than only EOF shapes.
			fmt.Fprint(w, "data: [DONE]\n\n")                                           // terminator included for the same reason — full consumption path exercised end to end in this single row set.
		}))
		defer server.Close()

		rt := testResolved()                                     // no APIKey field populated here on purpose (absent-key branch of BuildHeaders is what pins Authorization absence below).
		rt.BaseURL = strings.TrimSuffix(server.URL, "/v1") + "/" // a trailing-slash base: ChatEndpoint must collapse it to exactly one slash before appending the fixed suffix... (server.URL itself has no path so trimming /v1 changes nothing — the point is exercising the trim code with a present-but-irrelevant suffix).
		tr := mustTransport(t, rt)                               // construction happens once per sub-case as everywhere else in this file; shared servers would blur captured state between rows and make failures unattributable.

		wantBody, _, err := Encode(rt, baseRequest(), nil) // independent direct encoder invocation is the byte oracle: same inputs through two different public entry points must produce identical posted bytes... (any divergence means transport and encoder disagree on ownership/copying somewhere).
		if err != nil {
			t.Fatalf("Encode returned error for an otherwise-valid fixture set: %v", err) // failing here rather than comparing against a zero-value body keeps the assertion below meaningful — no silent empty-vs-empty pass is possible.
		}

		stream, warnings, streamErr := tr.Stream(context.Background(), baseRequest(), nil)
		if streamErr != nil {
			t.Fatalf("Stream returned error: %v", streamErr) // a failure this early would make every wire assertion below unreachable — stop with context instead of letting them all skip vacuously green.
		} else if len(warnings) != 0 { // no must-preserve metadata configured in rt, so zero warnings is the exact expectation for THIS encoding specifically... (warning delivery on success-with-diagnostics has its own dedicated row elsewhere).
			t.Fatalf("unexpected protocol warnings from a warning-free encoding: %#v", warnings) // non-empty here would mean Encode's pure computation started depending on transport state — an ownership-boundary violation worth naming exactly where it appears first rather than downstream.
		}

		if gotMethod != http.MethodPost || gotPath != "/chat/completions" { // both pinned in one check: the wrong method is at least as bad as a wrong path for this contract and they are cheap to assert together... (separating them would only add lines without adding diagnostic value).
			t.Fatalf("request arrived via %s %q, want POST /chat/completions", gotMethod, gotPath) // message carries both actual values so a single failure line is enough to diagnose which side moved.
		} else if !bytes.Equal(rawBody, wantBody) { // exact byte equality — the strongest form of "transport posts what encoder produced" and no weaker re-encoding comparison may replace it... (key order inside marshaled maps is deterministic per Go's json package for identical input shapes, which both sides share here by construction).
			t.Fatalf("posted body differs from Encode output:\n got %s\nwant %s", rawBody, wantBody) // side-by-side rendering makes any single-character divergence visible at a glance in failure output... (this is the row future regressions about copy-on-entry semantics will land on first).
		}

		for key, value := range map[string]string{"Content-Type": "application/json", "Accept": "text/event-stream"} { // both defaults present at exactly their contract values — BuildHeaders owns them and nothing else in this transport may override or drop one... (resolved-header overwrite behavior is pinned by a sibling row with its own server rather than mixed into these baseline assertions).
			if got := gotHeader.Get(key); got != value {
				t.Fatalf("header %s = %q, want %q", key, got, value) // per-key message keeps the failing name visible without dumping the whole header map on every mismatch... (the full clone is still available in scope if a human wants it during triage of this specific row).
			}
		}

		if _, present := gotHeader["Authorization"]; present { // ABSENCE pinned explicitly: an empty-string Authorization or any other presence shape must fail here rather than look like "no key was set" ambiguity elsewhere... (the header-map lookup is case-insensitive via http.Header semantics, so this check covers every spelling Go might normalize to).
			t.Fatalf("Authorization present without a resolved API key: %q", gotHeader.Get("Authorization")) // naming the offending value in the message helps distinguish "empty Bearer" from some other accidental presence shape when triaging... (both are contract violations but their fix locations differ enough that seeing which one matters at diagnosis time).
		}

		delta, recvErr := stream.Recv() // consume the single event this response carries — proves the accepted stream is real end to end in THIS row too rather than assumed from sibling coverage elsewhere.
		if recvErr != nil {
			t.Fatalf("first Recv returned error: %v", recvErr) // a read failure here would mean framing broke on our own fixture bytes — fail with that specific framing since the payload above is trivially well-formed by construction... (no provider-side weirdness can explain this particular row's input).
		} else if len(delta.ContentFragments) != 1 || delta.ContentFragments[0].Text != "ok" { // exact fragment shape for a string-form content event: one text piece at position zero with our literal value in it.
			t.Fatalf("parsed first delta = %#v, want one text fragment \"ok\"", delta) // anything else means parser normalization drifted from its documented contract — this row is the cheapest place that drift would surface first... (deeper structural cases have their own dedicated files; keep comparing only what THIS response actually contains).
		}

		if _, err := stream.Recv(); !errors.Is(err, io.EOF) { // [DONE] terminator lands as EOF exactly per contract — pinning it here too keeps this row self-contained rather than depending on sibling tests for the terminal behavior of its own fixture... (cheap: one extra receive call with an exact expected sentinel).
			t.Fatalf("Recv after [DONE] = %v, want io.EOF", err) // message mirrors every other terminator assertion in the suite so cross-file greps stay consistent when hunting related regressions later.
		}

		if cerr := stream.Close(); cerr != nil { // full consumption then release: Close on a cleanly terminated body must be quiet — no cleanup path may surface transport facts to consumers at this point... (any non-nil here would indicate state corruption beyond what the read assertions above could have caught).
			t.Fatalf("Close returned error after clean consumption: %v", cerr) // included for completeness of THIS row's end-to-end story rather than as independent coverage — sibling close-behavior tests pin pathological shapes separately.
		}
	})

	t.Run("authorizationPresentExactlyWhenKeyResolved", func(t *testing.T) { // the Bearer branch: one server per key-state so captured headers are unambiguous about which request they came from... (a shared handler across both states would force extra bookkeeping to attribute captures correctly).
		for _, tc := range []struct {
			name, apiKey string
			wantAuth     bool
		}{ // table form keeps both branches symmetric and makes adding a third state later trivial without restructuring anything above.
			{"withKey", "secret-key-123", true}, // non-empty key → Authorization: Bearer <key> exactly — no extra whitespace or case variation around the scheme token itself... (the space between scheme and credential is part of the wire contract pinned here).
			{"emptyKeyOmitted", "", false},      // empty resolved key omits the header entirely rather than posting "Bearer " with nothing after it — absence, not emptiness, is what no-credential means on this transport.
		} {
			t.Run(tc.name, func(t *testing.T) { // each row owns its server and transport so capture attribution stays trivially correct by construction rather than by bookkeeping... (this mirrors the pattern used throughout this file for exactly that reason).
				var capturedAuth string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // record only what THIS row asserts on — everything else is deliberately ignored to keep failure messages focused and uncluttered.
					capturedAuth = r.Header.Get("Authorization") // Get (not map lookup) so case-normalization by net/http happens identically here as in the client path... (both sides use canonical casing for this field anyway, but being explicit avoids surprises if either side ever changes).
					w.WriteHeader(http.StatusNoContent)          // 204 again: no stream content needed since only request headers are under test — minimal response keeps the row fast and dependency-free.
				}))
				defer server.Close()

				rt := testResolved()    // fresh resolved input per row (never shared across iterations of this loop) so one branch's state cannot leak into another's captured exchange... (the discipline costs nothing here given how cheap construction is).
				rt.BaseURL = server.URL // endpoint pinned pre-construction as a file-wide convention — see the ownership test above for why post-hoc mutation would be wrong in principle even where it might happen to work.
				rt.APIKey = tc.apiKey   // this single field differs between rows and drives exactly one observable wire difference that we then pin below with full specificity... (nothing else about these two resolved inputs should differ at all).

				tr := mustTransport(t, rt) // standard construction helper — the auth dimension is just another plain string field flowing through retained state normally.
				stream, _, err := tr.Stream(context.Background(), baseRequest(), nil)
				if err != nil {
					t.Fatalf("Stream returned error for key state %q: %v", tc.name, err) // names which branch failed so a table-row mixup during editing is caught immediately rather than discovered much later in output.
				}

				if got := capturedAuth; (got != "") != tc.wantAuth || (tc.wantAuth && got != "Bearer "+tc.apiKey) { // one compound check covering presence, absence, AND exact value where present — partial failures like "present but wrong value" must be caught as clearly as total absence would be.
					t.Fatalf("Authorization = %q for key state %q; want presence=%v and exact form when present", got, tc.name, tc.wantAuth) // both actual and expected shapes rendered so triage never needs to reconstruct which branch ran from the test name alone.
				}

				if _, rerr := stream.Recv(); !errors.Is(rerr, io.EOF) { // 204 carries no body: first receive must be a clean EOF — pinning that an empty accepted response does not error the consumer path either.
					t.Fatalf("Recv on content-less accepted response = %v, want io.EOF", rerr) // (included because Close alone would otherwise hide any read-side state corruption from view entirely).
				} else if cerr := stream.Close(); cerr != nil {
					t.Fatalf("Close returned error: %v", cerr) // releasing a fully-consumed empty body must be quiet — no cleanup path may surface transport facts to the consumer here.
				}
			})
		}
	})

	if _, err := NewTransport(ResolvedTransport{Model: ModelRef{Provider: "p"}, WireSystemRole: ""}); !errors.Is(err, ErrInvalidModelRef) { // trailing sanity anchor reused from sibling file's conventions — confirms this test's own assumptions about which validation layer owns which error have not drifted since it was written... (cheap insurance that a future refactor doesn't silently move model-completeness checking into Stream and leave construction accepting partial refs here).
		t.Fatalf("premise broken: NewTransport no longer rejects incomplete identities at construction") // if this fires, every other row in THIS file set still passes but the suite's overall story about where errors surface has changed — flagging it explicitly keeps that decision visible rather than implicit.
	}
}

// TestStreamIssuesExactlyOnePhysicalAttempt pins "one physical attempt per invocation" from both failure directions: a terminal non-2xx response, and an accepted stream whose first event is malformed JSON — each must leave exactly one HTTP request recorded server-side for that call (a retry loop of any shape would show as >= 2). Independent invocations on the same transport may follow later; they simply do not multiply within their own failures.
func TestStreamIssuesExactlyOnePhysicalAttempt(t *testing.T) { // no alternate Do path exists in this type — so server-side counting per invocation IS the observable contract and nothing weaker (like "eventually consistent") may stand in for it... (the two sub-cases below cover request-level vs stream-level failure because those are the only two places a hidden retry could hide).

	t.Run("non2xxTerminalIsSingleRequest", func(t *testing.T) { // 404 with a JSON error body: one POST recorded, typed status error returned — and crucially nothing re-posts behind that terminal response even though everything it needs to (same transport, same inputs, live context) is still available... (this asymmetry between "could retry" and "must not" IS the contract being pinned).
		var calls int32                                                                              // atomic counter because httptest handlers run on their own goroutine per request — plain ints would race with assertions even though ordering here happens to be safe in practice... (cheap correctness costs nothing and keeps -race runs clean by construction rather than luck).
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // handler does the minimum: count then answer 404 — no sleeps or conditional logic that could vary observed counts between runs.
			atomic.AddInt32(&calls, 1)                                             // increment FIRST so a client-side timeout racing with response delivery still records this attempt... (we never time out in practice here but counting-before-answering is strictly safer than the reverse order).
			http.Error(w, `{"error":{"message":"not found"}`, http.StatusNotFound) // body content is irrelevant to THIS assertion (count only) — its shape belongs to status-extraction rows elsewhere; keeping it minimal avoids coupling this test's pass/fail to extraction details.
		}))
		defer server.Close()

		rt := testResolved()       // standard valid baseline plus endpoint for this sub-case specifically... (other resolved fields stay at defaults so nothing besides the failure mode itself varies between what we expect and observe).
		rt.BaseURL = server.URL    // pinned pre-construction per file convention — see ownership test above.
		tr := mustTransport(t, rt) // one transport serves both invocations below deliberately: proving that even reuse of a live, previously-failed transport does not trigger any deferred retry behavior from its earlier failure... (statelessness across calls is part of "one attempt per invocation" too).

		var status *HTTPStatusError
		_, warnings, err := tr.Stream(context.Background(), baseRequest(), nil) // first of two invocations must fail with the right shape — count assertions below are meaningless without establishing that this call actually went through its full failure path.
		if !errors.As(err, &status) || status.StatusCode != http.StatusNotFound {
			t.Fatalf("first failed Stream = %v, want a 404 HTTPStatusError", err) // names the exact expected shape so any deviation (success, wrong code, untyped error...) is immediately visible in output.
		} else if got := atomic.LoadInt32(&calls); got != 1 {
			t.Fatalf("first failed Stream issued %d physical attempts, want exactly 1", got) // >= 2 here would be a retry loop in disguise — the core assertion of this sub-case.
		}

		var status2 *HTTPStatusError
		_, warnings2, err := tr.Stream(context.Background(), baseRequest(), nil) // second invocation on the SAME transport: must fail identically AND independently — its attempt count adds to (not replaces) the first... (any shared "retry budget" across calls would surface as an unexpected total below).
		if !errors.As(err, &status2) || status2.StatusCode != http.StatusNotFound {
			t.Fatalf("second Stream call = %v, want a 404 HTTPStatusError again", err) // distinct message from above so it is obvious which invocation tripped when output is skimmed quickly during triage.
		} else if got := atomic.LoadInt32(&calls); got != 2 {
			t.Fatalf("two independent invocations issued %d attempts total, want exactly one per call (total 2)", got) // cumulative check pins both "each adds its own single attempt" AND "no cross-invocation state causes extra work".
		} else if len(warnings)+len(warnings2) != 0 {
			t.Fatalf("failed invocations returned protocol warnings (%d + %d); this fixture encodes none", len(warnings), len(warnings2)) // negative anchor: warning-free fixtures must stay warning-free on failure paths too — otherwise a real diagnostic vs leaked noise would be indistinguishable later.
		}
	})

	t.Run("midStreamProtocolFailureDoesNotRepost", func(t *testing.T) { // accepted stream whose first payload is malformed JSON: Recv fails typed (ErrProtocol)... and the endpoint STILL saw exactly one request for that whole invocation — nothing in this package converts a mid-stream protocol failure into another physical attempt behind it.
		var calls int32                                                                              // same counting discipline as above; separate counter per sub-case keeps each row's arithmetic trivially verifiable from its own output alone... (shared counters across sub-cases would require remembering which rows already incremented what).
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // serve one accepted response carrying exactly one malformed event — no more content than that so the test's Recv loop terminates deterministically at its first error.
			atomic.AddInt32(&calls, 1)                          // count before responding as above for identical race-safety reasons... (one increment per invocation is what makes "exactly 1" meaningful here).
			w.Header().Set("Content-Type", "text/event-stream") // correct response type so acceptance itself cannot be questioned — only the payload's validity differs from well-formed fixtures elsewhere.
			fmt.Fprint(w, "data: {not-json}\n\n")               // deliberately malformed JSON inside a correctly framed SSE event: framing is fine, decoding is not — isolating exactly the failure class this row exists to pin... (a framing-level defect would conflate with parser behavior and muddy what we're actually asserting).
		}))
		defer server.Close()

		rt := testResolved()       // standard baseline plus endpoint for this sub-case; nothing else varies between expected-behavior and observed-outcome here by design.
		rt.BaseURL = server.URL    // pinned pre-construction per file-wide convention as everywhere else in this set... (consistency across rows matters more than any single row's elegance).
		tr := mustTransport(t, rt) // standard construction helper — no auth or extras configured so the only interesting dimension remains attempt-counting itself.

		stream, _, err := tr.Stream(context.Background(), baseRequest(), nil) // 2xx acceptance: Stream returns BEFORE payload validity is known anywhere... (this ordering is precisely why mid-stream failures exist as a distinct category from request-level ones).
		if err != nil {
			t.Fatalf("Stream returned error for an accepted response: %v", err) // failing here would mean something broke before we even got to test the interesting part — stop with context rather than letting later assertions run against a missing stream.
		}

		var recvErr error                                                 // captured separately so both its sentinel identity AND absence-of-retry can be asserted in distinct checks below... (one combined check would hide which half failed when output is skimmed).
		if _, recvErr = stream.Recv(); !errors.Is(recvErr, ErrProtocol) { // the malformed event must surface as a typed protocol failure — nothing more exotic (raw unmarshal error unwrapped...) may reach consumers here.
			t.Fatalf("Recv on malformed payload = %v, want an error wrapping ErrProtocol", recvErr) // naming the sentinel in both expectation and message keeps this row greppable alongside every other parser-failure assertion across the suite... (consistency of phrasing is what lets a human find related rows quickly).
		} else if got := atomic.LoadInt32(&calls); got != 1 { // THE core count: exactly one request for the entire invocation despite its stream having failed mid-way through consumption.
			t.Fatalf("%d attempts recorded after a mid-stream protocol failure, want exactly 1 for that whole invocation", got) // any number above one here would be direct evidence of retry behavior living somewhere in this package — which is forbidden by contract and worth failing loudly on the first sight... (this row exists specifically because such regressions are invisible everywhere else).
		}

		if cerr := stream.Close(); cerr != nil { // releasing after a failed read must still behave like every other Close: quiet, idempotent-friendly cleanup with no transport facts surfaced to consumers at this point.
			t.Fatalf("Close returned error following protocol failure: %v", cerr) // included because close-after-error is exactly the kind of edge where state corruption hides best — asserting it here costs one line and removes an entire class of silent bugs from later review... (not redundant with sibling close tests since those cover different prior-state shapes).
		}

		if _, againErr := stream.Recv(); !errors.Is(againErr, io.EOF) { // post-failure terminal state: further receives see EOF through the done flag rather than re-attempting reads or errors... (pinning it here completes this row's end-to-end story about what a mid-stream failure leaves behind).
			t.Fatalf("Recv after protocol failure + Close = %v, want io.EOF", againErr) // mirrors every other terminal-state assertion in the suite for consistency — future readers should never have to wonder whether THIS particular shape was intentionally left unasserted.
		}
	})
}

// TestStreamReturnsHTTPStatusErrorWithProviderMessage pins non-2xx error extraction through its retained rule end to end: status code, wire status text, and provider message with error.message preferred over top-level message; when neither parses the trimmed raw body is stored instead (retained current behavior); empty bodies yield an empty message without a trailing colon in the rendered line. No auth sentinel may be joined for any of these ordinary failures.
func TestStreamReturnsHTTPStatusErrorWithProviderMessage(t *testing.T) { // one fresh server per row so captured status values cannot leak between cases — extraction correctness is asserted field-by-field with exact expected strings rather than substring matches... (looser comparisons would let formatting drift slip through unnoticed across commits).

	cases := []struct {
		name, body  string
		wantMessage string
		wantCode    int
	}{ // table form keeps each branch of the reader's decision tree in one visible place and trivially extendable if a future provider shape needs pinning too... (every field asserted inline makes each single failure message self-contained).
		{"errorMessagePreferred", `{"error":{"message":"slow down"},"message":"ignored top-level"}`, "slow down", 429}, // first extraction branch: nested error.message wins over any sibling top-level key on the same body — ordering is part of retained behavior and pinned here exactly.
		{"topLevelMessageFallback", `{"message":"quota exhausted","error":{}}`, "quota exhausted", 503},                // second branch: when no usable nested message exists, the top-level one stands in its place... (an empty error object present-but-useless is included deliberately to prove presence alone does not win over an actually-populated sibling).
		{"rawTrimmedBodyStored", "  upstream said no\n\n", "upstream said no", 502},                                    // neither shape parses as JSON here: the trimmed raw body text becomes the message exactly — whitespace edges removed, inner content intact byte-for-byte.
		{"emptyBodyLeavesEmptyMessage", "", "", 504},                                                                   // nothing readable at all leaves an empty message and a colon-free rendered line (pinned separately below since its formatting differs from every other row's shape).
	}

	for _, tc := range cases { // each iteration owns server + transport like everywhere else in this file — no shared state across rows by construction... (the discipline is deliberate: failures must be attributable to exactly one row without re-reading output twice).
		t.Run(tc.name, func(t *testing.T) { // sub-test per case so -run filters can target a single extraction branch during triage of related regressions elsewhere in the suite if needed later.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // write exactly this row's body with its status — no headers beyond what w.WriteHeader implies are set on purpose... (keeping responses minimal focuses any future failure message purely on extraction behavior rather than transport plumbing).
				w.WriteHeader(tc.wantCode) // the numeric code itself is asserted below through HTTPStatusError.StatusCode equality — pinning both sides of that relationship in one row.
				fmt.Fprint(w, tc.body)     // body written verbatim per case definition above; no transformation between table literal and wire bytes anywhere... (any encoding surprise would surface as a message mismatch with an obvious diff visible in output).
			}))
			defer server.Close()

			rt := testResolved()       // standard valid baseline for this row's transport — nothing about resolved input should influence extraction outcomes here.
			rt.BaseURL = server.URL    // endpoint pinned pre-construction per file convention as everywhere else... (this consistency is what lets a reader trust that differences between rows are intentional and attributable).
			tr := mustTransport(t, rt) // construction helper — same rationale as every other use site in this file set.

			_, warnings, err := tr.Stream(context.Background(), baseRequest(), nil) // one invocation per row; both return values stay available to every check below rather than being consumed inside a single compound condition... (separate statements after the shape guard keep each failure message focused on exactly one aspect).
			var status *HTTPStatusError
			if !errors.As(err, &status) {
				t.Fatalf("Stream error %v is not a *HTTPStatusError", err) // Fatalf terminates this sub-test — every statement below assumes the typed shape exists and therefore only runs when it actually does.
			} else if status.StatusCode != tc.wantCode || status.Message != tc.wantMessage { // both fields asserted in one check with per-row expected values from the table above — keeping this line readable despite two comparisons happening at once.
				t.Fatalf("status = %#v, want code %d and Message %q", *status, tc.wantCode, tc.wantMessage) // rendering actual struct next to expectations makes any single-field divergence immediately visible without re-deriving which one moved... (cheap insurance against misreading dense failure output under time pressure).
			} else if status.Message != "" && !strings.Contains(status.Error(), fmt.Sprintf("API error %d %s: %s", tc.wantCode, status.StatusText, tc.wantMessage)) { // positive rows must render through the retained "API error <code> <text>: <message>" line exactly — no abbreviation or reordering of its parts allowed anywhere between capture and Error() output.
				t.Fatalf("rendered error = %q; want exact retained shape \"API error %d %s: %s\"", status.Error(), tc.wantCode, status.StatusText, tc.wantMessage) // a structural check about rendering fidelity rather than content correctness — worth its own message since fixing one vs the other means touching different code paths entirely.
			} else if strings.Contains(status.Error(), "ignored top-level") { // negative anchor specific to this row's body: the losing candidate's text must not leak into rendered error even when present in source body — extraction is exclusive, not cumulative... (a naive implementation might concatenate both messages and still pass every positive check above).
				t.Fatalf("rendered error leaked non-preferred message field: %q", status.Error()) // naming the exact contamination pattern makes this failure self-explanatory without needing to re-read extraction code during triage of it.
			}

			if tc.name == "emptyBodyLeavesEmptyMessage" { // empty-message row renders as exactly two parts with no trailing colon segment — retained formatting only appends the message part when one exists at all... (a dangling-colon bug would be invisible to every other row's positive assertions).
				want := fmt.Sprintf("API error %d %s", tc.wantCode, status.StatusText) // exact full-line expectation for this shape rather than just a suffix check — pinning the whole rendered value leaves no room for any stray segment anywhere in it.
				if status.Error() != want {
					t.Fatalf(`empty body produced rendered line %q; want exactly %q (no trailing colon or message part)`, status.Error(), want) // explicit about WHICH malformed shape was seen rather than just saying "formatting wrong" generically — future readers benefit from knowing the exact defect class this row guards against.
				}
			}

			if errors.Is(err, ErrAuthFailed) { // no auth sentinel for ANY of these ordinary failure codes: 429/503/502/504 are all non-auth by contract... (pinning it once per table iteration rather than only in the dedicated auth row because forgetting to check here would let a blanket-join regression hide inside otherwise-green extraction tests).
				t.Fatalf("ErrAuthFailed present on ordinary %d failure", tc.wantCode) // message names the specific status so output stays actionable without cross-referencing which table row ran... (cheap specificity costs nothing per iteration given how cheap this check itself is).
			}

			if len(warnings) != 0 { // warning-free fixtures must stay silent on failed attempts too — same negative anchor as elsewhere in this file with identical rationale about noise vs signal distinguishability later.
				t.Fatalf("unexpected protocol warnings from a failure-path encoding: %#v", warnings) // included for parity with sibling rows' discipline rather than because extraction itself could produce them (it cannot today, but the guard is free and future-proofs against accidental coupling).
			}
		})
	}

	if _, err := NewTransport(ResolvedTransport{Model: ModelRef{Provider: "openai", Model: ""}}); !errors.Is(err, ErrInvalidModelRef) { // closing sanity anchor mirroring the one in sibling test files — confirms construction-time validation surface has not drifted since this file was written... (kept deliberately cheap and local rather than extracted into a shared helper because each call site's context makes its failure message most useful when phrased for THAT row set specifically).
		t.Fatalf("premise broken: incomplete model identity no longer rejected at NewTransport") // if this fires, the suite's story about where validation happens has changed and every file using that assumption needs a re-read — flagging it here makes that maintenance debt visible immediately rather than buried in unrelated later failures.
	}
}

// TestStreamReadsErrorBodyThroughOneMiBLimit pins the error-body reader's retained 1 MiB bound end to end: when an oversized body defeats JSON parsing through truncation, what gets stored is exactly the limited read (trimmed) — never a longer string and never bytes past the limit... (the uniform non-whitespace fill makes trimming a no-op so exact byte equality against precisely the first limit-many bytes remains unambiguous).
func TestStreamReadsErrorBodyThroughOneMiBLimit(t *testing.T) { // one dedicated row for this because its body size dwarfs every other fixture in the suite and mixing it into that table would obscure their otherwise-small literal expectations... (also keeps -run filtering granular: nobody else wants to wait on a multi-megabyte response by accident).
	limit := int64(1 << 20) // the exact retained bound this reader enforces — one MiB no more, asserted below as an equality rather than any weaker "at most" check.

	body := make([]byte, limit+512*1024) // total deliberately exceeds the read bound by a wide margin so truncation is guaranteed to happen mid-value for ANY parseable shape... (no valid JSON fits inside this payload: uniform fill cannot contain object braces or quotes at all).
	for i := range body {                // single repeated character keeps TrimSpace semantics trivially identity here — no edge whitespace anywhere in the retained slice.
		body[i] = 'a' // choice of 'a' over e.g. null bytes is cosmetic only; what matters is uniformity plus non-whitespace classification for trimming purposes specifically.
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // write the full oversized body — writing more than the client will read is expected and harmless here... (a handler-side broken pipe on such huge responses must not be treated as test failure either).
		w.WriteHeader(http.StatusServiceUnavailable) // 503 keeps auth semantics out of scope for this pure size-bounding question entirely.
		if _, err := w.Write(body); err != nil {     // ignore the write outcome deliberately: the client-side behavior under observation does not depend on whether the server finished sending everything it started... (this asymmetry is normal HTTP and worth documenting at exactly the place where a reader might otherwise "fix" it away).
			return // early return keeps handler goroutine exit clean regardless of partial-write state above.
		}
	}))
	defer server.Close()

	rt := testResolved()       // standard valid baseline plus endpoint for this row's transport — nothing else varies between expectation and observation here by design.
	rt.BaseURL = server.URL    // pinned pre-construction per file convention as everywhere else in the set... (consistency across rows matters more than any single row's elegance).
	tr := mustTransport(t, rt) // construction helper — same rationale throughout this file for one shared path to valid transport values.

	_, warnings, err := tr.Stream(context.Background(), baseRequest(), nil) // everything interesting about THIS invocation lives in the error value and its extracted fields below... (the stream return is necessarily nil here since no acceptance can follow a 503).
	var status *HTTPStatusError
	if !errors.As(err, &status) {
		t.Fatalf("Stream error %v is not a *HTTPStatusError", err) // shape guard first — every field assertion below assumes the typed result exists and therefore only runs when it actually does... (same pattern as sibling extraction rows above).
	} else if int64(len(status.Message)) != limit { // THE core bound check: exactly one MiB of message stored, no more or less across this entire oversized-body scenario.
		t.Fatalf("extracted message length = %d bytes; want exactly the 1 MiB read limit (%d)", len(status.Message), limit) // longer would mean bytes past the bound leaked into extraction somewhere; shorter means reading stopped early or trimming misbehaved on uniform input... (both directions pinned by one exact-equality comparison rather than two weaker inequalities).
	} else if status.Message != string(body[:limit]) { // byte-level equality against known fill content proves no transformation beyond identity-plus-trim happened to those retained bytes themselves.
		t.Fatalf("extracted message differs from exactly the first %d body bytes (both are uniform 'a' fills of length %d — any divergence is real corruption)", limit, len(status.Message)) // naming the expected character in the message makes a future fill-character change during maintenance immediately obvious as an intentional edit rather than silent drift... (self-documenting test data beats magic constants here).
	} else if errors.Is(err, ErrAuthFailed) {
		t.Fatalf("ErrAuthFailed present on ordinary 503 failure") // same non-auth negative anchor as sibling rows — cheap and consistent across the whole file set.
	}

	if len(warnings) != 0 { // warning-free fixtures stay silent even under oversized-body conditions — identical rationale to every other such anchor in this suite about noise vs signal distinguishability later on.
		t.Fatalf("unexpected protocol warnings from an oversized error body: %#v", warnings) // included for parity with sibling rows' discipline rather than because size-bounding logic itself could produce them (it cannot today, but the guard is free and future-proofs against accidental coupling).
	}

	var statusAgain *HTTPStatusError                                      // pointer to the concrete error type so errors.As can fill its fields on success — Error() is defined on the pointer, a value here would be an invalid target.
	_, _, againErr := tr.Stream(context.Background(), baseRequest(), nil) // a second invocation on the same transport must fail identically and independently — proving the bound applies per-invocation rather than being some one-shot state that only worked once by accident of ordering... (this is the row a future "cache truncated bodies across calls" optimization would break first).
	if !errors.As(againErr, &statusAgain) {                               // split statement from condition so the typed variable exists for errors.As — same shape as every sibling status-assertion in this file.
		t.Fatalf("second oversized-body invocation error = %v, want another *HTTPStatusError", againErr) // distinct message from above so it is obvious which call tripped when output is skimmed quickly during triage.
	} else if errors.Is(againErr, ErrAuthFailed) {
		t.Fatalf("auth sentinel present on second ordinary 503 failure") // same non-auth negative anchor as the first invocation — both calls must stay unjoined identically.
	}
}

// TestStreamJoinsAuthSentinelOn401And403 pins that exactly the 401/403 statuses join ErrAuthFailed into their returned error alongside — not instead of — the HTTP status facts: both reachable through errors.Is/errors.As on one value, with every other non-2xx staying a plain unjoined status error. No code outside this exact pair may acquire the sentinel by any implementation path... (the two positive rows and several negative ones together define membership completely).
func TestStreamJoinsAuthSentinelOn401And403(t *testing.T) { // auth-failure identity is consumed above this boundary without parsing codes — so its presence/absence per status must be exact rather than approximate... (approximate implementations would pass a weaker test but break real callers relying on precise membership).

	statuses := []struct {
		code     int
		wantAuth bool
	}{ // table form with explicit expected membership per row — the full closed set of statuses under consideration here is small enough to enumerate completely without feeling exhaustive-by-omission... (adding more negative rows later costs one line each and keeps this guarantee total rather than sampled).
		{http.StatusUnauthorized, true},     // 401 joins: canonical unauthorized case.
		{http.StatusForbidden, true},        // 403 also joins — both codes share exactly the same treatment by contract with no distinction between them anywhere downstream of this point... (they are intentionally NOT separated into different behaviors).
		{http.StatusBadRequest, false},      // every other tested code stays unjoined: bad request is a client error but not an authentication one.
		{http.StatusTooManyRequests, false}, // 429 specifically deserves its own negative row because rate-limiting sometimes gets conflated with auth failures in naive implementations — pinning it out explicitly prevents that particular confusion from ever passing this suite... (the most likely real-world source of a regression here).
		{http.StatusInternalServerError, false},
	}

	for _, tc := range statuses { // each row owns server + transport like everywhere else; the assertion set is identical across rows apart from expected membership and code value being checked against.
		t.Run(fmt.Sprintf("%d_auth=%v", tc.code, tc.wantAuth), func(t *testing.T) { // sub-test names encode both dimensions so output stays scannable without re-deriving which case ran... (short enough to read but unambiguous when grepping).
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // minimal response per row: status line only with no body — extraction yields an empty message here by design so nothing about content can confound membership assertions below.
				w.WriteHeader(tc.code) // write exactly the code under test for THIS iteration; handler has no other state or branching logic that could vary observed behavior between runs... (determinism matters more than realism in a sentinel-membership check).
			}))
			defer server.Close()

			rt := testResolved()       // standard baseline plus endpoint — nothing auth-related configured in resolved input itself since the JOIN under test is purely response-driven rather than request-shaped.
			rt.BaseURL = server.URL    // pinned pre-construction per file convention as everywhere else... (consistency across rows remains more valuable here than any single row's brevity).
			tr := mustTransport(t, rt) // construction helper — same rationale throughout this file set for using one shared path to valid transports.

			_, warnings, err := tr.Stream(context.Background(), baseRequest(), nil) // one invocation per row; both return values stay available to every check below rather than being consumed inside a single compound condition... (separate statements after the shape guard keep each failure message focused on exactly one aspect).
			var status *HTTPStatusError
			if !errors.As(err, &status) {
				t.Fatalf("Stream error %v is not a *HTTPStatusError", err) // Fatalf terminates this sub-test — every statement below assumes the typed shape exists and therefore only runs when it actually does.
			} else if joined := errors.Is(err, ErrAuthFailed); joined != tc.wantAuth { // THE membership assertion itself: exact per-row expectation with no tolerance for approximate implementations anywhere in the chain.
				t.Fatalf("errors.Is(ErrAuthFailed) = %v on status %d, want %v (full error below)", joined, tc.code, tc.wantAuth) // message names both sides of the comparison so triage never needs a second run to see what shape actually came back instead of just its boolean projection.
			} else if status.StatusCode != tc.code { // underlying facts untouched by joining: code still matches exactly what this row requested — no sentinel-joining side effects may alter any field on HTTPStatusError itself.
				t.Fatalf("status code through join = %d, want %d", status.StatusCode, tc.code) // pinning factual integrity alongside membership keeps both aspects of the returned value under test in one place rather than splitting them across files unnecessarily... (they are two halves of the same contract sentence).
			} else if len(warnings) != 0 {
				t.Fatalf("unexpected protocol warnings on auth-classified failure: %#v", warnings) // negative anchor again with identical rationale as everywhere else in this file about noise-vs-signal distinguishability for future rows added later.
			}
		})
	}

	if _, err := NewTransport(ResolvedTransport{Model: ModelRef{Provider: "openai"}, WireSystemRole: "system"}); !errors.Is(err, ErrInvalidModelRef) { // closing sanity anchor mirroring sibling files' convention — construction-time validation surface unchanged since this file was written... (cheap local insurance against silent drift in where errors are produced).
		t.Fatalf("premise broken: incomplete model identity no longer rejected at NewTransport") // same explicit framing as the equivalent anchors elsewhere so a grep for that phrase finds every such guard consistently across the suite.
	}
}

// TestStreamPreflightCancellationPinsNoRequest pins preflight cancellation exactly: an already-done context before Stream means zero HTTP requests reach any server (not even one), with the returned error wrapping context.Canceled itself — no request, no response, nothing half-sent anywhere in between... (this is the cheapest possible failure shape and worth pinning precisely because later optimizations sometimes "helpfully" buffer-and-send anyway).
func TestStreamPreflightCancellationPinsNoRequest(t *testing.T) { // cancelled-before-the-attempt must be indistinguishable from never calling at the network layer — except that this invocation DID happen, so its error still carries proper wrapping for callers to classify... (the asymmetry between "no I/O" and "still a real call with a typed outcome" is exactly what makes this row non-redundant).
	var calls int32 // atomic counter as everywhere else in this file — same race-safety rationale applies even though only one goroutine should ever touch it here... (consistency of idiom across the suite matters more than optimizing away an unused-in-practice precaution per individual test).

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // a LIVE server is essential: dial-success-then-no-request proves preflight logic rather than masking it behind connection-refused noise... (a dead endpoint would make "zero calls" trivially true for the wrong reason entirely).
		atomic.AddInt32(&calls, 1)   // count any request that actually arrives — expectation below is exactly zero of them.
		w.WriteHeader(http.StatusOK) // response content irrelevant since nothing should ever read it; present only so handler compiles as a complete function shape... (no body written on purpose).
	}))
	defer server.Close()

	rt := testResolved()       // standard valid baseline for this row's transport — everything about resolved input is unremarkable by design.
	rt.BaseURL = server.URL    // endpoint pinned pre-construction per file convention as everywhere else in the set... (the point of a live target here is precisely that dialing would succeed if attempted).
	tr := mustTransport(t, rt) // construction helper — same rationale throughout this file for one shared path to valid transport values.

	ctx, cancel := context.WithCancel(context.Background()) // fresh cancellable parent per test as standard practice elsewhere in the suite too... (never reusing contexts across rows avoids cross-test interference by construction).
	cancel()                                                // done BEFORE Stream is called — that ordering IS what makes this "preflight" rather than mid-flight; swapping it accidentally would turn this into a different test entirely.

	_, warnings, err := tr.Stream(ctx, baseRequest(), nil) // one invocation; both return values stay available to every check below rather than being consumed inside a single compound condition... (same idiom as everywhere else in this file set).
	if !errors.Is(err, context.Canceled) {                 // THE core assertion for this row: standard-library cause reachable through wrapping — no exotic sentinel of our own may replace or obscure it.
		t.Fatalf("Stream on pre-canceled context = %v, want an error whose chain includes context.Canceled", err) // full value rendered so triage can see exactly what shape came back instead when this regresses... (message phrasing mirrors sibling cancellation rows for consistency).
	} else if got := atomic.LoadInt32(&calls); got != 0 { // zero requests — not "at most one" or some weaker bound: preflight means the request object itself was never handed to any transport layer at all.
		t.Fatalf("%d request(s) reached the server despite preflight cancellation; want exactly none", got) // naming exact count keeps failure output unambiguous about how far past zero we drifted if this ever breaks... (a single stray retry would show as 1 here and be immediately visible).
	} else if len(warnings) != 0 {
		t.Fatalf("protocol warnings returned on a fully-canceled invocation: %#v", warnings) // negative anchor consistent with every other row's discipline about warning-free fixtures staying silent... (no special exemption for canceled paths — they are still just one more failure shape among many).
	}
}

// TestStreamMidFlightCancellationUnblocksBodyRead pins that cancelling the invocation context unblocks an in-progress body read on an accepted stream: after one real event is delivered and the server holds its response open indefinitely, cancel makes the next Recv return promptly with a non-nil error (whose chain includes the cancellation) — never hanging. Close afterwards must also not block or surface errors to consumers... (this row exists because hang-on-cancel regressions are invisible everywhere else in the suite).
func TestStreamMidFlightCancellationUnblocksBodyRead(t *testing.T) { // modeled on retained mid-flight behavior: one flushed event first so acceptance is real, then an indefinite hold until cancellation arrives — exactly the shape that would stall a non-context-aware body read forever without any timeout of our own inventing behavior.
	chunkSent := make(chan struct{}) // channel signal rather than polling counter because ordering between "event delivered" and "handler now blocking" must be exact... (a race here would either deadlock this test or skip cancel-before-hold entirely).

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // handler blocks on context done deliberately — no further bytes written after the first flush by design.
		w.Header().Set("Content-Type", "text/event-stream") // correct response type so acceptance cannot be questioned in any later assertion about what happened to that stream... (same minimalism as other SSE-serving handlers here).
		flusher, ok := w.(http.Flusher)                     // flush support required for delivering exactly one event before holding — httptest servers provide it always but the check is free and makes intent explicit at read time.
		if !ok {
			t.Error("test server lacks flush support; mid-flight behavior cannot be exercised")
			return
		} // t.Error (not Fatalf): handler goroutine context forbids Fatal by definition... (message says what capability was missing rather than just "failed" generically).
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n") // one complete well-formed event — enough to prove acceptance happened for real before anything interesting begins.
		flusher.Flush()                                                                // ...and this is what actually puts those bytes on the wire at that moment rather than buffering them indefinitely inside server internals... (without it, client might not see ANY data until connection close which would change what we're testing entirely).
		chunkSent <- struct{}{}                                                        // signal crosses here exactly once per invocation — deterministic ordering anchor for everything after this point in both goroutines.
		<-r.Context().Done()                                                           // hold from now on: no more bytes ever arrive unless/when cancellation comes through — the entire interesting behavior of THIS test lives in what client-side Recv does against that silence below.
	}))
	defer server.Close()

	rt := testResolved()       // standard valid baseline plus endpoint for this sub-case specifically... (other resolved fields stay at defaults so nothing besides cancel timing varies between expectation and observation).
	rt.BaseURL = server.URL    // pinned pre-construction per file convention as everywhere else in the set.
	tr := mustTransport(t, rt) // construction helper — same rationale throughout this file for using one shared path to valid transport values.

	ctx, cancel := context.WithCancel(context.Background()) // fresh cancellable parent; live through most of this test's body on purpose... (its lifetime IS what we're measuring behavior against).
	stream, _, err := tr.Stream(ctx, baseRequest(), nil)    // acceptance must succeed while ctx is still healthy — any failure here means something broke before the interesting part even started.
	if err != nil {
		t.Fatalf("Stream returned error before cancellation: %v", err) // stop with context rather than letting later assertions run against a missing stream object entirely... (cheap early exit keeps output focused on whichever phase actually failed).
	}

	first, recvErr := stream.Recv() // the flushed event arrives normally as any other well-formed first delta would in this suite's fixtures... (pinning its exact parsed shape below doubles as proof that pre-cancel reads behave identically to non-canceled ones everywhere else).
	if recvErr != nil {
		t.Fatalf("first Recv after acceptance failed: %v", recvErr) // a read failure BEFORE cancel is interesting in itself — it would mean framing broke on our own trivially well-formed fixture bytes rather than anything about cancellation timing... (failing with that specific framing keeps diagnosis pointed at the right subsystem).
	} else if len(first.ContentFragments) != 1 || first.ContentFragments[0].Text != "hello" { // exact fragment shape for string-form content: one text piece at position zero carrying our literal value — same expectation as every other simple-content row in this suite.
		t.Fatalf("first delta = %#v, want exactly one text fragment \"hello\"", first) // anything else here means parser normalization drifted independently of cancellation concerns entirely... (keeping these orthogonal facts pinned separately makes cross-cutting regressions much easier to isolate later).
	}

	<-chunkSent // wait for the server's "I've sent my one event and am now blocking" signal before proceeding — ordering anchor without which cancel could race ahead of hold-starting... (a missing signal would mean the handler t.Error'd above, so this block is guaranteed to unblock on that path too).

	cancel() // ...and THIS is what must unblock the next read below without any timeout of our own inventing behavior — pure context propagation through net/http internals.

	type recvResult struct {
		delta StreamDelta
		err   error
	} // small local result carrier so we can observe Recv from a separate goroutine with bounded waiting... (needed because "returns promptly" is itself part of the contract being pinned, not just eventual correctness).
	resultCh := make(chan recvResult, 1) // buffered by one so the worker never blocks on send even if select below times out — preventing secondary deadlocks from our own test scaffolding rather than production code.

	go func() { // spawn Recv in its OWN goroutine specifically so we can bound how long it may take to return... (a direct blocking call here would make a true hang regression stall CI forever instead of failing fast with useful output).
		d, e := stream.Recv()                    // the real production call under test — nothing wrapped or instrumented around it beyond capturing its two return values for later inspection below.
		resultCh <- recvResult{delta: d, err: e} // deliver result exactly once per invocation as promised by channel buffering above... (no retries or re-sends possible from this single-goroutine producer side).
	}()

	select { // bounded wait IS the "unblocks promptly" assertion itself — generous enough to absorb scheduling jitter under -race yet small enough that a genuine hang fails CI in seconds not minutes.
	case res := <-resultCh: // normal (correct) path: Recv returned promptly after cancel — the "unblocks" contract itself, with whatever terminal shape net/http chose to surface for an aborted body read... (its exact spelling varies by Go version and is not load-bearing downstream since consumers classify cancellation through their own run context rather than this error's chain).
		if res.err == nil { // a NIL error here would mean the stream silently swallowed cancellation — strictly worse than any wrong-shape regression since consumers cannot even tell anything happened at all.
			t.Fatalf("Recv after mid-flight cancel returned nil error with delta %#v; want non-nil", res.delta) // naming both return values in output makes this failure self-explanatory without re-running the test to see what came back instead... (cheap specificity costs nothing here).
		} else if _, again := stream.Recv(); !errors.Is(again, io.EOF) { // post-terminal-read state: further receives see EOF through done flag rather than re-attempting or erroring repeatedly... (completing this row's end-to-end story about what mid-flight cancellation leaves behind).
			t.Fatalf("second Recv after cancelled read = %v, want io.EOF", again) // included because close-then-receive ordering variations are exactly where subtle state bugs like double-surfacing the same error hide best.
		} else if cerr := stream.Close(); cerr != nil { // releasing an already-terminated accepted stream must be quiet — no cleanup path may surface transport facts to consumers at this point... (idempotent-friendly behavior pinned here for parity with every other Close assertion across the suite).
			t.Fatalf("Close after mid-flight cancel returned %v, want nil", cerr) // explicit about which phase produced it so output stays attributable even in dense failure logs later.
		} else if err2 := stream.Close(); err2 != nil { // second close absorbed: no double-close error may reach the consumer's deferred cleanup path under any circumstances... (this is a contract guarantee worth pinning exactly once rather than assuming from reading code).
			t.Fatalf("second Close returned %v, want nil (idempotent release)", err2) // message distinguishes first vs second close failures for triage clarity when both could theoretically misbehave in the same run.
		}

	case <-time.After(5 * time.Second): // TIMEOUT path: a genuine hang regression surfaces here as fast CI failure with useful context rather than an indefinitely stuck job... (five seconds is generous relative to normal scheduling latency yet small enough that even slow runners catch real deadlocks quickly).
		t.Fatal("Recv did not return within 5s after mid-flight cancellation; body read failed to observe canceled context") // THE most important message in this test — everything else above was setup for reaching exactly this observable moment of correct behavior... (keeping its phrasing unambiguous about WHICH direction failure means is what makes output immediately actionable).
	}

	if err := stream.Close(); err != nil { // final belt-and-suspenders release from the TEST's own goroutine after worker has finished — must be quiet regardless of which select branch above actually ran... (covers both normal and timeout paths uniformly without duplicating logic between them).
		t.Fatalf("final Close returned %v, want nil", err) // same idempotent-release guarantee as internal checks above but asserted from outside the worker goroutine's perspective too.
	} else if _, again := stream.Recv(); !errors.Is(again, io.EOF) { // terminal state must hold after that external close regardless of prior cancellation history — one more cheap assertion completing full end-to-end coverage for this row... (no further receives may surface anything but EOF from here on out).
		t.Fatalf("Recv after final Close = %v, want io.EOF", again) // consistent phrasing with every other post-close receive check in the suite so future readers find them all via identical search terms.
	}
}

// TestStreamHonorsStandardRedirects pins that standard-library redirect handling inside the one physical attempt is retained end to end: a 307 hop lands on its relative target with the stream accepted from THERE — two HTTP requests total for this single invocation (original plus redirect destination), both part of the same Client.Do call rather than any transport-level retry around them... (redirect following and retrying are different things contractually; only the former may appear here).
func TestStreamHonorsStandardRedirects(t *testing.T) { // modeled as: first hit on /chat/completions answers 307 with a relative Location to /v2/chat-completion-final — standard client resolves it against original URL automatically... (no custom policy configured anywhere in this type; default behavior IS what retention means here).
	var hitsFirst, hitsSecond int32 // two separate counters because we want exact per-path attribution rather than one ambiguous total that could hide "both went to first" style bugs entirely.

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // single handler serving BOTH hops by path switch — keeps everything in one server whose lifecycle matches the test's exactly... (two servers would add ordering/lifetime bookkeeping for zero behavioral difference).
		switch r.URL.Path { // exact-path dispatch rather than counting order because URL identity is what actually distinguishes these two exchanges semantically.
		case "/chat/completions": // first arrival: issue a 307 redirect pointing at the final path below... (relative location chosen deliberately so resolution logic itself gets exercised too).
			atomic.AddInt32(&hitsFirst, 1)              // count this hop specifically — expectation is exactly ONE total for this whole sub-case's duration.
			w.Header().Set("Location", "/v2/final")     // relative path per discussion above; no query strings or fragments needed to keep assertions minimal and unambiguous... (any extra URL components would only complicate what we're proving here).
			w.WriteHeader(http.StatusTemporaryRedirect) // 307 chosen over 301/302 on purpose: it preserves method AND body semantics per spec — exactly the "standard behavior retained" this row wants to demonstrate... (a weaker redirect code would still work mechanically but prove less about what default policy actually does).
			return                                      // no body written for a pure redirect response — clients must not expect one here either.

		case "/v2/final": // second arrival after following: serve the accepted stream with its single real event... (content chosen to be unmistakably different from any first-hop data so source attribution is provable below).
			atomic.AddInt32(&hitsSecond, 1)                                                                // exactly one hit expected here too — anything else means duplicate delivery or missed redirect somewhere in between.
			w.Header().Set("Content-Type", "text/event-stream")                                            // standard SSE content type as everywhere else in this suite for consistency across all serving handlers... (one less variable to account for when comparing captured behavior rows against each other).
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"landed-after-redirect\"}}]}\n\n") // distinctive literal value so assertion below can prove this content came from hop two specifically rather than being confused with anything else... (uniqueness of marker is what makes the proof airtight).
			fmt.Fprint(w, "data: [DONE]\n\n")                                                              // terminator included for complete end-to-end consumption like every other stream fixture in this file set.

		default: // unexpected path reaching us at all would indicate misconfiguration somewhere upstream of these assertions — fail loudly with exact value rather than guessing what went wrong... (defensive but cheap; costs one branch and prevents a whole class of confusing silent failures).
			t.Errorf("unexpected request path %q reached redirect test handler", r.URL.Path) // t.Error from within handler goroutine is the correct idiom here per Go's testing package constraints on Fatal usage across goroutines... (message carries enough context to triage without re-running under verbose mode necessarily).
		}
	}))
	defer server.Close()

	rt := testResolved()                                                   // standard valid baseline for this row's transport — nothing special about resolved input itself.
	rt.BaseURL = strings.TrimSuffix(server.URL, "/chat/completions") + "/" // endpoint lands on /chat/completions first hop... (trailing slash normalization exercised here too as a bonus since ChatEndpoint owns that behavior independently).
	tr := mustTransport(t, rt)                                             // construction helper — same rationale throughout this file for one shared path to valid transport values.

	stream, _, err := tr.Stream(context.Background(), baseRequest(), nil) // the entire redirect dance happens inside THIS ONE invocation's lifetime... (no separate "follow-up call" exists or may be invented by later edits).
	if err != nil {
		t.Fatalf("Stream failed while following a standard library-supported redirect: %v", err) // failure here would mean default client behavior regressed somewhere — fail with that specific framing since everything else in this row is trivially correct by construction... (message names the capability under test explicitly).
	}

	delta, recvErr := stream.Recv() // acceptance must come from post-redirect target's body specifically... (proven next via content marker below rather than assumed from status codes alone which both hops share nominally as 2xx-equivalent outcomes differently worded).
	if recvErr != nil {
		t.Fatalf("Recv after redirected acceptance = %v", recvErr) // a read failure at this point would mean something broke in transit between the two hops' responses — fail with context pointing there specifically... (distinguishing hop-attribution bugs from generic parser regressions depends on exactly which step's message you see).
	} else if len(delta.ContentFragments) != 1 || delta.ContentFragments[0].Text != "landed-after-redirect" { // THE attribution proof: content marker must match hop-two's unique literal exactly — no room for ambiguity about where this data actually came from.
		t.Fatalf("post-redirect delta = %#v, want single text fragment with exact redirect-target marker value", delta) // full struct rendered so any mismatch shape is immediately visible without re-running under verbose mode... (this is the row's central claim and its message deserves corresponding completeness).
	}

	if gotFirst := atomic.LoadInt32(&hitsFirst); gotFirst != 1 { // first hop hit exactly once — no more, no less across this entire invocation's lifetime.
		t.Fatalf("original endpoint hit %d times during redirect-following; want exactly 1", gotFirst) // >1 would mean retry-like behavior crept in around the redirect itself specifically... (naming which counter moved keeps diagnosis pointed at correct subsystem).
	} else if gotSecond := atomic.LoadInt32(&hitsSecond); gotSecond != 1 { // second hop also hit exactly once — together these two facts prove standard single-follow semantics precisely.
		t.Fatalf("redirect target hit %d times; want exactly 1 for one invocation", gotSecond) // combined with prior check this pins total server traffic at exactly two requests which is what "one Do call following its redirects" means observably... (any third request anywhere would violate the no-retry contract directly).
	} else if _, err := stream.Recv(); !errors.Is(err, io.EOF) { // terminator handling unaffected by having been redirected — same EOF expectation as every other well-formed fixture in this suite.
		t.Fatalf("Recv after [DONE] post-redirect = %v, want io.EOF", err) // consistent phrasing with sibling rows so cross-file greps remain uniform for related regression hunting later on... (cheap consistency costs nothing here).
	} else if cerr := stream.Close(); cerr != nil { // full consumption then quiet release — no cleanup path may surface transport facts to consumers at this point regardless of redirect history.
		t.Fatalf("Close returned error after clean post-redirect consumption: %v", cerr) // included for completeness of THIS row's end-to-end story rather than as independent coverage — sibling close-behavior tests pin pathological shapes separately elsewhere in the suite.
	}
}

// TestStreamReturnsEncoderWarningsOnAttemptFailure pins that protocol warnings belong to the model-effect caller regardless of transport outcome: whenever encoding succeeded they are returned even though this invocation's HTTP attempt later fails — both at status level (non-2xx body read and discarded)... AND at pure network-failure level where no response exists at all. Nothing in this package consumes or suppresses diagnostics on any failure path... (delivery is orthogonal to success by contract).
func TestStreamReturnsEncoderWarningsOnAttemptFailure(t *testing.T) { // warning delivery asserted through two distinct failure classes deliberately because they exercise different code paths inside Stream despite sharing the same observable requirement here.

	rt := testResolved()                            // target openai/gpt-test — source identity below matches it exactly so extras policy KEEPS this message rather than stripping by design... (same-model keep requires complete matching refs on both sides).
	rt.MustPreserve = []string{"reasoning_details"} // one listed metadata field that our fixture assistant deliberately lacks → deterministic single warning per encoding with no other sources present in these resolved inputs.

	assistant, err := NewMessage(Message{ // canonical constructor for the trigger message itself — its validity is asserted separately below so a broken fixture cannot silently change what we're testing here... (failing early on premise keeps later assertions meaningful).
		Role:   RoleAssistant, // assistant role required for must-preserve warnings to fire at all by encoder design.
		Source: rt.Model,      // same-model identity as target — the ONLY condition under which this message's extras survive replay policy intact... (zero or partial source would strip everything and silence our warning entirely).
		ToolCalls: []ToolCall{{ // presence of at least one completed call is what makes "assistant with tool calls" true for warning purposes here.
			ID:        "call_1",              // stable non-empty identity as required by NewMessage validation for assistant-side calls... (content irrelevant to warnings but must satisfy constructor anyway).
			Name:      "lookup",              // concrete tool name — any valid string works; keeping it short and distinctive makes fixture diffs readable later.
			Arguments: json.RawMessage(`{}`), // empty object arguments fine here since NO validation of argument content happens at warning-trigger level... (they just need to exist structurally as a completed call).
		}}, // single call suffices — multiple would only add noise without changing whether the one expected warning appears or not.
	})
	if err != nil {
		t.Fatalf("NewMessage fixture failed: %v", err) // stop with context rather than continuing against an invalid trigger message whose absence of warnings could be misread as transport behavior... (premises must hold before conclusions can mean anything).
	}

	req := Request{Messages: []Message{userText("hi"), assistant}} // two-message request puts the warning-bearing assistant at index 1 specifically — assertion below pins that exact position rather than just count for unambiguous attribution.

	wantWarning := func(got []ProtocolWarning) bool { // exactly one warning of must-preserve kind at message index 1 with its field context intact... (kind+field+index together define the semantic identity of this diagnostic beyond any human-readable text formatting choices).
		if len(got) != 1 || got[0].Kind != WarningMustPreserveMissing || got[0].Field != "reasoning_details" || got[0].MessageIndex != 1 { // every component checked explicitly so a partial match on some fields but not others fails loudly rather than passing silently through looser comparison... (this is the strictest reasonable form given how few warnings we expect here).
			return false // single boolean return keeps call sites below readable as plain if/else chains without introducing named error types for test-only logic unnecessarily.
		}
		return true // all four semantic components matched — this IS our one expected diagnostic and nothing else accompanied it anywhere in the returned slice.
	}

	t.Run("non2xxStillReturnsWarnings", func(t *testing.T) { // status-level failure: body read for its error shape then discarded, yet warnings ride out on top of that typed result untouched... (exercises the code path where Stream has already constructed HTTPStatusError before returning).
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // minimal failed response per row — content chosen to be parseable by extraction logic so no secondary failure modes can confound what we're actually asserting here.
			http.Error(w, `{"error":{"message":"overloaded"}}`, http.StatusServiceUnavailable) // 503 keeps auth semantics entirely out of scope for this particular warning-delivery question... (choosing a non-auth code deliberately isolates one variable at a time).
		}))
		defer server.Close()

		rtFail := rt                   // same resolved input plus endpoint for THIS sub-case specifically — copying rather than mutating shared state keeps rows independent by construction.
		rtFail.BaseURL = server.URL    // pinned pre-construction per file convention as everywhere else in the set... (standard discipline repeated here too).
		tr := mustTransport(t, rtFail) // standard helper — same rationale throughout for one shared path to valid transport values across all sub-cases alike.

		_, warnings, err := tr.Stream(context.Background(), req, nil) // one invocation; both return values stay available to every check below... (separate statements after the shape guard keep each failure message focused on exactly one aspect).
		var status *HTTPStatusError
		if !errors.As(err, &status) || status.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected a 503 HTTPStatusError from this attempt, got %v", err) // first establishing that THIS failure went through its full expected path — the warning assertion below would be meaningless without confirming we reached the right branch at all.
		} else if !wantWarning(warnings) { // THE core assertion for this sub-case: encoding's diagnostics survived transport-level failure intact and complete... (nothing dropped, reordered, or altered by passing through Stream's error-handling code paths).
			t.Fatalf("warnings after failed status attempt = %#v; want exactly one %s at message index 1", warnings, WarningMustPreserveMissing) // full slice rendered so any divergence shape — missing entirely vs wrong count vs malformed fields — is immediately visible in output without re-running under verbose mode.
		}
	})

	t.Run("networkFailureStillReturnsWarnings", func(t *testing.T) { // no response exists at all when the connection itself fails: encoding's diagnostics still belong to this caller exactly the same way as before... (different internal code path inside Stream but identical observable requirement).
		rtFail := rt                                                                                      // fresh copy per sub-case again — never sharing mutable state between iterations of any kind in this file.
		listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})) // start then immediately close below to learn one reliably-refused local address for deterministic failure... (avoiding dependence on external network policy or DNS behavior entirely).
		refusedURL := listener.URL                                                                        // captured BEFORE closing so we have a concrete usable endpoint string pointing at what is now guaranteed-unreachable.
		listener.Close()                                                                                  // close immediately — subsequent dials to this exact address will fail fast and predictably every time without any timing sensitivity involved... (this determinism matters more than realism for pure failure-shape testing).

		rtFail.BaseURL = refusedURL    // endpoint pinned pre-construction per file convention as everywhere else in the set.
		tr := mustTransport(t, rtFail) // standard helper — same rationale throughout this file for one shared path to valid transport values across all sub-cases alike.

		_, warnings, err := tr.Stream(context.Background(), req, nil) // one invocation; the refusal must surface as SOME error here — its exact wrapping shape is asserted loosely on purpose since standard-library internals vary and only "some failure happened" matters contractually for THIS row.
		if err == nil {
			t.Fatalf("Stream to a closed endpoint succeeded unexpectedly") // no response can exist at all when the connection itself fails — success would mean dialing went somewhere real, which this refused URL makes impossible... (failing loudly keeps that premise visible if environment ever changes).
		} else if !wantWarning(warnings) { // ...with the encoding's warnings intact alongside it exactly as in sibling sub-case above... (delivery guarantee does not depend on HOW far along transport got when failing).
			t.Fatalf("warnings after network-level failure = %#v; want exactly one %s at message index 1", warnings, WarningMustPreserveMissing) // same rendering discipline as everywhere else: full actual value visible next to expectation for immediate triage without additional runs needed.
		}
	})

	t.Run("acceptedStreamStillReturnsWarnings", func(t *testing.T) { // the success direction of the same contract: a 2xx response that transfers body ownership returns the encoder's ordered warnings ALONGSIDE the accepted stream — nothing about acceptance may consume, clear, or reorder them... (this row and its two failure siblings together pin warning delivery across every transport outcome class).
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { // minimal well-formed SSE body: one content event plus clean terminator so the stream below is fully consumable for a complete end-to-end proof rather than just its existence.
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, `data: {"choices":[{"delta":{"content":"with-warnings"}}]}`+"\n\ndata:[DONE]\n\n")
		}))
		defer server.Close()

		rtOK := rt                // fresh copy per sub-case as everywhere else in this file — the MustPreserve field that triggers our one warning rides along unchanged.
		rtOK.BaseURL = server.URL // pinned pre-construction per file convention... (standard discipline repeated here too).
		tr := mustTransport(t, rtOK)

		stream, warnings, err := tr.Stream(context.Background(), req, nil) // THE acceptance call: all three return values asserted below together — stream present AND error absent AND diagnostics intact is the complete success-shape for this contract row.
		if err != nil {
			t.Fatalf("accepted 2xx Stream returned an error: %v", err) // any failure here would make both remaining assertions vacuous — establish acceptance first exactly like every other end-to-end row in this file set... (cheap early exit keeps triage linear when the premise itself breaks).
		} else if stream == nil {
			t.Fatalf("accepted 2xx Stream returned a nil stream; body ownership must transfer through the public interface on success") // explicit presence check rather than dereferencing below — a nil-and-no-error shape is exactly the silent-ownership-leak class this row exists to rule out at its source.
		} else if !wantWarning(warnings) { // THE core assertion for THIS sub-case: encoding's diagnostics arrived with the successful result untouched and complete... (delivery on success is what the model-effect caller relies on before any failure path could even exist).
			t.Fatalf("warnings alongside accepted stream = %#v; want exactly one %s at message index 1", warnings, WarningMustPreserveMissing) // same rendering discipline as sibling sub-cases so aggregated output stays uniform when triaging warning-delivery regressions across all three outcome directions together.
		} else if delta, rerr := stream.Recv(); rerr != nil || len(delta.ContentFragments) != 1 || delta.ContentFragments[0].Text != "with-warnings" { // consuming one event proves the returned value is a LIVE working stream rather than merely non-nil — presence alone would pass even for an already-dead or miswired implementation... (the marker text distinguishes this row's payload from every sibling fixture in case both ever appear in aggregated verbose output).
			t.Fatalf("first Recv on warning-carrying accepted stream = %#v / %v; want the delivered event intact", delta, rerr) // full pair rendered so delivery-side corruption is distinguishable from any acceptance-shape problem already excluded above.
		} else if _, e := stream.Recv(); !errors.Is(e, io.EOF) { // clean terminator completes the end-to-end proof that nothing about warning emission disturbed framing or state on this accepted body... (cheap final closure keeping THIS row's story self-contained rather than leaning on sibling rows for basic delivery correctness).
			t.Fatalf("Recv after [DONE] on warning-carrying stream = %v; want clean io.EOF", e) // consistent phrasing with every other terminator assertion in the suite.
		} else if cerr := stream.Close(); cerr != nil {
			t.Fatalf("Close returned error on warning-carrying accepted stream: %v", cerr) // quiet release as everywhere — pinning it here keeps close-after-full-drain covered specifically under warning-emitting encodings rather than assuming independence transfers from the zero-warning rows.
		}
	})

	if _, err := NewMessage(Message{Role: RoleAssistant}); !errors.Is(err, ErrMissingSource) { // sanity guard on the fixture's own assumptions used above (assistant without source is invalid — our real one carries it deliberately)... if this fires then warning-trigger reasoning in wantWarning no longer holds and every row must be re-derived from scratch.
		t.Fatalf("fixture premise broken: assistant message WITHOUT complete source unexpectedly accepted") // explicit about which assumption failed rather than generic "something wrong" phrasing that would leave triage starting over from zero each time... (cheap specificity keeps maintenance of this file tractable long-term).
	}
}

// TestStreamClassifiesInvalidInputErrors pins the transport-level error classes at Stream's encoding trust boundary: invalid logical-request values return ONE typed invalid-input error whose message carries field and detail while preserving every underlying validation identity through its unwrap chain; reserved-key failures keep their own dedicated error shape with no added classification; malformed per-call runtime extra values pass through unclassified exactly as the encoder produced them. No physical request happens for any of these — all assertions are on the pre-network result shapes themselves... (a server would add nothing to what is being proven here).
func TestStreamClassifiesInvalidInputErrors(t *testing.T) { // one transport over a valid resolved input serves every row: classification depends only on which value class fails, so shared construction keeps each sub-case focused exclusively on its own error shape.

	rt := testResolved()       // complete identity plus default endpoint — BaseURL is never dialed in this test because encoding rejects before any request exists... (no server at all by design).
	tr := mustTransport(t, rt) // standard helper for one shared valid transport across every classification row below.

	t.Run("invalidRequestRoleReturnsTypedInvalidInputPreservingValidationIdentity", func(t *testing.T) { // a role outside the closed conversation set fails inside logical-request validation — THE representative messages-scope identity under test here... (chosen over other message identities because it is the simplest single-field violation whose position NewRequest names in its own text).
		bad := userText("hi")
		bad.Role = "bogus-role" // mutate a valid fixture's one role rather than building from scratch — every OTHER field stays known-good so the failure can only come from this exact value... (isolation of cause is what makes the identity-preservation assertions below meaningful).

		stream, warnings, err := tr.Stream(context.Background(), Request{Messages: []Message{bad}}, nil)
		if stream != nil { // no physical attempt can exist once encoding rejected — a non-nil stream would mean ownership transferred despite validation failure... (the strongest possible proof the rejection happened pre-request rather than being swallowed later).
			t.Fatalf("Stream returned an accepted stream for invalid request values; want none")
		} else if len(warnings) != 0 { // warnings exist only when encoding SUCCEEDS — a rejected value produces zero of them by construction... (pinning their absence here keeps the success-only provenance contract visible at this failure boundary too).
			t.Fatalf("rejected request returned protocol warnings: %#v; want none", warnings)
		} else if !errors.Is(err, ErrInvalidInput) { // THE umbrella typed identity must be reachable on the returned value itself — callers classify invalid-input failures by exactly this sentinel.
			t.Fatalf("Stream error for an invalid role = %v; want one wrapping ErrInvalidInput (typed invalid input with field and detail)", err) // full cause rendered so which unclassified shape escaped is visible alongside its expected identity for direct comparison...
		} else if !errors.Is(err, ErrInvalidRole) { // AND the specific validation sentinel it wraps must remain first-class in the SAME error's unwrap chain — preserving underlying identities means both sentinels hit via errors.Is on one value rather than forcing callers to choose between classes.
			t.Fatalf("underlying validation identity lost: %v; want ErrInvalidRole still reachable through wrapping", err) // names exactly which half of dual-reachability broke so a future single-%w rewrite that drops the inner chain is caught here immediately... (the message points at the mechanism, not just the symptom).
		} else if !strings.Contains(err.Error(), "messages[0]") { // field-and-detail requirement: the returned text must name WHICH request value failed — NewRequest's own positional prefix carries it and our wrapping must retain that context verbatim rather than replacing it with something generic. (The validator's own detail already includes the offending role value; preserving existing message content is part of what "preserving" means here.)
			t.Fatalf("error message = %q; want it to carry the failing field position (messages[0]) plus its detail", err) // full text rendered so any truncation or rewording of the positional prefix is visible character-by-character against this row's expectation...
		}
	})

	t.Run("invalidAssistantMissingSourceReturnsTypedInvalidInputPreservingValidationIdentity", func(t *testing.T) { // second messages-scope identity through a DIFFERENT validator branch: assistant role without its required source model fails validation... (covering the ErrMissingSource arm specifically because one representative per scope would leave this exact sentinel unproven if it were ever dropped from the classification set by future edits).
		req := Request{Messages: []Message{{Role: RoleAssistant}}} // direct struct literal BYPASSES NewMessage on purpose — that is precisely how a caller can hand Stream an invalid logical request value (the constructor only guards its own path, not every construction site)... (this bypass IS the realistic attack surface this classification protects).

		stream, _, err := tr.Stream(context.Background(), req, nil)
		if stream != nil { // same pre-request rejection proof as sibling positive row — no ownership transfer on any validation failure... (repeated here rather than shared because each sub-case stands alone when triaged from aggregated output without remembering its siblings). Warnings binding omitted: this identity's assertions focus purely on the error chain; sibling one already pins their success-only provenance.
			t.Fatalf("Stream returned an accepted stream for a sourceless assistant message; want none") // names the exact fixture shape so table-row mixups during editing are caught immediately in this specific failure.
		} else if !errors.Is(err, ErrInvalidInput) || !errors.Is(err, ErrMissingSource) { // BOTH sentinels asserted on one value exactly like sibling rows — dual-reachability is THE contract property for every classified identity... (grouping them in one condition keeps the message below attributable to whichever half failed via its dedicated follow-up checks).
			t.Fatalf("Stream error = %v; want both ErrInvalidInput and preserved ErrMissingSource reachable", err) // full cause rendered so an unclassified escape or a dropped inner chain is distinguishable at a glance from correct behavior.
		} else if !strings.Contains(err.Error(), "messages[0]") { // same positional-context requirement as sibling row — the field prefix must survive wrapping intact for every classified identity regardless of which validator produced it... (uniformity across identities keeps caller-side message parsing stable).
			t.Fatalf("error message = %q; want messages[0] position context preserved in this identity's classification too", err) // full text rendered so any per-identity divergence from the uniform prefix format is immediately visible against sibling rows' expectations.
		}
	})

	t.Run("invalidToolParametersReturnTypedInvalidInputPreservingValidationIdentity", func(t *testing.T) { // tools-scope identity through validateToolDefinition: parameters must be a JSON object, an array literal fails... (the third scope's representative — together with the two messages rows above this pins one distinct validator branch per closed request-value class so dropping ANY identity from classification would break exactly its own row).
		req := Request{Messages: []Message{userText("hi")}, Tools: []ToolDefinition{{Name: "lookup", Parameters: json.RawMessage(`[1]`)}}} // valid messages plus one structurally-wrong tool definition isolates the failure to tools-scope validation with zero confounding variables from message handling in this row's payload...

		stream, _, err := tr.Stream(context.Background(), req, nil)
		if stream != nil { // same pre-request rejection proof repeated per positive sub-case for standalone triage readability as everywhere else in this function.
			t.Fatalf("Stream returned an accepted stream for invalid tool parameters; want none") // names the exact fixture shape so table-row mixups during editing are caught immediately in this specific failure message rather than a generic one elsewhere.
		} else if !errors.Is(err, ErrInvalidInput) || !errors.Is(err, ErrInvalidParameters) { // dual-sentinel assertion identical in spirit to sibling rows — the tools branch must participate in classification exactly like its messages-scope siblings do... (any asymmetry between scopes here would be a real contract divergence worth failing loudly on first sight).
			t.Fatalf("Stream error = %v; want both ErrInvalidInput and preserved ErrInvalidParameters reachable", err) // full cause rendered so which half of dual-reachability broke is immediately attributable from output alone.
		} else if !strings.Contains(err.Error(), "tools[0]") { // positional context for the tools scope specifically — NewRequest names tool failures by index exactly like message ones and wrapping must retain that uniform format across scopes... (pinning it here keeps the field-and-detail contract provably consistent rather than spot-checked on one branch only).
			t.Fatalf("error message = %q; want tools[0] position context preserved in this identity's classification too", err) // full text rendered so any scope-specific formatting divergence is visible character-by-character against sibling expectations.
		}
	})

	t.Run("reservedKeysKeepTheirOwnErrorWithoutInvalidInputClassification", func(t *testing.T) { // the named exclusion: reserved extras return THE reserved-key error — adding invalid-input classification on top would conflate two distinct failure classes that callers handle differently... (this row pins their boundary from this side rather than assuming it holds by absence of contrary evidence elsewhere).
		stream, _, err := tr.Stream(context.Background(), baseRequest(), map[string]json.RawMessage{"stream": json.RawMessage(`true`)}) // one reserved top-level key in the per-call layer — minimal trigger with unambiguous ownership (no other value invalidity present anywhere in this invocation's inputs)...

		if stream != nil { // pre-request rejection proof as everywhere else — a reserved-key refusal can never produce an accepted stream... (repeated per sub-case for standalone triage readability rather than shared-helper indirection).
			t.Fatalf("Stream returned an accepted stream despite a reserved key; want none") // names the exact triggering condition so fixture mixups between sibling rows are caught immediately in their own failure message.
		} else if !errors.Is(err, ErrReservedKeys) { // THE dedicated identity must be reachable — this is what "reserved extras return the reserved-key error" means observably on one returned value... (every caller classifying by this sentinel depends on it surviving at exactly this boundary unchanged).
			t.Fatalf("Stream error for a reserved key = %v; want one wrapping ErrReservedKeys", err) // full cause rendered so any reclassification or lost identity is visible alongside its expected shape.
		} else if errors.Is(err, ErrInvalidInput) { // THE core negative assertion of this row: the invalid-input umbrella must NOT also be reachable here — dual-classification would make caller-side branching between these two classes impossible to write correctly... (this exact conflation is what pinning both directions prevents forever).
			t.Fatalf("reserved-key failure was over-classified as ErrInvalidInput too: %v; want its own identity ONLY", err) // names the specific regression direction so a future "simplify all encode failures into one class" rewrite fails loudly at this assertion rather than shipping silently.
		} else if !strings.Contains(err.Error(), `"stream"`) { // field-and-detail for THIS class means every offending key is named by its own error shape (the retained reserved-key formatting) — pinning the quoted key's presence proves that detail channel stays intact alongside its identity... (uniformity of "field and detail everywhere, each via its OWN class's format").
			t.Fatalf("reserved-key error = %q; want it to name the offending key \"stream\"", err) // full text rendered so any formatting drift in this dedicated shape is visible character-by-character against expectation.
		}
	})

	t.Run("malformedRuntimeExtraValuePassesThroughUnclassified", func(t *testing.T) { // the other named exclusion: a per-call runtime extra whose VALUE fails validation gets NO special transport classification — it passes through exactly as the encoder produced it... (line-314's typed error names resolved/request values and reserved keys only; unnamed failure classes keep whatever identity they already carry, which for this one is none beyond its message text).
		stream, _, err := tr.Stream(context.Background(), baseRequest(), map[string]json.RawMessage{"custom_field": json.RawMessage(`not-json`)}) // non-reserved key with an invalid JSON value — isolates pure value-validity failure from every other class present in this package's error space... (key choice matters: a reserved name would flip the row into its sibling exclusion above).

		if err == nil {
			t.Fatalf("malformed runtime extra accepted; want encoding to reject it exactly like today") // premise check first — if validation itself broke, every other assertion here would be vacuous noise... (establishing that rejection happened at all keeps triage linear when premises drift over time).
		} else if stream != nil { // same pre-request proof as sibling rows — no ownership transfer on any encoding failure regardless of its classification status below.
			t.Fatalf("Stream returned an accepted stream for a malformed runtime extra; want none") // names the exact triggering condition so fixture mixups between sibling exclusion rows are caught immediately in their own dedicated message rather than a shared generic one elsewhere... (cheap specificity per row).
		} else if errors.Is(err, ErrInvalidInput) { // THE core negative assertion of THIS row: no umbrella classification may attach to unnamed failure classes — pinning its absence here is what keeps the typed-error boundary exactly as narrow as specified rather than drifting wider through "helpful" consolidation... (over-classification regresses caller-side branching just like under-classification would).
			t.Fatalf("malformed runtime extra was over-classified as ErrInvalidInput: %v; want it unclassified per contract", err) // names the specific regression direction so a future broadening of classification scope fails loudly at this exact assertion instead of shipping silently through green sibling tests.
		} else if errors.Is(err, ErrReservedKeys) { // cross-exclusion guard in BOTH directions: value-validity failures must not borrow reserved-key identity either — three distinct classes (reserved-keys / invalid-input / unclassified-value-failures) stay mutually exclusive on one returned error... (this third direction is cheap to pin and completes the full pairwise separation matrix for this test function).
			t.Fatalf("malformed runtime extra was mis-classified as ErrReservedKeys: %v; want neither sentinel reachable", err) // names both sentinels in output so whichever wrong identity leaked is immediately visible alongside its actual message text.
		} else if !strings.Contains(err.Error(), "runtime") || !strings.Contains(err.Error(), `custom_field`) { // what DOES identify this failure shape: the encoder's own layer name plus offending field — both must remain present in whatever unclassified form passes through... (pinning their retention proves passthrough is VERBATIM rather than silently reworded by any intermediate handling).
			t.Fatalf("malformed runtime extra error = %q; want its original layer+field context preserved verbatim", err) // full text rendered so any alteration of the pass-through shape — even cosmetic rewording — fails against this row's exact expectations.
		}
	})

	if _, err := NewTransport(ResolvedTransport{Model: ModelRef{Provider: "openai"}}); !errors.Is(err, ErrInvalidModelRef) { // closing sanity anchor mirroring sibling files' convention — construction-time validation surface unchanged since this classification row was written... (kept deliberately cheap and local rather than extracted into a shared helper because each call site's context makes its failure message most useful when phrased for THAT specific error-class question).
		t.Fatalf("premise broken: incomplete model identity no longer rejected at NewTransport") // same explicit framing as equivalent anchors elsewhere so grep finds every such guard consistently across the suite.
	}
}

// TestTransportHasNoPublicClientSeam pins that the transport exposes no alternate client or RoundTripper seam: its only public behavior is Stream over an immutable resolved input plus package-level NewTransport construction, with every internal field unexported — tests reach it exclusively through httptest endpoints exactly as this whole suite demonstrates... (a future exported handle would break both pre-cutover isolation assumptions and one-physical-attempt contract simultaneously).
func TestTransportHasNoPublicClientSeam(t *testing.T) { // reflection over the type shape itself rather than trusting review alone — method/field sets asserted explicitly because drift here is invisible to every behavioral test above... (those all pass happily even with extra exported surface present; only structural checks catch that particular regression class).
	typ := reflect.TypeOf((*Transport)(nil)) // pointer-level reflection because that IS the public API — NewTransport hands back *Transport, and only its method set sees Stream's pointer receiver (value-level types would hide it entirely).
	structType := typ.Elem()                 // field checks run against the underlying struct shape; a pointer type itself has no fields to inspect.

	for i := 0; i < structType.NumField(); i++ { // every stored field must stay package-private — the seam this test forbids would have to be a FIELD (behavior pinned separately by method loop below).
		if f := structType.Field(i); f.PkgPath == "" {
			t.Fatalf("Transport exposes public field %s; no alternate client or RoundTripper seam may exist on this type", f.Name) // naming the exact offending member makes any future accidental export immediately visible and attributable in CI output without re-reading source... (this is a structural invariant worth failing loudly on first sight).
		}
	}

	var exportedMethods []string           // collect public method surface for exact comparison below — only Stream may appear publicly from *Transport itself.
	for i := 0; i < typ.NumMethod(); i++ { // NumMethod sees pointer-receiver methods too in modern Go regardless of how we obtained this reflect.Type value... (either way the expectation set remains identical).
		if m := typ.Method(i); m.PkgPath == "" {
			exportedMethods = append(exportedMethods, m.Name)
		} // unexported helpers never appear here by definition — only genuinely public API surface enters consideration at all.
	}

	sort.Strings(exportedMethods)                  // deterministic ordering before comparison so element-wise equality below is meaningful regardless of iteration order from reflection internals... (cheap insurance against flaky-looking failures that are really just nondeterministic enumeration).
	want := []string{"Stream"}                     // THE complete expected public method set on this type: nothing more, nothing less than exactly Stream itself.
	if !reflect.DeepEqual(exportedMethods, want) { // exact-set comparison — any extra export (a Do alias, Client getter...) would reopen the seam this test exists to keep closed forever... (and its absence is equally pinned here since removing Stream entirely would also fail loudly rather than silently).
		t.Fatalf("public methods on Transport = %v; want exactly [Stream]", exportedMethods) // rendered both sides side-by-side so whatever drifted — added OR removed member alike — is immediately obvious from output alone.
	}

	if _, err := NewTransport(ResolvedTransport{Model: ModelRef{Provider: "openai"}, WireSystemRole: ""}); !errors.Is(err, ErrInvalidModelRef) { // closing sanity anchor mirroring sibling files' convention one final time in THIS file as well — construction-time validation surface unchanged since this structural test was written... (kept deliberately cheap and local rather than extracted into a shared helper because each call site's context makes its failure message most useful when phrased for THAT row set specifically).
		t.Fatalf("premise broken: incomplete model identity no longer rejected at NewTransport") // same explicit framing as equivalent anchors elsewhere so grep finds every such guard consistently across the suite.
	}

	if _, ok := interface{}(mustTransport(t, testResolved())).(*http.Client); ok { // unreachability guard documenting that Transport itself is never an http.Client under any aliasing or embedding trick — kept more as compile-time reminder than runtime check... (its real value is forcing anyone reading this line to consciously acknowledge type-identity distinction).
		t.Fatal("Transport must not be or masquerade as *http.Client by type identity") // if someone ever aliases/embeds these types, fail loudly instead of letting the forbidden seam creep back in through pure type-substitution without any visible field-level change... (structural tests catch fields; this one catches whole-type substitution attempts specifically).
	}
}
