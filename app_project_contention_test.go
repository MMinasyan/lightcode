package main

import (
	"time"

	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

func appIdentityLock(root, projectPath string) string {
	abs, _ := filepath.Abs(projectPath)
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return filepath.Join(root, ".locks", "identity", hex.EncodeToString(sum[:])+".lock")
}

func TestWailsProjectContentionProductionPaths(t *testing.T) {
	svc := newAppTestAgent(t)
	sourceID, err := svc.NewSession("", "primary")
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(svc)
	app.agent = svc
	app.started = true
	app.setCurrentSessionID(sourceID)
	app.seedPresented(sourceID)
	t.Cleanup(func() { app.shutdown(context.Background()) })
	if err := os.WriteFile(filepath.Join(svc.ProjectRoot(), "visible.txt"), []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ProjectCurrentForPath(svc.ProjectRoot()); err != nil {
		t.Fatal(err)
	}
	lock, err := atomicfs.Acquire(appIdentityLock(svc.Projects().Root(), svc.ProjectRoot()))
	if err != nil {
		t.Fatal(err)
	}
	// The reads must complete while another process holds the identity lock:
	// completion is the positive fact and the failsafe turns a blocking
	// regression into a failure, with no machine-speed latency oracle.
	listDone := make(chan error, 1)
	go func() {
		_, err := app.SessionList("active")
		listDone <- err
	}()
	select {
	case err := <-listDone:
		if err != nil {
			t.Fatalf("present Wails SessionList = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SessionList blocked on the held identity lock")
	}
	currentDone := make(chan error, 1)
	go func() {
		_, err := app.ProjectCurrent()
		currentDone <- err
	}()
	select {
	case err := <-currentDone:
		if err != nil {
			t.Fatalf("present Wails ProjectCurrent = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ProjectCurrent blocked on the held identity lock")
	}
	if result, readErr := app.ReadFileContent(sourceID, "visible.txt"); readErr != nil || result.Content != "source" {
		t.Fatalf("present Wails ReadFileContent = %q, %v", result.Content, readErr)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}

	other := t.TempDir()
	if _, err := svc.ProjectCurrentForPath(other); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "visible.txt"), []byte("destination"), 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err = atomicfs.Acquire(appIdentityLock(svc.Projects().Root(), other))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if err := app.ProjectSwitch(other); !errors.Is(err, agent.ErrProjectBusy) {
		t.Fatalf("Wails ProjectSwitch under identity contention = %v, want ErrProjectBusy", err)
	}
	if app.routeProjectPath != svc.ProjectRoot() {
		t.Fatalf("ProjectSwitch changed route to %q, want %q", app.routeProjectPath, svc.ProjectRoot())
	}
	if app.currentSessionID() != sourceID || app.presented != sourceID {
		t.Fatalf("ProjectSwitch changed source selection/presentation: session=%q presented=%q", app.currentSessionID(), app.presented)
	}
	app.routeProjectPath = other
	if err := app.SessionNew(); !errors.Is(err, agent.ErrProjectBusy) {
		t.Fatalf("Wails SessionNew under identity contention = %v, want ErrProjectBusy", err)
	}
	if result, err := app.ReadFileContent(sourceID, "visible.txt"); err != nil || result.Content != "source" {
		t.Fatalf("Wails presented source file read under routing contention = %q, %v", result.Content, err)
	}
	if sessions, listErr := svc.SessionListForProjectPath(other, "active"); listErr == nil && len(sessions) != 0 {
		t.Fatalf("contended existing destination published %d sessions", len(sessions))
	}
}

func TestWailsStartupProjectBusyThenSessionNewRetries(t *testing.T) {
	svc := newAppTestAgent(t)
	lock, err := atomicfs.Acquire(appIdentityLock(svc.Projects().Root(), svc.ProjectRoot()))
	if err != nil {
		t.Fatal(err)
	}
	app := newTestApp(svc)
	app.emitFn = func(string, any) {}
	app.titleFn = func(string) {}
	t.Cleanup(func() { app.shutdown(context.Background()) })
	output := captureStderrForAppTest(t, func() { app.startup(context.Background()) })
	if app.currentSessionID() != "" {
		t.Fatalf("busy startup selected session %q", app.currentSessionID())
	}
	if strings.Count(output, "startup project:") != 1 {
		t.Fatalf("startup stderr = %q, want one project-busy diagnostic", output)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	if err := app.SessionNew(); err != nil {
		t.Fatalf("SessionNew after project holder release: %v", err)
	}
}

func captureStderrForAppTest(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	fn()
	_ = w.Close()
	os.Stderr = old
	data, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
