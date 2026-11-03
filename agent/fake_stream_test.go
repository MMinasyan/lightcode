package agent

import (
	"io"

	"github.com/MMinasyan/lightcode/model"
)

var testRef = model.ModelRef{Provider: "acme", Model: "m-1"} // one complete source identity every assembler and boundary-validation test asserts against.

// fakeStream implements model.Stream over a fixed script of receive outcomes, counting Recv/Close calls so tests can pin exactly-once close and post-close read behavior; it never retries, classifies, or skips anything — each scripted outcome comes back verbatim in order, then io.EOF once the script is exhausted.
type fakeStream struct {
	script          []func() (model.StreamDelta, error) // one entry per Recv call until exhaustion.
	recvCount       int                                 // total Recv calls made; pins how far assembly read into a scripted tail.
	closeCount      int                                 // Close must land exactly once for any accepted stream the assembler owns.
	closed          bool                                // set by the first Close so later reads are observable as post-close recvs below.
	recvsAfterClose int                                 // reads observed after the first Close: must stay zero in every test (the owner never re-reads a closed stream).
	closeErr        error                               // optional close failure to exercise cleanup-only close-error behavior; nil means clean close.
}

func newFakeStream(script ...func() (model.StreamDelta, error)) *fakeStream {
	return &fakeStream{script: script}
}

// withCloseError returns the same scripted stream whose Close reports err on every call — a test builds its script first and only then decides whether closing itself may fail.
func (f *fakeStream) withCloseError(err error) *fakeStream { f.closeErr = err; return f }

func deltaStep(d model.StreamDelta) func() (model.StreamDelta, error) { // one successful receive carrying d verbatim on the next Recv call only.
	return func() (model.StreamDelta, error) { return d, nil }
}

func errStep(err error) func() (model.StreamDelta, error) { // one accepted-stream read failure on the next Recv call only; classification of that failure belongs to assembly under test, never here.
	return func() (model.StreamDelta, error) { return model.StreamDelta{}, err }
}

func (f *fakeStream) Recv() (model.StreamDelta, error) { // one scripted outcome per call in order and io.EOF once the script is exhausted or after Close — mirroring the accepted-stream contract without adding retry, classification, or skip behavior of its own.
	f.recvCount++
	if f.closed { // post-close reads are observable through recvsAfterClose; every test pins them at zero because the owner never re-reads a closed stream
		f.recvsAfterClose++
		return model.StreamDelta{}, io.EOF
	}

	idx := f.recvCount - 1
	if idx < len(f.script) {
		return f.script[idx]()
	}

	return model.StreamDelta{}, io.EOF // exhausted script reports normal termination.
}

func (f *fakeStream) Close() error {
	f.closeCount++
	f.closed = true // flip the observable post-close flag
	return f.closeErr
}
