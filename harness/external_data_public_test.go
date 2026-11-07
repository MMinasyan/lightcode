package harness_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// ownerHarness opens one cold Harness over the supplied store. Preparation is
// required by construction but never invoked by these owner rows.
func ownerHarness(t *testing.T, store harness.Storage) *harness.Harness {
	t.Helper()
	h, err := harness.New(context.Background(), harness.Dependencies{
		Storage: store,
		Prepare: func(context.Context, harness.PreparationRequest) (harness.PreparedExecution, error) {
			return harness.PreparedExecution{}, errors.New("unused preparation")
		},
	})
	testassert.NoError(t, err)
	return h
}

func storedOwners(t *testing.T, store harness.Storage, sessionID string) []string {
	t.Helper()
	reg, err := store.ReadRegister(context.Background(), harness.RegisterKey{SessionID: sessionID, Kind: harness.RegisterSession})
	testassert.NoError(t, err)
	var wire struct {
		State struct {
			Owners []string `json:"external_data_owners"`
		} `json:"state"`
	}
	testassert.NoError(t, json.Unmarshal(reg.Payload, &wire))
	return wire.State.Owners
}

// TestPublicExternalOwnerPersistenceAcrossColdHarnesses proves ordered owner
// claims survive archive/reopen, returned-record buffer mutation, cold
// rematerialization and committed deletion on both supported stores.
func TestPublicExternalOwnerPersistenceAcrossColdHarnesses(t *testing.T) {
	eachStore(t, func(t *testing.T, store harness.Storage) {
		ctx := context.Background()
		h1 := ownerHarness(t, store)
		created, err := h1.CreateSession(ctx, harness.CreateSessionRequest{Workspace: "/tmp/owners", AgentSelection: publicSelection("coder")})
		testassert.NoError(t, err)
		id := created.Identity.SessionID
		testassert.NoError(t, h1.ClaimExternalData(ctx, id, "owner.a"))
		testassert.NoError(t, h1.ClaimExternalData(ctx, id, "owner.b"))
		archived, err := h1.ArchiveSession(ctx, id)
		testassert.NoError(t, err)
		testassert.Require(t, len(archived.State.ExternalDataOwners) == 2 && archived.State.ExternalDataOwners[0] == "owner.a" && archived.State.ExternalDataOwners[1] == "owner.b", "archived owners = %v, want a,b", archived.State.ExternalDataOwners)
		archived.State.ExternalDataOwners[0] = "mutated"
		reopened, err := h1.ReopenSession(ctx, id)
		testassert.NoError(t, err)
		testassert.Require(t, len(reopened.State.ExternalDataOwners) == 2 && reopened.State.ExternalDataOwners[0] == "owner.a", "reopened owners = %v, want the stored a,b", reopened.State.ExternalDataOwners)
		reopened.State.ExternalDataOwners[1] = "mutated"
		testassert.Require(t, len(storedOwners(t, store, id)) == 2 && storedOwners(t, store, id)[0] == "owner.a" && storedOwners(t, store, id)[1] == "owner.b", "stored owners after returned-record mutation = %v, want a,b", storedOwners(t, store, id))

		h2 := ownerHarness(t, store)
		testassert.Require(t, len(storedOwners(t, store, id)) == 2, "cold owners = %v, want the durable a,b", storedOwners(t, store, id))
		testassert.NoError(t, h2.ClaimExternalData(ctx, id, "owner.c"))
		testassert.Require(t, len(storedOwners(t, store, id)) == 3 && storedOwners(t, store, id)[2] == "owner.c", "owners after cold claim = %v, want a,b,c", storedOwners(t, store, id))
		{
			_, err := h2.ArchiveSession(ctx, id)
			testassert.NoError(t, err)
		}

		h3 := ownerHarness(t, store)
		owners, err := h3.DeleteSession(ctx, id)
		testassert.NoError(t, err)
		testassert.Require(t, len(owners) == 3 && owners[0] == "owner.a" && owners[1] == "owner.b" && owners[2] == "owner.c", "cold delete owners = %v, want the ordered claims", owners)
		_, err = h3.DeleteSession(ctx, id)
		testassert.Require(t, errors.Is(err, harness.ErrNotFound), "repeated cold delete = %v, want not_found", err)
	})
}
