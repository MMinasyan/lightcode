package model

import "testing"

// TestBuildHeadersPinsDefaultsKeyAndOverrides pins the single header helper: JSON content type and SSE accept defaults, Bearer authorization exactly when a key is present (empty omits it entirely), then resolved headers overwriting matching defaults case-insensitively while adding new ones — every entry stored in MIME canonical form with http.Header.Set semantics.
func TestBuildHeadersPinsDefaultsKeyAndOverrides(t *testing.T) {
	t.Run("defaults-without-key", func(t *testing.T) { // no API key: exactly the two protocol defaults, nothing else may appear.
		got := BuildHeaders(ResolvedTransport{})
		want := map[string]string{"Content-Type": "application/json", "Accept": "text/event-stream"}
		assertHeaderMapEqual(t, got, want)
	})

	t.Run("bearer-when-key-present", func(t *testing.T) { // non-empty key adds the bearer line on top of both defaults.
		rt := testResolved()
		rt.APIKey = "sk-test"
		got := BuildHeaders(rt)
		want := map[string]string{"Content-Type": "application/json", "Accept": "text/event-stream", "Authorization": "Bearer sk-test"}
		assertHeaderMapEqual(t, got, want)
	})

	t.Run("resolved-overwrite-and-add", func(t *testing.T) { // resolved headers win over matching defaults regardless of spelling and may introduce new keys; the case-variant content-type must collapse into the canonical Content-Type entry rather than coexist as a second one.
		rt := testResolved()
		rt.APIKey = "sk-test"
		rt.Headers = map[string]string{
			"Accept":        "application/json", // overwrites the SSE accept default (exact key here, variant keys in the sibling row).
			"X-Lightcode-1": "on",               // new already-canonical keys are added.
			"content-type":  "text/plain; x=1",  // case-insensitive match on Content-Type: replaces it and is stored canonically, exactly as http.Header.Set would.
		}
		got := BuildHeaders(rt)
		want := map[string]string{
			"Content-Type":  "text/plain; x=1", // the resolved value won over the default through its case-variant key — one entry per name total.
			"Accept":        "application/json",
			"Authorization": "Bearer sk-test",
			"X-Lightcode-1": "on",
		}
		assertHeaderMapEqual(t, got, want) // len equality also pins that no lowercase duplicate entry survived alongside the canonical one.
	})

	t.Run("case-insensitive-overrides-canonicalize-keys", func(t *testing.T) { // every default is reachable through any spelling and non-canonical custom keys are stored in MIME canonical form (http.Header.Set behavior retained end to end).
		rt := testResolved()
		rt.APIKey = "sk-test"
		rt.Headers = map[string]string{
			"authorization":   "Basic zzz",     // replaces the Bearer default through a case-variant key; only one Authorization entry may remain.
			"x-custom-header": "v=1",           // custom header arrives non-canonical and must be stored as X-Custom-Header.
			"ACCEPT":          "application/*", // uppercase variant of the accept default also overwrites it case-insensitively.
		}
		got := BuildHeaders(rt)
		want := map[string]string{
			"Content-Type":    "application/json", // untouched by this row's resolved entries (their variants matched other names only).
			"Accept":          "application/*",    // overwritten through the ACCEPT spelling.
			"Authorization":   "Basic zzz",        // bearer replaced; canonical name retained for the single remaining entry.
			"X-Custom-Header": "v=1",              // stored in MIME canonical form, not as given.
		}
		assertHeaderMapEqual(t, got, want)
	})
}

// assertHeaderMapEqual fails with both maps shown when they differ in size or any value.
func assertHeaderMapEqual(t *testing.T, got, want map[string]string) {
	t.Helper() // one comparison helper for every header subtest above and below.
	if len(got) != len(want) {
		t.Fatalf("headers = %#v (len %d), want %#v (len %d)", got, len(got), want, len(want))
	}
	for k, v := range want { // every expected key present with the exact value.
		if gv, ok := got[k]; !ok || gv != v {
			t.Fatalf("header %q = %q (present=%v), want %q", k, gv, ok, v)
		}
	}
	for k := range got { // no unexpected key may appear either.
		if _, ok := want[k]; !ok {
			t.Fatalf("unexpected header %q: %#v", k, got)
		}
	}
}

// TestChatEndpointPinsDefaultAndCustom pins endpoint resolution: only an empty base uses the default OpenAI v1 chat-completions URL; EVERY non-empty base gets its trailing slashes removed and /chat/completions appended — including a slash-only one, whose trim leaves no host part but which is still input (no fallback for it).
func TestChatEndpointPinsDefaultAndCustom(t *testing.T) {
	cases := []struct {
		name string
		base string // resolved BaseURL under test (empty = unset).
		want string // exact expected endpoint for that input.
	}{
		{"default-when-empty", "", "https://api.openai.com/v1/chat/completions"},
		{"custom-no-trailing-slash", "http://localhost:8080/api", "http://localhost:8080/api/chat/completions"}, // no slash to trim, suffix appended as-is.
		{"custom-one-trailing-slash", "https://api.example.com/v1/", "https://api.example.com/v1/chat/completions"},
		{"custom-many-trailing-slashes", "http://h:9///", "http://h:9/chat/completions"}, // every trailing slash collapses away.
		{"slash-only-base-still-nonempty-input", "///", "/chat/completions"},             // trims to an empty host part but never takes the default — only a truly empty base does.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := testResolved() // identity fields are irrelevant to endpoint resolution; only BaseURL varies per row.
			rt.BaseURL = tc.base
			if got := ChatEndpoint(rt); got != tc.want {
				t.Fatalf("ChatEndpoint(base=%q) = %q, want %q", tc.base, got, tc.want)
			}
		})
	}
}
