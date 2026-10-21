package catalog

import (
	"encoding/json"
	"testing"

	"github.com/MMinasyan/lightcode/internal/testassert"
)

func FuzzDeepMergeCatalog(f *testing.F) {
	seedPairs := [][2]map[string]any{
		{
			{
				"transport": map[string]any{
					"base_url":    "https://openrouter.ai/api/v1",
					"api_key_env": "OPENROUTER_API_KEY",
					"headers": map[string]any{
						"OpenAI-Beta": "assistants=v2",
					},
				},
				"models": map[string]any{
					"gpt-5.4-mini": map[string]any{
						"input_modalities": []any{"text", "image"},
						"extra_body":       map[string]any{"reasoning_effort": "medium"},
					},
				},
			},
			{
				"transport": map[string]any{
					"headers": map[string]any{"HTTP-Referer": "https://lightcode.local"},
				},
				"models": map[string]any{
					"gpt-5.4-mini": map[string]any{
						"input_modalities": []any{"text"},
						"extra_body": map[string]any{
							"reasoning_effort": "high",
							"thinking":         map[string]any{"budget_tokens": float64(4000)},
						},
					},
				},
			},
		},
		{
			{"transport": map[string]any{"headers": map[string]any{"A": "b"}}},
			{"transport": map[string]any{"headers": map[string]any{"C": "d"}}},
		},
		{
			{"extra_body": map[string]any{"keep": "bundled", "nullable": "bundled-value"}},
			{"extra_body": map[string]any{"nullable": nil}},
		},
		{
			{"models": []any{"bundled"}},
			{"models": []any{"user"}},
		},
		{
			{},
			{},
		},
	}
	for _, pair := range seedPairs {
		bundled, err := json.Marshal(pair[0])
		testassert.Require(f, err == nil, "marshal bundled seed: %v", err)
		user, err := json.Marshal(pair[1])
		testassert.Require(f, err == nil, "marshal user seed: %v", err)
		f.Add(bundled, user)
	}
	for _, invalid := range []struct {
		bundled []byte
		user    []byte
	}{
		{[]byte(`{not-json}`), []byte(`{}`)},
		{[]byte(`[]`), []byte(`{}`)},
		{[]byte(`null`), []byte(`{}`)},
	} {
		f.Add(invalid.bundled, invalid.user)
	}

	f.Fuzz(func(t *testing.T, bundledRaw, userRaw []byte) {
		var bundled map[string]any
		if err := json.Unmarshal(bundledRaw, &bundled); err != nil || bundled == nil {
			return
		}
		var user map[string]any
		if err := json.Unmarshal(userRaw, &user); err != nil || user == nil {
			return
		}

		merged := DeepMergeCatalog(bundled, user)
		testassert.Require(t, merged != nil, "DeepMergeCatalog returned nil")
		{
			_, err := json.Marshal(merged)
			testassert.NoError(t, err)
		}
	})
}
