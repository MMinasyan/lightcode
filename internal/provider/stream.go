package provider

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"

	openai "github.com/sashabaranov/go-openai"
)

// Stream wraps an HTTP response body for Server-Sent Events parsing.
// It yields openai.ChatCompletionStreamResponse chunks and returns
// io.EOF on data:[DONE] or stream end.
type Stream struct {
	body   io.ReadCloser
	reader *bufio.Reader
	done   bool
}

// NewStream creates a Stream from a response body.
func NewStream(body io.ReadCloser) *Stream {
	return &Stream{body: body, reader: bufio.NewReader(body)}
}

// Recv reads the next SSE event and returns it as a ChatCompletionStreamResponse.
// Returns io.EOF when the stream is finished.
func (s *Stream) Recv() (openai.ChatCompletionStreamResponse, error) {
	if s == nil || s.body == nil || s.reader == nil || s.done {
		return openai.ChatCompletionStreamResponse{}, io.EOF
	}

	var data []string
	for {
		line, err := s.reader.ReadString('\n')
		if err != nil && len(line) == 0 {
			if err == io.EOF {
				s.done = true
			}
			return openai.ChatCompletionStreamResponse{}, err
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if len(data) == 0 {
				if err != nil {
					s.done = true
					return openai.ChatCompletionStreamResponse{}, err
				}
				continue
			}
			return decodeSSEData(data)
		}

		if strings.HasPrefix(line, ":") {
			if err != nil {
				s.done = true
				return openai.ChatCompletionStreamResponse{}, err
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
			return openai.ChatCompletionStreamResponse{}, err
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

func decodeSSEData(data []string) (openai.ChatCompletionStreamResponse, error) {
	payload := strings.Join(data, "\n")
	if strings.TrimSpace(payload) == "[DONE]" {
		return openai.ChatCompletionStreamResponse{}, io.EOF
	}
	var out openai.ChatCompletionStreamResponse
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		return openai.ChatCompletionStreamResponse{}, err
	}
	return out, nil
}
