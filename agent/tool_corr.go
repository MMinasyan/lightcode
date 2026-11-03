package agent

import "github.com/MMinasyan/lightcode/model"

type callAcc struct {
	id       string // non-empty once any fragment carried one; later different IDs are a conflicting-mapping conflict.
	wireType string // "" until the first supplied value arrives; repeats must agree, and every supplied value must be "function".
	name     string // concatenated name fragments in arrival order (empty pieces contribute nothing).
	args     string // verbatim argument-string concatenation, never validated or decoded on this path.
	extra    *model.ExtraAccumulator
}

func (st *assemblyState) applyToolEvent(frags []model.ToolCallFragment) {
	if st.toolDeltas == nil { // lazy init on the first tool fragment of this stream.
		st.toolDeltas = make(map[int]*callAcc)
	}

	for _, frag := range frags {
		idx := st.correlateToolFragment(len(frags), frag)

		if id := frag.ID; id != "" { // identity invariant: one non-empty call ID maps to exactly one normalized position.
			switch prev, known := st.toolIDs[id]; {
			case known && prev != idx: // the same stable ID arrived at a different position than its recorded mapping — errored conflict in wire order (identical repeats are not conflicts).
				st.noteConflict("tool call id %q maps to conflicting positions", id)
			case !known: // first appearance of this ID records its single normalized position.
				if st.toolIDs == nil {
					st.toolIDs = make(map[string]int)
				}
				st.toolIDs[id] = idx
			}
		}

		entry := st.toolDeltas[idx]          // correlateToolFragment materialized this slot before returning, so the entry is always present.
		if frag.ID != "" && entry.id == "" { // first non-empty ID landing on a still-unidentified slot decides its stable identity; later repeats must equal it by correlation rules.
			entry.id = frag.ID
		}

		if wt := frag.WireType; wt != "" { // supplied wire types are checked at arrival: omitted means function, any other value is invalid, and repeated values must agree with the first one.
			switch {
			case entry.wireType == "":
				entry.wireType = wt
				if wt != "function" {
					st.noteConflict("supplied tool call wire type %q is not function", wt)
				}
			case wt != entry.wireType:
				st.noteConflict("tool call wire type %q conflicts with earlier supplied type", wt)
			}
		}

		entry.name += frag.Name             // name fragments concatenate in arrival order; empty pieces contribute nothing.
		entry.args += frag.ArgumentFragment // verbatim argument-string concatenation without any validation or decoding on this path by contract.
		for key, value := range frag.Extra {
			if err := entry.extra.Add(key, value); err != nil { // an Extra accumulator error never changes output classification — latest kept, accumulation continues.
				_ = err
			}
		}

		st.lastToolIdx = idx // remembered for anonymous continuations in later events of this stream.
	}
}

func (st *assemblyState) correlateToolFragment(eventCount int, frag model.ToolCallFragment) int {
	pos, hasPos := 0, false // nil Position means no positional signal at all; zero is a legitimate supplied value distinct from omission.
	if frag.Position != nil {
		pos, hasPos = *frag.Position, true
	}

	idx := -1
	switch {
	case frag.ID == "" && frag.Name == "": // anonymous continuation: only the event shape and prior correlation state decide where it lands.
		switch {
		case eventCount == 1 && st.lastToolIdx >= 0: // single-fragment events always continue the last correlated call when one exists, regardless of any supplied position on this fragment.
			idx = st.lastToolIdx
		case hasPos:
			if _, ok := st.toolDeltas[pos]; ok { // a multi-fragment event continues its own slot only while that slot already holds an entry.
				idx = pos
			} else if st.lastToolIdx >= 0 { // no entry at the fragment's position — fall back to last per retained rules.
				idx = st.lastToolIdx
			}
		default:
			if st.lastToolIdx >= 0 { // no positional signal anywhere on this fragment — continue the last correlated call when one exists.
				idx = st.lastToolIdx
			}
		}

	case hasPos:
		if existing, claimed := st.toolDeltas[pos]; !claimed || existing.id == "" || (frag.ID != "" && existing.id == frag.ID) {
			idx = pos
		}

	case frag.ID != "": // no position on a non-anonymous fragment: correlate through the recorded ID map when present, otherwise claim a new slot below.
		if p, ok := st.toolIDs[frag.ID]; ok {
			idx = p
		}

	default: // nothing usable at all on this fragment — a brand-new claim follows immediately.
	}

	if idx < 0 { // allocate the lowest unclaimed slot, mirroring the retained omitted-index fill rule.
		for {
			if _, exists := st.toolDeltas[st.nextToolIdx]; !exists {
				idx = st.nextToolIdx
				st.nextToolIdx++
				break
			}

			st.nextToolIdx++ // scan forward only, never reusing a claimed slot within this event or stream.
		}
	}

	if _, ok := st.toolDeltas[idx]; !ok { // materialize every correlated slot before returning (explicit positions may claim not-yet-created ones); callers always find an entry at the returned index afterwards.
		st.toolDeltas[idx] = &callAcc{extra: model.NewExtraAccumulator()} // fresh per-position accumulator; id/name/args stay empty and wire type stays absent until first non-empty arrival decides them.
	}

	return idx
}
