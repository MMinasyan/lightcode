package jsonrpc

import (
	"bufio"
	"bytes"
	"strings"
	"testing"
)

func TestWriteReadMessageRoundTrip(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1}`)
	var buf bytes.Buffer
	if err := WriteMessage(&buf, body); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	got, err := ReadMessage(bufio.NewReader(&buf))
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	if string(got) != string(body) {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

func TestReadMessageHeadersAndErrors(t *testing.T) {
	got, err := ReadMessage(bufio.NewReader(strings.NewReader("X-Test: yes\r\nContent-Length: 2\r\n\r\n{}")))
	if err != nil {
		t.Fatalf("ReadMessage with extra header: %v", err)
	}
	if string(got) != "{}" {
		t.Fatalf("body = %q, want {}", got)
	}
	if _, err := ReadMessage(bufio.NewReader(strings.NewReader("Header: nope\r\n\r\n{}"))); err == nil {
		t.Fatal("missing Content-Length error = nil, want error")
	}
	if _, err := ReadMessage(bufio.NewReader(strings.NewReader("Content-Length: nope\r\n\r\n{}"))); err == nil {
		t.Fatal("bad Content-Length error = nil, want error")
	}
}
