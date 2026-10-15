package main

import (
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	wailsasset "github.com/wailsapp/wails/v2/pkg/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

// assetRefPattern collects the local asset URLs one served page references.
var assetRefPattern = regexp.MustCompile(`(?:src|href)="([^"]+)"`)

// TestProductionAssetsServe is the executable build/serve regression for the
// shipped frontend: it serves main's actual embedded assets and the preview
// directory's emitted filesystem through Wails' public asset handler, and
// requests the root page plus every asset URL that page references directly
// through the handler. Every referenced URL must answer 200 with nonempty
// content, the bundle must carry the target client invocation and no legacy
// owner bindings or events, and an absent asset must stay a missing file.
// The assets come from the real default Wails build; a missing build is a
// clear failure, never a skipped oracle.
func TestProductionAssetsServe(t *testing.T) {
	previewDir, err := filepath.Abs(filepath.Join("frontend", "dist"))
	if err != nil {
		t.Fatalf("resolve preview directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(previewDir, "index.html")); err != nil {
		t.Fatalf("production frontend assets are not built (run the default Wails build first): %v", err)
	}
	for _, tc := range []struct {
		name   string
		assets fs.FS
	}{
		{name: "embedded", assets: assets},
		{name: "preview directory", assets: os.DirFS(previewDir)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := wailsasset.NewAssetHandler(assetserver.Options{Assets: tc.assets}, nil)
			if err != nil {
				t.Fatalf("NewAssetHandler: %v", err)
			}

			page := serveDirect(t, handler, "/")
			if !strings.Contains(page, "<script") {
				t.Fatalf("root page = %q, want the client bundle reference", page)
			}
			refs := assetRefPattern.FindAllStringSubmatch(page, -1)
			if len(refs) == 0 {
				t.Fatal("the served page references no local asset")
			}
			// The built page emits local root-relative references; each is
			// requested exactly as written.
			var sawScript bool
			for _, match := range refs {
				ref := match[1]
				body := serveDirect(t, handler, ref)
				if len(body) == 0 {
					t.Fatalf("GET %s served an empty asset", ref)
				}
				if strings.HasSuffix(ref, ".js") {
					sawScript = true
					// The bundle must carry the actual invocation module: its
					// invalid-delivery literal survives minification.
					if !strings.Contains(body, "received an unusable response") {
						t.Fatalf("GET %s served %d bytes without the client invocation marker", ref, len(body))
					}
					if strings.Contains(body, "ApplyTurnAction") || strings.Contains(body, "background_process_complete") {
						t.Fatalf("GET %s carries legacy owner bindings or events", ref)
					}
				}
			}
			if !sawScript {
				t.Fatalf("the served page references no JavaScript bundle: %v", refs)
			}

			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/no-such-asset.js", nil))
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("GET /no-such-asset.js = %d, want 404", recorder.Code)
			}
		})
	}
}

// serveDirect performs one direct handler GET and returns the body of an
// exact 200 response.
func serveDirect(t *testing.T, handler http.Handler, target string) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, target, nil))
	response := recorder.Result()
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET %s = %s, want 200", target, response.Status)
	}
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}
	return string(data)
}
