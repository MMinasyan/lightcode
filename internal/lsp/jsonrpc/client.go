package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

type ResponseError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *ResponseError) Error() string {
	return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message)
}

type rawResponse struct {
	Result json.RawMessage
	Error  *ResponseError
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int            `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ResponseError  `json:"error,omitempty"`
}

type Client struct {
	stdin  io.WriteCloser
	stdout *bufio.Reader

	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int
	pending map[int]chan rawResponse
	closed  bool

	OnNotify func(method string, params json.RawMessage)
}

func NewClient(stdin io.WriteCloser, stdout io.Reader, stderr io.Reader, onNotify func(string, json.RawMessage)) *Client {
	c := &Client{
		stdin:    stdin,
		stdout:   bufio.NewReader(stdout),
		pending:  make(map[int]chan rawResponse),
		OnNotify: onNotify,
	}
	if stderr != nil {
		go io.Copy(io.Discard, stderr)
	}
	return c
}

func (c *Client) Start() {
	go c.readLoop()
}

func (c *Client) readLoop() {
	for {
		body, err := ReadMessage(c.stdout)
		if err != nil {
			c.mu.Lock()
			c.closed = true
			for id, ch := range c.pending {
				ch <- rawResponse{Error: &ResponseError{Code: -1, Message: "connection closed"}}
				delete(c.pending, id)
			}
			c.mu.Unlock()
			return
		}

		var msg message
		if err := json.Unmarshal(body, &msg); err != nil {
			continue
		}

		if msg.ID != nil && msg.Method == "" {
			c.mu.Lock()
			ch, ok := c.pending[*msg.ID]
			if ok {
				delete(c.pending, *msg.ID)
			}
			c.mu.Unlock()
			if ok {
				ch <- rawResponse{Result: msg.Result, Error: msg.Error}
			}
		} else if msg.Method != "" && msg.ID == nil {
			if c.OnNotify != nil {
				c.OnNotify(msg.Method, msg.Params)
			}
		} else if msg.Method != "" && msg.ID != nil {
			c.respondToRequest(*msg.ID, msg.Method)
		}
	}
}

func (c *Client) respondToRequest(id int, method string) {
	resp := message{
		JSONRPC: "2.0",
		ID:      &id,
		Result:  json.RawMessage("null"),
	}
	body, _ := json.Marshal(resp)
	c.writeMu.Lock()
	WriteMessage(c.stdin, body)
	c.writeMu.Unlock()
}

func (c *Client) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, fmt.Errorf("connection closed")
	}
	id := c.nextID
	c.nextID++
	ch := make(chan rawResponse, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	var rawParams json.RawMessage
	if params != nil {
		var err error
		rawParams, err = json.Marshal(params)
		if err != nil {
			c.mu.Lock()
			delete(c.pending, id)
			c.mu.Unlock()
			return nil, fmt.Errorf("marshal params: %w", err)
		}
	}

	msg := message{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  rawParams,
	}
	body, _ := json.Marshal(msg)

	c.writeMu.Lock()
	err := WriteMessage(c.stdin, body)
	c.writeMu.Unlock()
	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("write request: %w", err)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

func (c *Client) Notify(method string, params any) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return fmt.Errorf("connection closed")
	}
	c.mu.Unlock()

	var rawParams json.RawMessage
	if params != nil {
		var err error
		rawParams, err = json.Marshal(params)
		if err != nil {
			return fmt.Errorf("marshal params: %w", err)
		}
	}

	msg := message{
		JSONRPC: "2.0",
		Method:  method,
		Params:  rawParams,
	}
	body, _ := json.Marshal(msg)

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return WriteMessage(c.stdin, body)
}

func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	for id, ch := range c.pending {
		ch <- rawResponse{Error: &ResponseError{Code: -1, Message: "client closed"}}
		delete(c.pending, id)
	}
	c.mu.Unlock()
	c.stdin.Close()
}

func (c *Client) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
