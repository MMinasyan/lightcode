package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	start := time.Now()
	if _, err = app.SessionList("active"); err != nil || time.Since(start) > time.Second {
		t.Fatalf("present Wails SessionList = %v after %v", err, time.Since(start))
	}
	if _, err = app.ProjectCurrent(); err != nil || time.Since(start) > time.Second {
		t.Fatalf("present Wails ProjectCurrent = %v after %v", err, time.Since(start))
	}
	if content, readErr := app.ReadFileContent("visible.txt"); readErr != nil || content != "source" {
		t.Fatalf("present Wails ReadFileContent = %q, %v", content, readErr)
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
	if content, err := app.ReadFileContent("visible.txt"); err != nil || content != "destination" {
		t.Fatalf("Wails present destination file read under identity contention = %q, %v", content, err)
	}
	if sessions, listErr := svc.SessionListForProjectPath(other, "active"); listErr == nil && len(sessions) != 0 {
		t.Fatalf("contended existing destination published %d sessions", len(sessions))
	}
	app.shutdown(context.Background())
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
	app.shutdown(context.Background())
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
