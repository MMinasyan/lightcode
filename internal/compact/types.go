package compact

import "github.com/MMinasyan/lightcode/internal/provider"

// Config holds parameters for a compaction run.
type Config struct {
	SummarizerClient *provider.Client
	ContextWindow    int
	SummarizerPrompt string
}

// Result is the output of a successful compaction.
type Result struct {
	Summary         string
	SummarizerModel string
}
