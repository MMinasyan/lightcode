// Package client connects one Go caller to an authenticated Lightcode
// Runtime over the generated protocol: strict startup-file discovery,
// authenticated health, and the one SSE notification stream. It imports only
// the standard library and the generated protocol contracts; adapter
// selection, projection state, and Runtime lifetime stay with the caller.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/MMinasyan/lightcode/protocol"
)

// The client's closed failure classification. ErrNoRuntime means no verified
// usable owner: missing, malformed, insecure, dead, or unauthenticatable
// discovery. ErrIncompatibleProtocol means a correctly authenticated live
// owner that speaks a different wire version. ErrResyncRequired means the
// event stream lost continuity — disconnect, malformed frame, or overflow —
// so the caller must repair its projection from authoritative HTTP reads.
// Neither error authorizes killing or replacing another process.
var (
	ErrNoRuntime            = errors.New("client: no usable runtime owner")
	ErrIncompatibleProtocol = errors.New("client: incompatible runtime protocol version")
	ErrResyncRequired       = errors.New("client: event stream resync required")
)

// discoveryRecord is the private four-field startup-file shape published by
// the Runtime owner: the loopback endpoint, the process-lifetime instance
// identity, the wire protocol version, and the bearer credential. It is a
// connection record, not an HTTP DTO and not ownership authority.
type discoveryRecord struct {
	Endpoint        string `json:"endpoint"`
	InstanceId      string `json:"instance_id"`
	ProtocolVersion string `json:"protocol_version"`
	Credential      string `json:"credential"`
}

// Client is one compatible authenticated transport to a live Runtime: it
// embeds the generated protocol client so every generated operation is
// available directly, and adds only the authenticated instance identity and
// the event stream. The credential lives in the request editor alone; no
// projection cache, Session selection, or work lifetime is owned here.
type Client struct {
	*protocol.ClientWithResponses
	instance string
}

// InstanceID returns the authenticated Runtime instance identity this
// connection belongs to. Revisioned events carry the same identity inside
// their revision; a changed identity means the caller discards its old
// projections before accepting the new owner's reads.
func (c *Client) InstanceID() string { return c.instance }

// Connect reads one strict mode-0600 discovery record, authenticates its
// endpoint's health with the record's credential, and returns one compatible
// transport. The authenticated live owner decides compatibility: its instance
// identity must match the record, its health must carry a nonempty live wire
// version equal to the generated protocol version, and a record whose version
// disagrees with that authenticated live owner is unverifiable discovery.
// Missing, malformed, insecure, dead, unauthenticatable, or
// version-disagreeing discovery — including a health body with no protocol
// version — collapses to ErrNoRuntime; only a correctly authenticated live
// owner whose own version differs reports ErrIncompatibleProtocol. A
// deliberately canceled caller context is preserved as ctx.Err().
func Connect(ctx context.Context, discoveryPath string) (*Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	record, err := readDiscoveryRecord(discoveryPath)
	if err != nil {
		return nil, fmt.Errorf("connect discovery: %v: %w", err, ErrNoRuntime)
	}
	transport, err := protocol.NewClientWithResponses(record.Endpoint,
		protocol.WithHTTPClient(&http.Client{CheckRedirect: noRedirect}),
		protocol.WithRequestEditorFn(bearerEditor(record.Credential)))
	if err != nil {
		return nil, fmt.Errorf("connect transport: %v: %w", err, ErrNoRuntime)
	}
	health, err := transport.GetHealthWithResponse(ctx)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, fmt.Errorf("authenticated health: %v: %w", err, ErrNoRuntime)
	}
	if health.HTTPResponse.StatusCode != http.StatusOK || health.JSON200 == nil {
		return nil, fmt.Errorf("authenticated health is not a live protocol owner: %w", ErrNoRuntime)
	}
	if live := health.JSON200; live.InstanceId != record.InstanceId {
		return nil, fmt.Errorf("health instance %q does not match discovery %q: %w", live.InstanceId, record.InstanceId, ErrNoRuntime)
	}
	if health.JSON200.ProtocolVersion == "" {
		return nil, fmt.Errorf("authenticated health carries no protocol version: %w", ErrNoRuntime)
	}
	if live := health.JSON200; live.ProtocolVersion != protocol.N1 {
		return nil, fmt.Errorf("live protocol version %q: %w", live.ProtocolVersion, ErrIncompatibleProtocol)
	}
	if record.ProtocolVersion != string(protocol.N1) {
		return nil, fmt.Errorf("discovery protocol version %q disagrees with the authenticated live owner: %w", record.ProtocolVersion, ErrNoRuntime)
	}
	return &Client{ClientWithResponses: transport, instance: record.InstanceId}, nil
}

// noRedirect keeps the readiness probe — and every later operation — from
// silently following a transport-level redirect away from the authenticated
// loopback owner.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// bearerEditor installs the record's credential on every request. The value
// stays captured here and is never exposed by the client surface.
func bearerEditor(credential string) protocol.RequestEditorFn {
	return func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", "Bearer "+credential)
		return nil
	}
}

// readDiscoveryRecord reads one regular mode-0600 file carrying exactly one
// JSON document with the exact four fields and a loopback HTTP endpoint with
// no userinfo, path, query, or fragment.
func readDiscoveryRecord(path string) (discoveryRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return discoveryRecord{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return discoveryRecord{}, err
	}
	if !info.Mode().IsRegular() {
		return discoveryRecord{}, fmt.Errorf("discovery record %s is not a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		return discoveryRecord{}, fmt.Errorf("discovery record %s is mode %04o, want 0600", path, info.Mode().Perm())
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return discoveryRecord{}, err
	}
	var record discoveryRecord
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return discoveryRecord{}, fmt.Errorf("decode discovery record: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return discoveryRecord{}, errors.New("discovery record carries more than one document")
	}
	if err := record.validate(); err != nil {
		return discoveryRecord{}, err
	}
	return record, nil
}

// validate checks the private record shape: the loopback endpoint, the
// 32-lowercase-hex instance identity, a nonempty protocol version, and the
// 64-lowercase-hex credential.
func (r discoveryRecord) validate() error {
	if err := validateEndpoint(r.Endpoint); err != nil {
		return err
	}
	if !isLowerHex(r.InstanceId, 32) {
		return errors.New("discovery instance_id is not 32 lowercase hex")
	}
	if r.ProtocolVersion == "" {
		return errors.New("discovery protocol_version is empty")
	}
	if !isLowerHex(r.Credential, 64) {
		return errors.New("discovery credential is not 64 lowercase hex")
	}
	return nil
}

// validateEndpoint accepts exactly the producer's host-facing address:
// http://127.0.0.1:<numeric port> with no userinfo, path, query, or fragment.
func validateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("discovery endpoint: %w", err)
	}
	if parsed.Scheme != "http" {
		return errors.New("discovery endpoint is not http")
	}
	if parsed.Opaque != "" || parsed.User != nil || parsed.Path != "" || parsed.RawPath != "" ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return errors.New("discovery endpoint carries userinfo, path, query, or fragment")
	}
	if parsed.Hostname() != "127.0.0.1" {
		return errors.New("discovery endpoint host is not 127.0.0.1")
	}
	port := parsed.Port()
	if port == "" {
		return errors.New("discovery endpoint has no numeric port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return errors.New("discovery endpoint port is not numeric")
	}
	return nil
}

// isLowerHex reports whether value is exactly size lowercase hex characters.
func isLowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}
