package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// rootExecSelector is the private test environment marker: when inherited,
// TestMain runs the real command entry with the child's exact argv instead of
// the test suite. Children re-enter through os.Executable and the marker
// travels into every runtime contender that clienthost starts.
const rootExecSelector = "LIGHTCODE_TEST_ROOT_EXEC"

// TestMain routes self-exec children into the production entry point before
// any test-flag parsing, so their argv stays the real command argv.
func TestMain(m *testing.M) {
	if os.Getenv(rootExecSelector) == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// oldSessionID is the sentinel pre-cutover filesystem Session: exactly 8
// lowercase hex, the retained old-state shape the new store must never load.
const oldSessionID = "deadbeef"

// rootFixtureConfig is the one configured provider projection the models
// command must print: one keyless connected provider with a visible, a
// hidden, and an incomplete model.
const rootFixtureConfig = `{
  "providers": {
    "fixtureprov": {
      "transport": {"base_url": "https://fixture.test/v1", "api_key_env": ""},
      "discovery": false,
      "models": {
        "vis": {"name": "Visible", "context_window": 8192, "max_output_tokens": 1024},
        "hid": {"name": "Hidden", "context_window": 16384, "max_output_tokens": 512, "hidden": true},
        "inc": {"name": "Incomplete"}
      }
    }
  }
}`

// rootSandbox is one isolated production-composition fixture: a private HOME,
// data root, config path and workspace, sentinel pre-cutover conversation and
// snapshot bytes, and the exact child environment every spawned command runs
// with. Nothing here touches live product data or provider accounts.
type rootSandbox struct {
	t          *testing.T
	exe        string
	home       string
	dataDir    string
	configPath string
	workspace  string
}

func newRootSandbox(t *testing.T) *rootSandbox {
	t.Helper()
	home := t.TempDir()
	workspace := t.TempDir()
	dataDir := filepath.Join(home, ".lightcode")
	testassert.NoError(t, os.MkdirAll(dataDir, 0o700))
	configPath := filepath.Join(home, "config.json")
	testassert.NoError(t, os.WriteFile(configPath, []byte(rootFixtureConfig), 0o600))
	t.Setenv("HOME", home)
	t.Setenv("LIGHTCODE_CONFIG", configPath)
	isolateBundledCredentials(t)
	exe, err := os.Executable()
	testassert.NoError(t, err)
	s := &rootSandbox{t: t, exe: exe, home: home, dataDir: dataDir, configPath: configPath, workspace: workspace}
	s.seedOldState()
	return s
}

// seedOldState writes one pre-cutover filesystem conversation with snapshot
// content at the retained old layout under the isolated home.
func (s *rootSandbox) seedOldState() {
	s.t.Helper()
	sum := sha256.Sum256([]byte(filepath.Clean(s.workspace)))
	hash := hex.EncodeToString(sum[:])
	sessionDir := filepath.Join(s.dataDir, "projects", "p-"+hash, "sessions", oldSessionID)
	snapEntry := filepath.Join(sessionDir, "snapshots", "1", "0123456789abcdef")
	testassert.NoError(s.t, os.MkdirAll(snapEntry, 0o700))
	meta := fmt.Sprintf(`{"id":%q,"created_at":"2026-01-01T00:00:00Z","project_path":%q,"project_hash":%q,"lightcode_version":"v0.0.0","state":"active","archived_at":0,"last_activity":0}`, oldSessionID, filepath.Clean(s.workspace), hash[:16])
	files := map[string]string{
		filepath.Join(sessionDir, "meta.json"):     meta,
		filepath.Join(sessionDir, "messages.json"): `["old conversation sentinel"]`,
		filepath.Join(snapEntry, "meta.json"):      `{"original_path":"sentinel.txt","existed":true}`,
		filepath.Join(snapEntry, "sentinel.txt"):   "old snapshot sentinel\n",
	}
	for path, body := range files {
		{
			err := os.WriteFile(path, []byte(body), 0o600)
			testassert.Require(s.t, err == nil, "seed old state %s: %v", path, err)
		}
	}
}

// childEnv is the exact environment every fixture child runs with: the
// private self-exec selector beside the already-isolated HOME and
// LIGHTCODE_CONFIG of the parent process.
func (s *rootSandbox) childEnv() []string {
	return append(os.Environ(), rootExecSelector+"=1")
}

// runtimeArgs is the one production runtime command argv for this sandbox.
func (s *rootSandbox) runtimeArgs(configPath string) []string {
	return []string{"runtime", "--data-dir", s.dataDir, "--config", configPath}
}

// runOne runs one command to completion and captures its streams and exit
// code. The child handles are joined and reaped on every path.
func (s *rootSandbox) runOne(args ...string) (stdout, stderr string, code int) {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, s.exe, args...)
	cmd.Dir = s.workspace
	cmd.Env = s.childEnv()
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if ctx.Err() != nil {
		s.t.Fatalf("%v: timed out\nstdout=%s\nstderr=%s", args, out.String(), errb.String())
	}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			s.t.Fatalf("%v: %v\nstdout=%s\nstderr=%s", args, err, out.String(), errb.String())
		}
		code = exitErr.ExitCode()
	}
	return out.String(), errb.String(), code
}

// rootChild is one fixture-launched interactive process with captured output,
// an open stdin, and an exactly-once join.
type rootChild struct {
	t     *testing.T
	args  []string
	cmd   *exec.Cmd
	stdin io.WriteCloser
	out   *lockedBuffer
	done  chan struct{}
	err   error
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (s *rootSandbox) start(args ...string) *rootChild {
	s.t.Helper()
	cmd := exec.Command(s.exe, args...)
	cmd.Dir = s.workspace
	cmd.Env = s.childEnv()
	stdin, err := cmd.StdinPipe()
	testassert.Require(s.t, err == nil, "%v: StdinPipe: %v", args, err)
	out := &lockedBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out
	{
		err := cmd.Start()
		testassert.Require(s.t, err == nil, "%v: start: %v", args, err)
	}
	c := &rootChild{t: s.t, args: args, cmd: cmd, stdin: stdin, out: out, done: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	s.t.Cleanup(c.kill)
	return c
}

// signal delivers one shutdown signal to the known child process.
func (c *rootChild) signal(sig os.Signal) {
	c.t.Helper()
	{
		err := c.cmd.Process.Signal(sig)
		testassert.Require(c.t, err == nil, "%v: signal: %v", c.args, err)
	}
}

// awaitExit joins the child and returns its exit code within the bound.
func (c *rootChild) awaitExit() int {
	c.t.Helper()
	select {
	case <-c.done:
	case <-time.After(30 * time.Second):
		c.kill()
		c.t.Fatalf("%v: did not exit in time\noutput=%s", c.args, c.out.String())
	}
	code := 0
	if c.err != nil {
		var exitErr *exec.ExitError
		if !errors.As(c.err, &exitErr) {
			c.t.Fatalf("%v: %v\noutput=%s", c.args, c.err, c.out.String())
		}
		code = exitErr.ExitCode()
	}
	return code
}

// kill is the registered hard cleanup: terminate and join a child that a test
// left running. Only fixture-started handles are touched.
func (c *rootChild) kill() {
	_ = c.cmd.Process.Kill()
	<-c.done
}

// awaitReady is the authenticated rendezvous: it returns the first client
// connection that verifies the published discovery record against the live
// owner. The credential never leaves the isolated record.
func (s *rootSandbox) awaitReady() *client.Client {
	s.t.Helper()
	recordPath := filepath.Join(s.dataDir, "runtime.json")
	deadline := time.Now().Add(45 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		conn, err := client.Connect(ctx, recordPath)
		cancel()
		if err == nil {
			return conn
		}
		testassert.Require(s.t, !time.Now().After(deadline), "owner never became ready: %v", err)
		time.Sleep(100 * time.Millisecond)
	}
}

// withdrawn proves the discovery record is gone after a managed shutdown.
func (s *rootSandbox) withdrawn() {
	s.t.Helper()
	recordPath := filepath.Join(s.dataDir, "runtime.json")
	{
		_, err := os.Lstat(recordPath)
		testassert.Require(s.t, os.IsNotExist(err), "discovery record still present after shutdown: %v", err)
	}
}

// cliAsk runs one root CLI command line and returns the output once it holds
// want, rewriting the command while the CLI reports no connection yet. The
// run ends with /exit on stdin and must exit 0.
func (s *rootSandbox) cliAsk(command, want string) string {
	s.t.Helper()
	child := s.start("cli")
	deadline := time.Now().Add(45 * time.Second)
	for {
		{
			_, err := io.WriteString(child.stdin, command+"\n")
			testassert.Require(child.t, err == nil, "cli stdin: %v", err)
		}
		for {
			out := child.out.String()
			switch {
			case strings.Contains(out, want):
				{
					_, err := io.WriteString(child.stdin, "/exit\n")
					testassert.Require(child.t, err == nil, "cli stdin: %v", err)
				}
				_ = child.stdin.Close()
				{
					code := child.awaitExit()
					testassert.Require(child.t, code == 0, "cli exit = %d\noutput=%s", code, out)
				}
				return child.out.String()
			case strings.Contains(out, "Runtime is not connected"):
			case time.Now().After(deadline):
				child.t.Fatalf("cli never answered %q\noutput=%s", command, out)
			case childExited(child):
				child.t.Fatalf("cli exited before answering %q\noutput=%s", command, out)
			default:
				time.Sleep(50 * time.Millisecond)
				continue
			}
			break
		}
	}
}

func childExited(c *rootChild) bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

// acpExchange runs one root ACP bridge, performs initialize and
// session/current, and returns the reported Runtime Health and the composed
// Session identity. A clean stdin end exits 0.
func (s *rootSandbox) acpExchange() (healthInstance, sessionID string) {
	s.t.Helper()
	child := s.start("acp")
	send := func(body string) {
		{
			_, err := io.WriteString(child.stdin, body+"\n")
			testassert.Require(child.t, err == nil, "acp stdin: %v", err)
		}
	}
	read := func(id string, into any) {
		child.t.Helper()
		deadline := time.Now().Add(30 * time.Second)
		for {
			out := child.out.String()
			for _, line := range strings.Split(out, "\n") {
				if !strings.Contains(line, `"id":`+id) {
					continue
				}
				var resp struct {
					Result json.RawMessage `json:"result"`
					Error  *struct {
						Code    int    `json:"code"`
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal([]byte(line), &resp); err != nil {
					continue
				}
				if resp.Error != nil {
					child.t.Fatalf("acp id %s error %d %s", id, resp.Error.Code, resp.Error.Message)
				}
				{
					err := json.Unmarshal(resp.Result, into)
					testassert.Require(child.t, err == nil, "acp id %s result: %v (%s)", id, err, resp.Result)
				}
				return
			}
			testassert.Require(child.t, !time.Now().After(deadline), "acp id %s never answered\noutput=%s", id, out)
			testassert.Require(child.t, !childExited(child), "acp exited before answering id %s\noutput=%s", id, out)
			time.Sleep(50 * time.Millisecond)
		}
	}

	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	var init struct {
		Runtime struct {
			InstanceID string `json:"instance_id"`
		} `json:"runtime"`
	}
	read("1", &init)
	send(`{"jsonrpc":"2.0","id":2,"method":"session/current","params":{}}`)
	var hydration struct {
		Session struct {
			SessionID string `json:"session_id"`
		} `json:"session"`
	}
	read("2", &hydration)
	_ = child.stdin.Close()
	if code := child.awaitExit(); code != 0 {
		child.t.Fatalf("acp exit = %d\noutput=%s", code, child.out.String())
	}
	return init.Runtime.InstanceID, hydration.Session.SessionID
}

// TestRootRuntimeComposition starts the production runtime command through
// the real entry, attaches the shipped models, CLI and ACP commands to that
// one owner, and proves client exits leave it reachable, a contended startup
// exits quietly without replacing the record, SIGINT and SIGTERM join managed
// shutdown and withdraw discovery, and a replacement owner with a fresh
// instance reads the same committed Session. The seeded pre-cutover
// conversation is never enumerated and its bytes never move.
func TestRootRuntimeComposition(t *testing.T) {
	s := newRootSandbox(t)
	oldState := filepath.Join(s.dataDir, "projects")
	before := snapshotHome(t, oldState)

	owner := s.start(s.runtimeArgs(s.configPath)...)
	first := s.awaitReady()
	instance := first.InstanceID()
	testassert.Require(t, instance != "", "owner reported an empty instance")

	t.Run("contended startup exits 0 without replacing the record", func(t *testing.T) {
		_, stderr, code := s.runOne(s.runtimeArgs(s.configPath)...)
		testassert.Require(t, code == 0, "contender exit = %d, want 0\nstderr=%s", code, stderr)
		again := s.awaitReady()
		testassert.Equal(t, again.InstanceID(), instance)
	})

	t.Run("models prints the configured provider projection", func(t *testing.T) {
		stdout, stderr, code := s.runOne("models", "--json", "--all")
		testassert.Require(t, code == 0, "exit = %d\nstderr=%s", code, stderr)
		var rows []map[string]any
		{
			err := json.Unmarshal([]byte(stdout), &rows)
			testassert.Require(t, err == nil, "stdout not JSON: %v\n%q", err, stdout)
		}
		want := []map[string]any{
			{"ref": "fixtureprov/hid", "provider": "fixtureprov", "providerName": "fixtureprov", "model": "hid", "displayName": "Hidden", "contextWindow": float64(16384), "maxOutputTokens": float64(512), "hidden": true, "providerHidden": false, "incomplete": false, "default": false, "source": "user"},
			{"ref": "fixtureprov/inc", "provider": "fixtureprov", "providerName": "fixtureprov", "model": "inc", "displayName": "Incomplete", "contextWindow": float64(0), "maxOutputTokens": float64(0), "hidden": false, "providerHidden": false, "incomplete": true, "default": false, "source": "user"},
			{"ref": "fixtureprov/vis", "provider": "fixtureprov", "providerName": "fixtureprov", "model": "vis", "displayName": "Visible", "contextWindow": float64(8192), "maxOutputTokens": float64(1024), "hidden": false, "providerHidden": false, "incomplete": false, "default": false, "source": "user"},
		}
		testassert.Require(t, len(rows) == len(want), "rows = %v", rows)
		for i := range want {
			for field, value := range want[i] {
				testassert.Require(t, rows[i][field] == value, "rows[%d][%q] = %v, want %v", i, field, rows[i][field], value)
			}
			{
				_, present := rows[i]["cost"]
				testassert.Require(t, !present, "rows[%d] carries an unset cost: %v", i, rows[i])
			}
		}
	})

	t.Run("models keeps ordinary trailing flags and hides incomplete rows by default", func(t *testing.T) {
		stdout, stderr, code := s.runOne("models", "fixtureprov", "--all")
		testassert.Require(t, code == 0, "exit = %d\nstderr=%s", code, stderr)
		want := "fixtureprov/hid\t16384\t512\thidden\n" +
			"fixtureprov/inc\t0\t0\tincomplete\n" +
			"fixtureprov/vis\t8192\t1024\t-\n"
		testassert.Equal(t, stdout, want)
		stdout, stderr, code = s.runOne("models", "--json")
		testassert.Require(t, code == 0, "exit = %d\nstderr=%s", code, stderr)
		var rows []map[string]any
		{
			err := json.Unmarshal([]byte(stdout), &rows)
			testassert.Require(t, err == nil, "stdout not JSON: %v\n%q", err, stdout)
		}
		testassert.Require(t, len(rows) == 1 && rows[0]["ref"] == "fixtureprov/vis", "default rows = %v, want only the visible complete model", rows)
	})

	t.Run("terminated flag-like operands stay provider operands", func(t *testing.T) {
		stdout, stderr, code := s.runOne("models", "--", "--json=true")
		testassert.Require(t, code == 2, "exit = %d, want 2\nstdout=%s\nstderr=%s", code, stdout, stderr)
		testassert.Require(t, strings.Contains(stderr, `unknown provider "--json=true"`), "stderr = %q, want the operand reported as an unknown provider", stderr)
	})

	t.Run("cli exposes the same instance through the typed dispatcher", func(t *testing.T) {
		out := s.cliAsk("/runtime GetHealth {}", instance)
		testassert.Require(t, strings.Contains(out, instance), "cli output = %q, want instance %q", out, instance)
	})

	var session string
	t.Run("acp initialize reports the generated Runtime health", func(t *testing.T) {
		healthInstance, sessionID := s.acpExchange()
		testassert.Equal(t, healthInstance, instance)
		testassert.Require(t, sessionID != "", "acp composed no session")
		session = sessionID
	})

	t.Run("target session reads never enumerate the old conversation", func(t *testing.T) {
		out := s.cliAsk(`/runtime ListSessions {"query":{"workspace":`+jsonString(s.workspace)+`,"lifecycle":"open"}}`, session)
		testassert.Require(t, strings.Contains(out, session) && !strings.Contains(out, oldSessionID), "session list = %q, want the committed session %q; session list = %q, want no old filesystem conversation %q", out, session, out, oldSessionID)
	})

	t.Run("client exit leaves the owner reachable", func(t *testing.T) {
		again := s.awaitReady()
		testassert.Equal(t, again.InstanceID(), instance)
	})

	t.Run("SIGINT joins managed shutdown and a fresh owner reads the same session", func(t *testing.T) {
		owner.signal(os.Interrupt)
		if code := owner.awaitExit(); code != 0 {
			t.Fatalf("SIGINT exit = %d, want 0\noutput=%s", code, owner.out.String())
		}
		s.withdrawn()

		replacement := s.start(s.runtimeArgs(s.configPath)...)
		fresh := s.awaitReady()
		{
			got := fresh.InstanceID()
			testassert.Require(t, got != instance, "replacement instance = %q, want a fresh instance", got)
		}
		out := s.cliAsk(`/runtime ListSessions {"query":{"workspace":`+jsonString(s.workspace)+`,"lifecycle":"open"}}`, session)
		testassert.Require(t, strings.Contains(out, session), "replacement session list = %q, want the committed session %q", out, session)
		replacement.signal(syscall.SIGTERM)
		if code := replacement.awaitExit(); code != 0 {
			t.Fatalf("SIGTERM exit = %d, want 0\noutput=%s", code, replacement.out.String())
		}
		s.withdrawn()
	})

	{
		after := snapshotHome(t, oldState)
		testassert.Require(t, maps.Equal(before, after), "old filesystem conversation moved: before=%v after=%v", before, after)
	}
}

// TestRootACPSignalExit pins the ACP entry's retained successful signal
// exit: once the bridge is serving — its parse-error answer proves the run
// loop and its signal handling are armed — a signal shutdown exits 0 like the
// adapter entry always did, and only other errors propagate as failures.
func TestRootACPSignalExit(t *testing.T) {
	s := newRootSandbox(t)
	child := s.start("acp")
	{
		_, err := io.WriteString(child.stdin, "not-json\n")
		testassert.NoError(t, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(child.out.String(), "parse error") {
		if time.Now().After(deadline) {
			t.Fatalf("acp bridge never answered\noutput=%s", child.out.String())
		}
		if childExited(child) {
			t.Fatalf("acp exited before serving\noutput=%s", child.out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	child.signal(os.Interrupt)
	if code := child.awaitExit(); code != 0 {
		t.Fatalf("acp signal exit = %d, want 0\noutput=%s", code, child.out.String())
	}
}

// TestRootRuntimeStartupFailures pins the exit-1 startup paths of the real
// command: an invalid config and a non-file discovery destination. Neither
// initializes an owner or leaves the sentinel destination behind broken.
func TestRootRuntimeStartupFailures(t *testing.T) {
	t.Run("invalid config exits 1", func(t *testing.T) {
		s := newRootSandbox(t)
		badPath := filepath.Join(s.home, "bad-config.json")
		testassert.NoError(t, os.WriteFile(badPath, []byte(`{"providers": [`), 0o600))
		stdout, stderr, code := s.runOne(s.runtimeArgs(badPath)...)
		testassert.Require(t, code == 1, "exit = %d, want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
		testassert.Require(t, strings.HasPrefix(stderr, "lightcode: "), "stderr = %q, want the dispatcher diagnostic", stderr)
		s.withdrawn()
	})

	t.Run("non-file discovery destination exits 1", func(t *testing.T) {
		s := newRootSandbox(t)
		recordPath := filepath.Join(s.dataDir, "runtime.json")
		testassert.NoError(t, os.MkdirAll(recordPath, 0o700))
		sentinel := filepath.Join(recordPath, "keep.txt")
		testassert.NoError(t, os.WriteFile(sentinel, []byte("sentinel"), 0o600))
		stdout, stderr, code := s.runOne(s.runtimeArgs(s.configPath)...)
		testassert.Require(t, code == 1, "exit = %d, want 1\nstdout=%s\nstderr=%s", code, stdout, stderr)
		testassert.Require(t, strings.HasPrefix(stderr, "lightcode: "), "stderr = %q, want the dispatcher diagnostic", stderr)
		{
			_, err := os.Stat(sentinel)
			testassert.NoError(t, err)
		}
	})
}

// TestRootRuntimeCleanupFailure pins cleanup-error propagation: when the
// discovery record cannot be withdrawn at shutdown, the command reports exit
// 1 and keeps the blocking directory's content intact.
func TestRootRuntimeCleanupFailure(t *testing.T) {
	s := newRootSandbox(t)
	owner := s.start(s.runtimeArgs(s.configPath)...)
	s.awaitReady()

	recordPath := filepath.Join(s.dataDir, "runtime.json")
	testassert.NoError(t, os.Remove(recordPath))
	testassert.NoError(t, os.MkdirAll(recordPath, 0o700))
	sentinel := filepath.Join(recordPath, "keep.txt")
	testassert.NoError(t, os.WriteFile(sentinel, []byte("sentinel"), 0o600))

	owner.signal(os.Interrupt)
	if code := owner.awaitExit(); code != 1 {
		t.Fatalf("cleanup failure exit = %d, want 1\noutput=%s", code, owner.out.String())
	}
	if !strings.Contains(owner.out.String(), "lightcode: ") {
		t.Fatalf("output = %q, want the dispatcher diagnostic", owner.out.String())
	}
	{
		_, err := os.Stat(sentinel)
		testassert.NoError(t, err)
	}
}

// jsonString quotes one JSON string operand with the standard library encoder.
func jsonString(s string) string {
	data, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(data)
}

// isolateBundledCredentials empties every api_key_env declared by the bundled
// catalog in this process and its children, so no bundled provider connects
// from a captured developer environment during the fixture.
func isolateBundledCredentials(t *testing.T) {
	t.Helper()
	catalogDir := filepath.Join("internal", "catalog", "builtin")
	entries, err := os.ReadDir(catalogDir)
	testassert.NoError(t, err)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(catalogDir, entry.Name()))
		if err != nil {
			t.Fatalf("read bundled provider %s: %v", entry.Name(), err)
		}
		var provider struct {
			Transport struct {
				APIKeyEnv string `json:"api_key_env"`
			} `json:"transport"`
		}
		if err := json.Unmarshal(data, &provider); err != nil {
			t.Fatalf("parse bundled provider %s: %v", entry.Name(), err)
		}
		if provider.Transport.APIKeyEnv != "" {
			t.Setenv(provider.Transport.APIKeyEnv, "")
		}
	}
}
