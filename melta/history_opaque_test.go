package melta

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/internal/testassert"
	"github.com/Syntheresis/melta/protocol"
)

// TestHistoryRoundTripsOpaqueNumbers proves the Session history projection
// preserves opaque text-part extras exactly: a large integer, a null, and a
// member named instance_id survive typed and raw round-trips unchanged.
func TestHistoryRoundTripsOpaqueNumbers(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		r, e := openProjectionRuntime(t, store)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		caller := testCaller(r)

		workspace := filepath.Join(e.home, "exact")
		created, err := r.CreateSession(ctx, caller, workspace, wireSelection("solo"))
		testassert.Require(t, err == nil, "create: %v", err)
		session := created.SessionId
		var part protocol.ContentPart
		testassert.NoError(t, part.FromTextPart(protocol.TextPart{
			Kind: protocol.TextPartKindText,
			Text: "exact",
			Extra: &protocol.JSONObject{
				"n":           json.RawMessage("9007199254740993"),
				"nullable":    json.RawMessage("null"),
				"instance_id": json.RawMessage(`"opaque-identity"`),
			},
		}))
		submitted, err := r.SubmitSession(ctx, caller, session, protocol.SubmitRequest{
			OperationId: "op-1",
			Mode:        protocol.SubmitRequestModeRegular,
			Content:     []protocol.ContentPart{part},
		})
		testassert.NoError(t, err)
		testassert.Require(t, submitted.Disposition == protocol.SubmitResultDispositionAdmitted, "submit = %+v, want admitted", submitted)
		awaitOperation(t, r, session, "op-1", harness.OperationSuccess)

		history, err := r.GetHistory(ctx, caller, session, nil)
		testassert.Require(t, err == nil, "history: %v", err)
		var input *protocol.InputItem
		for _, item := range history.Items {
			if value, err := item.AsInputItem(); err == nil {
				input = &value
				break
			}
		}
		testassert.Require(t, input != nil && len(input.Content) != 0, "history = %d items with no input, want the submitted input item", len(history.Items))
		text, err := input.Content[0].AsTextPart()
		testassert.NoError(t, err)
		testassert.Require(t, text.Extra != nil, "history part lost its extra object")
		{
			n, ok := (*text.Extra)["n"]
			testassert.Require(t, ok && string(bytes.TrimSpace(n)) == "9007199254740993", "extra n = %s, want the exact integer 9007199254740993", n)
		}
		{
			nullable, ok := (*text.Extra)["nullable"]
			testassert.Require(t, ok && bytes.Equal(bytes.TrimSpace(nullable), []byte("null")), "extra nullable = %s, want the preserved opaque null", nullable)
		}
		{
			opaque, ok := (*text.Extra)["instance_id"]
			testassert.Require(t, ok && string(bytes.TrimSpace(opaque)) == `"opaque-identity"`, "extra instance_id = %s, want the untouched opaque member", opaque)
		}
	})
}
