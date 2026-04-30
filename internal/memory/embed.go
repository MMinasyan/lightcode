package memory

import (
	"context"
	_ "embed"
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

type Embedder struct {
	mu       sync.Mutex
	session  *hugot.Session
	pipeline *pipelines.FeatureExtractionPipeline
	modelDir string
}

func NewEmbedder() (*Embedder, error) {
	dir, err := os.MkdirTemp("", "lightcode-model-*")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "model.onnx"), modelOnnx, 0644); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "tokenizer.json"), modelTokenizer, 0644); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), modelConfig, 0644); err != nil {
		os.RemoveAll(dir)
		return nil, err
	}

	session, err := hugot.NewGoSession(context.Background())
	if err != nil {
		os.RemoveAll(dir)
		return nil, err
	}

	config := hugot.FeatureExtractionConfig{
		ModelPath:    dir,
		Name:         "bge-small",
		OnnxFilename: "model.onnx",
		Options: []hugot.FeatureExtractionOption{
			pipelines.WithNormalization(),
		},
	}
	pipeline, err := hugot.NewPipeline(session, config)
	if err != nil {
		session.Destroy()
		os.RemoveAll(dir)
		return nil, err
	}

	return &Embedder{
		session:  session,
		pipeline: pipeline,
		modelDir: dir,
	}, nil
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

func (e *Embedder) Close() {
	if e.session != nil {
		e.session.Destroy()
	}
	if e.modelDir != "" {
		os.RemoveAll(e.modelDir)
	}
}
