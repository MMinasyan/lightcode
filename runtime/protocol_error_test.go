package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/extension/worker"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/melta"
	"github.com/MMinasyan/lightcode/protocol"
)

// TestProtocolErrorStatusTable proves every public class crosses the HTTP
// boundary unchanged: the producer's sentinel selects both the generated code
// and its one status.
func TestProtocolErrorStatusTable(t *testing.T) {
	for _, row := range []struct {
		name   string
		err    error
		code   protocol.ErrorCode
		status int
	}{
		{"denied", fmt.Errorf("target: %w", melta.ErrDenied), protocol.ErrorCodeDenied, http.StatusForbidden},
		{"cancelled", fmt.Errorf("wait: %w", context.Canceled), protocol.ErrorCodeCancelled, http.StatusRequestTimeout},
		{"invalid", fmt.Errorf("shape: %w", harness.ErrInvalid), protocol.ErrorCodeInvalid, http.StatusBadRequest},
		{"not found", fmt.Errorf("subject: %w", harness.ErrNotFound), protocol.ErrorCodeNotFound, http.StatusNotFound},
		{"conflict", fmt.Errorf("revision: %w", harness.ErrConflict), protocol.ErrorCodeConflict, http.StatusConflict},
		{"incompatible", fmt.Errorf("contract: %w", harness.ErrIncompatible), protocol.ErrorCodeIncompatible, http.StatusConflict},
		{"closed", fmt.Errorf("owner: %w", melta.ErrClosed), protocol.ErrorCodeClosed, http.StatusGone},
		{"extension closed", fmt.Errorf("binding: %w", extension.ErrClosed), protocol.ErrorCodeClosed, http.StatusGone},
		{"worker denied", fmt.Errorf("worker call: %w", &worker.Error{Class: worker.ClassDenied, Message: "denied by handler"}), protocol.ErrorCodeDenied, http.StatusForbidden},
		{"corrupt", fmt.Errorf("store: %w", harness.ErrCorrupt), protocol.ErrorCodeCorrupt, http.StatusUnprocessableEntity},
		{"configuration", fmt.Errorf("candidate: %w", melta.ErrConfiguration), protocol.ErrorCodeConfiguration, http.StatusUnprocessableEntity},
		{"storage", fmt.Errorf("store: %w", harness.ErrStorage), protocol.ErrorCodeStorage, http.StatusInternalServerError},
		{"internal", errors.New("unclassified"), protocol.ErrorCodeInternal, http.StatusInternalServerError},
	} {
		t.Run(row.name, func(t *testing.T) {
			code, status := classifyProtocolError(row.err)
			testassert.Require(t, code == row.code && status == row.status, "classify = (%s, %d), want (%s, %d)", code, status, row.code, row.status)
		})
	}
}

// TestProtocolRevertPartialResponseStatus proves a produced partial result
// keeps its result body while its error member supplies the mapped status.
func TestProtocolRevertPartialResponseStatus(t *testing.T) {
	handlers := &protocolHandlers{server: &ProtocolServer{}}
	result := protocol.CodeRevertResult{Error: &protocol.Error{Code: protocol.ErrorCodeStorage, Message: "restore failed"}}
	recorder := httptest.NewRecorder()
	handlers.writeCodeRevertResponse(recorder, result, fmt.Errorf("restore: %w", harness.ErrStorage))
	testassert.Equal(t, recorder.Code, http.StatusInternalServerError)
	var body protocol.CodeRevertResult
	testassert.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	testassert.Require(t, body.Error != nil && body.Error.Message == "restore failed", "partial body = %+v, want the produced result", body)
}

// TestProtocolCorruptErrorCarriesSession proves the typed error body retains
// the corrupt Session identity without exposing any private cause.
func TestProtocolCorruptErrorCarriesSession(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeProtocolError(recorder, &harness.CorruptionError{SessionID: "0123456789abcdef0123456789abcdef", Detail: "corrupt rows"})
	var body protocol.Error
	testassert.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	testassert.Require(t, recorder.Code == http.StatusUnprocessableEntity && body.Code == protocol.ErrorCodeCorrupt && body.SessionId != nil && *body.SessionId == "0123456789abcdef0123456789abcdef", "corrupt body = (%d, %+v), want the typed corrupt class and Session identity", recorder.Code, body)
}
