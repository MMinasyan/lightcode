package provider

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// Stream wraps an HTTP response body for Server-Sent Events parsing.
// It yields typed chunks with the raw JSON payload and returns io.EOF on
// data:[DONE] or stream end.
type Stream struct {
	body   io.ReadCloser
	reader *bufio.Reader
	done   bool
}

// StreamChunk is one SSE payload decoded into the SDK type while preserving
// the original JSON for provider-specific metadata parsing.
type StreamChunk struct {
	Typed openai.ChatCompletionStreamResponse
	Raw   json.RawMessage
}

// NewStream creates a Stream from a response body.
func NewStream(body io.ReadCloser) *Stream {
	return &Stream{body: body, reader: bufio.NewReader(body)}
}

// Recv reads the next SSE event and returns it as a StreamChunk.
// Returns io.EOF when the stream is finished.
func (s *Stream) Recv() (StreamChunk, error) {
	if s == nil || s.body == nil || s.reader == nil || s.done {
		return StreamChunk{}, io.EOF
	}

	var data []string
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			if err == io.EOF {
				s.done = true
			}
			return StreamChunk{}, err
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(data) == 0 {
				if err != nil {
					s.done = true
					return StreamChunk{}, err
				}
				continue
			}
			return decodeSSEData(data)
		}

		if strings.HasPrefix(line, ":") {
			if err != nil {
				s.done = true
				return StreamChunk{}, err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			value := strings.TrimPrefix(line, "data:")
			value = strings.TrimPrefix(value, " ")
			data = append(data, value)
		}
		if err != nil {
			if len(data) > 0 {
				return decodeSSEData(data)
			}
			s.done = true
			return StreamChunk{}, err
		}
	}
}

// Close closes the underlying response body.
func (s *Stream) Close() error {
	if s == nil || s.body == nil {
		return nil
	}
	s.done = true
	return s.body.Close()
}

func decodeSSEData(data []string) (StreamChunk, error) {
	payload := strings.Join(data, "\n")
	if strings.TrimSpace(payload) == "[DONE]" {
		return StreamChunk{}, io.EOF
	}
	var out openai.ChatCompletionStreamResponse
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		return StreamChunk{}, err
	}
	return StreamChunk{Typed: out, Raw: json.RawMessage(payload)}, nil
}
