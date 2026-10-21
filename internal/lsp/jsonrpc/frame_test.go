package jsonrpc

import (
	"bufio"
	"bytes"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestWriteReadMessageRoundTrip(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1}`)
	var buf bytes.Buffer
	testassert.NoError(t, WriteMessage(&buf, body))
	got, err := ReadMessage(bufio.NewReader(&buf))
	testassert.NoError(t, err)
	testassert.Require(t, string(got) == string(body), "body = %q, want %q", got, body)
}

func TestReadMessageHeadersAndErrors(t *testing.T) {
	got, err := ReadMessage(bufio.NewReader(strings.NewReader("X-Test: yes\r\nContent-Length: 2\r\n\r\n{}")))
	testassert.NoError(t, err)
	testassert.Require(t, string(got) == "{}", "body = %q, want {}", got)
	{
		_, err := ReadMessage(bufio.NewReader(strings.NewReader("Header: nope\r\n\r\n{}")))
		testassert.Require(t, err != nil, "missing Content-Length error = nil, want error")
	}
	{
		_, err := ReadMessage(bufio.NewReader(strings.NewReader("Content-Length: nope\r\n\r\n{}")))
		testassert.Require(t, err != nil, "bad Content-Length error = nil, want error")
	}
}
