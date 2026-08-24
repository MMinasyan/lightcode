package acp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/agent"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/memory"
	"github.com/MMinasyan/lightcode/internal/snapshot"
)

const (
	acpSessionNewHolderEnv = "LIGHTCODE_ACP_SESSION_NEW_HOLDER"
	acpSessionNewRunnerEnv = "LIGHTCODE_ACP_SESSION_NEW_RUNNER"
)

func acpIdentityLock(root, projectPath string) string {
	abs, _ := filepath.Abs(projectPath)
	sum := sha256.Sum256([]byte(filepath.Clean(abs)))
	return filepath.Join(root, ".locks", "identity", hex.EncodeToString(sum[:])+".lock")
}

func TestACPSessionNewUnderIdentityContentionReturnsBusyAndJoins(t *testing.T) {
	if lockPath := os.Getenv(acpSessionNewHolderEnv); lockPath != "" {
		l, err := atomicfs.Acquire(lockPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Println("ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		_ = l.Release()
		os.Exit(0)
	}
	if os.Getenv(acpSessionNewRunnerEnv) != "" {
		home := os.Getenv("LIGHTCODE_ACP_SESSION_NEW_HOME")
		root := os.Getenv("LIGHTCODE_ACP_SESSION_NEW_ROOT")
		configPath := filepath.Join(home, ".lightcode", "config.json")
		cfg, err := config.Load(configPath)
		if err != nil {
			t.Fatal(err)
		}
		a, err := agent.New(agent.Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: root, Home: home, NewMemoryEmbedder: func(string) (*memory.Embedder, error) { return nil, nil }})
		if err != nil {
			t.Fatal(err)
		}
		r := New(a)
		r.in = os.Stdin
		r.out = os.Stdout
		if err := r.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		return
	}

	a := newACPTestAgent(t)
	lock, err := atomicfs.Acquire(acpIdentityLock(a.Projects().Root(), a.ProjectRoot()))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	r := New(a)
	r.out = &bytes.Buffer{}
	if !r.admitDispatch() {
		t.Fatal("dispatch admission refused unexpectedly")
	}
	done := make(chan struct{})
	go func() {
		r.processLine(context.Background(), []byte(`{"jsonrpc":"2.0","id":"new","method":"session/new"}`))
		r.dispatchWG.Done()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("session/new did not complete under identity contention")
	}
	r.dispatchWG.Wait()
	r.closeDispatch()
	r.drainForTest()
	lines := strings.Split(strings.TrimSpace(r.out.(*bytes.Buffer).String()), "\n")
	if !bytes.Contains([]byte(lines[len(lines)-1]), []byte("project is busy")) {
		t.Fatalf("session/new response = %s, want project-busy error", lines[len(lines)-1])
	}
	if sessions, err := a.SessionListForProjectPath(a.ProjectRoot(), "active"); err == nil && len(sessions) != 0 {
		t.Fatalf("session/new under contention published %d sessions", len(sessions))
	}
}

func TestACPSessionNewContentionEOFAndSIGTERMJoin(t *testing.T) {
	for _, termination := range []string{"eof", "sigterm"} {
		t.Run(termination, func(t *testing.T) {
			a, home := newACPTestAgentEnv(t, "http://127.0.0.1:9/v1", false)
			if _, err := a.NewSession("", "primary"); err != nil {
				t.Fatal(err)
			}
			root := a.ProjectRoot()
			p, err := a.Projects().Current()
			if err != nil || p == nil {
				t.Fatalf("current project: %v", err)
			}
			if !a.ShutdownOwner() {
				t.Fatal("seed owner shutdown reported abandoned work")
			}
			lockPath := acpIdentityLock(a.Projects().Root(), root)
			release := startACPSessionNewHolder(t, lockPath)
			defer release()

			cmd := exec.Command(os.Args[0], "-test.run=^TestACPSessionNewUnderIdentityContentionReturnsBusyAndJoins$")
			cmd.Env = append(os.Environ(), acpSessionNewRunnerEnv+"=1", "LIGHTCODE_ACP_SESSION_NEW_HOME="+home, "LIGHTCODE_ACP_SESSION_NEW_ROOT="+root)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			request := []byte(`{"jsonrpc":"2.0","id":"new","method":"session/new"}` + "\n")
			if _, err := stdin.Write(request); err != nil {
				t.Fatal(err)
			}
			lines := make(chan string, 8)
			go func() {
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					lines <- scanner.Text()
				}
				close(lines)
			}()
			var response string
			if termination == "eof" {
				_ = stdin.Close()
				deadline := time.After(10 * time.Second)
				for response == "" {
					select {
					case line, ok := <-lines:
						if !ok {
							t.Fatal("EOF runner produced no terminal response")
						}
						if strings.Contains(line, "project is busy") {
							response = line
						}
					case <-deadline:
						t.Fatal("EOF runner produced no terminal response")
					}
				}
			} else {
				deadline := time.After(10 * time.Second)
				for response == "" {
					select {
					case line := <-lines:
						if strings.Contains(line, "project is busy") {
							response = line
						}
					case <-deadline:
						t.Fatal("SIGTERM runner produced no terminal response")
					}
				}
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
				_ = stdin.Close()
			}
			if !strings.Contains(response, "project is busy") {
				t.Fatalf("%s response = %q, want terminal project-busy error", termination, response)
			}
			waited := make(chan error, 1)
			go func() { waited <- cmd.Wait() }()
			select {
			case err := <-waited:
				if err != nil {
					t.Fatalf("runner %s exit: %v", termination, err)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("runner %s did not join dispatchWG", termination)
			}
			infos, err := snapshot.List(a.Projects().SessionsRoot(p.ID), p.Path, snapshot.StateActive)
			if err != nil {
				t.Fatal(err)
			}
			if len(infos) != 1 {
				t.Fatalf("runner %s published %d sessions, want seed only", termination, len(infos))
			}
		})
	}
}

func startACPSessionNewHolder(t *testing.T, lockPath string) func() {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestACPSessionNewUnderIdentityContentionReturnsBusyAndJoins$")
	cmd.Env = append(os.Environ(), acpSessionNewHolderEnv+"="+lockPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "ready" {
				close(ready)
				return
			}
		}
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal("identity holder did not become ready")
	}
	return func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}
}

func TestACPProjectRoutesUnderIdentityContention(t *testing.T) {
	a := newACPTestAgent(t)
	sid, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a.ProjectRoot(), "visible.txt"), []byte("visible"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := New(a)
	r.out = &bytes.Buffer{}
	summary, err := a.SessionSummaryForSession(sid)
	if err != nil {
		t.Fatal(err)
	}
	r.setCurrent(sid, summary)
	lock, err := atomicfs.Acquire(acpIdentityLock(a.Projects().Root(), a.ProjectRoot()))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	tests := []struct {
		name    string
		request Request
	}{
		{name: "session/list", request: Request{JSONRPC: "2.0", ID: "list", Method: "session/list"}},
		{name: "project/current", request: Request{JSONRPC: "2.0", ID: "current", Method: "project/current"}},
		{name: "file/read", request: Request{JSONRPC: "2.0", ID: "file", Method: "file/read", Params: json.RawMessage(`{"path":"visible.txt"}`)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r.dispatch(context.Background(), tc.request)
			r.drainForTest()
			lines := strings.Split(strings.TrimSpace(r.out.(*bytes.Buffer).String()), "\n")
			if bytes.Contains([]byte(lines[len(lines)-1]), []byte(`"error"`)) {
				t.Fatalf("present %s route errored: %s", tc.name, lines[len(lines)-1])
			}
		})
	}

	other := t.TempDir()
	otherLock, err := atomicfs.Acquire(acpIdentityLock(a.Projects().Root(), other))
	if err != nil {
		t.Fatal(err)
	}
	defer otherLock.Release()
	r.routeProjectPath = other
	r.setCurrentSessionID("")
	for _, tc := range []Request{
		{JSONRPC: "2.0", ID: "abs-list", Method: "session/list"},
		{JSONRPC: "2.0", ID: "abs-current", Method: "project/current"},
		{JSONRPC: "2.0", ID: "abs-file", Method: "file/read", Params: json.RawMessage(`{"path":"missing.txt"}`)},
	} {
		r.dispatch(context.Background(), tc)
		r.drainForTest()
		lines := strings.Split(strings.TrimSpace(r.out.(*bytes.Buffer).String()), "\n")
		if !bytes.Contains([]byte(lines[len(lines)-1]), []byte(`"error"`)) {
			t.Fatalf("absent route %s returned success: %s", tc.Method, lines[len(lines)-1])
		}
	}
	if _, err := a.ProjectCurrentForPath(other); !errors.Is(err, agent.ErrProjectBusy) {
		t.Fatalf("absent route did not preserve identity contention: %v", err)
	}
}
