package agent

// Legacy current-session convenience surface. Production code addresses
// sessions explicitly; these helpers exist only for tests and therefore
// live in a test file. Bodies are unchanged from their previous location.

import (
	"context"
	"fmt"
	"strings"

	"github.com/MMinasyan/lightcode/internal/project"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// CompactNow triggers manual compaction. Must not be called while busy.
func (a *Agent) CompactNow(ctx context.Context) error {
	return a.ensureRuntime().compactNow(ctx)
}

func (rt *runtime) compactNow(ctx context.Context) error {
	a := rt.agent
	rt.mu.Lock()
	unit := rt.sessionLocked()
	if unit.busy {
		rt.mu.Unlock()
		return fmt.Errorf("cannot compact while a turn is running")
	}
	if !a.store.Active() {
		rt.mu.Unlock()
		return fmt.Errorf("no session open")
	}
	unit.busy = true
	compactCtx, cancel := context.WithCancel(ctx)
	unit.turnCancel = cancel
	unit.turnCtx = compactCtx
	rt.mu.Unlock()

	defer func() {
		rt.mu.Lock()
		unit.busy = false
		unit.turnCancel = nil
		unit.turnCtx = nil
		rt.mu.Unlock()
		cancel()
		// Manual compaction uses the same busy gate as a turn. If input was
		// queued while compaction was running, wake the backend drainer now that
		// the gate is open again.
		rt.nudgeQueueDrainer()
		rt.nudgeSignalScheduler()
	}()

	return a.runCompaction(compactCtx, false)
}

// Submit is the single entry point for new user input. If the agent is idle
// with an empty queue it starts a turn immediately; otherwise it appends to the
// backend-owned in-memory queue (drained automatically after the active turn
// ends). It rejects with an error while a session change is in flight rather
// than accepting input it would then discard. Any queue mutation emits a
// versioned EventQueueChanged.
func (a *Agent) Submit(ctx context.Context, content string) (SubmitResult, error) {
	return a.ensureRuntime().submit(ctx, a.session, content)
}

// QueueSnapshot returns a versioned copy of the current queue, for adapter
// hydration (subscribe-then-GET: register the queue_changed handler first).
func (a *Agent) QueueSnapshot() QueueState {
	return a.ensureRuntime().queueSnapshot(a.session)
}

// Cancel aborts the current turn.
func (a *Agent) Cancel() error {
	cancel := a.ensureRuntime().turnCancelSnapshot(a.session)
	if cancel != nil {
		cancel()
	}
	if a.gate != nil {
		a.gate.CancelSession(a.currentPermissionSessionID())
	}
	return nil
}

// CurrentModel returns the active model identity and catalog metadata.
func (a *Agent) CurrentModel() ModelInfo {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	unit := rt.sessionLocked()
	a.ensureActiveModelForSessionLocked(unit)
	a.setWarningGroup("setup", a.setupWarningsLocked())
	return a.modelInfo(unit.currentRef)
}

// SessionSwitch closes the current session and loads another.
func (a *Agent) SessionSwitch(id string) error {
	id = strings.TrimSpace(id)
	if _, err := a.sessionsRootForUserManagedSession(id); err != nil {
		return err
	}
	// Mark the transition and register the clear BEFORE cancelAndWaitIdle so it
	// fires on every return — including the pre-lock error return below — and
	// never leaves transitioning stuck true.
	a.ensureRuntime().beginTransition()
	defer a.ensureRuntime().endTransition()
	if err := a.cancelAndWaitIdle(); err != nil {
		return err
	}
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()

	if a.store.Active() && a.store.SessionID() == id {
		a.refreshCurrentSessionProjectLocked()
		return nil // same-session no-op: queue is preserved
	}
	var proj *project.Project
	if a.projects != nil {
		proj, _ = a.projects.Current()
	}
	oldID := a.store.SessionID()
	if _, err := a.store.Close(); err != nil {
		return err
	}
	// The old session is now irreversibly detached: clear the queue here, not
	// after LoadSession — a LoadSession failure must not leave the queue bound
	// to a closed (inactive) session, which endTransition could not drain.
	a.ensureRuntime().clearQueueLocked()
	if err := a.store.LoadSession(id); err != nil {
		return err
	}
	a.setSessionProject(a.session, proj)
	delete(a.sessions, oldID)
	a.setCurrentSessionLocked(a.session)
	meta, err := a.store.Meta()
	if err == nil && metaState(meta.State) == snapshot.StateArchived {
		_ = a.store.SetState(snapshot.StateActive)
		_ = a.store.TouchActivity()
	}
	if err := a.loadHistoryIntoLoop(); err != nil {
		return err
	}
	a.resetFileTracker()
	a.loadTokensFromDisk()
	if err := a.reloadLocked(); err != nil {
		return err
	}
	a.restoreModelFromSession()
	a.tokensMu.Lock()
	a.lastContextUsed = 0
	a.tokensMu.Unlock()
	if a.lspDiagnostics != nil {
		a.lspDiagnostics.Reset()
	}
	return nil
}

// SessionNew closes the current session and starts fresh.
func (a *Agent) SessionNew() error {
	// Registered before the lock so it emits after unlock (defer LIFO).
	var clearedVersion int
	var queueCleared bool
	var eventSessionID string
	var eventProjectID string
	defer func() {
		if queueCleared {
			a.emitEvent(Event{Kind: EventQueueChanged, SessionID: eventSessionID, ProjectID: eventProjectID, Queue: emptyQueue(), QueueVersion: clearedVersion})
		}
	}()
	a.ensureRuntime().mu.Lock()
	if a.ensureRuntime().sessionLocked().busy {
		a.ensureRuntime().mu.Unlock()
		return fmt.Errorf("cannot start new session while a turn is running")
	}
	defer a.ensureRuntime().mu.Unlock()

	eventSessionID = sessionIDOf(a.session)
	eventProjectID = a.session.projectID
	if _, err := a.store.Close(); err != nil {
		return err
	}
	delete(a.sessions, eventSessionID)
	_, clearedVersion, queueCleared = a.ensureRuntime().clearQueueLocked()
	a.resetCurrentSessionStateLocked()
	return nil
}

func (a *Agent) SessionMessages() []DisplayMessage {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if a.store == nil || !a.store.Active() {
		return nil
	}
	msgs, _ := a.messagesForFrontendForSession("")
	return msgs
}
