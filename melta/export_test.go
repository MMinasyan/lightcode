package melta

import (
	"context"
	"encoding/json"

	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/internal/catalog"
	"github.com/Syntheresis/melta/protocol"
)

type ToolConstraints = harness.ToolConstraints

func wireSelection(name string) *protocol.AgentSelection {
	value := projectAgentSelection(harness.AgentSelection{Definition: &harness.DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: name}})
	return &value
}

// ConfiguredInvocationForTest builds one configured Invocation whose
// captured snapshot carries the given top-level baseline sections and
// per-plugin sections. It is absent from production builds.
func ConfiguredInvocationForTest(tools, jobs, compaction string, sections map[string]string) (Invocation, error) {
	plugins := make(map[string]json.RawMessage, len(sections))
	for id, section := range sections {
		plugins[id] = json.RawMessage(section)
	}
	doc := capturedConfigDocument{
		Tools:      json.RawMessage(tools),
		Jobs:       json.RawMessage(jobs),
		Compaction: json.RawMessage(compaction),
		Extensions: plugins,
	}
	snapshot, err := newConfiguration(1, doc, catalog.BuildResult{}, nil)
	if err != nil {
		return Invocation{}, err
	}
	return Invocation{snapshot: snapshot}, nil
}

// EqualEventForTest exposes the field-wise Event comparison to tests.
func EqualEventForTest(a, b Event) bool {
	return equalEvent(a, b)
}

// createSession is the shared in-package fixture: it enters the owner's
// admitted-call gate and runs the one normalized creation core so tests
// needing the full Harness record observe the real gate.
func (r *Melta) createSession(ctx context.Context, workspace, agentType string) (harness.SessionRecord, error) {
	var record harness.SessionRecord
	if err := r.withHarness(ctx, func(ctx context.Context, h *harness.Harness) error {
		var err error
		record, err = r.createSessionRecord(ctx, h, workspace, harness.AgentSelection{Definition: &harness.DefinitionRef{ExtensionID: "agents", CapabilityID: "agents.definitions", DefinitionID: agentType}})
		return err
	}); err != nil {
		return harness.SessionRecord{}, err
	}
	return record, nil
}
