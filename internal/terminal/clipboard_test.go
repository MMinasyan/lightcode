package terminal

import (
	"bytes"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestWriteClipboardOSC52Bytes pins the exact OSC-52 sequence: the escape
// introducer, the "c" clipboard selection, the standard base64 payload and the
// BEL terminator.
func TestWriteClipboardOSC52Bytes(t *testing.T) {
	var b bytes.Buffer
	testassert.NoError(t, WriteClipboardOSC52(&b, "hello"))
	want := "\x1b]52;c;aGVsbG8=\x07"
	testassert.Equal(t, b.String(), want)
}
