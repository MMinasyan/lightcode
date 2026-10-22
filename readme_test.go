package main

import (
	"os"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestREADMEConfigExamplesDoNotUseRemovedModelFields(t *testing.T) {
	data, err := os.ReadFile("README.md")
	testassert.NoError(t, err)
	text := string(data)
	for _, field := range []string{`"default_model"`, `"summarizer_model"`, `"subagents.model"`} {
		testassert.Require(t, !strings.Contains(text, field), "README.md must not advertise removed model ownership field %s", field)
	}
	testassert.Require(t, strings.Contains(text, `"primary"`) && strings.Contains(text, `"model": "openrouter/z-ai/glm-5.1"`), "README.md must show primary.model in agents.json for model selection")
}
