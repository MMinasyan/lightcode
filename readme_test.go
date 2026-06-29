package main

import (
	"os"
	"strings"
	"testing"
)

func TestREADMEConfigExamplesDoNotUseRemovedModelFields(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, field := range []string{`"default_model"`, `"summarizer_model"`, `"subagents.model"`} {
		if strings.Contains(text, field) {
			t.Fatalf("README.md must not advertise removed model ownership field %s", field)
		}
	}
	if !strings.Contains(text, `"primary"`) || !strings.Contains(text, `"model": "openrouter/z-ai/glm-5.1"`) {
		t.Fatal("README.md must show primary.model in agents.json for model selection")
	}
}
