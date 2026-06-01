// Package modelclient defines engine-facing model client interfaces.
package modelclient

import (
	"context"
	"fmt"

	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	openai "github.com/sashabaranov/go-openai"
)

// ChatRequest is the engine-facing request shape for chat model calls.
type ChatRequest struct {
	Messages []message.Message
	Tools    []openai.Tool
}

// ChatResponse is the engine-facing non-streaming response shape.
type ChatResponse struct {
	Content   string
	HasChoice bool
}

// ChatStreamer is the streaming model client surface used by the loop.
type ChatStreamer interface {
	ChatStream(ctx context.Context, req ChatRequest) (ChatStream, error)
	ProtocolWarnings(messages []message.Message) []ProtocolWarning
	Model() string
	ModelRef() coremodel.ModelRef
}

// Summarizer is the non-streaming model client surface used by compaction.
type Summarizer interface {
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
	Model() string
	ModelRef() coremodel.ModelRef
}

// ChatStream is a parsed model stream.
type ChatStream interface {
	Recv() (StreamDelta, error)
	Close() error
}

// ProtocolWarning describes a non-fatal protocol metadata diagnostic.
type ProtocolWarning struct {
	Kind         string
	Message      string
	Provider     string
	Model        string
	Field        string
	MessageIndex int
}

// StreamDelta is the engine-facing parsed view of one streaming chunk.
type StreamDelta struct {
	Role         string
	Content      string
	Refusal      string
	ToolCalls    []openai.ToolCall
	FinishReason openai.FinishReason
	Usage        *openai.Usage
	HasChoice    bool

	MessageExtra     message.Extra
	ToolCallExtra    map[int]message.Extra
	ContentPartExtra map[int]message.Extra
	ContentParts     []message.ContentPart
}

// ChunkParseError wraps a malformed provider chunk. Streams use this for
// non-fatal parse errors so the loop can skip the bad chunk and keep reading.
type ChunkParseError struct {
	Err error
}

func (e ChunkParseError) Error() string {
	return fmt.Sprintf("protocol chunk parse: %v", e.Err)
}

func (e ChunkParseError) Unwrap() error {
	return e.Err
}
