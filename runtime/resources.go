package runtime

import (
	"bytes"
	"context"
	"errors"
	"sync"
)

// resourceSet owns one Runtime scope and its Workspace registry. Publication
// holds one reference; preparations transfer their references to the Harness.
type resourceSet struct {
	owner       *configurationService
	cancel      context.CancelFunc
	runtime     *scope
	composition *composition
	workspaces  *workspaceScopes

	mu        sync.Mutex
	holds     int
	warnings  [][3]string
	committed bool
}

func (s *configurationService) prepareResources(caller context.Context, snapshot *configuration) error {
	if s.composition == nil {
		return nil
	}
	previous := s.current()
	if previous != nil {
		equal := true
		for _, p := range s.plugins {
			a, aok := previous.extensions[p.ID]
			b, bok := snapshot.extensions[p.ID]
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
			snapshot.resources = previous.resources
			snapshot.composition = previous.resources.composition
			return nil
		}
	}
	ctx, cancel := context.WithCancel(s.owner)
	stopCaller := context.AfterFunc(caller, cancel)
	r := &resourceSet{owner: s, cancel: cancel, holds: 1, composition: snapshot.composition}
	snapshot.composition.standardValues = s.composition.standardValues
	snapshot.composition.reportWarning = s.composition.reportWarning
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
	runtime, err := snapshot.composition.openScopeReporting(ctx, ScopeInfo{Kind: ScopeRuntime, DataDir: s.dataDir}, nil, report)
	stopCaller()
	if err != nil {
		cancel()
		if canceled := s.canceled(caller); canceled != nil {
			return errors.Join(canceled, err)
		}
		return configurationFailure(err)
	}
	if err := s.canceled(caller); err != nil {
		cancel()
		return errors.Join(err, runtime.cleanup())
	}
	r.runtime = runtime
	r.workspaces = newWorkspaceScopes(ctx, snapshot.composition, []*scope{runtime}, s.obs)
	snapshot.resources = r
	return nil
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
	r.owner.cleanupMu.Lock()
	r.owner.cleanupErr = errors.Join(r.owner.cleanupErr, err)
	r.owner.cleanupMu.Unlock()
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
