package compact

import (
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/engine/modelclient"
)

// Config holds parameters for a compaction run.
type Config struct {
	SummarizerClient modelclient.Summarizer
	ContextWindow    int
	SummarizerPrompt string
}

// Result is the output of a successful compaction.
type Result struct {
	Summary         string
	SummarizerModel string
	SummarizerRef   coremodel.ModelRef
}
