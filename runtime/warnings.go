package runtime

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"sync"

	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

// warningSources is the closed retained source set in the fixed group order
// every read preserves.
var warningSources = []protocol.WarningSource{"setup", "prompt", "catalog", "agents", "lsp", "protocol"}

// warningGroup is one store group's key: a source with an optional Session
// identity. Setup, catalog, agents, and LSP groups are global; prompt and
// protocol groups are per Session.
type warningGroup struct {
	source    protocol.WarningSource
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

// setGroup replaces one group, deduplicating identical (kind,message) pairs
// in producer order. It is the non-reentrant mutation core: an unchanged
// replacement advances nothing; a changed one replaces or deletes the group
// and advances the revision — a successful preparation with no warnings
// clears its Session's prior group. Publication (the warning_changed hint)
// is composed by the caller inside its observation section; this core never
// publishes.
func (s *warningStore) setGroup(group warningGroup, warnings []protocol.Warning) bool {
	if s == nil {
		return false
	}
	deduped := dedupeWarnings(warnings)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	if existing, ok := s.groups[group]; ok && equalWarnings(existing, deduped) {
		return false
	}
	if len(deduped) == 0 {
		if _, ok := s.groups[group]; !ok {
			return false // clearing an absent group changed nothing
		}
		delete(s.groups, group)
	} else {
		s.groups[group] = deduped
	}
	s.revision++
	return true
}

// setGlobal replaces one global group from a successful configuration
// publication.
func (s *warningStore) setGlobal(source protocol.WarningSource, warnings []protocol.Warning) bool {
	return s.setGroup(warningGroup{source: source}, warnings)
}

// setSessionPrompt replaces one Session's prompt group from a successful
// preparation.
func (s *warningStore) setSessionPrompt(sessionID string, warnings []protocol.Warning) bool {
	return s.setGroup(warningGroup{source: "prompt", sessionID: sessionID}, warnings)
}

// appendProtocol adds one execution's protocol diagnostics to the owning
// Session's protocol group, deduplicating identical (kind,message) pairs
// while preserving producer order. Encoding succeeded whenever diagnostics
// are present, including a physical transport failure after encoding. The
// core never publishes.
func (s *warningStore) appendProtocol(sessionID string, warnings []protocol.Warning) bool {
	if s == nil || len(warnings) == 0 {
		return false
	}
	return s.appendGroup(warningGroup{source: "protocol", sessionID: sessionID}, warnings)
}

// addLSP appends one Runtime-scoped plugin report to the Runtime-global LSP
// group — the retained append shape, never a replacement — deduplicating
// identical (kind,message) pairs in producer order. Reports after closure are
// ignored; the core never publishes.
func (s *warningStore) addLSP(kind, message string) bool {
	if s == nil {
		return false
	}
	return s.appendGroup(warningGroup{source: "lsp"}, []protocol.Warning{{Source: "lsp", Kind: kind, Message: message}})
}

// appendGroup is the shared append core: producer order with (kind,message)
// dedupe, change-gated on real growth.
func (s *warningStore) appendGroup(group warningGroup, warnings []protocol.Warning) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	merged := append([]protocol.Warning(nil), s.groups[group]...)
	changed := false
	for _, warning := range warnings {
		if slices.ContainsFunc(merged, func(existing protocol.Warning) bool { return warningIdentity(existing, warning) }) {
			continue
		}
		merged = append(merged, warning)
		changed = true
	}
	if !changed {
		return false
	}
	s.groups[group] = merged
	s.revision++
	return true
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

// read is the shared captured read: global groups in fixed source order,
// then per-Session groups in sorted Session ID order with each Session's
// sources in fixed order. A nil filter is the complete unfiltered read; a
// Session filter looks up the global groups plus only that Session's own
// groups directly — a foreign Session's warnings never appear. The returned
// warnings are fully owned copies (including every SessionId pointee), so a
// caller's mutation cannot reach the groups, the dedupe, or another read.
// The revision and the warnings are captured once under the same mutex hold.
func (s *warningStore) read(sessionFilter *string) (uint64, []protocol.Warning) {
	s.mu.Lock()
	defer s.mu.Unlock()
	revision := s.revision
	out := make([]protocol.Warning, 0) // a present collection reads [], never null
	for _, source := range warningSources {
		if warnings, ok := s.groups[warningGroup{source: source}]; ok {
			out = append(out, ownWarnings(warnings)...)
		}
	}
	if sessionFilter == nil {
		seen := make(map[string]bool, len(s.groups))
		var ids []string
		for group := range s.groups {
			if group.sessionID != "" && !seen[group.sessionID] {
				seen[group.sessionID] = true
				ids = append(ids, group.sessionID)
			}
		}
		sort.Strings(ids)
		for _, id := range ids {
			for _, source := range warningSources {
				if warnings, ok := s.groups[warningGroup{source: source, sessionID: id}]; ok {
					out = append(out, ownWarnings(warnings)...)
				}
			}
		}
	} else {
		for _, source := range warningSources {
			if warnings, ok := s.groups[warningGroup{source: source, sessionID: *sessionFilter}]; ok {
				out = append(out, ownWarnings(warnings)...)
			}
		}
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

// warningIdentity is the contracted (kind,message) comparator: a group's
// Session identity is fixed by its key, so dedupe and change detection
// compare Kind and Message only.
func warningIdentity(a, b protocol.Warning) bool {
	return a.Kind == b.Kind && a.Message == b.Message
}

// dedupeWarnings keeps the first occurrence of each (kind,message) pair in
// producer order; a group's Session identity is fixed by its key.
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
// incomplete. The connection samples are live-env presentations only, never
// a second catalog.
const (
	setupNoProviderKind      = "setup_no_provider"
	setupNoProviderMessage   = "No provider connected — configure a provider with credentials and at least one usable model."
	setupNoModelKind         = "setup_no_model"
	setupNoModelMessage      = "No model is configured. Select a model to get started."
	setupModelUnavailable    = "setup_model_unavailable"
	unavailableModelTemplate = "Configured model %q is unavailable because its provider is not connected or the model is incomplete."
)

// setupWarnings derives the setup group from one published candidate under
// the retained rules: any connected provider clears no_provider, and the
// primary definition's configured model must resolve to a usable
// (positive-window) model of a connected provider. The connection samples
// are live-env presentations only, never a second catalog.
func setupWarnings(c *configuration) []protocol.Warning {
	connected := func(prov *catalog.Provider) bool {
		return catalog.ProviderConnected(prov, liveEnvIsSet)
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
		out = append(out, protocol.Warning{Source: "setup", Kind: setupNoProviderKind, Message: setupNoProviderMessage})
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
		out = append(out, protocol.Warning{Source: "setup", Kind: setupNoModelKind, Message: setupNoModelMessage})
	default:
		available := false
		prov, entry, lookupErr := c.catalog.LookupOrIncomplete(catalog.ModelRef{Provider: configured.Provider, Model: configured.Model})
		if lookupErr == nil && entry.ContextWindow > 0 && connected(prov) {
			available = true
		}
		if !available {
			out = append(out, protocol.Warning{Source: "setup", Kind: setupModelUnavailable, Message: fmt.Sprintf(unavailableModelTemplate, configured.String())})
		}
	}
	return out
}

// catalogWarning composes one catalog build warning's retained message form:
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
		out = append(out, protocol.Warning{Source: "catalog", Kind: w.Kind, Message: message})
	}
	return out
}

// agentWarnings composes one definition parse's warnings with the retained
// "name: message" form.
func agentWarnings(warnings []agents.Warning) []protocol.Warning {
	out := make([]protocol.Warning, 0, len(warnings))
	for _, w := range warnings {
		out = append(out, protocol.Warning{Source: "agents", Kind: w.Kind, Message: w.Error()})
	}
	return out
}
