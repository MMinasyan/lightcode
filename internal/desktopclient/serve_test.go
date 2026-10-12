package desktopclient

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	wailsasset "github.com/wailsapp/wails/v2/pkg/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

// TestPreviewAssetsServe is the executable build/serve regression: it runs
// the actual client build, serves the resulting dist-client directory
// through Wails' public asset handler over the real os.DirFS, and requests
// the root page plus every asset URL that page actually references directly
// through the handler. Every referenced URL must answer 200 with nonempty
// correct content, so the preview entry provably serves its own emitted
// bundle. The toolchain environment check follows the mounted-test
// convention: skip only for an absent installed Vite entry or an npm that is
// not on PATH; with the toolchain present, any build or serve error fails.

var assetRefPattern = regexp.MustCompile(`(?:src|href)="([^"]+)"`)

func TestPreviewAssetsServe(t *testing.T) {
	frontendDir, err := filepath.Abs(filepath.Join("..", "..", "frontend"))
	if err != nil {
		t.Fatalf("resolve frontend directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(frontendDir, "node_modules", "vite", "bin", "vite.js")); err != nil {
		t.Skipf("frontend toolchain is not installed: %v", err)
	}
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skipf("npm is not on PATH: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	build := exec.CommandContext(ctx, "npm", "run", "build:client")
	build.Dir = frontendDir
	build.WaitDelay = 15 * time.Second
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("npm run build:client: %v\n%s", err, output)
	}

	handler, err := wailsasset.NewAssetHandler(assetserver.Options{
		Assets: os.DirFS(filepath.Join(frontendDir, "dist-client")),
	}, nil)
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
	// The built page emits local root-relative references; each is requested
	// exactly as written.
	var sawScript bool
	for _, match := range refs {
		ref := match[1]
		body := serveDirect(t, handler, ref)
		if len(body) == 0 {
			t.Fatalf("GET %s served an empty asset", ref)
		}
		if strings.HasSuffix(ref, ".js") {
			sawScript = true
			// The bundle must carry the actual transport module: the
			// placeholder origin string survives minification.
			if !strings.Contains(body, "runtime.invalid") {
				t.Fatalf("GET %s served %d bytes without the client transport marker", ref, len(body))
			}
		}
	}
	if !sawScript {
		t.Fatalf("the served page references no JavaScript bundle: %v", refs)
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
