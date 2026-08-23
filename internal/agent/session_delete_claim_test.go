package agent

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/memory"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

// sessionClaimHolder* select the child half of
// TestSessionDeleteRefusedWhileAnotherProcessHoldsTheClaim and name the
// session whose claim it holds.
const (
	sessionClaimHolderRootEnv    = "LIGHTCODE_SESSION_CLAIM_HOLDER_ROOT"
	sessionClaimHolderProjectEnv = "LIGHTCODE_SESSION_CLAIM_HOLDER_PROJECT"
	sessionClaimHolderSessionEnv = "LIGHTCODE_SESSION_CLAIM_HOLDER_SESSION"
)

// TestSessionDeleteRefusedWhileAnotherProcessHoldsTheClaim covers delete
// against a foreign-held claim, beside the archive and sweep rows that
// already cover contention for their own operations. Delete is the only
// destructive session operation that also removes summaries, so a refusal
// that still ran the summaries removal would destroy a live session's
// history. While a self-exec child owns the claim the delete is refused with
// the user-facing contention message, the session directory and its indexed
// summaries are both untouched, and the same delete succeeds once the child
// releases.
func TestSessionDeleteRefusedWhileAnotherProcessHoldsTheClaim(t *testing.T) {
	const holdBound = 30 * time.Second

	if root := os.Getenv(sessionClaimHolderRootEnv); root != "" {
		// Child: hold the session claim until the parent closes stdin.
		claim, ok, err := snapshot.AcquireSessionClaim(root,
			os.Getenv(sessionClaimHolderProjectEnv),
			os.Getenv(sessionClaimHolderSessionEnv))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "child could not take the session claim")
			os.Exit(3)
		}
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		if err := snapshot.ReleaseSessionClaim(claim, os.Getenv(sessionClaimHolderSessionEnv)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		os.Exit(0)
	}

	first, second := newSharedHomeAgentPair(t)
	projectsRoot := first.projects.Root()
	id, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	seedCompleteTurns(t, first, 1)
	// The project record is created lazily with the first session.
	proj, err := first.projects.Current()
	if err != nil || proj == nil {
		t.Fatalf("current project = %+v, %v", proj, err)
	}
	// Archive releases the creating owner's claim, so the session is
	// persisted-only for both owners and the delete below takes a temporary
	// claim of its own — the path the child contends with.
	if err := first.SessionArchive(id); err != nil {
		t.Fatalf("SessionArchive: %v", err)
	}

	memStore := memory.NewStoreWithEmbedder(deterministicMemoryEmbedder{}, projectsRoot, first.home)
	if err := memStore.IndexSummary(id, proj.ID, proj.Name, "## Goal\nsummary body", "now", "/c.json"); err != nil {
		t.Fatalf("IndexSummary: %v", err)
	}
	second.memoryHooks = memStore

	sessionDir := filepath.Join(projectsRoot, proj.ID, "sessions", id)
	summariesDir := filepath.Join(first.home, ".lightcode", "summaries", id)
	for _, dir := range []string{sessionDir, summariesDir} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("seeded %s: %v", dir, err)
		}
	}

	holderCtx, holderCancel := context.WithTimeout(context.Background(), holdBound)
	defer holderCancel()
	cmd := exec.CommandContext(holderCtx, os.Args[0], "-test.run=^TestSessionDeleteRefusedWhileAnotherProcessHoldsTheClaim$")
	cmd.Env = append(os.Environ(),
		sessionClaimHolderRootEnv+"="+projectsRoot,
		sessionClaimHolderProjectEnv+"="+proj.ID,
		sessionClaimHolderSessionEnv+"="+id,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("child stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("child stdout pipe: %v", err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	// One cleanup path for every outcome: close the child's stdin (releasing
	// its claim), reap it, and join the marker scanner at pipe EOF. The
	// bounded context kills the child at the deadline if it ever hangs, so a
	// started child cannot outlive the test. Repeating the call after the
	// success path is harmless.
	ready := make(chan struct{})
	scannerDone := make(chan struct{})
	go func() {
		defer close(scannerDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "ready" {
				close(ready)
				return
			}
		}
	}()
	reap := func() error {
		_ = stdin.Close()
		err := cmd.Wait()
		<-scannerDone
		return err
	}
	// fail kills the child (cancelling its context), reaps it, and only then
	// returns its stderr for diagnostics: the buffer is never read while the
	// child can still write to it.
	fail := func() string {
		holderCancel()
		_ = reap()
		return stderr.String()
	}
	defer func() { holderCancel(); _ = reap() }()

	select {
	case <-ready:
	case <-holderCtx.Done():
		t.Fatalf("child never held the session claim within %v: %v\n%s", holdBound, holderCtx.Err(), fail())
	}

	err = second.SessionDelete(id)
	if err == nil {
		t.Fatalf("SessionDelete succeeded while another process held %s's claim\n%s", id, fail())
	}
	want := fmt.Sprintf("session %q is being driven by another process", id)
	if err.Error() != want {
		t.Fatalf("SessionDelete error = %q, want %q\n%s", err.Error(), want, fail())
	}
	for _, dir := range []string{sessionDir, summariesDir} {
		if _, statErr := os.Stat(dir); statErr != nil {
			t.Fatalf("%s after the refused delete: %v\n%s", dir, statErr, fail())
		}
	}

	if err := reap(); err != nil {
		t.Fatalf("child after release: %v\n%s", err, stderr.String())
	}
	if err := second.SessionDelete(id); err != nil {
		t.Fatalf("SessionDelete after the child released: %v", err)
	}
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatalf("session dir stat error = %v, want not exist after the delete", err)
	}
	if _, err := os.Stat(summariesDir); !os.IsNotExist(err) {
		t.Fatalf("summaries dir stat error = %v, want not exist after the delete", err)
	}
}
