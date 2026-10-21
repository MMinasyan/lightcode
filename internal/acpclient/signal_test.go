package acpclient_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/acpclient"
	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestACPSignalHelper is the subprocess entry: one bridge parked on open
// stdin with isolated startup inputs, reporting readiness on stderr. It is
// skipped in an ordinary run.
func TestACPSignalHelper(t *testing.T) {
	if os.Getenv("ACP_SIGNAL_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	dataDir := os.Getenv("ACP_SIGNAL_DATADIR")
	runner := acpclient.New(acpclient.Options{
		Startup: clienthost.Options{
			DataDir:    dataDir,
			ConfigPath: filepath.Join(dataDir, "config.json"),
			Executable: filepath.Join(dataDir, "absent-runtime"),
		},
		Workspace: "/ws",
	})
	// The helper's own signal context covers the window before Run installs
	// its handler; later signals reach both and cancel the same run.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	fmt.Fprintln(os.Stderr, "acp-signal-ready")
	if err := runner.Run(ctx, os.Stdin, io.Discard); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(os.Stderr, "run: %v\n", err)
		os.Exit(3)
	}
}

// TestACPSignalCancelsRun drives the real signal path in an isolated
// subprocess: SIGTERM cancels the parked Run and the process joins within the
// bound, never signalling the main test process.
func TestACPSignalCancelsRun(t *testing.T) {
	dataDir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=TestACPSignalHelper", "-test.v")
	cmd.Env = append(os.Environ(), "ACP_SIGNAL_HELPER=1", "ACP_SIGNAL_DATADIR="+dataDir)
	stdin, err := cmd.StdinPipe()
	testassert.NoError(t, err)
	defer stdin.Close()
	stderr, err := cmd.StderrPipe()
	testassert.NoError(t, err)
	testassert.NoError(t, cmd.Start())
	joined := false
	defer func() {
		if !joined {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	}()

	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if scanner.Text() == "acp-signal-ready" {
				close(ready)
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("the helper never reported readiness")
	}
	testassert.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	select {
	case err := <-waited:
		joined = true
		testassert.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the helper did not join within the bounded wait")
	}
}
