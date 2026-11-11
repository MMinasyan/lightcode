// Package worker implements the executable-extension protocol shared by the
// host and Go authoring adapters: bounded NDJSON frames, the JSONBytes opaque
// value encoding, the declaration wire form, the typed codecs for every fixed
// method, and the author-side serving loop.
package worker

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// FrameLimit is the maximum size of one encoded frame envelope, excluding its
// line terminator.
const FrameLimit = 4 << 20

// The closed frame kinds.
const (
	KindRequest  = "request"
	KindResponse = "response"
	KindCancel   = "cancel"
	KindProgress = "progress"
)

// The closed request methods.
const (
	MethodActivate = "activate"
	MethodOpen     = "open"
	MethodCall     = "call"
	MethodDispose  = "dispose"
	MethodHostcall = "hostcall"
)

// The closed call target kinds.
const (
	TargetMetadata = "metadata"
	TargetInstance = "instance"
)

// ErrFrameTooLarge marks a frame whose encoded envelope exceeds FrameLimit.
var ErrFrameTooLarge = errors.New("worker: frame exceeds the size limit")

// JSONBytes is one opaque byte value carried as a base64 JSON string. Decoding
// accepts only canonical padded standard base64; the decoded bytes are
// preserved unchanged and may be any bytes, not only JSON.
type JSONBytes []byte

// MarshalJSON encodes the bytes as one base64 string.
func (b JSONBytes) MarshalJSON() ([]byte, error) {
	return json.Marshal(base64.StdEncoding.EncodeToString(b))
}

// UnmarshalJSON decodes one canonical base64 string.
func (b *JSONBytes) UnmarshalJSON(data []byte) error {
	var encoded *string
	if err := json.Unmarshal(data, &encoded); err != nil || encoded == nil {
		return errors.New("opaque value must be a base64 string")
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(*encoded)
	if err != nil || base64.StdEncoding.EncodeToString(decoded) != *encoded {
		return errors.New("opaque value must be canonical base64")
	}
	*b = decoded
	return nil
}

// Frame is one protocol envelope. The members used by each kind are:
// request carries id, method and params; response carries id and exactly one
// of result or error; cancel carries id; progress carries id and value.
type Frame struct {
	Kind   string          `json:"kind"`
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
	Value  json.RawMessage `json:"value,omitempty"`
}

// Reader reads bounded NDJSON frames.
type Reader struct {
	scanner *bufio.Scanner
}

// NewReader returns a frame reader bounded by FrameLimit envelope bytes per
// line. The scanner buffer carries one extra byte for the line terminator, so
// an exactly-FrameLimit envelope is accepted.
func NewReader(r io.Reader) *Reader {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), FrameLimit+1)
	return &Reader{scanner: scanner}
}

// ReadFrame reads one frame. It returns io.EOF at a clean end of input and a
// wrapped ErrFrameTooLarge for a line over the limit. The kind is decoded
// first: a progress frame is dropped without inspecting its other members, so
// only unparseable JSON or an over-limit line is fatal for that kind. Any
// non-progress frame carrying a JSON null payload member is invalid.
func (r *Reader) ReadFrame() (Frame, error) {
	if !r.scanner.Scan() {
		err := r.scanner.Err()
		if errors.Is(err, bufio.ErrTooLong) {
			return Frame{}, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, FrameLimit)
		}
		if err != nil {
			return Frame{}, err
		}
		return Frame{}, io.EOF
	}
	line := r.scanner.Bytes()
	if len(line) > FrameLimit {
		return Frame{}, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(line))
	}
	var envelope struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(line, &envelope); err != nil {
		return Frame{}, fmt.Errorf("worker: malformed frame: %w", err)
	}
	if envelope.Kind == KindProgress {
		return Frame{Kind: KindProgress}, nil
	}
	if err := rejectNullMembers(line, envelope.Kind == KindResponse); err != nil {
		return Frame{}, fmt.Errorf("worker: malformed frame: %w", err)
	}
	var frame Frame
	if err := json.Unmarshal(line, &frame); err != nil {
		return Frame{}, fmt.Errorf("worker: malformed frame: %w", err)
	}
	return frame, nil
}

// rejectNullMembers rejects any JSON null token in one frame envelope except
// the protocol-level null result of a response: no payload or routing member is
// nullable.
func rejectNullMembers(raw []byte, allowNullResult bool) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	if members == nil {
		return errors.New("frame must be a JSON object")
	}
	for key, value := range members {
		if allowNullResult && key == "result" && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			continue
		}
		if err := rejectNullTokens(value); err != nil {
			return fmt.Errorf("member %q: %w", key, err)
		}
	}
	return nil
}

func rejectNullTokens(raw json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if token == nil {
			return errors.New("null member is not permitted")
		}
	}
}

// Writer writes bounded NDJSON frames with serialized writes.
type Writer struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriter returns a frame writer.
func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

// Encode returns the exact bounded envelope bytes, excluding the line
// terminator.
func (f Frame) Encode() ([]byte, error) {
	encoded, err := json.Marshal(f)
	if err != nil {
		return nil, err
	}
	if len(encoded) > FrameLimit {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(encoded))
	}
	return encoded, nil
}

// WriteFrame encodes and writes one frame followed by a newline. It returns a
// wrapped ErrFrameTooLarge instead of writing an over-limit envelope.
func (w *Writer) WriteFrame(frame Frame) error {
	encoded, err := frame.Encode()
	if err != nil {
		return err
	}
	return w.WriteEncoded(encoded)
}

// WriteEncoded writes one already-encoded bounded envelope followed by a
// newline with serialized writes.
func (w *Writer) WriteEncoded(encoded []byte) error {
	if len(encoded) > FrameLimit {
		return fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, len(encoded))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	buffer := make([]byte, 0, len(encoded)+1)
	buffer = append(buffer, encoded...)
	buffer = append(buffer, '\n')
	n, err := w.w.Write(buffer)
	if err != nil {
		return err
	}
	if n != len(buffer) {
		return io.ErrShortWrite
	}
	return nil
}
