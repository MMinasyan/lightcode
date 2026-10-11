package terminal

import (
	"bytes"
	"testing"
)

// TestWriteClipboardOSC52Bytes pins the exact OSC-52 sequence: the escape
// introducer, the "c" clipboard selection, the standard base64 payload and the
// BEL terminator.
func TestWriteClipboardOSC52Bytes(t *testing.T) {
	var b bytes.Buffer
	if err := WriteClipboardOSC52(&b, "hello"); err != nil {
		t.Fatalf("WriteClipboardOSC52: %v", err)
	}
	want := "\x1b]52;c;aGVsbG8=\x07"
	if got := b.String(); got != want {
		t.Fatalf("OSC-52 bytes = %q, want %q", got, want)
	}
}
