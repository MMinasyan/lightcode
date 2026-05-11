package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/MMinasyan/lightcode/internal/catalog"
	openai "github.com/sashabaranov/go-openai"
)

// New returns a Client configured with catalog-resolved provider and model
// metadata. Construction is cheap and performs no I/O.
func New(provider *catalog.Provider, model *catalog.Model, apiKey string) *Client {
	return &Client{
		provider: provider,
		model:    model,
		apiKey:   apiKey,
		http:     &http.Client{},
	}
}

// ChatStream opens a streaming chat completion request and returns a Stream.
// The caller must call Stream.Close() when done.
func (c *Client) ChatStream(ctx context.Context, messages []openai.ChatCompletionMessage, tools []openai.Tool, runtimeExtras map[string]any) (*Stream, error) {
	body, err := buildRequestBody(requestConfig{
		provider:      c.provider,
		model:         c.model,
		messages:      messages,
		tools:         tools,
		runtimeExtras: runtimeExtras,
	})
	if err != nil {
		return nil, err
	}
	return c.postStream(ctx, body)
}

// Chat performs a single non-streaming chat completion request. It is kept for
// compaction while the rest of the runtime moves to the streaming client.
func (c *Client) Chat(ctx context.Context, messages []openai.ChatCompletionMessage, tools []openai.Tool) (openai.ChatCompletionResponse, error) {
	body, err := buildRequestBody(requestConfig{
		provider: c.provider,
		model:    c.model,
		messages: messages,
		tools:    tools,
	})
	if err != nil {
		return openai.ChatCompletionResponse{}, err
	}
	delete(body, "stream")
	delete(body, "stream_options")

	resp, err := c.postJSON(ctx, body)
	if err != nil {
		return openai.ChatCompletionResponse{}, err
	}
	defer resp.Body.Close()

	var out openai.ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return openai.ChatCompletionResponse{}, fmt.Errorf("decode chat completion response: %w", err)
	}
	return out, nil
}

// ModelID returns the catalog model ID string.
func (c *Client) ModelID() string {
	if c == nil || c.model == nil {
		return ""
	}
	return c.model.ID
}

// Model returns the catalog model ID string (alias for ModelID, kept for
// backward compatibility with callers that used the old interface).
func (c *Client) Model() string {
	return c.ModelID()
}

func (c *Client) postStream(ctx context.Context, body map[string]any) (*Stream, error) {
	resp, err := c.postJSON(ctx, body)
	if err != nil {
		return nil, err
	}
	return NewStream(resp.Body), nil
}

func (c *Client) postJSON(ctx context.Context, body map[string]any) (*http.Response, error) {
	buf, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal chat completion request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, completionURL(c.provider), bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	req.Header = buildHeaders(c.provider, c.apiKey)

	httpClient := c.http
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	defer resp.Body.Close()
	statusErr := statusError(resp)
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, errors.Join(ErrAuthFailed, statusErr)
	}
	return nil, statusErr
}

func completionURL(provider *catalog.Provider) string {
	base := "https://api.openai.com/v1"
	if provider != nil && provider.Transport.BaseURL != "" {
		base = provider.Transport.BaseURL
	}
	return strings.TrimRight(base, "/") + "/chat/completions"
}

func statusError(resp *http.Response) *HTTPStatusError {
	message := ""
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if len(body) > 0 {
		message = strings.TrimSpace(string(body))
		var raw struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &raw) == nil {
			if raw.Error.Message != "" {
				message = raw.Error.Message
			} else if raw.Message != "" {
				message = raw.Message
			}
		}
	}
	return &HTTPStatusError{StatusCode: resp.StatusCode, StatusText: resp.Status, Message: message}
}
