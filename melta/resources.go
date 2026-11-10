package melta

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"sync"
)

// resourceSet owns one Melta scope and its Workspace registry. Publication
// holds one reference; preparations transfer their references to the Harness.
type resourceSet struct {
	owner       *configurationService
	revision    uint64
	cancel      context.CancelFunc
	runtime     *scope
	composition *composition
	workspaces  *workspaceScopes

	mu        sync.Mutex
	holds     int
	warnings  [][3]string
	committed bool

	// heldNames are the declared names this resource set keeps excluded for
	// its whole candidate/open/retirement lifetime. They are released only
	// after shutdown and runtime cleanup finish.
	heldNames []string

	// activations are the executable extensions this set holds for its whole
	// lifetime. The last release stops them.
	activations []string
}

func (s *configurationService) prepareResources(Caller context.Context, snapshot *configuration) error {
	if s.composition == nil {
		return nil
	}
	previous := s.current()
	if previous != nil {
		equal := true
		for _, native := range s.natives {
			id := native.Declaration.ID
			a, aok := previous.extensions[id]
			b, bok := snapshot.extensions[id]
			aok = aok && sectionEnabled(a)
			bok = bok && sectionEnabled(b)
			if !aok && !bok {
				continue
			}
			if aok != bok || !bytes.Equal(a, b) {
				equal = false
				break
			}
		}
		if equal {
			equal = maps.Equal(previous.declaredBindings, snapshot.declaredBindings)
		}
		if equal {
			snapshot.resources = previous.resources
			snapshot.composition = previous.resources.composition
			return nil
		}
	}
	ctx, cancel := context.WithCancel(s.owner)
	stopCaller := context.AfterFunc(Caller, cancel)
	r := &resourceSet{owner: s, revision: snapshot.generation, cancel: cancel, holds: 1, composition: snapshot.composition}
	snapshot.composition.standardValues = s.composition.standardValues
	snapshot.composition.reportWarning = s.composition.reportWarning
	snapshot.composition.hosts = s.composition.hosts
	var report func(string, string, string)
	if s.composition.reportWarning != nil {
		report = func(id, kind, message string) {
			r.mu.Lock()
			if !r.committed {
				r.warnings = append(r.warnings, [3]string{id, kind, message})
				r.mu.Unlock()
				return
			}
			r.mu.Unlock()
			s.composition.reportWarning(id, kind, message)
		}
	}
	runtimeInfo := ScopeInfo{Kind: ScopeRuntime, DataDir: s.dataDir, Revision: fmt.Sprint(r.revision)}
	runtimeInfo.sections = snapshot.extensions
	runtimeInfo.declaredBindings = snapshot.declaredBindings
	// The candidate's declared names are held before any author Open. The
	// hold survives failed-prefix cleanup and transfers to the successful set.
	r.heldNames = snapshot.declaredSecretNames()
	s.holdCredentialNames(r.heldNames)
	activations, err := s.acquireExecutables(ctx, snapshot)
	if err != nil {
		cancel()
		s.releaseCredentialNames(r.heldNames)
		return err
	}
	r.activations = activations
	runtime, err := snapshot.composition.openScopeReporting(ctx, runtimeInfo, nil, report)
	stopCaller()
	if err != nil {
		cancel()
		s.releaseExecutables(r.activations)
		s.releaseCredentialNames(r.heldNames)
		if canceled := s.canceled(Caller); canceled != nil {
			return errors.Join(canceled, err)
		}
		return configurationFailure(err)
	}
	if err := s.canceled(Caller); err != nil {
		cancel()
		cleanupErr := runtime.cleanup()
		s.releaseExecutables(r.activations)
		s.releaseCredentialNames(r.heldNames)
		return errors.Join(err, cleanupErr)
	}
	r.runtime = runtime
	r.workspaces = newWorkspaceScopes(ctx, snapshot.composition, []*scope{runtime}, s.obs, snapshot.extensions)
	snapshot.resources = r
	return nil
}

// acquireExecutables retains the activation of every executable extension the
// candidate enables, starting the ones no set holds.
func (s *configurationService) acquireExecutables(ctx context.Context, snapshot *configuration) ([]string, error) {
	if s.supervisor == nil {
		return nil, nil
	}
	var acquired []string
	for _, executable := range s.executables {
		if !sectionEnabled(snapshot.extensions[executable.Declaration.ID]) {
			continue
		}
		if err := s.supervisor.acquire(ctx, executable, snapshot.credentialNames); err != nil {
			s.supervisor.release(executable.Declaration.ID)
			s.releaseExecutables(acquired)
			return nil, configurationFailure(err)
		}
		acquired = append(acquired, executable.Declaration.ID)
	}
	return acquired, nil
}

// releaseExecutables drops one set's activation holds in reverse order.
func (s *configurationService) releaseExecutables(ids []string) {
	if s.supervisor == nil {
		return
	}
	for i := len(ids) - 1; i >= 0; i-- {
		s.supervisor.release(ids[i])
	}
}

func (r *resourceSet) retain() {
	r.mu.Lock()
	r.holds++
	r.mu.Unlock()
}

func (r *resourceSet) release() error {
	r.mu.Lock()
	r.holds--
	last := r.holds == 0
	r.mu.Unlock()
	if !last {
		return nil
	}
	r.cancel() // builders must observe cancellation before shutdown joins them
	err := errors.Join(r.workspaces.shutdown(), r.runtime.close())
	r.owner.releaseExecutables(r.activations)
	r.owner.cleanupMu.Lock()
	r.owner.cleanupErr = errors.Join(r.owner.cleanupErr, err)
	r.owner.cleanupMu.Unlock()
	r.owner.releaseCredentialNames(r.heldNames)
	return err
}

func (r *resourceSet) commitWarnings() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	changed := false
	for _, w := range r.warnings {
		changed = r.owner.warnings.appendPlugin(w[0], w[1], w[2]) || changed
	}
	r.warnings = nil
	r.committed = true
	return changed
}

func (r *resourceSet) revisionString() string {
	if r == nil {
		return ""
	}
	return fmt.Sprint(r.revision)
}
