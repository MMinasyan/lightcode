package runtime_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/plugins/builtin"
	"github.com/MMinasyan/lightcode/runtime"
)

// jobsComposedDoc wraps one plugins section in the shared composed provider
// document, the same split the tools publication matrix uses: direct decoder
// tests live in the plugin package, and the composed half drives the real
// configuration service here. Config writes go through the package's existing
// writeToolsConfig helper (identical to the internal-package writeServiceFile,
// which a runtime_test file importing the plugins cannot reference without an
// import cycle).
func jobsComposedDoc(plugins string) string {
	return strings.TrimSuffix(composedConfigDocument, "}") + `,"plugins":` + plugins + `}`
}

// TestComposedJobsPluginPublication composes the shipped five-plugin
// registration through the OpenForTest bridge and proves the plugins.jobs
// publication matrix: the five compiled sections publish together with a
// well-formed jobs object (unknown keys ignored, a null member retaining its
// default), a consumed-range violation, a wrong consumed-field type, and a
// null section each fail publication with the jobs decoder's text through the
// object-only boundary, and a failed publication leaves the live revision
// untouched so the next success continues the generation sequence.
func TestComposedJobsPluginPublication(t *testing.T) {
	ctx := context.Background()
	const fiveSections = `"sqlite":{},"tools":{},"adaptation":{},"lsp":{}`
	accept := jobsComposedDoc(`{` + fiveSections + `,"jobs":{"max_background_processes":5,"unknown_member":1,"max_output_bytes":null}}`)
	violation := jobsComposedDoc(`{` + fiveSections + `,"jobs":{"max_output_bytes":1023}}`)
	badType := jobsComposedDoc(`{` + fiveSections + `,"jobs":{"max_output_bytes":"many"}}`)
	nullSection := jobsComposedDoc(`{` + fiveSections + `,"jobs":null}`)

	e := newComposeEnv(t)
	writeToolsConfig(t, e.configPath, accept)
	r, err := runtime.OpenForTest(ctx, e.dataDir, e.configPath, builtin.Plugins())
	if err != nil {
		t.Fatalf("OpenForTest over the shipped five with a valid jobs section: %v", err)
	}
	if rev, err := r.Reload(ctx); err != nil || rev != "2" {
		_ = r.Close(ctx)
		t.Fatalf("first Reload = (%q, %v), want revision 2", rev, err)
	}

	writeToolsConfig(t, e.configPath, violation)
	if _, err := r.Reload(ctx); err == nil || !errors.Is(err, runtime.ErrConfiguration) {
		_ = r.Close(ctx)
		t.Fatalf("Reload with an out-of-range jobs limit = %v, want a complete publication failure", err)
	} else if !strings.Contains(err.Error(), "jobs: max_output_bytes must be between 1024 and 1048576") {
		_ = r.Close(ctx)
		t.Fatalf("Reload range failure error = %v, want the jobs range text preserved", err)
	}

	writeToolsConfig(t, e.configPath, badType)
	if _, err := r.Reload(ctx); err == nil || !errors.Is(err, runtime.ErrConfiguration) {
		_ = r.Close(ctx)
		t.Fatalf("Reload with a wrong-typed jobs field = %v, want a complete publication failure", err)
	} else if !strings.Contains(err.Error(), "jobs: invalid settings:") {
		_ = r.Close(ctx)
		t.Fatalf("Reload type-failure error = %v, want the jobs invalid-settings wrap preserved", err)
	}

	writeToolsConfig(t, e.configPath, nullSection)
	if _, err := r.Reload(ctx); err == nil || !errors.Is(err, runtime.ErrConfiguration) {
		_ = r.Close(ctx)
		t.Fatalf("Reload with a null jobs section = %v, want the publisher's non-object rejection", err)
	}

	writeToolsConfig(t, e.configPath, accept)
	rev, err := r.Reload(ctx)
	if err != nil || rev != "3" {
		_ = r.Close(ctx)
		t.Fatalf("Reload after the failures = (%q, %v), want generation 3: failed publications published nothing", rev, err)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
}
