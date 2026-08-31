package model

import "testing"

// TestBuildHeadersPinsDefaultsKeyAndOverrides pins the single header helper: JSON content type and SSE accept defaults, Bearer authorization exactly when a key is present (empty omits it entirely), then resolved headers overwriting matching defaults by exact case-sensitive key while adding new ones.
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

	t.Run("resolved-overwrite-and-add", func(t *testing.T) { // resolved headers win over matching defaults and may introduce new keys; case is preserved exactly as given.
		rt := testResolved()
		rt.APIKey = "sk-test"
		rt.Headers = map[string]string{
			"Accept":        "application/json", // overwrites the SSE accept default for its exact key only (Content-Type stays).
			"X-Lightcode-1": "on",               // new keys are added.
			"content-type":  "text/plain; x=1",  // different case from Content-Type: a distinct header, both must coexist verbatim.
		}
		got := BuildHeaders(rt)
		want := map[string]string{
			"Content-Type":  "application/json",
			"Accept":        "application/json",
			"Authorization": "Bearer sk-test",
			"X-Lightcode-1": "on",
			"content-type":  "text/plain; x=1",
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

// TestChatEndpointPinsDefaultAndCustom pins endpoint resolution: empty base uses the default OpenAI v1 chat-completions URL; a non-empty base gets trailing slashes removed and /chat/completions appended.
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
