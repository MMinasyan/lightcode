package agent

import (
	"sort"

	"github.com/MMinasyan/lightcode/model"
)

type partAcc struct {
	kind   model.PartKind // first fragment's closed kind decides this position permanently.
	opaque string         // structural original wire type, decided by the first non-empty value and required to agree thereafter.
	text   string
	url    string
	extra  *model.ExtraAccumulator
}

func (st *assemblyState) applyContent(f model.ContentFragment) {
	acc := st.partAt(f.Position)

	if f.Kind == "" || acc.kind != "" && acc.kind != f.Kind { // unknown/empty kind, or a fragment whose kind disagrees with the position's already-decided one: structural conflict — nothing from this fragment may touch the part after pinning it.
		st.noteConflict("content at position %d carries conflicting content kinds", f.Position)
		return
	}

	if acc.kind == "" { // undecided positions take their closed kind on first arrival; later fragments must agree with it or the conflict above fires.
		acc.kind = f.Kind
	}

	switch f.Kind { // per-kind field rules for a fragment agreeing with its position's decided kind; each case writes only fields of its own kind.
	case model.PartText:
		acc.text += f.Text // text fragments concatenate in arrival order; empty pieces contribute nothing.
	case model.PartImageURL:
		if acc.url == "" {
			acc.url = f.URL // first image URL wins for this position.
		} else if f.URL != "" && acc.url != f.URL { // repeated image URLs must agree with the decided value or the output is errored.
			st.noteConflict("content at position %d carries conflicting image urls", f.Position)
		}
	case model.PartOpaque:
		if !st.agreeOpaque(acc, f.Position, f.OpaqueWireType) { // structural wire type must agree when repeated non-empty; empty pieces carry no information.
			return
		}
	default: // unknown kind already pinned above as a conflict; nothing else to accumulate for it here.
	}

	for key, value := range f.Extra { // part-scope extras through the ordinary accumulator with its errors ignored for classification by contract.
		if err := acc.extra.Add(key, value); err != nil {
			_ = err
		}
	}
}

func (st *assemblyState) agreeOpaque(acc *partAcc, pos int, wt string) bool {
	if wt == "" { // absent structural type carries no information to add or check anywhere on this path.
		return true
	}
	if acc.opaque == "" { // first non-empty value decides the position's wire type permanently from here on.
		acc.opaque = wt
		return true
	}
	if acc.opaque != wt { // a different structural type at one position is a content conflict under this contract's rules above it in this file now.
		st.noteConflict("content at position %d carries conflicting opaque wire types", pos)
	}
	return false
}

func (st *assemblyState) partAt(pos int) *partAcc {
	if acc, ok := st.content[pos]; ok {
		return acc
	}
	if st.content == nil { // no content map yet — initialize lazily on first fragment rather than paying for state every invocation never needs.
		st.content = make(map[int]*partAcc)
	}
	acc := &partAcc{extra: model.NewExtraAccumulator()}
	st.content[pos] = acc
	st.contentOrder = append(st.contentOrder, pos)
	return acc
}

func (p *partAcc) nonEmpty() bool {
	return p.text != "" || p.url != "" || p.opaque != "" || len(p.extra.Finalize()) > 0
}

func (st *assemblyState) buildParts() []model.ContentPart {
	if len(st.contentOrder) == 0 {
		return nil
	}

	order := make([]int, len(st.contentOrder)) // final content parts are ordered by ascending position regardless of fragment arrival order.
	copy(order, st.contentOrder)
	sort.Ints(order)

	var parts []model.ContentPart
	for _, pos := range order {
		acc := st.content[pos]
		if !acc.nonEmpty() || (acc.kind == model.PartOpaque && acc.opaque == "") { // empty positions carry nothing to emit; an opaque position that never received its structural wire type is incomplete and must not reach the output constructor through any path, completed or retained partial alike.
			continue
		}

		parts = append(parts, model.ContentPart{Kind: acc.kind, Text: acc.text, URL: acc.url, OpaqueWireType: acc.opaque, Extra: acc.extra.Finalize()})
	}

	return parts
}

func (st *assemblyState) hasPayload() bool {
	if st.refusal != "" {
		return true
	}

	for _, pos := range st.contentOrder {
		if st.content[pos].nonEmpty() {
			return true
		}
	}

	return len(st.msgExtra.Finalize()) > 0
}
