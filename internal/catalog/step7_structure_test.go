package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestStep7DiscoveryMigrationStructure(t *testing.T) {
	files, err := filepath.Glob("*.go")
	testassert.NoError(t, err)
	for _, path := range files {
		if path == "step7_structure_test.go" {
			continue
		}
		data, err := os.ReadFile(path)
		testassert.NoError(t, err)
		source := string(data)
		testassert.Require(t, !strings.Contains(source, "map[string]time.Time"), "%s retains a parallel provider-ID attempt map", path)
		testassert.Require(t, !strings.Contains(source, "map[string]DiscoveredProvider"), "%s retains a parallel provider-ID cache map", path)
	}

	doctorSource := readStep7Source(t, filepath.Join("..", "doctor", "doctor.go"))
	testassert.Require(t, strings.Contains(doctorSource, "catalog.ProviderConnected("), "Doctor no longer retains ProviderConnected")

	refreshSource := readStep7Source(t, "refresh.go")
	testassert.Require(t, strings.Contains(refreshSource, "record.BoundTo(provider.Transport)"), "refresh candidate selection is not transport-bound")
	testassert.Require(t, !strings.Contains(refreshSource, "attempts["), "refresh candidate selection retains provider-ID-only TTL")
}

func readStep7Source(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	testassert.NoError(t, err)
	return string(data)
}
