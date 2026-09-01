package model

import (
	"errors"
	"fmt"
)

// Stream is the accepted-model-stream contract. Recv yields one parsed delta at a time; Close releases transport resources exactly once, and Recv after close reports io.EOF through its error return.
type Stream interface {
	Recv() (StreamDelta, error)
	Close() error
}

// ErrInvalidPosition is returned for negative content-fragment positions or tool-call fragment/warning indices: position identity must be nonnegative everywhere in this package.
var ErrInvalidPosition = errors.New("position and index values must not be negative")

// ContentFragment is one positioned content delta from a streaming choice. Position is the single identity used to accumulate content across deltas: string-form wire content arrives as one text fragment at position zero, array-form wire content uses its array index. The opaque-type fragment carries an original provider wire type structurally; it never enters the generic extra accumulator.
type ContentFragment struct {
	Position       int // nonnegative accumulation identity (zero-based).
	Kind           PartKind
	Text           string // text fragments carry a text piece to concatenate.
	URL            string // image_url fragments carry the URL value.
	OpaqueWireType string // original wire type for opaque-kind fragments; structural only.
	Extra          Extra  // raw extras accumulated through the ordinary extra accumulator.
}

// ToolCallFragment is one positioned tool-call delta: optional nonnegative position (omitted positions are normalized by the fixed parser before deltas reach consumers), id, raw wire type, name fragment, argument fragment, and extras. Argument fragments stay as verbatim string pieces; concatenation into completed call arguments happens during stream assembly, not here — no JSON validation is applied at any point on this path.
type ToolCallFragment struct {
	Position         *int // nil means omitted on the wire; nonnegative when present.
	ID               string
	WireType         string // raw supplied type value; empty means absent (normalized to function by the parser).
	Name             string // name fragment for this call delta.
	ArgumentFragment string // argument piece, preserved verbatim without JSON validation or decoding.
	Extra            Extra  // raw extras at tool-call scope.
}

// StreamDelta is one parsed streaming chunk in provider-neutral form: whether a choice exists, the raw role string (empty means absent; wire-role normalization such as developer-to-system is parser work and never happens on this value), refusal fragment, positioned content fragments, tool-call fragments, optional finish reason (raw non-empty OpenAI-compatible string; empty means absent), optional usage (nil means unknown/not reported — an all-zero present value is known zero), and message-scope extras.
type StreamDelta struct {
	HasChoice        bool
	Role             string // raw wire role as supplied by the provider chunk.
	RefusalFragment  string
	ContentFragments []ContentFragment
	ToolFragments    []ToolCallFragment
	FinishReason     string
	Usage            *Usage
	MessageExtra     Extra
}

// validateContentFragment applies the closed content-kind and field-exclusivity contract to one transient content fragment at an accepting boundary. Unlike canonical parts it permits empty kind-specific pieces — a text piece, URL value, or opaque wire type may arrive in a later delta — so only unknown kinds and cross-kind field combinations are rejected here.
func validateContentFragment(f ContentFragment) error {
	switch f.Kind {
	case PartText:
		if f.URL != "" || f.OpaqueWireType != "" {
			return fmt.Errorf("%w: text fragment must not carry a URL or opaque wire type", ErrForbiddenField)
		}
	case PartImageURL:
		if f.Text != "" || f.OpaqueWireType != "" {
			return fmt.Errorf("%w: image_url fragment must not carry text or an opaque wire type", ErrForbiddenField)
		}
	case PartOpaque: // the original wire type may still be empty while the fragment is transient.
		if f.Text != "" || f.URL != "" {
			return fmt.Errorf("%w: opaque fragment must not carry a URL or text; its original wire fields belong in Extra", ErrForbiddenField)
		}
	default:
		return fmt.Errorf("%w: content fragment kind %q is not one of text, image_url or opaque", ErrForbiddenField, f.Kind)
	}
	if err := validateExtraValues(f.Extra); err != nil {
		return err
	}
	return nil
}

// NewStreamDelta validates in (nonnegative fragment positions including nil-checked pointer tool positions; closed content-fragment kinds and field exclusivity with empty transient pieces allowed; well-formed extra JSON at every scope) and returns an independent owned copy: content/tool extras are deep-copied at the accepting boundary. Role strings and finish reasons pass through as raw wire data; usage pointers are copied by value so caller mutations cannot reach retained deltas.
func NewStreamDelta(in StreamDelta) (StreamDelta, error) {
	for _, frag := range in.ContentFragments {
		if frag.Position < 0 {
			return StreamDelta{}, fmt.Errorf("%w: content fragment position %d", ErrInvalidPosition, frag.Position)
		}
		if err := validateContentFragment(frag); err != nil {
			return StreamDelta{}, fmt.Errorf("content fragment at position %d: %w", frag.Position, err)
		}
	}
	for i, frag := range in.ToolFragments {
		if frag.Position != nil && *frag.Position < 0 {
			return StreamDelta{}, fmt.Errorf("%w: tool call fragment position %d", ErrInvalidPosition, *frag.Position)
		}
		if err := validateExtraValues(frag.Extra); err != nil {
			return StreamDelta{}, fmt.Errorf("tool call fragment[%d]: %w", i, err)
		}
	}
	if err := validateExtraValues(in.MessageExtra); err != nil {
		return StreamDelta{}, err
	}

	out := in
	if len(in.ContentFragments) > 0 {
		out.ContentFragments = make([]ContentFragment, len(in.ContentFragments))
		for i, frag := range in.ContentFragments {
			frag.Extra = frag.Extra.Clone()
			out.ContentFragments[i] = frag
		}
	} else { // Drop any caller-owned spare capacity on empty slices.
		out.ContentFragments = nil
	}
	if len(in.ToolFragments) > 0 {
		out.ToolFragments = make([]ToolCallFragment, len(in.ToolFragments))
		for i, frag := range in.ToolFragments {
			frag.Extra = frag.Extra.Clone()
			if frag.Position != nil {
				posCopy := *frag.Position
				frag.Position = &posCopy
			}
			out.ToolFragments[i] = frag
		}
	} else {
		out.ToolFragments = nil
	}
	if in.Usage != nil {
		usageCopy := *in.Usage
		out.Usage = &usageCopy
	}
	out.MessageExtra = in.MessageExtra.Clone()
	return out, nil
}
