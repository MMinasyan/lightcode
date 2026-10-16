package catalog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStep7DiscoveryMigrationStructure(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range files {
		if path == "step7_structure_test.go" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		source := string(data)
		if strings.Contains(source, "map[string]time.Time") {
			t.Fatalf("%s retains a parallel provider-ID attempt map", path)
		}
		if strings.Contains(source, "map[string]DiscoveredProvider") {
			t.Fatalf("%s retains a parallel provider-ID cache map", path)
		}
	}

	doctorSource := readStep7Source(t, filepath.Join("..", "doctor", "doctor.go"))
	if !strings.Contains(doctorSource, "catalog.ProviderConnected(") {
		t.Fatal("Doctor no longer retains ProviderConnected")
	}

	refreshSource := readStep7Source(t, "refresh.go")
	if !strings.Contains(refreshSource, "record.BoundTo(provider.Transport)") {
		t.Fatal("refresh candidate selection is not transport-bound")
	}
	if strings.Contains(refreshSource, "attempts[") {
		t.Fatal("refresh candidate selection retains provider-ID-only TTL")
	}
}

func readStep7Source(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
