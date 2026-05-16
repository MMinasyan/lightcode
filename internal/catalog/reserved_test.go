package catalog

import "testing"

func TestReservedKeysMatchesLoopOwnedFields(t *testing.T) {
	want := []string{
		"model",
		"messages",
		"tools",
		"tool_choice",
		"stream",
		"stream_options",
		"max_tokens",
		"max_completion_tokens",
		"n",
	}
	if len(ReservedKeys) != len(want) {
		t.Fatalf("ReservedKeys length = %d, want %d", len(ReservedKeys), len(want))
	}
	for i := range want {
		if ReservedKeys[i] != want[i] {
			t.Fatalf("ReservedKeys[%d] = %q, want %q", i, ReservedKeys[i], want[i])
		}
	}
}

func TestCheckReservedKeysReturnsEmptyForCleanBody(t *testing.T) {
	got := CheckReservedKeys(map[string]any{
		"temperature":      0.2,
		"reasoning_effort": "high",
	})
	if len(got) != 0 {
		t.Fatalf("CheckReservedKeys returned %#v, want empty", got)
	}
}

func TestCheckReservedKeysReturnsReservedKeysInStableOrder(t *testing.T) {
	got := CheckReservedKeys(map[string]any{
		"n":              2,
		"model":          "bad",
		"stream_options": map[string]any{"include_usage": true},
		"temperature":    0.2,
	})
	want := []string{"model", "stream_options", "n"}
	if len(got) != len(want) {
		t.Fatalf("reserved keys = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reserved keys = %#v, want %#v", got, want)
		}
	}
}
