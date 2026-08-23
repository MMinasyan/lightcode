package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

func cliIdentityLock(root, projectPath string) string {
	abs, _ := filepath.Abs(projectPath)
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return filepath.Join(root, ".locks", "identity", hex.EncodeToString(sum[:])+".lock")
}

func TestCLIProjectContentionProductionPaths(t *testing.T) {
	svc, _ := newTestAgent(t)
	source, err := svc.NewSession("", "primary")
	if err != nil {
		t.Fatal(err)
	}
	cli := New(svc)
	cli.setCurrentSessionID(source)
	cli.out = &bytes.Buffer{}
	cli.readKeyFn = func() (keyMsg, error) { return keyMsg{Special: keyEscape}, nil }
	cli.showProjectMenu()
	lock, err := atomicfs.Acquire(cliIdentityLock(svc.Projects().Root(), svc.ProjectRoot()))
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	cli.showProjectMenu()
	if time.Since(started) > time.Second {
		t.Fatalf("present CLI project menu took %v", time.Since(started))
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}

	target := t.TempDir()
	if _, err := svc.ProjectCurrentForPath(target); err != nil {
		t.Fatal(err)
	}
	lock, err = atomicfs.Acquire(cliIdentityLock(svc.Projects().Root(), target))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	cli.projectSwitch(target)
	if got := cli.scope.ProjectPath(); got != svc.ProjectRoot() {
		t.Fatalf("CLI existing-destination switch changed source selection to %q", got)
	}
	wantCurrent(t, cli, source)
	if !sourceClaimRetained(cli, source) {
		t.Fatalf("source session %q lost its live claim after existing-destination contention", source)
	}
	absent := t.TempDir()
	absentLock, err := atomicfs.Acquire(cliIdentityLock(svc.Projects().Root(), absent))
	if err != nil {
		t.Fatal(err)
	}
	defer absentLock.Release()
	cli.projectSwitch(absent)
	if got := cli.scope.ProjectPath(); got != svc.ProjectRoot() {
		t.Fatalf("CLI source selection changed to %q", got)
	}
	wantCurrent(t, cli, source)
	if !sourceClaimRetained(cli, source) {
		t.Fatalf("source session %q lost its live claim after absent-destination contention", source)
	}
	releaseSource, err := svc.ReserveSelectionSource(source)
	if err != nil {
		t.Fatalf("source reservation after busy switch: %v", err)
	}
	releaseSource()
}
