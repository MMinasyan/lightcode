package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
)

// recordingSvc stubs the AdapterService methods the navMu barriers exercise. It
// records the session id an operation targets and blocks the long owner calls so
// a test can force operation ordering. Unused methods are never called (a.ctx is
// nil, so the session_changed emit is skipped).
type recordingSvc struct {
	agent.AdapterService
	targeted chan string
	release  chan struct{}
}

func (s *recordingSvc) SubmitToSession(_ context.Context, id, _ string) (agent.SubmitResult, error) {
	s.targeted <- id
	<-s.release
	return agent.SubmitResult{}, nil
}

func (s *recordingSvc) CompactNowForSession(_ context.Context, id string) error {
	s.targeted <- id
	<-s.release
	return nil
}

func (s *recordingSvc) OpenSession(id string) (agent.SessionSummary, error) {
	return agent.SessionSummary{ID: id}, nil
}

func (s *recordingSvc) SessionSummaryForSession(id string) (agent.SessionSummary, error) {
	return agent.SessionSummary{ID: id}, nil
}

func (s *recordingSvc) CancelSession(string) error { return nil }

func newNavApp(svc agent.AdapterService) *App {
	app := &App{svc: svc}
	app.navMu.Lock()
	app.setCurrentSessionID("A")
	app.navMu.Unlock()
	return app
}

// TestNavRoutingCaptureOperationFirstTargetsA verifies a bounded current-target call
// reads and targets the pre-switch session while it holds navMu, and a switch
// cannot commit the new session until that call releases navMu.
func TestNavRoutingCaptureOperationFirstTargetsA(t *testing.T) {
	svc := &recordingSvc{targeted: make(chan string, 1), release: make(chan struct{})}
	app := newNavApp(svc)

	submitDone := make(chan struct{})
	go func() { _, _ = app.Submit("hi"); close(submitDone) }()

	if got := <-svc.targeted; got != "A" {
		t.Fatalf("Submit targeted %q, want A (pre-switch)", got)
	}

	switchDone := make(chan struct{})
	go func() { _ = app.SessionSwitch("B"); close(switchDone) }()
	select {
	case <-switchDone:
		t.Fatal("SessionSwitch committed while Submit still held navMu")
	case <-time.After(100 * time.Millisecond):
	}

	close(svc.release)
	<-submitDone
	select {
	case <-switchDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SessionSwitch did not commit after Submit released navMu")
	}
}

// TestNavCompactCarveoutContinuesAgainstA verifies the cancellable carve-out captures
// its id under navMu then releases it, so compaction continues against the captured
// session while a switch and a Cancel run freely against navMu.
func TestNavCompactCarveoutContinuesAgainstA(t *testing.T) {
	svc := &recordingSvc{targeted: make(chan string, 1), release: make(chan struct{})}
	app := newNavApp(svc)

	compactDone := make(chan struct{})
	go func() { _ = app.CompactNow(); close(compactDone) }()

	if got := <-svc.targeted; got != "A" {
		t.Fatalf("CompactNow targeted %q, want A", got)
	}

	// navMu is free during the long compaction: a switch commits and Cancel runs.
	switchDone := make(chan struct{})
	go func() { _ = app.SessionSwitch("B"); close(switchDone) }()
	select {
	case <-switchDone:
	case <-time.After(2 * time.Second):
		t.Fatal("SessionSwitch blocked while CompactNow ran: navMu was not released")
	}
	if err := app.Cancel(); err != nil {
		t.Fatalf("Cancel during compaction: %v", err)
	}

	close(svc.release)
	<-compactDone
}

// TestNavCloseWinsNavMuRejects verifies a call waiting for navMu rejects when close
// wins navMu first, without reaching the owner.
func TestNavCloseWinsNavMuRejects(t *testing.T) {
	app := &App{svc: &recordingSvc{}}
	app.navMu.Lock() // stand in for the operation/close that currently holds navMu

	submitErr := make(chan error, 1)
	go func() { _, err := app.Submit("hi"); submitErr <- err }()

	time.Sleep(50 * time.Millisecond) // let Submit reach navMu and block
	app.navClosed = true
	app.navMu.Unlock()

	select {
	case err := <-submitErr:
		if !errors.Is(err, errAdapterClosed) {
			t.Fatalf("Submit = %v, want errAdapterClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Submit did not return after close won navMu")
	}
}
