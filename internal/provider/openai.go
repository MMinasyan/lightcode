// Package provider builds an OpenAI-compatible HTTP client for any
// configured provider. Every provider in Lightcode speaks the OpenAI API
// schema; the only per-provider variation is base URL and API key.
package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

// Client is a thin wrapper around the go-openai client that remembers
// which model string to use for every Chat call.
type Client struct {
	inner   *openai.Client
	model   string
	baseURL string
	apiKey  string
}

// New returns a Client configured against an arbitrary OpenAI-compatible
// endpoint. An empty baseURL falls back to go-openai's default.
func New(baseURL, apiKey, model string) *Client {
	cfg := openai.DefaultConfig(apiKey)
	if baseURL != "" {
		cfg.BaseURL = baseURL
	}
	effectiveBase := cfg.BaseURL
	if effectiveBase == "" {
		effectiveBase = "https://api.openai.com/v1"
	}
	return &Client{
		inner:   openai.NewClientWithConfig(cfg),
		model:   model,
		baseURL: effectiveBase,
		apiKey:  apiKey,
	}
}

// Model returns the model string this client is configured to use.
func (c *Client) Model() string { return c.model }

// Chat performs a single non-streaming chat completion request. Errors
// propagate verbatim — no retry logic in Slice 1.
func (c *Client) Chat(ctx context.Context, messages []openai.ChatCompletionMessage, tools []openai.Tool) (openai.ChatCompletionResponse, error) {
	req := openai.ChatCompletionRequest{
		Model:    c.model,
		Messages: messages,
	}
	if len(tools) > 0 {
		req.Tools = tools
	}
	return c.inner.CreateChatCompletion(ctx, req)
}

// FetchContextWindow tries two approaches to get the model's context
// window size: first GET /models/{id} (OpenAI standard), then
// GET /models (list all) and searches for a match. Returns 0 if
// neither works.
func (c *Client) FetchContextWindow(ctx context.Context) int {
	httpClient := &http.Client{Timeout: 10 * time.Second}

	// Try individual model endpoint first.
	if n := c.fetchModelContextWindow(ctx, httpClient, c.baseURL+"/models/"+c.model); n > 0 {
		return n
	}

	// Fall back to models list.
	return c.fetchFromModelsList(ctx, httpClient)
}

func (c *Client) fetchModelContextWindow(ctx context.Context, httpClient *http.Client, url string) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0
	}
	var raw struct {
		ContextLength int `json:"context_length"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return 0
	}
	return raw.ContextLength
}

func (c *Client) fetchFromModelsList(ctx context.Context, httpClient *http.Client) int {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/models", nil)
	if err != nil {
		return 0
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return 0
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0
	}
	var raw struct {
		Data []struct {
			ID            string `json:"id"`
			ContextLength int    `json:"context_length"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return 0
	}
	for _, m := range raw.Data {
		if m.ID == c.model || strings.HasPrefix(c.model, m.ID+":") {
			return m.ContextLength
		}
	}
	return 0
}

// ChatStream opens a streaming chat completion. The caller is responsible
// for calling stream.Close() when done.
func (c *Client) ChatStream(ctx context.Context, messages []openai.ChatCompletionMessage, tools []openai.Tool) (*openai.ChatCompletionStream, error) {
	req := openai.ChatCompletionRequest{
		Model:         c.model,
		Messages:      messages,
		StreamOptions: &openai.StreamOptions{IncludeUsage: true},
	}
	if len(tools) > 0 {
		req.Tools = tools
	}
	return c.inner.CreateChatCompletionStream(ctx, req)
}
