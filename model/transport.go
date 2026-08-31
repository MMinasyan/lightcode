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

// ErrInvalidInput wraps Stream's encoding failure exactly when the invalid value is a resolved-transport field or a logical-request value: one typed identity whose returned error also preserves every underlying validation sentinel through its unwrap chain, with the offending field and detail in its text. Reserved-key failures keep their own dedicated shape (ErrReservedKeys), as do extra-body value and runtime-extra failures — neither gains this classification.
var ErrInvalidInput = errors.New("invalid resolved or request input")

// invalidRequestValueIdentities is the closed set of validation sentinels Stream classifies under ErrInvalidInput: every logical-request identity NewRequest can return (messages scope, tools scope) plus the two resolved-value re-validations Encode performs on each call. Deliberately excluded are ErrReservedKeys and all extra-body value failures — their own shapes stay unclassified at this boundary.
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

// classifyEncodeError wraps one encoding failure under ErrInvalidInput exactly when its unwrap chain carries a logical-request or resolved-value validation sentinel: double %w keeps both sentinels reachable through errors.Is on the single returned value while retaining NewRequest's field-position prefix and every validator detail in the text. Every other shape — reserved keys, extra-body values, runtime extras, marshal boundaries — passes through unchanged so each keeps exactly its own classification surface.
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

// NewTransport builds a fixed streaming chat transport from one immutable resolved input, deep-copying every map and slice it carries so later caller mutations cannot reach the retained values. It performs no network or filesystem I/O. Construction fails with a typed invalid-input error naming the offending field when the target model identity is not complete (zero or partial) or the wire system role falls outside the closed set; everything else — reserved extras, malformed extra bytes, request validation — is enforced at the Stream trust boundary through Encode on every invocation.
func NewTransport(in ResolvedTransport) (*Transport, error) {
	resolved := cloneResolvedInput(in) // own every resolved value before retaining it.

	if !resolved.Model.complete() { // a transport without a complete target identity cannot encode any request body.
		return nil, fmt.Errorf("%w: resolved transport field Model is %s; a complete provider/model pair is required", ErrInvalidModelRef, describeRef(resolved.Model))
	}
	if _, err := resolveWireSystemRole(resolved.WireSystemRole); err != nil { // same closed set the encoder enforces per call.
		return nil, fmt.Errorf("resolved transport field WireSystemRole: %w", err)
	}

	return &Transport{
		resolved: resolved,
		client:   &http.Client{}, // zero-value client retains standard redirect handling and no retry behavior of its own.
	}, nil
}

// Stream opens one physical streaming chat completion attempt for the given logical request and per-call runtime extra layer over this transport's immutable resolved input. Before a response stream is returned, failures are transport-level: invalid resolved or request values return typed errors with field and detail (re-validated here at this trust boundary through Encode), reserved extras return the ErrReservedKeys error identifying every present key, request construction/marshal/network failures wrap their standard-library cause, non-2xx responses return an HTTPStatusError carrying status code, status text, and provider message — joined with ErrAuthFailed on 401/403. Warnings are returned whenever encoding succeeded even if the attempt later fails; a 2xx response transfers body ownership to the returned stream and establishes accepted model work from that moment onward. The request uses ctx for its lifetime: cancelling it unblocks an in-progress body read, but this transport never turns any failure into another physical request — retry policy is Harness-owned.
func (t *Transport) Stream(ctx context.Context, req Request, runtimeExtras map[string]json.RawMessage) (Stream, []ProtocolWarning, error) {
	body, warnings, err := Encode(t.resolved, req, runtimeExtras) // deep-copies resolved input, request, and the per-call layer on entry; reserved keys rejected before any value parsing.
	if err != nil {
		return nil, nil, classifyEncodeError(err) // encoding failed: no physical attempt happens and nothing to warn about yet; logical-request/resolved-value failures gain their typed identity here while every other shape passes through unchanged.
	}

	endpoint := ChatEndpoint(t.resolved)
	httpReq, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if reqErr != nil {
		return nil, warnings, fmt.Errorf("build chat completions request for %s: %w", endpoint, reqErr) // encoding already succeeded, so its warnings still go to the caller.
	}
	for key, value := range BuildHeaders(t.resolved) { // one header helper shared with encoder tests; resolved headers overwrite matching defaults inside it. Exact case is retained on the wire request.
		httpReq.Header[key] = []string{value}
	}

	chunkPath := t.dumpRequest(body)    // retained wire diagnostics: "" when disabled or any write fails — never alters results below.
	resp, doErr := t.client.Do(httpReq) // exactly one physical attempt per invocation; no retry loop exists anywhere in this type.
	if doErr != nil {
		return nil, warnings, fmt.Errorf("post chat completions request to %s: %w", endpoint, doErr) // wraps the standard-library cause (including a canceled context).
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		statusErr := httpStatusError(resp) // reads the error body through its retained limit before it is closed.
		resp.Body.Close()
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
