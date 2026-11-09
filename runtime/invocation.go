package runtime

import (
	"encoding/json"
	"strconv"

	"github.com/MMinasyan/lightcode/protocol"
)

// Invocation is immutable captured baseline data for one host call tree: the
// revision snapshots and each extension's own non-secret section. It carries
// no lifetime or capability handle; extension implementations receive their
// equivalent captured values through the native contexts instead.
type Invocation struct {
	snapshot *configuration
}

// Revision returns the captured snapshot's publication generation as an
// unsigned decimal string, or empty for the zero Invocation.
func (in Invocation) Revision() string {
	if in.snapshot == nil {
		return ""
	}
	return strconv.FormatUint(in.snapshot.generation, 10)
}

// Config returns the captured snapshot's owned JSON for extensionID, or nil
// when the extension has no configuration in this revision.
func (in Invocation) Config(extensionID string) json.RawMessage {
	if in.snapshot == nil {
		return nil
	}
	raw, ok := in.snapshot.extensions[extensionID]
	if !ok {
		return nil
	}
	return append(json.RawMessage(nil), raw...)
}

// toolsSettings returns the captured snapshot's owned top-level tools section.
func (in Invocation) toolsSettings() json.RawMessage {
	if in.snapshot == nil {
		return nil
	}
	return append(json.RawMessage(nil), in.snapshot.tools...)
}

// jobsSettings returns the captured snapshot's owned top-level jobs section.
func (in Invocation) jobsSettings() json.RawMessage {
	if in.snapshot == nil {
		return nil
	}
	return append(json.RawMessage(nil), in.snapshot.jobs...)
}

// AgentTypes returns the captured snapshot's owned roster projection of the
// loaded Agent definitions, preserving the snapshot's ordering.
func (in Invocation) AgentTypes() []protocol.Agent {
	if in.snapshot == nil {
		return nil
	}
	return projectAgents(in.snapshot)
}
