package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
	wailsasset "github.com/wailsapp/wails/v2/pkg/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

// assetRefPattern collects the local asset URLs one served page references.
var assetRefPattern = regexp.MustCompile(`(?:src|href)="([^"]+)"`)

// TestProductionAssetsServe is the executable build/serve regression for the
// shipped frontend: it serves main's actual embedded assets through Wails'
// public asset handler, and requests the root page plus every asset URL that
// page references directly through the handler. Every referenced URL must
// answer 200 with nonempty content, the bundle must carry the target client
// invocation and no legacy owner bindings or events, and an absent asset must
// stay a missing file. The assets come from the real default Wails build; a
// missing build is a clear failure, never a skipped oracle.
func TestProductionAssetsServe(t *testing.T) {
	handler, err := wailsasset.NewAssetHandler(assetserver.Options{Assets: assets}, nil)
	testassert.NoError(t, err)

	page := serveDirect(t, handler, "/")
	testassert.Require(t, strings.Contains(page, "<script"), "root page = %q, want the client bundle reference", page)
	refs := assetRefPattern.FindAllStringSubmatch(page, -1)
	testassert.Require(t, len(refs) != 0, "the served page references no local asset")
	// The built page emits local root-relative references; each is
	// requested exactly as written.
	var sawScript bool
	for _, match := range refs {
		ref := match[1]
		body := serveDirect(t, handler, ref)
		testassert.Require(t, len(body) != 0, "GET %s served an empty asset", ref)
		if strings.HasSuffix(ref, ".js") {
			sawScript = true
			// The bundle must carry the actual invocation module: its
			// invalid-delivery literal survives minification.
			testassert.Require(t, strings.Contains(body, "received an unusable response"), "GET %s served %d bytes without the client invocation marker", ref, len(body))
			testassert.Require(t, !strings.Contains(body, "ApplyTurnAction") && !strings.Contains(body, "background_process_complete"), "GET %s carries legacy owner bindings or events", ref)
		}
	}
	testassert.Require(t, sawScript, "the served page references no JavaScript bundle: %v", refs)

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/no-such-asset.js", nil))
	testassert.Equal(t, recorder.Code, http.StatusNotFound)
}

// serveDirect performs one direct handler GET and returns the body of an
// exact 200 response.
func serveDirect(t *testing.T, handler http.Handler, target string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	response := recorder.Result()
	defer response.Body.Close()
	testassert.Require(t, response.StatusCode == http.StatusOK, "GET %s = %s, want 200", target, response.Status)
	data, err := io.ReadAll(response.Body)
	testassert.Require(t, err == nil, "read %s: %v", target, err)
	return string(data)
}
