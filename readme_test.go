package main

import (
	"os"
	"strings"
	"testing"

	"github.com/Syntheresis/melta/internal/testassert"
)

func TestREADMEConfigExamplesDoNotUseRemovedModelFields(t *testing.T) {
	data, err := os.ReadFile("README.md")
	testassert.NoError(t, err)
	text := string(data)
	for _, field := range []string{`"default_model"`, `"summarizer_model"`, `"subagents.model"`} {
		testassert.Require(t, !strings.Contains(text, field), "README.md must not advertise removed model ownership field %s", field)
	}
	testassert.Require(t, strings.Contains(text, `"extensions"`) && strings.Contains(text, `"agents"`) && strings.Contains(text, `"definitions"`) && strings.Contains(text, `"primary"`) && strings.Contains(text, `"model": "openrouter/z-ai/glm-5.1"`) && !strings.Contains(text, "agents.json"), "README.md must show the primary model in extensions.agents.definitions")
}
