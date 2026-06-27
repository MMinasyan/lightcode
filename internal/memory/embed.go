package memory

import (
	"context"
	_ "embed"
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
	mu        sync.Mutex
	closeOnce sync.Once
	session   *hugot.Session
	pipeline  *pipelines.FeatureExtractionPipeline
	modelDir  string
}

var (
	newGoSession                 = hugot.NewGoSession
	newFeatureExtractionPipeline = func(session *hugot.Session, config hugot.FeatureExtractionConfig) (*pipelines.FeatureExtractionPipeline, error) {
		return hugot.NewPipeline(session, config)
	}
)

func NewEmbedder(home string) (*Embedder, error) {
	dir, err := ensureEmbeddedModelDir(home)
	if err != nil {
		return nil, err
	}

	session, err := newGoSession(context.Background())
	if err != nil {
		return nil, err
	}
	config := hugot.FeatureExtractionConfig{
		ModelPath:    dir,
		Name:         embeddedModelName,
		OnnxFilename: "model.onnx",
		Options: []hugot.FeatureExtractionOption{
			pipelines.WithNormalization(),
		},
	}
	pipeline, err := newFeatureExtractionPipeline(session, config)
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

func (e *Embedder) Embed(text string) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pipeline == nil {
		return nil, errEmbedderUnavailable
	}
	result, err := e.pipeline.RunPipeline(context.Background(), []string{text})
	if err != nil {
		return nil, err
	}
	return result.Embeddings[0], nil
}

func (e *Embedder) Close() {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.session != nil {
			_ = e.session.Destroy()
			e.session = nil
			e.pipeline = nil
		}
	})
}
