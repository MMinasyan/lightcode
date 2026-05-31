package provider

import (
	"context"
	"fmt"

	"github.com/MMinasyan/lightcode/internal/coremodel"
	"github.com/MMinasyan/lightcode/internal/message"
	"github.com/MMinasyan/lightcode/internal/modelclient"
)

// Adapter exposes Client through the engine-facing modelclient interfaces.
type Adapter struct {
	client *Client
}

// NewAdapter returns an engine-facing adapter over a concrete provider client.
func NewAdapter(client *Client) *Adapter {
	if client == nil {
		return nil
	}
	return &Adapter{client: client}
}

// ChatStream opens a parsed streaming chat completion.
func (a *Adapter) ChatStream(ctx context.Context, req modelclient.ChatRequest) (modelclient.ChatStream, error) {
	if a == nil || a.client == nil {
		return nil, fmt.Errorf("no model configured")
	}
	stream, err := a.client.ChatStream(ctx, req.Messages, req.Tools, nil)
	if err != nil {
		return nil, err
	}
	return parsedStream{stream: stream}, nil
}

// Chat performs a non-streaming chat completion.
func (a *Adapter) Chat(ctx context.Context, req modelclient.ChatRequest) (modelclient.ChatResponse, error) {
	if a == nil || a.client == nil {
		return modelclient.ChatResponse{}, fmt.Errorf("no model configured")
	}
	resp, err := a.client.Chat(ctx, req.Messages, req.Tools)
	if err != nil {
		return modelclient.ChatResponse{}, err
	}
	if len(resp.Choices) == 0 {
		return modelclient.ChatResponse{}, nil
	}
	return modelclient.ChatResponse{Content: resp.Choices[0].Message.Content, HasChoice: true}, nil
}

// ProtocolWarnings returns non-fatal protocol metadata diagnostics.
func (a *Adapter) ProtocolWarnings(messages []message.Message) []modelclient.ProtocolWarning {
	if a == nil || a.client == nil {
		return nil
	}
	return a.client.ProtocolWarnings(messages)
}

// Model returns the catalog model ID string.
func (a *Adapter) Model() string {
	if a == nil || a.client == nil {
		return ""
	}
	return a.client.Model()
}

// ModelRef returns the resolved provider/model identity.
func (a *Adapter) ModelRef() coremodel.ModelRef {
	if a == nil || a.client == nil {
		return coremodel.ModelRef{}
	}
	return a.client.ModelRef()
}

type parsedStream struct {
	stream *Stream
}

func (s parsedStream) Recv() (modelclient.StreamDelta, error) {
	chunk, err := s.stream.Recv()
	if err != nil {
		return modelclient.StreamDelta{}, err
	}
	delta, err := ParseChunk(chunk.Raw)
	if err != nil {
		return modelclient.StreamDelta{}, modelclient.ChunkParseError{Err: err}
	}
	return delta, nil
}

func (s parsedStream) Close() error {
	if s.stream == nil {
		return nil
	}
	return s.stream.Close()
}

var _ modelclient.ChatStreamer = (*Adapter)(nil)
var _ modelclient.Summarizer = (*Adapter)(nil)
var _ modelclient.ChatStream = parsedStream{}
