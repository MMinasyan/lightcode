package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrAuthFailed is joined with the HTTP status error on 401/403 responses so callers can classify authentication failures without parsing status codes. Retry classification of any kind belongs to later Harness work, not this value or its errors.
var ErrAuthFailed = errors.New("authentication failed")

// ErrInvalidInput wraps invalid resolved or request values at the transport's two owning boundaries with one typed identity: NewTransport classifies malformed resolved extra-body values, and Stream classifies every logical-request validation failure through NewRequest — including malformed message/content/tool extras — preserving any underlying specific validation sentinel in the same unwrap chain, with the offending field and detail in the text. Reserved-key failures keep their own dedicated shape (ReservedKeyError), and per-call runtime-extra failures keep no classification at all.
var ErrInvalidInput = errors.New("invalid resolved or request input")

// invalidRequestValueIdentities is the closed set of validation sentinels classifyEncodeError matches inside Encode's error path: every logical-request identity NewRequest can return (messages scope, tools scope) plus the two resolved-value re-validations Encode performs on each call. With Stream pre-validating its request and NewTransport pre-validating the resolved layers, Encode's path can only fail this way if a validation rule stops being idempotent — the set keeps those defensive failures classified identically. Deliberately excluded are ErrReservedKeys and all extra-body value failures — their own shapes stay unclassified on Encode's path.
var invalidRequestValueIdentities = []error{ // order is diagnostic only; matching short-circuits on first hit because every listed identity maps to the same wrapping shape.
	ErrInvalidRole,
	ErrMissingSource,
	ErrUnexpectedSource,
	ErrForbiddenField,
	ErrMissingField,
	ErrDuplicateToolName,
	ErrInvalidParameters,
	ErrInvalidModelRef,       // NewTransport gates this on Stream's path; listed so Encode's defensive per-call re-validation classifies identically if ever reached.
	ErrInvalidWireSystemRole, // same reasoning as above for the wire system role closed set.
}

// classifyEncodeError wraps one Encode failure under ErrInvalidInput exactly when its unwrap chain carries a logical-request or resolved-value validation sentinel: double %w keeps both sentinels reachable through errors.Is on the single returned value while retaining NewRequest's field-position prefix and every validator detail in the text. Every other shape — reserved keys, extra-body values, runtime extras, marshal boundaries — passes through unchanged so each keeps exactly its own classification surface.
func classifyEncodeError(err error) error {
	for _, identity := range invalidRequestValueIdentities {
		if errors.Is(err, identity) {
			return fmt.Errorf("%w: %w", ErrInvalidInput, err) // umbrella first in the rendered text (class before detail), original second so its sentinel and positional context survive intact.
		}
	}
	return err
}

// Transport is one fixed OpenAI-compatible streaming chat transport over a single standard-library HTTP client and an immutable deep copy of the resolved input it was built from. It owns no retry policy: one physical attempt per Stream invocation, never more (standard redirect handling inside that one call is retained but is not a retry), and no alternate client or RoundTripper seam exists — tests use the resolved base URL with httptest.
type Transport struct {
	resolved ResolvedTransport // deep-copied at construction; read-only afterwards.
	client   *http.Client      // single standard-library HTTP client owned by this transport.
}

// NewTransport builds a fixed streaming chat transport from one immutable resolved input, deep-copying every map and slice it carries so later caller mutations cannot reach the retained values. It performs no network or filesystem I/O. Construction fails with a typed invalid-input error naming the offending field when the target model identity is not complete (zero or partial) or the wire system role falls outside the closed set; the two resolved extra-body layers are validated here too — reserved-key presence rejects with the ReservedKeyError naming every present key (checked before any value parsing), and malformed non-reserved values reject as typed ErrInvalidInput with their layer and field. Request-level validation belongs to the Stream trust boundary.
func NewTransport(in ResolvedTransport) (*Transport, error) {
	resolved := cloneResolvedInput(in) // own every resolved value before retaining it.

	if !resolved.Model.complete() { // a transport without a complete target identity cannot encode any request body.
		return nil, classifyEncodeError(fmt.Errorf("%w: resolved transport field Model is %s; a complete provider/model pair is required", ErrInvalidModelRef, describeRef(resolved.Model)))
	}
	if _, err := resolveWireSystemRole(resolved.WireSystemRole); err != nil { // same closed set the encoder enforces per call.
		return nil, classifyEncodeError(fmt.Errorf("resolved transport field WireSystemRole: %w", err))
	}

	var reserved []string // reserved-key pass over both resolved layers runs before any value parsing so a malformed value can never hide a reservation.
	for _, layer := range []Extra{resolved.ProviderExtraBody, resolved.ModelExtraBody} {
		reserved = collectReservedKeys(reserved, layer)
	}
	if len(reserved) > 0 {
		return nil, &ReservedKeyError{Keys: reserved} // same shape and precedence as the per-call pass inside Encode.
	}
	if err := validateExtraValues(resolved.ProviderExtraBody); err != nil { // deterministic layer order: provider is reported first when both layers are malformed.
		return nil, fmt.Errorf("%w: provider extra body: %w", ErrInvalidInput, err)
	}
	if err := validateExtraValues(resolved.ModelExtraBody); err != nil {
		return nil, fmt.Errorf("%w: model extra body: %w", ErrInvalidInput, err)
	}

	return &Transport{
		resolved: resolved,
		client:   &http.Client{}, // zero-value client retains standard redirect handling and no retry behavior of its own.
	}, nil
}

// Stream opens one physical streaming chat completion attempt for the given logical request and per-call runtime extra layer over this transport's immutable resolved input. Before a response stream is returned, failures are transport-level: the logical request is validated and owned through NewRequest here, with every failure returning the typed ErrInvalidInput identity (preserving each underlying validation sentinel) plus positional field/detail; resolved values were already validated at construction; reserved extras return the ReservedKeyError identifying every present key; request construction/marshal/network failures wrap their standard-library cause; non-2xx responses return an HTTPStatusError carrying status code, status text, and provider message — joined with ErrAuthFailed on 401/403. Per-call runtime extras keep no classification of their own. Warnings are returned whenever encoding succeeded even if the attempt later fails; a 2xx response transfers body ownership to the returned stream and establishes accepted model work from that moment onward. The request uses ctx for its lifetime: cancelling it unblocks an in-progress body read, but this transport never turns any failure into another physical request — retry policy is Harness-owned.
func (t *Transport) Stream(ctx context.Context, req Request, runtimeExtras map[string]json.RawMessage) (Stream, []ProtocolWarning, error) {
	request, err := NewRequest(req) // validate and own the logical request at this trust boundary: sentinel-carrying failures keep their specific identity, malformed message/content/tool extras carry none — both classify under the umbrella with NewRequest's positional detail intact.
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %w", ErrInvalidInput, err)
	}

	body, warnings, err := Encode(t.resolved, request, runtimeExtras) // reserved keys rejected inside before any value parsing; runtime extras stay unclassified and the validated request passes through its re-validation unchanged.
	if err != nil {
		return nil, nil, classifyEncodeError(err) // defensive for sentinel-carrying shapes only; reserved/runtime/marshal pass through unchanged.
	}

	endpoint := ChatEndpoint(t.resolved)
	chunkPath := t.dumpRequest(body) // retained wire diagnostics: attempted immediately after encoding succeeds and BEFORE request construction (matching the retained legacy producer's ordering), so a request that fails to construct still leaves its dump behind; "" when disabled or any write fails — never alters results below.
	httpReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if reqErr != nil {
		return nil, warnings, fmt.Errorf("build chat completions request for %s: %w", endpoint, reqErr) // encoding already succeeded, so its warnings still go to the caller.
	}
	for key, value := range BuildHeaders(t.resolved) { // one header helper shared with encoder tests; resolved headers overwrite matching defaults inside it. Exact case is retained on the wire request.
		httpReq.Header[key] = []string{value}
	}

	resp, doErr := t.client.Do(httpReq) // exactly one physical attempt per invocation; no retry loop exists anywhere in this type.
	if doErr != nil {
		return nil, warnings, fmt.Errorf("post chat completions request to %s: %w", endpoint, doErr) // wraps the standard-library cause (including a canceled context).
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		statusErr := httpStatusError(resp) // reads the error body through its retained limit before it is closed.
		_ = resp.Body.Close()              // Cleanup cannot replace the status error.
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
			return nil, warnings, errors.Join(ErrAuthFailed, statusErr) // auth failures keep every transport fact plus the sentinel identity.
		}
		return nil, warnings, statusErr
	}

	return newStream(resp.Body, chunkPath), warnings, nil // 2xx transfers body ownership to the public stream; accepted model work begins now.
}

// HTTPStatusError carries one non-2xx response from a chat endpoint as transport facts only: numeric code, wire status text, and extracted provider message. It has no retryability method — every retry-classification decision remains later Harness work over these same facts.
type HTTPStatusError struct {
	StatusCode int    // raw HTTP status code of the failed attempt.
	StatusText string // response.Status line as received (e.g. "429 Too Many Requests").
	Message    string // provider message extracted from the error body, or its trimmed raw form when no known field carried it; empty for an unreadable/empty body.
}

func (e *HTTPStatusError) Error() string { // retained legacy formatting verbatim: "API error <code> <text>" with ": <message>" appended only when present.
	if e.Message != "" {
		return fmt.Sprintf("API error %d %s: %s", e.StatusCode, e.StatusText, e.Message)
	}
	return fmt.Sprintf("API error %d %s", e.StatusCode, e.StatusText)
}

// httpStatusError builds the status error for one failed response by extracting its provider message through the retained rule: the body is read with a 1 MiB limit (bytes beyond it are ignored), then "error.message" wins over top-level "message"; when neither parses, the trimmed raw body text becomes the message.
func httpStatusError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // read failures leave an empty message rather than hiding the status itself.
	message := ""
	if len(body) > 0 {
		message = strings.TrimSpace(string(body)) // retained current behavior: unstructured bodies are stored trimmed as-is.
		var raw struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(body, &raw) == nil { // a body that is neither shape keeps its trimmed text above.
			if raw.Error.Message != "" {
				message = raw.Error.Message
			} else if raw.Message != "" {
				message = raw.Message
			}
		}
	}
	return &HTTPStatusError{StatusCode: resp.StatusCode, StatusText: resp.Status, Message: message}
}

// dumpRequest writes the encoded request body (without headers) to a timestamped file in the resolved wire-debug directory and returns the matching chunks artifact path for this exchange. It is retained legacy producer behavior adapted to this boundary: empty debug dir disables diagnostics entirely, every open/write/close failure is ignored so it can never alter transport or stream results, and no framework or sink abstraction exists around these two artifacts.
func (t *Transport) dumpRequest(body []byte) string {
	dir := t.resolved.WireDebugDir // resolved input carries the directory; this package performs no environment lookup of its own.
	if dir == "" {
		return ""
	}
	id := time.Now().UTC().Format("20060102-150405.000")
	reqPath := filepath.Join(dir, id+"-req.json")
	chunkPath := filepath.Join(dir, id+"-chunks.jsonl")
	if err := os.WriteFile(reqPath, body, 0o600); err != nil { // a failed request dump disables the whole pair for this exchange.
		return ""
	}
	return chunkPath
}

// appendDebugChunk appends one successfully decoded raw SSE payload to its chunks artifact as one JSONL line; empty path or payload is a no-op and every open/write/close failure is ignored so diagnostics can never alter stream results.
func appendDebugChunk(chunkPath string, raw json.RawMessage) { // retained legacy producer behavior verbatim at this boundary.
	if chunkPath == "" || len(raw) == 0 {
		return
	}
	f, err := os.OpenFile(chunkPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close() //nolint:errcheck // diagnostic write failures are ignored by design.
	_, _ = f.Write(raw)
	_, _ = f.Write([]byte{'\n'})
}
