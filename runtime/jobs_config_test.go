package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

// jobsComposedDoc appends top-level baseline roots to the shared composed
// provider document, the same split the tools publication matrix uses: direct
// decoder tests live in the jobs package, and the composed half drives the
// real configuration service here. Config writes go through the package's
// existing writeToolsConfig helper.
func jobsComposedDoc(roots string) string {
	return strings.TrimSuffix(composedConfigDocument, "}") + `,` + roots + `}`
}

// TestComposedJobsSettingsPublication proves the top-level jobs publication
// matrix over the directly constructed baseline: the baseline sections publish
// together with a well-formed jobs object (unknown keys ignored, a null
// member retaining its default), a consumed-range violation, a wrong
// consumed-field type, and a null section each fail publication with the jobs
// decoder's text through the object-only boundary, and a failed publication
// leaves the live revision untouched so the next success continues the
// generation sequence.
func TestComposedJobsSettingsPublication(t *testing.T) {
	ctx := context.Background()
	accept := jobsComposedDoc(`"tools":{},"jobs":{"max_background_processes":5,"unknown_member":1,"max_output_bytes":null}`)
	violation := jobsComposedDoc(`"tools":{},"jobs":{"max_output_bytes":1023}`)
	badType := jobsComposedDoc(`"tools":{},"jobs":{"max_output_bytes":"many"}`)
	nullSection := jobsComposedDoc(`"tools":{},"jobs":null`)

	e := newComposeEnv(t)
	writeToolsConfig(t, e.configPath, accept)
	r, err := Open(ctx, Options{DataDir: e.dataDir, ConfigPath: e.configPath})
	testassert.NoError(t, err)
	if rev, err := r.Reload(ctx); err != nil || rev != "2" {
		_ = r.Close(ctx)
		t.Fatalf("first Reload = (%q, %v), want revision 2", rev, err)
	}

	writeToolsConfig(t, e.configPath, violation)
	if _, err := r.Reload(ctx); err == nil || !errors.Is(err, ErrConfiguration) {
		_ = r.Close(ctx)
		t.Fatalf("Reload with an out-of-range jobs limit = %v, want a complete publication failure", err)
	} else if !strings.Contains(err.Error(), "jobs: max_output_bytes must be between 1024 and 1048576") {
		_ = r.Close(ctx)
		t.Fatalf("Reload range failure error = %v, want the jobs range text preserved", err)
	}

	writeToolsConfig(t, e.configPath, badType)
	if _, err := r.Reload(ctx); err == nil || !errors.Is(err, ErrConfiguration) {
		_ = r.Close(ctx)
		t.Fatalf("Reload with a wrong-typed jobs field = %v, want a complete publication failure", err)
	} else if !strings.Contains(err.Error(), "jobs: invalid settings:") {
		_ = r.Close(ctx)
		t.Fatalf("Reload type-failure error = %v, want the jobs invalid-settings wrap preserved", err)
	}

	writeToolsConfig(t, e.configPath, nullSection)
	if _, err := r.Reload(ctx); err == nil || !errors.Is(err, ErrConfiguration) {
		_ = r.Close(ctx)
		t.Fatalf("Reload with a null jobs section = %v, want the publisher's non-object rejection", err)
	}

	writeToolsConfig(t, e.configPath, accept)
	rev, err := r.Reload(ctx)
	if err != nil || rev != "3" {
		_ = r.Close(ctx)
		t.Fatalf("Reload after the failures = (%q, %v), want generation 3: failed publications published nothing", rev, err)
	}
	testassert.NoError(t, r.Close(ctx))
}
