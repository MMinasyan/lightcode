package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/MMinasyan/lightcode/protocol"
)

// Request performs one authenticated byte request against the connected
// Runtime over the transport Connect verified: the same endpoint, HTTP client,
// and request editors, with no second discovery decode and no exposed
// credential. The target is a relative request URI whose decoded path begins
// /v1/ and carries no scheme, authority, or fragment; query text stays
// intact; any other target fails before network I/O. A supplied body is sent
// as JSON. The generated event route is refused for every method and spelling
// because Events remains the one subscription path. The response is returned
// raw — status, headers, and exact body bytes, partial and error results
// included — a redirect is returned, never followed, and the caller closes
// the body. Cancellation cancels this request's transport alone.
func (c *Client) Request(ctx context.Context, method, path string, body io.Reader) (*http.Response, error) {
	generated, ok := c.ClientInterface.(*protocol.Client)
	if !ok {
		return nil, errors.New("client: connected transport is not a generated protocol client")
	}
	target, err := url.Parse(path)
	if err != nil {
		return nil, fmt.Errorf("request target %q: %w", path, err)
	}
	if target.Scheme != "" || target.Host != "" || strings.Contains(path, "#") {
		return nil, fmt.Errorf("request target %q carries a scheme, authority, or fragment", path)
	}
	if !strings.HasPrefix(target.Path, "/v1/") {
		return nil, fmt.Errorf("request target %q is outside /v1", path)
	}
	if target.Path == "/v1/events" {
		return nil, errors.New("request target is the event stream route")
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(generated.Server, "/")+path, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	for _, editor := range generated.RequestEditors {
		if err := editor(ctx, request); err != nil {
			return nil, err
		}
	}
	return generated.Client.Do(request)
}
