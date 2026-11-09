package melta_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestExternalModuleGOWORKOff runs the independent acceptance module with its
// own go.mod and the repository's go.sum, workspace mode disabled and the
// network module proxy off. The module imports only public packages, so Go's
// internal-package rule is the public-only enforcement.
func TestExternalModuleGOWORKOff(t *testing.T) {
	command := exec.Command("go", "test", "./...")
	command.Dir = filepath.Join("testdata", "external")
	command.Env = environWith(os.Environ(), map[string]string{
		"GOWORK":  "off",
		"GOFLAGS": "-mod=mod",
		"GOPROXY": "off",
	})
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("external module acceptance failed: %v\n%s", err, output)
	}
}

func environWith(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	for _, entry := range base {
		name := entry
		if index := strings.IndexByte(entry, '='); index >= 0 {
			name = entry[:index]
		}
		if _, replaced := overrides[name]; replaced {
			continue
		}
		out = append(out, entry)
	}
	for name, value := range overrides {
		out = append(out, name+"="+value)
	}
	return out
}
