package memory

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/knights-analytics/hugot"
	"github.com/knights-analytics/hugot/pipelines"
)

//go:embed model/model.onnx
var modelOnnx []byte

//go:embed model/tokenizer.json
var modelTokenizer []byte

//go:embed model/config.json
var modelConfig []byte

const (
	modelName    = "bge-small"
	modelVersion = "bge-small-v1"
)

// requiredCacheFiles are the files that must be present (along with the
// .version marker) for an extracted model cache directory to be considered
// complete and reusable across launches.
var requiredCacheFiles = []string{"model.onnx", "tokenizer.json", "config.json"}

type Embedder struct {
	mu        sync.Mutex
	closeOnce sync.Once
	session   *hugot.Session
	pipeline  *pipelines.FeatureExtractionPipeline
	modelDir  string
}

// NewEmbedder constructs an Embedder, extracting the embedded ONNX model
// bundle into a persistent, versioned cache under home/.lightcode/cache/models
// if a complete cache does not already exist. The cache is reused across
// launches; it is never written to /tmp.
func NewEmbedder(home string) (*Embedder, error) {
	dir, err := ensureEmbeddedModelDir(home)
	if err != nil {
		return nil, fmt.Errorf("ensure embedded model dir: %w", err)
	}

	session, err := hugot.NewGoSession(context.Background())
	if err != nil {
		return nil, err
	}

	config := hugot.FeatureExtractionConfig{
		ModelPath:    dir,
		Name:         modelName,
		OnnxFilename: "model.onnx",
		Options: []hugot.FeatureExtractionOption{
			pipelines.WithNormalization(),
		},
	}
	pipeline, err := hugot.NewPipeline(session, config)
	if err != nil {
		_ = session.Destroy()
		return nil, err
	}

	return &Embedder{
		session:  session,
		pipeline: pipeline,
		modelDir: dir,
	}, nil
}

// ensureEmbeddedModelDir returns the path to a complete, versioned model
// cache under home/.lightcode/cache/models/<modelVersion>. If a complete
// cache already exists it is returned as-is. Otherwise the bundle is
// extracted into a temporary sibling directory and atomically renamed into
// place so a crash mid-extraction cannot leave a partial cache behind.
func ensureEmbeddedModelDir(home string) (string, error) {
	modelsRoot := filepath.Join(home, ".lightcode", "cache", "models")
	modelDir := filepath.Join(modelsRoot, modelVersion)

	if embeddedModelCacheComplete(modelDir) {
		return modelDir, nil
	}

	if err := os.MkdirAll(modelsRoot, 0700); err != nil {
		return "", err
	}

	tmpDir, err := os.MkdirTemp(modelsRoot, "."+modelVersion+"-*")
	if err != nil {
		return "", err
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	if err := os.WriteFile(filepath.Join(tmpDir, "model.onnx"), modelOnnx, 0600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "tokenizer.json"), modelTokenizer, 0600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "config.json"), modelConfig, 0600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(tmpDir, ".version"), []byte(modelVersion), 0600); err != nil {
		return "", err
	}

	// Remove any incomplete existing target so the rename can succeed.
	_ = os.RemoveAll(modelDir)

	if err := os.Rename(tmpDir, modelDir); err != nil {
		return "", err
	}
	cleanup = false
	return modelDir, nil
}

// embeddedModelCacheComplete reports whether dir contains every required
// cache file and a .version marker whose contents match modelVersion.
func embeddedModelCacheComplete(dir string) bool {
	versionPath := filepath.Join(dir, ".version")
	data, err := os.ReadFile(versionPath)
	if err != nil {
		return false
	}
	if string(data) != modelVersion {
		return false
	}
	for _, name := range requiredCacheFiles {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

func (e *Embedder) Embed(text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	result, err := e.pipeline.RunPipeline(context.Background(), []string{text})
	if err != nil {
		return nil, err
	}
	return result.Embeddings[0], nil
}

// Close releases the Hugot session. It is idempotent and does not delete
// the model cache directory — that directory is shared, persistent state.
func (e *Embedder) Close() {
	e.closeOnce.Do(func() {
		if e.session != nil {
			_ = e.session.Destroy()
		}
	})
}
