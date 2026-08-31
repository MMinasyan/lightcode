package model

import (
	"errors"
	"fmt"
)

// Usage carries the token counts reported by one model response, each as a signed Go int exactly like its retained producer. An absent usage value means unknown/not reported; an all-zero present value is known zero — presence itself is the distinction and callers express it with nil versus non-nil.
type Usage struct {
	InputTokens       int // uncached input tokens (prompt minus cached, floored at zero by the parser normalization).
	CachedInputTokens int // cached-input token count as reported.
	OutputTokens      int // completion/output token count.
}

// OutputStatus is the closed status of a finalized model output: completed, errored, or interrupted.
type OutputStatus string

const (
	OutputCompleted   OutputStatus = "completed"   // requires one assistant message with an eligible payload and empty detail.
	OutputErrored     OutputStatus = "errored"     // requires non-empty detail; the optional partial message carries no tool calls.
	OutputInterrupted OutputStatus = "interrupted" // same shape rules as errored: non-empty detail, tool-call-free partial message when present.
)

func validOutputStatus(s OutputStatus) bool {
	switch s {
	case OutputCompleted, OutputErrored, OutputInterrupted:
		return true
	default:
		return false
	}
}

// ErrSourceMismatch is returned by NewOutput when a present assistant message carries a source identity different from the output's own source.
var ErrSourceMismatch = errors.New("message source does not match output source")

// ProtocolWarning is one non-blocking diagnostic value produced at protocol boundaries: kind, human-readable message, target model identity (diagnostic context — no nonzero requirement), field name, and zero-based message index. Warnings never block encoding or transport; their ordered delivery belongs to the caller that received them.
type ProtocolWarning struct {
	Kind         string   // diagnostic category (e.g. a must-preserve metadata warning).
	Message      string   // human-readable detail text.
	Target       ModelRef // model identity the observation targets.
	Field        string   // wire field name involved, when applicable.
	MessageIndex int      // zero-based index into the message list; nonnegative only.
}

// NewProtocolWarning validates in (the zero-based message index must be nonnegative; the target identity carries no nonzero requirement — it is diagnostic context) and returns it as-is: warnings carry no reference-typed fields needing ownership transfer, so validation is their whole boundary work. Warnings are diagnostic values that never block encoding or transport.
func NewProtocolWarning(in ProtocolWarning) (ProtocolWarning, error) {
	if in.MessageIndex < 0 {
		return ProtocolWarning{}, fmt.Errorf("%w: warning message index %d", ErrInvalidPosition, in.MessageIndex)
	}
	return in, nil
}

// Output is the finalized result of one model response attempt: closed status, always-complete source identity, optional assistant message (nil means no eligible partial content was retained), optional usage, and detail text. Completed requires exactly one assistant payload; errored/interrupted require non-empty detail and a tool-call-free message when present. Detail stays empty for completed output and is mandatory otherwise.
type Output struct {
	Status  OutputStatus // closed enum: completed / errored / interrupted.
	Source  ModelRef     // always complete (nonzero); the model identity that produced this output.
	Message *Message     // optional assistant message; when present its source equals Source, it is an assistant role, and non-completed outputs carry no tool calls on it.
	Usage   *Usage       // nil means unknown/not reported; a zero-valued pointer target is known zero.
	Detail  string       // empty for completed; mandatory diagnostic text for errored/interrupted.
}

// hasAssistantPayload reports whether an assistant message carries model-visible payload under the finalization view: at least one finalized non-empty content part, a non-empty refusal, at least one tool call, or at least one finalized non-null message extra. Finalized parts drop empty pieces and null extras; role, source, name, finish reason, and usage never count as payload here (they are not fields of this value).
func hasAssistantPayload(m *Message) bool {
	if m.Refusal != "" || len(m.ToolCalls) > 0 {
		return true
	}
	for _, part := range m.Content {
		if finalizedPartNonEmpty(part) {
			return true
		}
	}
	return len(m.Extra.Finalize()) > 0
}

// finalizedPartNonEmpty is the finalization view of one content part: non-empty when it has non-empty text, a non-empty URL, a non-empty opaque wire type, or at least one extra that survives null removal. Finalization omits empty parts; this predicate decides what counts toward an assistant payload before they are dropped.
func finalizedPartNonEmpty(p ContentPart) bool {
	return p.Text != "" || p.URL != "" || p.OpaqueWireType != "" || len(p.Extra.Finalize()) > 0
}

// NewOutput validates in and returns an independent owned copy. Rules enforced — closed status set; complete nonzero output source; present messages are re-validated at this trust boundary (closed role, per-role field combinations) and must be assistant-role carrying exactly the output's own source identity; completed requires one message with an eligible payload plus empty detail; errored/interrupted require non-empty detail and a tool-call-free optional partial message. Usage pointers are copied by value so caller mutations never reach retained outputs.
func NewOutput(in Output) (Output, error) {
	if !validOutputStatus(in.Status) {
		return Output{}, fmt.Errorf("%w: %q", ErrInvalidStatus, in.Status)
	}
	if !in.Source.complete() {
		return Output{}, fmt.Errorf("%w: output requires a complete source model identity, got zero or partial ref", ErrMissingSource)
	}

	switch in.Status {
	case OutputCompleted:
		if in.Message == nil {
			return Output{}, fmt.Errorf("%w: completed output requires one assistant message", ErrMissingField)
		}
		if err := validateOutputMessage(*in.Message, in.Source); err != nil {
			return Output{}, err
		}
		if !hasAssistantPayload(in.Message) {
			return Output{}, errors.New("completed output requires an assistant payload (content parts, refusal, tool calls, or finalized extras)")
		}
		if in.Detail != "" {
			return Output{}, fmt.Errorf("%w: completed output must carry empty detail", ErrForbiddenField)
		}
	default: // errored/interrupted may omit the message entirely.
		if in.Message != nil {
			if err := validateOutputMessage(*in.Message, in.Source); err != nil {
				return Output{}, err
			}
			if len(in.Message.ToolCalls) > 0 {
				return Output{}, errors.New("errored or interrupted output message must not carry tool calls")
			}
		}
		if in.Detail == "" {
			return Output{}, fmt.Errorf("%w: %s output requires non-empty detail", ErrMissingField, string(in.Status))
		}
	}

	out := in
	if in.Usage != nil {
		usageCopy := *in.Usage
		out.Usage = &usageCopy
	}
	if in.Message != nil {
		m := cloneOwnedMessage(*in.Message)
		out.Message = &m
	}
	return out, nil
}

// validateOutputMessage re-validates a present output message at the trust boundary: full per-role validation (closed role, field combinations, embedded parts/calls), then the output-specific rules that it carries only an assistant role and exactly the output's own source identity.
func validateOutputMessage(m Message, outSource ModelRef) error {
	if err := validateMessage(m); err != nil {
		return fmt.Errorf("output message: %w", err)
	}
	if m.Role != RoleAssistant {
		return fmt.Errorf("%w: model outputs carry only assistant messages, got role %q", ErrInvalidRole, m.Role)
	}
	if !m.Source.complete() || m.Source.Provider != outSource.Provider || m.Source.Model != outSource.Model { // field-based identity: String() rendering is lossy (first-slash split).
		return fmt.Errorf("%w: message source differs from output source", ErrSourceMismatch)
	}
	return nil
}
