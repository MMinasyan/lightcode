package provider

import (
	"fmt"
	"testing"

	openai "github.com/sashabaranov/go-openai"
)

func BenchmarkEnsureToolCallIndexes(b *testing.B) {
	calls := make([]openai.ToolCall, 40)
	for i := range calls {
		calls[i] = openai.ToolCall{
			ID:   fmt.Sprintf("call_%d", i),
			Type: openai.ToolTypeFunction,
			Function: openai.FunctionCall{
				Name:      "read_file",
				Arguments: fmt.Sprintf(`{"path":"file_%d.go"}`, i),
			},
		}
		if i%4 == 0 {
			idx := i
			calls[i].Index = &idx
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchCalls := make([]openai.ToolCall, len(calls))
		copy(benchCalls, calls)
		ensureToolCallIndexes(benchCalls)
		if benchCalls[len(benchCalls)-1].Index == nil {
			b.Fatal("last tool call index was not filled")
		}
	}
}
