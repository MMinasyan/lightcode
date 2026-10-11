// Package clientview owns the target clients' pure view rules: component-wise
// session revision ordering, same-instance hydration acceptance, conversation
// item identity extraction and merging of already-connected ordered ranges.
// It holds no state and performs no I/O; the desktop and terminal readers
// supply their own ranges and acceptance decisions when they land.
package clientview

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/MMinasyan/lightcode/protocol"
)

// errInvalidConversationDelivery is the one rejection shared by every merge
// input that is not two connected, ordered, duplicate-free ranges: a repeated
// identity inside one input and an inconsistent or reversed overlap are the
// same invalid delivery.
var errInvalidConversationDelivery = errors.New("invalid conversation delivery")

// sessionClock is one parsed Session revision.
type sessionClock struct {
	instance string
	durable  int64
	local    uint64
}

// counterClock is one parsed configuration generation or warning revision.
type counterClock struct {
	instance string
	value    uint64
}

// CompareSessionRevision returns -1, 0 or 1 when a is older than, equal to or
// newer than b under the component-wise durable/local partial order. A pair
// that is newer in one component and older in the other is incomparable, and
// different instances, malformed components or out-of-range values are errors,
// never a fabricated order.
func CompareSessionRevision(a, b protocol.SessionRevision) (int, error) {
	left, err := parseSessionRevision(a)
	if err != nil {
		return 0, err
	}
	right, err := parseSessionRevision(b)
	if err != nil {
		return 0, err
	}
	return compareSessionClocks(left, right)
}

// AcceptHydration reports whether the complete top-level Session,
// configuration and warning clocks of next are at least as new as those of the
// previously accepted current hydration and the independently accepted
// configuration and warning floors. Equal clocks are accepted; a next clock
// older than any compared clock is stale and returns false with no error;
// malformed, foreign or incomparable clocks are errors. instance is the
// mandatory authenticated runtime instance: it must be a lowercase 32-hex id
// and every compared revision and optional floor must belong to it, so an
// empty, malformed or foreign expected or body identity is an error, never a
// silent bypass. The optional floors are validated even when current is nil.
// Only these canonical top-level clocks govern acceptance; the nested Session
// header, Session IDs and every other DTO member are not validated or compared
// here. Neither current nor next is retained or mutated.
func AcceptHydration(current *protocol.Hydration, next protocol.Hydration, instance string, configuration *protocol.ConfigurationRevision, warnings *protocol.WarningsRevision) (bool, error) {
	nextSession, err := parseSessionRevision(next.SessionRevision)
	if err != nil {
		return false, err
	}
	nextConfiguration, err := parseConfigurationRevision(next.ConfigurationRevision)
	if err != nil {
		return false, err
	}
	nextWarnings, err := parseWarningsRevision(next.WarningsRevision)
	if err != nil {
		return false, err
	}
	instances := []string{nextSession.instance, nextConfiguration.instance, nextWarnings.instance}
	instances = append(instances, instance)
	var currentSession sessionClock
	var currentConfiguration, currentWarnings counterClock
	if current != nil {
		currentSession, err = parseSessionRevision(current.SessionRevision)
		if err != nil {
			return false, err
		}
		currentConfiguration, err = parseConfigurationRevision(current.ConfigurationRevision)
		if err != nil {
			return false, err
		}
		currentWarnings, err = parseWarningsRevision(current.WarningsRevision)
		if err != nil {
			return false, err
		}
		instances = append(instances, currentSession.instance, currentConfiguration.instance, currentWarnings.instance)
	}
	var configurationFloor, warningsFloor counterClock
	if configuration != nil {
		configurationFloor, err = parseConfigurationRevision(*configuration)
		if err != nil {
			return false, err
		}
		instances = append(instances, configurationFloor.instance)
	}
	if warnings != nil {
		warningsFloor, err = parseWarningsRevision(*warnings)
		if err != nil {
			return false, err
		}
		instances = append(instances, warningsFloor.instance)
	}
	for _, candidate := range instances[1:] {
		if candidate != instances[0] {
			return false, fmt.Errorf("revisions belong to different runtime instances %q and %q", instances[0], candidate)
		}
	}
	if current != nil {
		order, err := compareSessionClocks(currentSession, nextSession)
		if err != nil {
			return false, err
		}
		if order > 0 {
			return false, nil
		}
		if currentConfiguration.value > nextConfiguration.value {
			return false, nil
		}
		if currentWarnings.value > nextWarnings.value {
			return false, nil
		}
	}
	if configuration != nil && configurationFloor.value > nextConfiguration.value {
		return false, nil
	}
	if warnings != nil && warningsFloor.value > nextWarnings.value {
		return false, nil
	}
	return true, nil
}

// ItemID returns the nonempty item_id of one generated conversation item
// variant. It reads the closed discriminator and then the matching generated
// accessor, so unknown kinds and members the generated types cannot decode are
// errors; it validates no other item field.
func ItemID(item protocol.ConversationItem) (string, error) {
	kind, err := item.Discriminator()
	if err != nil {
		return "", fmt.Errorf("conversation item discriminator: %w", err)
	}
	var id string
	switch kind {
	case "input":
		value, err := item.AsInputItem()
		if err != nil {
			return "", fmt.Errorf("input conversation item: %w", err)
		}
		id = value.ItemId
	case "assistant":
		value, err := item.AsAssistantItem()
		if err != nil {
			return "", fmt.Errorf("assistant conversation item: %w", err)
		}
		id = value.ItemId
	case "signal":
		value, err := item.AsSignalItem()
		if err != nil {
			return "", fmt.Errorf("signal conversation item: %w", err)
		}
		id = value.ItemId
	case "compaction":
		value, err := item.AsCompactionItem()
		if err != nil {
			return "", fmt.Errorf("compaction conversation item: %w", err)
		}
		id = value.ItemId
	case "operation_end":
		value, err := item.AsOperationEndItem()
		if err != nil {
			return "", fmt.Errorf("operation end conversation item: %w", err)
		}
		id = value.ItemId
	default:
		return "", fmt.Errorf("unknown conversation item kind %q", kind)
	}
	if id == "" {
		return "", fmt.Errorf("%s conversation item has an empty item_id", kind)
	}
	return id, nil
}

// MergeConversation combines two caller-certified connected, ordered ranges by
// stable item identity: the values of the supplied newer range replace the
// shared identities, the prefix of the range starting earlier is kept, and the
// suffix that extends farther is kept. Disjoint ranges concatenate older then
// newer and empty ranges are legal. Repeated identities inside one input and
// inconsistent or reversed overlaps are invalid delivery errors. The result is
// a fresh slice; neither input is mutated, decoded, sorted or retained.
func MergeConversation(older, newer []protocol.ConversationItem) ([]protocol.ConversationItem, error) {
	olderIDs, err := conversationItemIDs(older)
	if err != nil {
		return nil, err
	}
	newerIDs, err := conversationItemIDs(newer)
	if err != nil {
		return nil, err
	}
	newerPositions := make(map[string]int, len(newerIDs))
	for j, id := range newerIDs {
		newerPositions[id] = j
	}
	start, newerStart := -1, -1
	for i, id := range olderIDs {
		if j, ok := newerPositions[id]; ok {
			start, newerStart = i, j
			break
		}
	}
	if start < 0 {
		merged := make([]protocol.ConversationItem, 0, len(older)+len(newer))
		merged = append(merged, older...)
		merged = append(merged, newer...)
		return merged, nil
	}
	length := 0
	for start+length < len(olderIDs) && newerStart+length < len(newerIDs) && olderIDs[start+length] == newerIDs[newerStart+length] {
		length++
	}
	olderPositions := make(map[string]int, len(olderIDs))
	for i, id := range olderIDs {
		olderPositions[id] = i
	}
	for i, id := range olderIDs {
		if _, shared := newerPositions[id]; shared && (i < start || i >= start+length) {
			return nil, errInvalidConversationDelivery
		}
	}
	for j, id := range newerIDs {
		if _, shared := olderPositions[id]; shared && (j < newerStart || j >= newerStart+length) {
			return nil, errInvalidConversationDelivery
		}
	}
	if start > 0 && newerStart > 0 {
		return nil, errInvalidConversationDelivery
	}
	if start+length < len(olderIDs) && newerStart+length < len(newerIDs) {
		return nil, errInvalidConversationDelivery
	}
	merged := make([]protocol.ConversationItem, 0, len(olderIDs)+len(newerIDs))
	if start > 0 {
		merged = append(merged, older[:start]...)
	} else if newerStart > 0 {
		merged = append(merged, newer[:newerStart]...)
	}
	merged = append(merged, newer[newerStart:newerStart+length]...)
	if start+length < len(olderIDs) {
		merged = append(merged, older[start+length:]...)
	} else if newerStart+length < len(newerIDs) {
		merged = append(merged, newer[newerStart+length:]...)
	}
	return merged, nil
}

// conversationItemIDs extracts each range's stable identities and rejects a
// repeated identity inside one input.
func conversationItemIDs(items []protocol.ConversationItem) ([]string, error) {
	ids := make([]string, len(items))
	seen := make(map[string]bool, len(items))
	for i := range items {
		id, err := ItemID(items[i])
		if err != nil {
			return nil, fmt.Errorf("%w: item %d: %v", errInvalidConversationDelivery, i, err)
		}
		if seen[id] {
			return nil, fmt.Errorf("%w: range repeats item_id %q", errInvalidConversationDelivery, id)
		}
		seen[id] = true
		ids[i] = id
	}
	return ids, nil
}

// parseSessionRevision validates one Session revision's instance shape and
// parses its durable and local components.
func parseSessionRevision(revision protocol.SessionRevision) (sessionClock, error) {
	if err := validateInstanceID(revision.InstanceId); err != nil {
		return sessionClock{}, err
	}
	durable, err := parseDurableRevision(revision.DurableRevision)
	if err != nil {
		return sessionClock{}, err
	}
	local, err := parseUnsignedRevision(revision.LocalRevision, "local_revision")
	if err != nil {
		return sessionClock{}, err
	}
	return sessionClock{instance: revision.InstanceId, durable: durable, local: local}, nil
}

// parseConfigurationRevision validates one configuration revision's instance
// shape and parses its generation.
func parseConfigurationRevision(revision protocol.ConfigurationRevision) (counterClock, error) {
	if err := validateInstanceID(revision.InstanceId); err != nil {
		return counterClock{}, err
	}
	generation, err := parseUnsignedRevision(revision.Generation, "generation")
	if err != nil {
		return counterClock{}, err
	}
	return counterClock{instance: revision.InstanceId, value: generation}, nil
}

// parseWarningsRevision validates one warnings revision's instance shape and
// parses its revision counter.
func parseWarningsRevision(revision protocol.WarningsRevision) (counterClock, error) {
	if err := validateInstanceID(revision.InstanceId); err != nil {
		return counterClock{}, err
	}
	revisionValue, err := parseUnsignedRevision(revision.Revision, "revision")
	if err != nil {
		return counterClock{}, err
	}
	return counterClock{instance: revision.InstanceId, value: revisionValue}, nil
}

// compareSessionClocks applies the component-wise partial order; the caller has
// already validated both clocks.
func compareSessionClocks(a, b sessionClock) (int, error) {
	if a.instance != b.instance {
		return 0, fmt.Errorf("session revisions belong to different runtime instances %q and %q", a.instance, b.instance)
	}
	switch {
	case a.durable == b.durable && a.local == b.local:
		return 0, nil
	case a.durable <= b.durable && a.local <= b.local:
		return -1, nil
	case a.durable >= b.durable && a.local >= b.local:
		return 1, nil
	default:
		return 0, fmt.Errorf("session revision components are incomparable: durable %d vs %d, local %d vs %d", a.durable, b.durable, a.local, b.local)
	}
}

// parseDurableRevision accepts one nonempty ASCII decimal digit string within
// the positive signed 64-bit range. Leading zeroes are integer notation.
func parseDurableRevision(value string) (int64, error) {
	if !isDecimalDigits(value) {
		return 0, fmt.Errorf("durable_revision %q is not a nonempty decimal digit string", value)
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 1 {
		return 0, fmt.Errorf("durable_revision %q is outside 1..MaxInt64", value)
	}
	return parsed, nil
}

// parseUnsignedRevision accepts one nonempty ASCII decimal digit string within
// the unsigned 64-bit range. Leading zeroes are integer notation.
func parseUnsignedRevision(value, field string) (uint64, error) {
	if !isDecimalDigits(value) {
		return 0, fmt.Errorf("%s %q is not a nonempty decimal digit string", field, value)
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q is outside 0..MaxUint64", field, value)
	}
	return parsed, nil
}

// isDecimalDigits reports whether value is a nonempty string of ASCII digits.
func isDecimalDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}

// validateInstanceID enforces the generated schema's lowercase 32-hex shape.
func validateInstanceID(value string) error {
	if !isLowercaseHex32(value) {
		return fmt.Errorf("instance_id %q is not a lowercase 32-character hex id", value)
	}
	return nil
}

// isLowercaseHex32 reports whether value is exactly 32 lowercase hex digits.
func isLowercaseHex32(value string) bool {
	if len(value) != 32 {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
