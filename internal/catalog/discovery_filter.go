package catalog

import (
	"sort"
	"strings"
)

func discoveredModelAllowed(model DiscoveredModel) bool {
	meta := model.metadata
	if discoveryMetadataEmpty(meta) {
		return true
	}
	if hasExplicitTextOutput(meta) {
		return true
	}
	if hasPositiveDiscoveryEvidence(meta) {
		return true
	}
	if hasExplicitNonTextOutputOnly(meta) {
		return false
	}
	if hasNegativeTypeTask(meta.Type, meta.Task) || hasNegativeCapabilities(meta.Capabilities) {
		return false
	}
	return true
}

func hasExplicitTextOutput(meta *discoveryModelMetadata) bool {
	return hasTextOutput(meta.OutputModalities) ||
		hasTextOutput(meta.ArchitectureOutputModalities) ||
		hasPositiveArchitectureModality(meta.ArchitectureModality)
}

func hasExplicitNonTextOutputOnly(meta *discoveryModelMetadata) bool {
	return hasNonTextOutputOnly(meta.OutputModalities) ||
		hasNonTextOutputOnly(meta.ArchitectureOutputModalities) ||
		hasNegativeArchitectureModality(meta.ArchitectureModality)
}

func hasPositiveDiscoveryEvidence(meta *discoveryModelMetadata) bool {
	if positiveTypeTask(meta.Type, meta.Task) {
		return true
	}
	for capability, enabled := range meta.Capabilities {
		if enabled && isChatCapability(capability) {
			return true
		}
	}
	return false
}

func positiveTypeTask(values ...string) bool {
	for _, value := range values {
		if isPositiveDiscoveryCategory(value) {
			return true
		}
	}
	return false
}

func hasNegativeTypeTask(values ...string) bool {
	hasNegative := false
	for _, value := range values {
		if value == "" {
			continue
		}
		if isPositiveDiscoveryCategory(value) {
			return false
		}
		if isNegativeDiscoveryCategory(value) {
			hasNegative = true
			continue
		}
		return false
	}
	return hasNegative
}

func hasTextOutput(values []string) bool {
	for _, value := range values {
		if value == "text" {
			return true
		}
	}
	return false
}

func hasNonTextOutputOnly(values []string) bool {
	if len(values) == 0 {
		return false
	}
	for _, value := range values {
		if value == "text" {
			return false
		}
		if !isKnownNonTextOutput(value) {
			return false
		}
	}
	return true
}

func hasPositiveArchitectureModality(value string) bool {
	outputs, ok := architectureModalityOutputTokens(value)
	return ok && hasTextOutput(outputs)
}

func hasNegativeArchitectureModality(value string) bool {
	outputs, ok := architectureModalityOutputTokens(value)
	return ok && hasNonTextOutputOnly(outputs)
}

func architectureModalityOutputTokens(value string) ([]string, bool) {
	if value == "" {
		return nil, false
	}
	_, output, ok := strings.Cut(value, "->")
	if !ok || strings.TrimSpace(output) == "" {
		return nil, false
	}
	return normalizeDiscoveryTokens(splitDiscoveryComposite(output)), true
}

func hasNegativeCapabilities(capabilities map[string]bool) bool {
	if len(capabilities) == 0 {
		return false
	}
	completionTrue := false
	explicitChatFalse := false
	hasNonChatTrue := false
	hasUnknownTrue := false
	hasAmbiguousTrue := false
	for capability, enabled := range capabilities {
		if !enabled {
			if isChatCapability(capability) {
				explicitChatFalse = true
			}
			continue
		}
		if isChatCapability(capability) {
			return false
		}
		switch {
		case capability == "completion":
			completionTrue = true
			hasAmbiguousTrue = true
		case isNeutralCapability(capability):
			hasAmbiguousTrue = true
		case isNonChatCapability(capability):
			hasNonChatTrue = true
		default:
			hasUnknownTrue = true
		}
	}
	if hasUnknownTrue {
		return false
	}
	if completionTrue && explicitChatFalse {
		return true
	}
	if hasAmbiguousTrue {
		return false
	}
	return hasNonChatTrue
}

func normalizeDiscoveryToken(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	replacer := strings.NewReplacer("-", "_", " ", "_", ".", "_", "/", "_")
	value = replacer.Replace(value)
	for strings.Contains(value, "__") {
		value = strings.ReplaceAll(value, "__", "_")
	}
	return strings.Trim(value, "_")
}

func normalizeDiscoveryTokens(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		normalized := normalizeDiscoveryToken(value)
		if normalized == "" {
			continue
		}
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		out = append(out, normalized)
	}
	sort.Strings(out)
	return out
}

func normalizeDiscoveryModality(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "→", "->")
	value = strings.ReplaceAll(value, "=>", "->")
	value = strings.ReplaceAll(value, " ", "")
	return value
}

func splitDiscoveryComposite(value string) []string {
	return strings.FieldsFunc(value, func(r rune) bool {
		switch r {
		case '+', ',', '|', '&', '/', ' ', '\t', '\n', '\r':
			return true
		default:
			return false
		}
	})
}

func isPositiveDiscoveryCategory(value string) bool {
	switch value {
	case "chat", "chat_completion", "chat_completions", "completion_chat",
		"text", "text_generation", "text_to_text", "language", "llm",
		"conversational", "conversation", "instruct":
		return true
	default:
		return false
	}
}

func isNegativeDiscoveryCategory(value string) bool {
	switch value {
	case "embedding", "embeddings", "image_generation", "text_to_image",
		"tts", "text_to_speech", "transcription",
		"speech_to_text", "moderation", "safety", "rerank", "reranker",
		"ranking", "classification", "classifier", "fine_tune",
		"fine_tuning", "training", "completion_fim":
		return true
	default:
		return false
	}
}

func isKnownNonTextOutput(value string) bool {
	switch value {
	case "image", "audio", "speech", "video", "document", "embedding", "embeddings":
		return true
	default:
		return false
	}
}

func isChatCapability(value string) bool {
	switch value {
	case "chat", "chat_completion", "chat_completions", "completion_chat",
		"conversation", "conversational":
		return true
	default:
		return false
	}
}

func isNonChatCapability(value string) bool {
	switch value {
	case "embedding", "embeddings", "completion_fim", "fine_tune",
		"fine_tuning", "training", "image_generation", "text_to_image",
		"tts", "text_to_speech", "transcription",
		"speech_to_text", "moderation", "safety", "rerank", "reranker",
		"ranking", "classification", "classifier":
		return true
	default:
		return false
	}
}

func isNeutralCapability(value string) bool {
	switch value {
	case "completion", "vision", "function_calling", "tool_calling", "tools",
		"structured_output", "structured_outputs", "json_mode":
		return true
	default:
		return false
	}
}
