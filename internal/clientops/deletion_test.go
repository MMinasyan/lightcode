package clientops_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/MMinasyan/lightcode/internal/clientops"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

func TestDecodeDeletionResultAcceptsClosedShape(t *testing.T) {
	for _, row := range []struct {
		name string
		body string
		want []protocol.CleanupFailure
	}{
		{"empty", `{"cleanup_failures":[]}`, []protocol.CleanupFailure{}},
		{"local", `{"cleanup_failures":[{"code":"storage","message":"remove tree: denied"}]}`, []protocol.CleanupFailure{{Code: protocol.CleanupFailureCodeStorage, Message: "remove tree: denied"}}},
		{"external", `{"cleanup_failures":[{"extension_id":"guest","code":"unavailable","message":"extension data cleanup is unavailable"}]}`, []protocol.CleanupFailure{{ExtensionId: pointer("guest"), Code: protocol.CleanupFailureCodeUnavailable, Message: "extension data cleanup is unavailable"}}},
		{"empty-message", `{"cleanup_failures":[{"code":"storage","message":""}]}`, []protocol.CleanupFailure{{Code: protocol.CleanupFailureCodeStorage, Message: ""}}},
		{"mixed", `{"cleanup_failures":[{"code":"storage","message":"local"},{"extension_id":"guest","code":"unavailable","message":"external"}]}`, []protocol.CleanupFailure{{Code: protocol.CleanupFailureCodeStorage, Message: "local"}, {ExtensionId: pointer("guest"), Code: protocol.CleanupFailureCodeUnavailable, Message: "external"}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			result, err := clientops.DecodeDeletionResult([]byte(row.body))
			testassert.NoError(t, err)
			testassert.Equal(t, len(result.CleanupFailures), len(row.want))
			for i, want := range row.want {
				got := result.CleanupFailures[i]
				testassert.Require(t, got.Code == want.Code && got.Message == want.Message, "failure[%d] = %+v, want %+v", i, got, want)
				if want.ExtensionId == nil {
					testassert.Require(t, got.ExtensionId == nil, "failure[%d] owner = %v, want local", i, got.ExtensionId)
				} else {
					testassert.Require(t, got.ExtensionId != nil && *got.ExtensionId == *want.ExtensionId, "failure[%d] owner = %v, want %q", i, got.ExtensionId, *want.ExtensionId)
				}
			}
		})
	}
}

func TestDecodeDeletionResultRejectsUnusableShape(t *testing.T) {
	for _, row := range []struct {
		name string
		body string
	}{
		{"empty body", ``},
		{"not json", `not json`},
		{"null", `null`},
		{"array root", `[]`},
		{"string root", `"x"`},
		{"missing failures", `{}`},
		{"null failures", `{"cleanup_failures":null}`},
		{"wrong failures type", `{"cleanup_failures":{}}`},
		{"number failures", `{"cleanup_failures":5}`},
		{"extra root member", `{"cleanup_failures":[],"extra":true}`},
		{"case alias root", `{"Cleanup_Failures":[]}`},
		{"failure not object", `{"cleanup_failures":[null]}`},
		{"failure array", `{"cleanup_failures":[[]]}`},
		{"failure string", `{"cleanup_failures":["x"]}`},
		{"missing code", `{"cleanup_failures":[{"message":"x"}]}`},
		{"null code", `{"cleanup_failures":[{"code":null,"message":"x"}]}`},
		{"numeric code", `{"cleanup_failures":[{"code":1,"message":"x"}]}`},
		{"unknown code", `{"cleanup_failures":[{"code":"teapot","message":"x"}]}`},
		{"case code", `{"cleanup_failures":[{"code":"Storage","message":"x"}]}`},
		{"missing message", `{"cleanup_failures":[{"code":"storage"}]}`},
		{"null message", `{"cleanup_failures":[{"code":"storage","message":null}]}`},
		{"numeric message", `{"cleanup_failures":[{"code":"storage","message":1}]}`},
		{"unknown member", `{"cleanup_failures":[{"code":"storage","message":"x","path":"/tmp"}]}`},
		{"case member", `{"cleanup_failures":[{"Code":"storage","Message":"x"}]}`},
		{"null owner", `{"cleanup_failures":[{"extension_id":null,"code":"unavailable","message":"x"}]}`},
		{"empty owner", `{"cleanup_failures":[{"extension_id":"","code":"unavailable","message":"x"}]}`},
		{"numeric owner", `{"cleanup_failures":[{"extension_id":5,"code":"unavailable","message":"x"}]}`},
		{"storage with owner", `{"cleanup_failures":[{"extension_id":"guest","code":"storage","message":"x"}]}`},
		{"unavailable without owner", `{"cleanup_failures":[{"code":"unavailable","message":"x"}]}`},
		{"trailing data", `{"cleanup_failures":[]}{}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := clientops.DecodeDeletionResult([]byte(row.body)); err == nil {
				t.Fatalf("DecodeDeletionResult accepted unusable body %s", row.body)
			}
		})
	}
}

// TestInvokeValidatesDeleteSessionSuccess proves DeleteSession's usable 200
// body is accepted while a malformed body retains its status/body as invalid
// delivery, never as a Runtime rejection.
func TestInvokeValidatesDeleteSessionSuccess(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		c, doer := newScriptFixture(t)
		body := `{"cleanup_failures":[{"extension_id":"guest","code":"unavailable","message":"extension data cleanup is unavailable"}]}`
		doer.respond = func(*http.Request) (*http.Response, error) {
			return scriptedResponse(http.StatusOK, body), nil
		}
		status, raw, err := clientops.Invoke(context.Background(), c, "DeleteSession", json.RawMessage(`{"session_id":"s1"}`))
		testassert.NoError(t, err)
		testassert.Require(t, status == http.StatusOK && string(raw) == body, "Invoke = (%d, %q), want the accepted 200 body", status, raw)
	})
	for _, row := range []struct {
		name string
		body string
	}{
		{"missing failures", `{"cleanup_failures":null}`},
		{"wrong code", `{"cleanup_failures":[{"code":"teapot","message":"x"}]}`},
		{"bad pair", `{"cleanup_failures":[{"code":"storage","extension_id":"guest","message":"x"}]}`},
	} {
		t.Run(row.name, func(t *testing.T) {
			c, doer := newScriptFixture(t)
			doer.respond = func(*http.Request) (*http.Response, error) {
				return scriptedResponse(http.StatusOK, row.body), nil
			}
			status, raw, err := clientops.Invoke(context.Background(), c, "DeleteSession", json.RawMessage(`{"session_id":"s1"}`))
			testassert.Require(t, status == http.StatusOK && string(raw) == row.body, "Invoke = (%d, %q), want the received status/body retained", status, raw)
			testassert.Require(t, errors.Is(err, clientops.ErrInvalidResponse), "Invoke error = %v, want ErrInvalidResponse", err)
			var responseErr *clientops.ResponseError
			testassert.Require(t, !errors.As(err, &responseErr), "invalid success became a ResponseError: %v", err)
		})
	}
}

func pointer[T any](value T) *T { return &value }
