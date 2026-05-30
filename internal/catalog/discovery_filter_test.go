package catalog

import "testing"

func TestDiscoveredModelAllowedClassifier(t *testing.T) {
	tests := []struct {
		name  string
		meta  *discoveryModelMetadata
		allow bool
	}{
		{name: "nil metadata keeps", allow: true},
		{name: "empty metadata keeps", meta: &discoveryModelMetadata{}, allow: true},
		{name: "chat type keeps", meta: &discoveryModelMetadata{Type: "chat"}, allow: true},
		{name: "text generation task keeps", meta: &discoveryModelMetadata{Task: "text_generation"}, allow: true},
		{name: "completion type only keeps ambiguous", meta: &discoveryModelMetadata{Type: "completion"}, allow: true},
		{name: "completion task only keeps ambiguous", meta: &discoveryModelMetadata{Task: "completion"}, allow: true},
		{name: "text output keeps", meta: &discoveryModelMetadata{ArchitectureOutputModalities: []string{"text"}}, allow: true},
		{name: "image input plus text output keeps", meta: &discoveryModelMetadata{
			ArchitectureInputModalities:  []string{"image", "text"},
			ArchitectureOutputModalities: []string{"text"},
		}, allow: true},
		{name: "input only image keeps", meta: &discoveryModelMetadata{InputModalities: []string{"image"}}, allow: true},
		{name: "top level modalities only keeps ambiguous", meta: &discoveryModelMetadata{Modalities: []string{"text", "image"}}, allow: true},
		{name: "supported parameters only keeps", meta: &discoveryModelMetadata{SupportedParameters: []string{"tools"}}, allow: true},
		{name: "image output only drops", meta: &discoveryModelMetadata{ArchitectureOutputModalities: []string{"image"}}, allow: false},
		{name: "speech output only drops", meta: &discoveryModelMetadata{OutputModalities: []string{"speech"}}, allow: false},
		{name: "chat type overrides image output", meta: &discoveryModelMetadata{
			Type:             "chat",
			OutputModalities: []string{"image"},
		}, allow: true},
		{name: "chat capability overrides architecture image output", meta: &discoveryModelMetadata{
			Capabilities:         map[string]bool{"chat_completion": true},
			ArchitectureModality: "text->image",
		}, allow: true},
		{name: "top level modalities do not override image output", meta: &discoveryModelMetadata{
			Modalities:       []string{"text", "image"},
			OutputModalities: []string{"image"},
		}, allow: false},
		{name: "top level modalities do not override architecture output", meta: &discoveryModelMetadata{
			Modalities:           []string{"text", "image"},
			ArchitectureModality: "text->image",
		}, allow: false},
		{name: "top level modalities do not override non text task", meta: &discoveryModelMetadata{
			Modalities: []string{"text", "image"},
			Task:       "image_generation",
		}, allow: false},
		{name: "mixed unknown output keeps", meta: &discoveryModelMetadata{OutputModalities: []string{"custom", "image"}}, allow: true},
		{name: "text to image drops", meta: &discoveryModelMetadata{ArchitectureModality: "text->image"}, allow: false},
		{name: "text to text keeps", meta: &discoveryModelMetadata{ArchitectureModality: "text->text"}, allow: true},
		{name: "bare image type keeps ambiguous", meta: &discoveryModelMetadata{Type: "image", InputModalities: []string{"text", "image"}}, allow: true},
		{name: "bare audio task keeps ambiguous", meta: &discoveryModelMetadata{Task: "audio"}, allow: true},
		{name: "bare speech type keeps ambiguous", meta: &discoveryModelMetadata{Type: "speech"}, allow: true},
		{name: "embedding type drops", meta: &discoveryModelMetadata{Type: "embedding"}, allow: false},
		{name: "negative plus unknown type task keeps", meta: &discoveryModelMetadata{Type: "embedding", Task: "model"}, allow: true},
		{name: "negative plus positive keeps", meta: &discoveryModelMetadata{Type: "embedding", OutputModalities: []string{"text"}}, allow: true},
		{name: "chat capability keeps", meta: &discoveryModelMetadata{Capabilities: map[string]bool{"chat_completion": true}}, allow: true},
		{name: "completion only keeps ambiguous", meta: &discoveryModelMetadata{Capabilities: map[string]bool{"completion": true}}, allow: true},
		{name: "completion with chat false drops", meta: &discoveryModelMetadata{Capabilities: map[string]bool{"completion": true, "chat_completion": false}}, allow: false},
		{name: "completion with chat false and unknown true keeps", meta: &discoveryModelMetadata{Capabilities: map[string]bool{"completion": true, "chat_completion": false, "custom": true}}, allow: true},
		{name: "embeddings only drops", meta: &discoveryModelMetadata{Capabilities: map[string]bool{"embeddings": true}}, allow: false},
		{name: "completion plus embeddings keeps ambiguous", meta: &discoveryModelMetadata{Capabilities: map[string]bool{"completion": true, "embeddings": true}}, allow: true},
		{name: "vision only keeps", meta: &discoveryModelMetadata{Capabilities: map[string]bool{"vision": true}}, allow: true},
		{name: "vision plus embeddings keeps ambiguous", meta: &discoveryModelMetadata{Capabilities: map[string]bool{"vision": true, "embeddings": true}}, allow: true},
		{name: "audio capability only keeps ambiguous", meta: &discoveryModelMetadata{Capabilities: map[string]bool{"audio": true}}, allow: true},
		{name: "false chat flag is not positive", meta: &discoveryModelMetadata{Capabilities: map[string]bool{"chat_completion": false}}, allow: true},
		{name: "unknown capability prevents drop", meta: &discoveryModelMetadata{Capabilities: map[string]bool{"embeddings": true, "custom": true}}, allow: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := discoveredModelAllowed(DiscoveredModel{metadata: tt.meta})
			if got != tt.allow {
				t.Fatalf("discoveredModelAllowed = %v, want %v", got, tt.allow)
			}
		})
	}
}
