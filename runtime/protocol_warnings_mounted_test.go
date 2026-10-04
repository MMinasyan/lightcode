package runtime

import (
	"context"
	"net/http"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
)

// The mounted warning-lifetime row: the generated Go client drives the real
// loopback listener of one attached protocol server over a real submitted
// Session, proving the archive-retain/delete-remove lifetime crosses the HTTP
// boundary rather than only the in-process store.

// TestMountedGeneratedGoClientWarningLifetime proves the warning lifetime
// contract over the real HTTP boundary with the generated Go client: a real
// Session's prompt warning is visible through GET /warnings, archive retains
// it, and the committed deletion removes it while a sibling Session's warning
// remains.
func TestMountedGeneratedGoClientWarningLifetime(t *testing.T) {
	eachProductionStore(t, func(t *testing.T, e *productionEnv) {
		ctx := context.Background()
		r, err := e.open(ctx, e.hookedHook(), &parkingHook{})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer closeProjectionRuntime(r)
		ps := openProtocolServer(t, r)
		client := protocolClient(t, ps)

		victim, err := r.createSession(ctx, e.workspace("mounted-warn-victim"), "worker")
		if err != nil {
			t.Fatalf("createSession(victim): %v", err)
		}
		sibling, err := r.createSession(ctx, e.workspace("mounted-warn-sibling"), "worker")
		if err != nil {
			t.Fatalf("createSession(sibling): %v", err)
		}
		victimID, siblingID := victim.Identity.SessionID, sibling.Identity.SessionID
		submitThroughRuntime(t, r, victimID, "op-victim", "please write")
		awaitOperation(t, r, victimID, "op-victim", harness.OperationSuccess)
		submitThroughRuntime(t, r, siblingID, "op-sibling", "please write")
		awaitOperation(t, r, siblingID, "op-sibling", harness.OperationSuccess)

		find := func(sessionID string) bool {
			t.Helper()
			response, err := client.GetWarningsWithResponse(ctx)
			if err != nil {
				t.Fatalf("GetWarnings: %v", err)
			}
			if response.JSON200 == nil {
				t.Fatalf("GetWarnings body = %+v, want the typed snapshot", response)
			}
			for _, warning := range response.JSON200.Warnings {
				if warning.Source == "runtime:prompt" && warning.SessionId != nil && *warning.SessionId == sessionID {
					return true
				}
			}
			return false
		}
		if !find(victimID) || !find(siblingID) {
			t.Fatalf("the mounted read misses a real submitted Session's prompt warning")
		}

		archived, err := client.ArchiveSessionWithResponse(ctx, victimID)
		if err != nil || archived.JSON200 == nil {
			t.Fatalf("ArchiveSession = (%+v, %v), want the archived header", archived, err)
		}
		if !find(victimID) {
			t.Fatalf("archive dropped the Session's warning over the mounted read")
		}

		deleted, err := client.DeleteSessionWithResponse(ctx, victimID)
		if err != nil || deleted.HTTPResponse.StatusCode != http.StatusNoContent {
			t.Fatalf("DeleteSession = (%+v, %v), want the 204 deletion", deleted, err)
		}
		if find(victimID) {
			t.Fatalf("the committed deletion kept the Session's warning over the mounted read")
		}
		if !find(siblingID) {
			t.Fatalf("the committed deletion removed the sibling Session's warning")
		}
	})
}
