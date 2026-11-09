package melta

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

// TestUpdateSettingsBaselineSectionsRequireObjects proves the producer's
// object-when-present rule reaches embedded callers too: every non-object
// tools/jobs/compaction member is the shared invalid class before any
// candidate work, and the published generation is unchanged.
func TestUpdateSettingsBaselineSectionsRequireObjects(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, _ := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		generation := r.config.current().generation
		for _, member := range []string{"tools", "jobs", "compaction"} {
			for _, wrong := range []struct {
				name string
				raw  json.RawMessage
			}{
				{"null", json.RawMessage("null")},
				{"array", json.RawMessage("[]")},
				{"number", json.RawMessage("7")},
				{"boolean", json.RawMessage("true")},
				{"string", json.RawMessage(`"value"`)},
			} {
				t.Run(member+" "+wrong.name, func(t *testing.T) {
					settings := protocol.Settings{}
					switch member {
					case "tools":
						settings.Tools = wrong.raw
					case "jobs":
						settings.Jobs = wrong.raw
					case "compaction":
						settings.Compaction = wrong.raw
					}
					_, err := r.UpdateSettings(ctx, testCaller(r), protocol.UpdateSettingsRequest{Settings: &settings})
					testassert.Require(t, errors.Is(err, harness.ErrInvalid), "%s = %v, want the shared invalid class", member, err)
					testassert.Equal(t, r.config.current().generation, generation)
				})
			}
		}
	})
}
