package runtime

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"sync"

	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// The Core producer source names. A source is an open string, not a closed
// capability taxonomy: these constants name the Runtime's own producers, and
// every registered plugin producer is named "plugin:<registered ID>".
const (
	setupSource    = "runtime:setup"
	promptSource   = "runtime:prompt"
	catalogSource  = "runtime:catalog"
	agentsSource   = "runtime:agents"
	protocolSource = "runtime:protocol"
)

// warningGroup is one store group's key: an open producer source with an
// optional Session identity. Global groups carry no Session identity; prompt
// and protocol groups are per Session.
type warningGroup struct {
	source    string
	sessionID string
}

// warningStore is the Runtime-owned presentation snapshot of every warning:
// owned typed slices grouped by (source, session), a monotonic revision that
// advances only on an observed change, and no durable or Session-counter
// state. Reports after closure are ignored. Every method is a short
// in-memory section: no network, plugin, or shutdown work runs under the
// mutex.
type warningStore struct {
	mu       sync.Mutex
	closed   bool
	revision uint64
	groups   map[warningGroup][]protocol.Warning
}

func newWarningStore() *warningStore {
	return &warningStore{groups: make(map[warningGroup][]protocol.Warning)}
}

// close ends the store's admission: every later report is ignored. It is a
// short in-memory coordination the Runtime shutdown calls once.
func (s *warningStore) close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// mutateGroup is the one group mutation core shared by the two contracted
// shapes: a complete computation replaces its group with the returned list
// (appendMode false, including an empty list clearing the obsolete value),
// while an incremental report appends and dedupes within its producer group
// (appendMode true). The resulting members are deduplicated on (kind,message)
// in producer order; an unchanged result — including clearing an absent group
// — advances nothing, while a real change replaces or deletes the group and
// advances the revision. Publication (the warning_changed hint) is composed
// by the caller inside its observation section; this core never publishes.
func (s *warningStore) mutateGroup(group warningGroup, warnings []protocol.Warning, appendMode bool) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	current := s.groups[group]
	var next []protocol.Warning
	if appendMode {
		next = dedupeWarnings(slices.Concat(current, warnings))
	} else {
		next = dedupeWarnings(warnings)
	}
	if equalWarnings(current, next) {
		return false
	}
	if len(next) == 0 {
		delete(s.groups, group)
	} else {
		s.groups[group] = next
	}
	s.revision++
	return true
}

// setGlobal replaces one global group from a successful configuration
// publication.
func (s *warningStore) setGlobal(source string, warnings []protocol.Warning) bool {
	return s.mutateGroup(warningGroup{source: source}, warnings, false)
}

// setSessionPrompt replaces one Session's prompt group from a successful
// admitted preparation.
func (s *warningStore) setSessionPrompt(sessionID string, warnings []protocol.Warning) bool {
	return s.mutateGroup(warningGroup{source: promptSource, sessionID: sessionID}, warnings, false)
}

// setSessionProtocol replaces one Session's protocol diagnostics with the
// complete returned list of one model or compact Stream attempt, including an
// empty list: a diagnostic-free attempt clears the obsolete value.
func (s *warningStore) setSessionProtocol(sessionID string, warnings []protocol.Warning) bool {
	return s.mutateGroup(warningGroup{source: protocolSource, sessionID: sessionID}, warnings, false)
}

// appendPlugin adds one Runtime-scoped plugin report to its own global group,
// deduplicating identical (kind,message) pairs in producer order. Reports
// after closure are ignored; the core never publishes.
func (s *warningStore) appendPlugin(pluginID, kind, message string) bool {
	source := "plugin:" + pluginID
	return s.mutateGroup(warningGroup{source: source}, []protocol.Warning{{Source: source, Kind: kind, Message: message}}, true)
}

// removeSessions removes every group owned by the named Sessions in one
// in-memory section: a committed deletion — explicit or swept — takes its
// Session groups with it, and a batch of deletions advances the revision once
// and publishes one hint. Global groups are never touched. A closed store
// ignores the removal like every later mutation.
func (s *warningStore) removeSessions(ids []string) bool {
	if s == nil || len(ids) == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	changed := false
	for group := range s.groups {
		if group.sessionID == "" {
			continue
		}
		if slices.Contains(ids, group.sessionID) {
			delete(s.groups, group)
			changed = true
		}
	}
	if changed {
		s.revision++
	}
	return changed
}

// storeRevision reads the current revision under the store mutex; every
// production mutation happens inside an observation section, so the value is
// stable within one.
func (s *warningStore) storeRevision() uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revision
}

// snapshot is the complete unfiltered read.
func (s *warningStore) snapshot() (uint64, []protocol.Warning) {
	return s.read(nil)
}

// hydrate is the hydration read for one Session: every global group plus
// that Session's own groups.
func (s *warningStore) hydrate(sessionID string) (uint64, []protocol.Warning) {
	return s.read(&sessionID)
}

// read is the shared captured read: every actually stored group is
// enumerated deterministically — global groups before Session groups, then
// lexical Session ID and lexical source within a Session — never a fixed
// source list, so an arbitrary plugin source is a first-class group. A nil
// filter is the complete unfiltered read; a Session filter looks up the
// global groups plus only that Session's own groups directly — a foreign
// Session's warnings never appear. The returned warnings are fully owned
// copies (including every SessionId pointee), so a caller's mutation cannot
// reach the groups, the dedupe, or another read. The revision and the
// warnings are captured once under the same mutex hold.
func (s *warningStore) read(sessionFilter *string) (uint64, []protocol.Warning) {
	s.mu.Lock()
	defer s.mu.Unlock()
	revision := s.revision
	groups := make([]warningGroup, 0, len(s.groups))
	for group := range s.groups {
		if group.sessionID == "" || sessionFilter == nil || group.sessionID == *sessionFilter {
			groups = append(groups, group)
		}
	}
	sort.Slice(groups, func(i, j int) bool {
		left, right := groups[i], groups[j]
		if (left.sessionID == "") != (right.sessionID == "") {
			return left.sessionID == ""
		}
		if left.sessionID != right.sessionID {
			return left.sessionID < right.sessionID
		}
		return left.source < right.source
	})
	out := make([]protocol.Warning, 0) // a present collection reads [], never null
	for _, group := range groups {
		out = append(out, ownWarnings(s.groups[group])...)
	}
	return revision, out
}

// ownWarnings copies one group's warnings into owned memory, including fresh
// SessionId pointees.
func ownWarnings(warnings []protocol.Warning) []protocol.Warning {
	out := make([]protocol.Warning, len(warnings))
	for i, warning := range warnings {
		out[i] = warning
		if warning.SessionId != nil {
			sessionID := *warning.SessionId
			out[i].SessionId = &sessionID
		}
	}
	return out
}

// warningIdentity is the contracted (source,session_id,kind,message)
// comparator within a producer group: a group's source and Session identity
// are fixed by its key, so dedupe and change detection compare Kind and
// Message only.
func warningIdentity(a, b protocol.Warning) bool {
	return a.Kind == b.Kind && a.Message == b.Message
}

// dedupeWarnings keeps the first occurrence of each (kind,message) pair in
// producer order; a group's source and Session identity are fixed by its key.
func dedupeWarnings(warnings []protocol.Warning) []protocol.Warning {
	if len(warnings) == 0 {
		return nil
	}
	out := make([]protocol.Warning, 0, len(warnings))
	for _, warning := range warnings {
		if !slices.ContainsFunc(out, func(existing protocol.Warning) bool { return warningIdentity(existing, warning) }) {
			out = append(out, warning)
		}
	}
	return out
}

func equalWarnings(a, b []protocol.Warning) bool {
	return slices.EqualFunc(a, b, warningIdentity)
}

// formatWarningRevision renders the store revision as its decimal-string wire
// form; revision 0 is a real initialized counter's value.
func formatWarningRevision(revision uint64) string {
	return strconv.FormatUint(revision, 10)
}

// setupWarningKinds are the retained setup diagnostics: no provider is
// connected, the primary type configures no model, or its model is
// unavailable because its provider is not connected or the model is
// incomplete. The connection samples come from the candidate's own capture,
// never a second catalog or a live environment read.
const (
	setupNoProviderKind      = "setup_no_provider"
	setupNoProviderMessage   = "No provider connected — configure a provider with credentials and at least one usable model."
	setupNoModelKind         = "setup_no_model"
	setupNoModelMessage      = "No model is configured. Select a model to get started."
	setupModelUnavailable    = "setup_model_unavailable"
	unavailableModelTemplate = "Configured model %q is unavailable because its provider is not connected or the model is incomplete."
)

// setupWarnings derives the setup group from one published candidate and its
// frozen credential observations under the retained rules: any connected
// provider clears no_provider, and the primary definition's configured model
// must resolve to a usable (positive-window) model of a connected provider.
// The connection samples come from the candidate's own capture, never a live
// environment read.
func setupWarnings(c *configuration, credentials map[string]config.EnvValue) []protocol.Warning {
	connected := func(prov *catalog.Provider) bool {
		return catalog.ProviderConnected(prov, capturedEnvIsSet(credentials))
	}
	var out []protocol.Warning
	anyConnected := false
	for _, prov := range c.catalog.Providers {
		if connected(prov) {
			anyConnected = true
			break
		}
	}
	if !anyConnected {
		out = append(out, protocol.Warning{Source: setupSource, Kind: setupNoProviderKind, Message: setupNoProviderMessage})
	}
	// The primary selection comes from the same agentTypes normalization
	// admission uses: its Model is the resolved ref, zero when the definition
	// configures none — every accepted nonempty model is already parseable,
	// so no parse-failure case exists.
	var configured model.ModelRef
	for _, at := range c.agentTypes() {
		if at.Name == "primary" {
			configured = at.Model
			break
		}
	}
	switch {
	case configured.IsZero():
		out = append(out, protocol.Warning{Source: setupSource, Kind: setupNoModelKind, Message: setupNoModelMessage})
	default:
		available := false
		prov, entry, lookupErr := c.catalog.LookupOrIncomplete(catalog.ModelRef{Provider: configured.Provider, Model: configured.Model})
		if lookupErr == nil && entry.ContextWindow > 0 && connected(prov) {
			available = true
		}
		if !available {
			out = append(out, protocol.Warning{Source: setupSource, Kind: setupModelUnavailable, Message: fmt.Sprintf(unavailableModelTemplate, configured.String())})
		}
	}
	return out
}

// catalogWarnings composes one catalog build warning's retained message form:
// the message (or its kind when empty) prefixed by the provider and model
// identities when present.
func catalogWarnings(warnings []catalog.Warning) []protocol.Warning {
	out := make([]protocol.Warning, 0, len(warnings))
	for _, w := range warnings {
		message := w.Message
		if message == "" {
			message = w.Kind
		}
		if w.Provider != "" && w.Model != "" {
			message = fmt.Sprintf("%s/%s: %s", w.Provider, w.Model, message)
		} else if w.Provider != "" {
			message = fmt.Sprintf("%s: %s", w.Provider, message)
		}
		out = append(out, protocol.Warning{Source: catalogSource, Kind: w.Kind, Message: message})
	}
	return out
}

// agentWarnings composes one definition parse's warnings with the retained
// "name: message" form.
func agentWarnings(warnings []agents.Warning) []protocol.Warning {
	out := make([]protocol.Warning, 0, len(warnings))
	for _, w := range warnings {
		out = append(out, protocol.Warning{Source: agentsSource, Kind: w.Kind, Message: w.Error()})
	}
	return out
}
