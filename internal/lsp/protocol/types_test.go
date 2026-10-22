package protocol

import (
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestURIPathRoundTrip(t *testing.T) {
	path := "/tmp/light code/файл.go"
	uri := URIFromPath(path)
	testassert.Equal(t, PathFromURI(uri), path)
	testassert.Equal(t, PathFromURI("file:///tmp/a%20b.go"), "/tmp/a b.go")
	{
		got := PathFromURI("not a uri")
		testassert.Require(t, got != "", "PathFromURI invalid URI returned empty path")
	}
}

func TestHoverContentsUnmarshalVariants(t *testing.T) {
	tests := []struct{ name, input, want string }{
		{"string", `"plain"`, "plain"},
		{"markup", `{"kind":"markdown","value":"**doc**"}`, "**doc**"},
		{"array-markup", `[{"kind":"markdown","value":"first"}]`, "first"},
		{"array-string", `["first"]`, "first"},
		{"fallback", `{"kind":"markdown"}`, `{"kind":"markdown"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got HoverContents
			testassert.NoError(t, json.Unmarshal([]byte(tt.input), &got))
			testassert.Equal(t, got.Value, tt.want)
		})
	}
}
