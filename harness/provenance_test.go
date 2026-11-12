package harness

import (
	"encoding/json"
	"testing"

	"github.com/Syntheresis/melta/internal/testassert"
	"github.com/Syntheresis/melta/model"
)

func provenanceCapture() ExecutionCapture {
	return ExecutionCapture{
		ConfigurationRevision: "1",
		Model:                 model.ModelRef{Provider: "p", Model: "m"},
		ContextWindow:         4096,
		OutputReserve:         1024,
		SystemPrompt:          "system",
		Compact:               CompactCapture{Model: model.ModelRef{Provider: "p", Model: "m"}, SystemPrompt: "summarize"},
		Strategy:              StrategyCapture{ID: IncludedStrategyID, RequiresContextWindow: true},
		Implementations: []ImplementationCapture{
			{ExtensionID: "agents", Version: "1", Settings: json.RawMessage(`{"enabled":true,"value":9}`)},
			{ExtensionID: "adaptation", Version: "2"},
		},
	}
}

func TestExecutionCaptureImplementationProvenanceRoundTrip(t *testing.T) {
	raw, err := encodeExecutionCapture(provenanceCapture())
	testassert.NoError(t, err)
	obj, err := decodePayloadObject(raw)
	testassert.NoError(t, err)
	got, err := decodeExecutionCapture(obj)
	testassert.NoError(t, err)
	testassert.Require(t, len(got.Implementations) == 2, "implementations = %+v, want two roots", got.Implementations)
	testassert.Equal(t, got.Implementations[0].ExtensionID, "agents")
	testassert.Equal(t, got.Implementations[0].Version, "1")
	testassert.Equal(t, string(got.Implementations[0].Settings), `{"enabled":true,"value":9}`)
	testassert.Equal(t, got.Implementations[1].ExtensionID, "adaptation")
}

func TestExecutionCaptureImplementationProvenanceRejectsMalformed(t *testing.T) {
	for _, row := range []struct {
		name   string
		mutate func(*ExecutionCapture)
		raw    json.RawMessage
	}{
		{name: "empty identity", mutate: func(c *ExecutionCapture) {
			c.Implementations = []ImplementationCapture{{ExtensionID: "", Version: "1"}}
		}},
		{name: "duplicate", mutate: func(c *ExecutionCapture) {
			c.Implementations = []ImplementationCapture{{ExtensionID: "agents", Version: "1"}, {ExtensionID: "agents", Version: "1"}}
		}},
		{name: "invalid settings", mutate: func(c *ExecutionCapture) {
			c.Implementations = []ImplementationCapture{{ExtensionID: "agents", Version: "1", Settings: json.RawMessage(`{not json`)}}
		}},
	} {
		t.Run(row.name, func(t *testing.T) {
			capture := provenanceCapture()
			row.mutate(&capture)
			_, err := encodeExecutionCapture(capture)
			testassert.Require(t, err != nil, "encode accepted an invalid provenance value")
		})
	}
	// An undeclared member in the decoded array is rejected rather than
	// silently ignored.
	capture := provenanceCapture()
	raw, err := encodeExecutionCapture(capture)
	testassert.NoError(t, err)
	var obj map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(raw, &obj))
	obj["implementations"] = json.RawMessage(`[{"extension_id":"agents","version":"1","unknown":true}]`)
	_, err = decodeExecutionCapture(obj)
	testassert.Require(t, err != nil, "decode accepted an unknown implementation member")
}
