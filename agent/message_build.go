package agent

import (
	"encoding/json"
	"sort"

	"github.com/MMinasyan/lightcode/model"
)

func (st *assemblyState) buildCalls() []model.ToolCall {
	if len(st.toolDeltas) == 0 {
		return nil
	}

	order := make([]int, 0, len(st.toolDeltas))
	for pos := range st.toolDeltas {
		order = append(order, pos)
	}

	sort.Ints(order)

	var calls []model.ToolCall
	for _, pos := range order {
		entry := st.toolDeltas[pos]
		if entry.id == "" || entry.name == "" {
			continue
		}

		call := model.ToolCall{ID: entry.id, Name: entry.name, Arguments: json.RawMessage(entry.args)}
		call.Extra = entry.extra.Finalize()
		calls = append(calls, call)
	}

	return calls
}

func (st *assemblyState) assembleMessage(includeCalls bool) model.Message {
	// Model outputs carry only assistant messages; a raw non-assistant wire role is pinned as a conflict at finalization instead of leaking into the retained partial.
	msg := model.Message{Role: model.RoleAssistant, Source: st.source, Refusal: st.refusal}
	if parts := st.buildParts(); len(parts) > 0 {
		msg.Content = parts
	}

	if includeCalls {
		if calls := st.buildCalls(); len(calls) > 0 {
			msg.ToolCalls = calls
		}
	}

	msg.Extra = st.msgExtra.Finalize()
	return msg
}

func msgHasEligiblePartialContent(m model.Message) bool {
	if m.Refusal != "" {
		return true
	}

	for _, part := range m.Content {
		if part.Text != "" || part.URL != "" || part.OpaqueWireType != "" || len(part.Extra.Finalize()) > 0 {
			return true
		}
	}

	return len(m.Extra.Finalize()) > 0
}
