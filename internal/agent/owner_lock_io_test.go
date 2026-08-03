package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"

	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/engine/message"
)

// TestDurableIOAgainstOwnerLocks asserts the position of each site's durable
// I/O relative to its owner lock under the current arrangement: the fork
// staging tree copy runs outside runtime.mu while every candidate read runs
// under it, resume's listing/history loads run under lifecycleMu, the tokens
// file read runs outside the unit's tokensMu, and the tokens file write runs
// inside it.
//
// The assertion needs durableReadHook because lock ownership is unobservable
// from outside the locked code. The hook fires at the exact moment each durable
// I/O is about to run, and each scenario's closure probes the site's owner lock
// with TryLock: a lock held by the current goroutine cannot be acquired, so the
// probe reads true exactly when the I/O would run under the lock, while an
// uncontended lock is acquired and immediately released, so a false reading
// means the I/O is running outside it.
func TestDurableIOAgainstOwnerLocks(t *testing.T) {
	t.Run("fork_tree_copy_outside_runtime_lock", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		appendUserTurn(t, a, "one")
		sourceID := a.SessionCurrent().ID
		obs := probeOwnerLock(a, &a.ensureRuntime().mu)
		defer func() { obs() }()
		if err := a.ForkSessionForSession(sourceID, 1); err != nil {
			t.Fatalf("fork: %v", err)
		}
		// First fire is the hoisted tree copy; the candidate reads that follow
		// run back under the re-acquired lock.
		got := obs()
		if len(got) == 0 {
			t.Fatal("no durable fork I/O observed")
		}
		if got[0] {
			t.Fatal("fork tree copy ran while runtime.mu was held")
		}
		for i, held := range got[1:] {
			if !held {
				t.Fatalf("fork candidate read %d ran outside runtime.mu", i+1)
			}
		}
	})

	t.Run("fork_action_copy_outside_runtime_lock", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		clicked := appendUserTurn(t, a, "one")
		sourceID := a.SessionCurrent().ID
		obs := probeOwnerLock(a, &a.ensureRuntime().mu)
		defer func() { obs() }()
		if _, err := a.ApplyTurnActionForSession(sourceID, clicked, TurnActionFork, false); err != nil {
			t.Fatalf("fork action: %v", err)
		}
		got := obs()
		if len(got) == 0 {
			t.Fatal("no durable fork I/O observed")
		}
		if got[0] {
			t.Fatal("fork tree copy ran while runtime.mu was held")
		}
		for i, held := range got[1:] {
			if !held {
				t.Fatalf("fork candidate read %d ran outside runtime.mu", i+1)
			}
		}
	})

	t.Run("resume_listing_and_loading_under_lifecycle_lock", func(t *testing.T) {
		t.Setenv("LIGHTCODE_RESUME_KEY", "test-key")
		home, projectRoot := t.TempDir(), t.TempDir()
		a := newResumeRaceAgent(t, home, projectRoot)
		proj, err := a.projects.Ensure()
		if err != nil {
			t.Fatalf("ensure project: %v", err)
		}
		if err := a.store.AttachSessionsRoot(a.projects.SessionsRoot(proj.ID), a.projects.Root(), proj.ID); err != nil {
			t.Fatalf("attach sessions root: %v", err)
		}
		if err := a.store.BeginNewSession(projectRoot); err != nil {
			t.Fatalf("begin session: %v", err)
		}
		// A session is only persisted (and thus resumable) once it carries a
		// completed turn.
		raw, err := json.Marshal(message.NewText(message.RoleUser, "hello"))
		if err != nil {
			t.Fatalf("marshal message: %v", err)
		}
		if err := a.store.AppendMessage(1, raw); err != nil {
			t.Fatalf("append message: %v", err)
		}
		if err := a.store.MarkTurnComplete(1); err != nil {
			t.Fatalf("mark turn complete: %v", err)
		}
		if _, err := a.store.Close(); err != nil {
			t.Fatalf("close session: %v", err)
		}

		obs := probeOwnerLock(a, &a.ensureRuntime().lifecycleMu)
		defer func() { obs() }()
		if _, err := a.resumeMostRecent(); err != nil {
			t.Fatalf("resume: %v", err)
		}
		got := obs()
		if len(got) == 0 {
			t.Fatal("no durable resume I/O observed")
		}
		for i, held := range got {
			if !held {
				t.Fatalf("resume durable I/O %d ran outside lifecycleMu", i)
			}
		}
	})

	t.Run("tokens_read_outside_tokens_mutex", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		unit := a.session
		writeTokenFile(t, unit, TokenEntry{Provider: "test", Model: "test-model", Input: 11, Output: 7, Known: true})
		obs := probeOwnerLock(a, &unit.tokensMu)
		defer func() { obs() }()
		a.loadTokensFromDiskForSession(unit)
		got := obs()
		if len(got) == 0 {
			t.Fatal("no durable tokens read observed")
		}
		for i, held := range got {
			if held {
				t.Fatalf("tokens file read %d ran while tokensMu was held", i)
			}
		}
		report, err := a.TokenUsageForSession(unit.store.SessionID())
		if err != nil {
			t.Fatalf("TokenUsageForSession: %v", err)
		}
		if report.Total.Input != 11 || report.Total.Output != 7 {
			t.Fatalf("loaded token report = %#v, want 11/7", report.Total)
		}
	})

	t.Run("tokens_write_under_tokens_mutex", func(t *testing.T) {
		a := newCatalogBackedTestAgent(t)
		if _, err := a.NewSession("", "primary"); err != nil {
			t.Fatalf("NewSession: %v", err)
		}
		unit := a.session
		obs := probeOwnerLock(a, &unit.tokensMu)
		defer func() { obs() }()
		ref := coremodel.ModelRef{Provider: "test", Model: "test-model"}
		a.recordUsageForSession(unit, loop.Event{Model: ref.Model, ModelRef: ref, UsageKnown: true, Cache: 1, Input: 2, Output: 3})
		got := obs()
		if len(got) == 0 {
			t.Fatal("no durable tokens write observed")
		}
		for i, held := range got {
			if !held {
				t.Fatalf("tokens file write %d ran outside tokensMu", i)
			}
		}
		// The write landed inside the section and carries the recorded entry.
		data, err := os.ReadFile(filepath.Join(unit.store.Dir(), tokensFileName))
		if err != nil {
			t.Fatalf("read persisted tokens: %v", err)
		}
		var entries []TokenEntry
		if err := json.Unmarshal(data, &entries); err != nil {
			t.Fatalf("unmarshal persisted tokens: %v", err)
		}
		if len(entries) != 1 || entries[0].Input != 2 || entries[0].Output != 3 || entries[0].Cache != 1 {
			t.Fatalf("persisted entries = %#v, want one cache 1 / input 2 / output 3 entry", entries)
		}
	})
}

// probeOwnerLock installs a durableReadHook that records, for every fire,
// whether m was held at that moment, and returns a func that restores the hook
// and reports the recorded observations in fire order.
func probeOwnerLock(a *Agent, m *sync.Mutex) func() []bool {
	var obs []bool
	a.durableReadHook = func() {
		if !m.TryLock() {
			obs = append(obs, true)
		} else {
			m.Unlock()
			obs = append(obs, false)
		}
	}
	return func() []bool {
		a.durableReadHook = nil
		return obs
	}
}
