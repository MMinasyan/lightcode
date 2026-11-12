package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/melta"
	"github.com/MMinasyan/lightcode/protocol"
)

// This file owns the one transport boundary of the mounted protocol server:
// request decoding, request-shape validation, response writing, error
// classification, and response writing. Everything reuses the
// generated protocol types' own JSON tags, pointer/omitempty shape, and
// generated enum validators; no handwritten request DTO or validation
// dependency exists, and no credential, request body, or header value is ever
// rendered into an error or a log.

// unauthorizedMessage is the transport's one authentication refusal text: the
// credential never rides the message, and no presented header value is
// echoed. HTTP authentication is a transport gate, not a command-producer
// class.
const unauthorizedMessage = "missing or invalid bearer credential"

// decodeRequestBody reads one uncapped request body as exactly one JSON
// document and decodes it into the generated request type through the shared
// generated-value decoder: exact numbers, unknown-member rejection, one
// document, required-member presence, non-null declared members, closed
// generated enums, and the one request union. Every failure wraps the shared
// invalid-input sentinel.
func decodeRequestBody(r *http.Request, dst any) error {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return invalidRequest("read request body: %v", err)
	}
	if err := protocol.DecodeValue(data, dst); err != nil {
		return invalidRequest("%v", err)
	}
	return nil
}

// decodeOneDocument is the lower strict one-document entry retained for
// readiness: it checks one complete document with unknown-member rejection but
// applies no generated presence or enum rules. Readiness explicitly compares
// the decoded protocol version and reports an incompatible owner itself.
func decodeOneDocument(data []byte, dst any) error {
	return protocol.DecodeDocument(data, dst)
}

// invalidRequest wraps one transport-shape rejection in the shared invalid
// input sentinel, so the error mapper classifies every decode and validation
// failure as one class.
func invalidRequest(format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), harness.ErrInvalid)
}

// protocolErrorStatus is the one public class-to-status table.
func protocolErrorStatus(code protocol.ErrorCode) int {
	switch code {
	case protocol.ErrorCodeInvalid:
		return http.StatusBadRequest
	case protocol.ErrorCodeUnauthorized:
		return http.StatusUnauthorized
	case protocol.ErrorCodeDenied:
		return http.StatusForbidden
	case protocol.ErrorCodeNotFound:
		return http.StatusNotFound
	case protocol.ErrorCodeConflict, protocol.ErrorCodeIncompatible:
		return http.StatusConflict
	case protocol.ErrorCodeClosed:
		return http.StatusGone
	case protocol.ErrorCodeCancelled:
		return http.StatusRequestTimeout
	case protocol.ErrorCodeConfiguration, protocol.ErrorCodeCorrupt:
		return http.StatusUnprocessableEntity
	case protocol.ErrorCodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// classifyProtocolError maps one error to its schema error code and HTTP
// status through the one public class table: classification has already
// happened at the producer, and no transport case reinterprets a private
// error.
func classifyProtocolError(err error) (protocol.ErrorCode, int) {
	code := melta.ErrorCode(err)
	return code, protocolErrorStatus(code)
}

// writeProtocolAuthError writes the transport gate's one authentication
// refusal. It is not a command-producer class, so it does not pass through the
// public-class mapper.
func writeProtocolAuthError(w http.ResponseWriter) {
	body, _ := json.Marshal(protocol.Error{Code: protocol.ErrorCodeUnauthorized, Message: unauthorizedMessage}) // plain string members; the marshal cannot fail
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write(body)
}

// writeProtocolError writes one schema-defined typed error body. The
// message is the error's own text — never a credential, header value, or
// request body — and a corrupt Session's typed identity rides the optional
// session_id member.
func writeProtocolError(w http.ResponseWriter, err error) {
	code, status := classifyProtocolError(err)
	body := protocol.Error{Code: code, Message: err.Error()}
	var corrupt *harness.CorruptionError
	if errors.As(err, &corrupt) && corrupt.SessionID != "" {
		session := corrupt.SessionID
		body.SessionId = &session
	}
	data, _ := json.Marshal(body) // plain string members; the marshal cannot fail
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// writeJSON writes one owner-built response body with its status; revision
// instance identities are the owner's own values and are forwarded unchanged.
func (rt *protocolHandlers) writeJSON(w http.ResponseWriter, status int, body any) {
	data, err := json.Marshal(body)
	if err != nil {
		writeProtocolError(w, fmt.Errorf("marshal response: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
