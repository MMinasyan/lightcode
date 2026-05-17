package protocol

import (
	"encoding/json"
	"testing"
)

func TestURIPathRoundTrip(t *testing.T) {
	path := "/tmp/light code/файл.go"
	uri := URIFromPath(path)
	if got := PathFromURI(uri); got != path {
		t.Fatalf("PathFromURI(URIFromPath(%q)) = %q", path, got)
	}
	if got := PathFromURI("file:///tmp/a%20b.go"); got != "/tmp/a b.go" {
		t.Fatalf("PathFromURI decoded = %q", got)
	}
	if got := PathFromURI("not a uri"); got == "" {
		t.Fatal("PathFromURI invalid URI returned empty path")
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
			if err := json.Unmarshal([]byte(tt.input), &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Value != tt.want {
				t.Fatalf("Value = %q, want %q", got.Value, tt.want)
			}
		})
	}
}
