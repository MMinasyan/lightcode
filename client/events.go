package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/MMinasyan/lightcode/protocol"
)

// The one event stream's bounded delivery: a capacity-256 event queue and a
// single-slot terminal error queue. The caller must consume events to keep
// the queue from overflowing; an overflow is a missed-event gap and ends the
// stream with ErrResyncRequired, never a silent drop.
const eventQueueCapacity = 256

// Events establishes one authenticated generated GET /v1/events stream and
// returns the notification channel and the single terminal-error channel.
// When the stream is established the server-side subscription exists, so a
// caller may synchronously trigger work and observe its committed hint. Every
// delivered Event is one schema notification frame with its revision identity
// checked against this connection; keepalive comments are discarded. The
// stream ends on disconnect, malformed or incomplete frame, or queue
// overflow with ErrResyncRequired, and on intentional caller cancellation
// with ctx.Err(); both channels are closed after the one error is queued.
// Closing the stream cancels no Runtime, Session, Job, or Agent work: the
// caller repairs its projection from fresh generated HTTP reads.
func (c *Client) Events(ctx context.Context) (<-chan protocol.Event, <-chan error) {
	events := make(chan protocol.Event, eventQueueCapacity)
	errs := make(chan error, 1)
	response, err := c.GetEvents(ctx)
	if err != nil {
		errs <- c.streamError(ctx, fmt.Errorf("open event stream: %v", err))
		close(events)
		close(errs)
		return events, errs
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		errs <- fmt.Errorf("event stream status %q: %w", response.Status, ErrResyncRequired)
		close(events)
		close(errs)
		return events, errs
	}
	go func() {
		defer response.Body.Close()
		errs <- c.streamError(ctx, c.consumeEvents(response.Body, events))
		close(events)
		close(errs)
	}()
	return events, errs
}

// streamError classifies one terminal stream failure: a deliberately canceled
// caller context is preserved as ctx.Err(), and every continuity failure is
// ErrResyncRequired.
func (c *Client) streamError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, ErrResyncRequired) {
		return err
	}
	return fmt.Errorf("%v: %w", err, ErrResyncRequired)
}

// consumeEvents reads the never-ending body with native buffered line
// reading — no Scanner size cap — and delivers one validated schema Event per
// declared notification frame. Every exit closes the body.
func (c *Client) consumeEvents(body io.Reader, events chan<- protocol.Event) error {
	reader := bufio.NewReader(body)
	var (
		haveEvent bool
		haveData  bool
		data      []byte
	)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read event stream: %v: %w", err, ErrResyncRequired)
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		switch {
		case line == "":
			if !haveEvent && !haveData {
				continue // the blank line closing a keepalive comment
			}
			if !haveEvent || !haveData {
				return fmt.Errorf("incomplete notification frame: %w", ErrResyncRequired)
			}
			event, err := decodeNotification(data, c.instance)
			if err != nil {
				return fmt.Errorf("notification frame: %v: %w", err, ErrResyncRequired)
			}
			select {
			case events <- event:
			default:
				return fmt.Errorf("event queue overflow at %d events: %w", eventQueueCapacity, ErrResyncRequired)
			}
			haveEvent, haveData, data = false, false, nil
		case strings.HasPrefix(line, ":"):
			// keepalive comment
		case strings.HasPrefix(line, "event:"):
			if haveEvent {
				return fmt.Errorf("duplicate event field: %w", ErrResyncRequired)
			}
			haveEvent = true
			if name := strings.TrimPrefix(strings.TrimPrefix(line, "event:"), " "); name != "notification" {
				return fmt.Errorf("event name %q: %w", name, ErrResyncRequired)
			}
		case strings.HasPrefix(line, "data:"):
			if haveData {
				return fmt.Errorf("multiple data lines in one frame: %w", ErrResyncRequired)
			}
			haveData = true
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " ")...)
		default:
			return fmt.Errorf("unexpected stream line %q: %w", line, ErrResyncRequired)
		}
	}
}

// decodeNotification strictly decodes one notification frame into its
// generated Event variant: the discriminator must be a known kind, every
// member must belong to that variant, required members must be present and
// non-null, closed enums and schema domains must hold, and a revisioned
// event's embedded instance identity must match this authenticated
// connection. Transient events carry no revision and therefore no identity.
func decodeNotification(raw []byte, instance string) (protocol.Event, error) {
	var envelope struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return protocol.Event{}, err
	}
	switch envelope.Kind {
	case "configuration_changed":
		var event protocol.ConfigurationChangedEvent
		members, err := decodeStrict(raw, &event)
		if err != nil {
			return protocol.Event{}, err
		}
		if err := requireMembers(members, "kind", "scope", "configuration_revision"); err != nil {
			return protocol.Event{}, err
		}
		if err := validateScope(members["scope"]); err != nil {
			return protocol.Event{}, err
		}
		if err := validateRevision(members["configuration_revision"], instance, "instance_id", "generation"); err != nil {
			return protocol.Event{}, err
		}
		var out protocol.Event
		if err := out.FromConfigurationChangedEvent(event); err != nil {
			return protocol.Event{}, err
		}
		return out, nil
	case "scope_opened", "scope_closed":
		var event protocol.ScopeEvent
		members, err := decodeStrict(raw, &event)
		if err != nil {
			return protocol.Event{}, err
		}
		if err := requireMembers(members, "kind", "scope"); err != nil {
			return protocol.Event{}, err
		}
		if err := validateScope(members["scope"]); err != nil {
			return protocol.Event{}, err
		}
		var out protocol.Event
		if err := out.FromScopeEvent(event); err != nil {
			return protocol.Event{}, err
		}
		return out, nil
	case "session_changed":
		var event protocol.SessionChangedEvent
		members, err := decodeStrict(raw, &event)
		if err != nil {
			return protocol.Event{}, err
		}
		if err := requireMembers(members, "kind", "scope", "session_revision"); err != nil {
			return protocol.Event{}, err
		}
		if err := validateScope(members["scope"]); err != nil {
			return protocol.Event{}, err
		}
		if err := validateRevision(members["session_revision"], instance, "instance_id", "durable_revision", "local_revision"); err != nil {
			return protocol.Event{}, err
		}
		var out protocol.Event
		if err := out.FromSessionChangedEvent(event); err != nil {
			return protocol.Event{}, err
		}
		return out, nil
	case "warning_changed":
		var event protocol.WarningChangedEvent
		members, err := decodeStrict(raw, &event)
		if err != nil {
			return protocol.Event{}, err
		}
		if err := requireMembers(members, "kind", "scope", "warnings_revision"); err != nil {
			return protocol.Event{}, err
		}
		if err := validateScope(members["scope"]); err != nil {
			return protocol.Event{}, err
		}
		if err := validateRevision(members["warnings_revision"], instance, "instance_id", "revision"); err != nil {
			return protocol.Event{}, err
		}
		var out protocol.Event
		if err := out.FromWarningChangedEvent(event); err != nil {
			return protocol.Event{}, err
		}
		return out, nil
	case "text_delta":
		var event protocol.TextDeltaEvent
		members, err := decodeStrict(raw, &event)
		if err != nil {
			return protocol.Event{}, err
		}
		if err := requireMembers(members, "kind", "scope", "position", "content"); err != nil {
			return protocol.Event{}, err
		}
		if err := validateScope(members["scope"]); err != nil {
			return protocol.Event{}, err
		}
		if event.Position < 0 || event.Content == "" {
			return protocol.Event{}, errors.New("text_delta carries a negative position or empty content")
		}
		var out protocol.Event
		if err := out.FromTextDeltaEvent(event); err != nil {
			return protocol.Event{}, err
		}
		return out, nil
	case "tool_started":
		var event protocol.ToolStartedEvent
		members, err := decodeStrict(raw, &event)
		if err != nil {
			return protocol.Event{}, err
		}
		if err := requireMembers(members, "kind", "scope", "call_id", "ordinal", "name"); err != nil {
			return protocol.Event{}, err
		}
		if err := validateScope(members["scope"]); err != nil {
			return protocol.Event{}, err
		}
		if event.Ordinal < 0 {
			return protocol.Event{}, errors.New("tool_started carries a negative ordinal")
		}
		var out protocol.Event
		if err := out.FromToolStartedEvent(event); err != nil {
			return protocol.Event{}, err
		}
		return out, nil
	case "tool_finished":
		var event protocol.ToolFinishedEvent
		members, err := decodeStrict(raw, &event)
		if err != nil {
			return protocol.Event{}, err
		}
		if err := requireMembers(members, "kind", "scope", "call_id", "status"); err != nil {
			return protocol.Event{}, err
		}
		if err := validateScope(members["scope"]); err != nil {
			return protocol.Event{}, err
		}
		if !event.Status.Valid() {
			return protocol.Event{}, fmt.Errorf("tool_finished status %q is outside the closed set", event.Status)
		}
		var out protocol.Event
		if err := out.FromToolFinishedEvent(event); err != nil {
			return protocol.Event{}, err
		}
		return out, nil
	default:
		return protocol.Event{}, fmt.Errorf("unknown event kind %q", envelope.Kind)
	}
}

// decodeStrict decodes one JSON object into its generated variant with
// unknown members rejected at every nesting level and exactly one document
// required, and returns the top-level members for presence and null checks.
func decodeStrict(raw []byte, target any) (map[string]json.RawMessage, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, err
	}
	if members == nil {
		return nil, errors.New("event is not a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return nil, err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("event carries trailing data")
	}
	if err := rejectNullMembers(members); err != nil {
		return nil, err
	}
	return members, nil
}

// rejectNullMembers proves no declared member carries the null literal: the
// closed variants type every member, so null is never a valid value.
func rejectNullMembers(members map[string]json.RawMessage) error {
	for name, value := range members {
		if isJSONNull(value) {
			return fmt.Errorf("event member %q is null", name)
		}
	}
	return nil
}

// requireMembers proves every named required member is present.
func requireMembers(members map[string]json.RawMessage, names ...string) error {
	for _, name := range names {
		if _, ok := members[name]; !ok {
			return fmt.Errorf("event is missing required member %q", name)
		}
	}
	return nil
}

// The closed scope member vocabulary shared by every union branch, and the
// identities each kind must carry to address its subject; the runtime scope
// alone names the whole Runtime with none.
var (
	scopeMembers = map[string]bool{
		"kind":         true,
		"workspace":    true,
		"session_id":   true,
		"operation_id": true,
		"job_id":       true,
	}
	scopeRequiredIdentities = map[string][]string{
		"runtime":   nil,
		"workspace": {"workspace"},
		"session":   {"session_id"},
		"operation": {"session_id", "operation_id"},
		"agent":     {"session_id", "operation_id"},
		"job":       {"session_id", "job_id"},
	}
)

// validateScope proves one delivered scope is inside the schema's union
// domain: the raw member map proves the scope is a JSON object with no null
// member, a present kind, and no member outside the closed vocabulary; every
// supplied identifier is a nonempty string with session_id inside its 32-hex
// pattern; and the kind selects one branch whose required identities are
// present. The generated union's decode stores the raw value without
// inspecting it, so this is the scope's one validation.
func validateScope(raw json.RawMessage) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return fmt.Errorf("scope: %w", err)
	}
	if members == nil {
		return errors.New("scope is not a JSON object")
	}
	if err := rejectNullMembers(members); err != nil {
		return fmt.Errorf("scope: %w", err)
	}
	if err := requireMembers(members, "kind"); err != nil {
		return fmt.Errorf("scope: %w", err)
	}
	for name, value := range members {
		if !scopeMembers[name] {
			return fmt.Errorf("scope member %q is unknown", name)
		}
		if name == "kind" {
			continue
		}
		var text string
		if err := json.Unmarshal(value, &text); err != nil {
			return fmt.Errorf("scope member %q: %w", name, err)
		}
		if name == "session_id" {
			if !isLowerHex(text, 32) {
				return fmt.Errorf("scope session_id %q is not 32 lowercase hex", text)
			}
			continue
		}
		if text == "" {
			return fmt.Errorf("scope member %q is empty", name)
		}
	}
	var kind string
	if err := json.Unmarshal(members["kind"], &kind); err != nil {
		return fmt.Errorf("scope kind: %w", err)
	}
	required, known := scopeRequiredIdentities[kind]
	if !known {
		return fmt.Errorf("scope kind %q is outside the closed set", kind)
	}
	if err := requireMembers(members, required...); err != nil {
		return fmt.Errorf("scope: %w", err)
	}
	return nil
}

// validateRevision proves one revision object's members are non-null, its
// required members are present, and its embedded instance identity is this
// connection.
func validateRevision(raw json.RawMessage, instance string, names ...string) error {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return err
	}
	if members == nil {
		return errors.New("revision is not a JSON object")
	}
	if err := rejectNullMembers(members); err != nil {
		return err
	}
	if err := requireMembers(members, names...); err != nil {
		return err
	}
	var identity string
	if err := json.Unmarshal(members["instance_id"], &identity); err != nil {
		return err
	}
	if identity != instance {
		return fmt.Errorf("revision instance %q does not match the authenticated connection %q", identity, instance)
	}
	return nil
}

// isJSONNull reports whether one raw JSON member is the null literal.
func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
