package runtime

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/protocol"
)

// The cross-process discovery ownership fixtures: one self-exec test binary
// plays both the owning incumbent and the losing contender against the real
// ownership lock, the real SQLite derivation, the real loopback listener, and
// the real one-record publisher. Every child isolates HOME, DataDir and
// ConfigPath under test temporary roots; the parent reads the credential only
// from the isolated 0600 discovery record, and no child ever prints it.

const (
	discoveryChildDataEnv   = "LIGHTCODE_RUNTIME_DISCOVERY_CHILD"
	discoveryChildHomeEnv   = "LIGHTCODE_RUNTIME_DISCOVERY_HOME"
	discoveryChildRecordEnv = "LIGHTCODE_RUNTIME_DISCOVERY_RECORD"
	discoveryChildMarkerEnv = "LIGHTCODE_RUNTIME_DISCOVERY_MARKER"
	discoveryChildModeEnv   = "LIGHTCODE_RUNTIME_DISCOVERY_MODE"
)

// The child roles. owner publishes and serves; incompatible publishes and then
// answers readiness with a different nonempty version; hold owns the root and
// the listener without publishing; unready refuses publication because
// authenticated readiness reports an incompatible version; cancel refuses
// publication because cancellation wins while readiness is parked.
const (
	discoveryModeOwner        = "owner"
	discoveryModeIncompatible = "incompatible"
	discoveryModeHold         = "hold"
	discoveryModeUnready      = "unready"
	discoveryModeCancel       = "cancel"
)

// TestRuntimeDiscoveryOwnershipAcrossProcesses proves isolated ownership and
// discovery over real processes: two contenders race one runtime.lock, only
// the winner initializes authoritative state and publishes, and no discovery
// state — stale/dead, incompatible live, or canceled before ready — ever
// authorizes a contender to replace a live owner. When the child environment
// names a data directory this same test binary runs one child role instead.
func TestRuntimeDiscoveryOwnershipAcrossProcesses(t *testing.T) {
	if dataDir := os.Getenv(discoveryChildDataEnv); dataDir != "" {
		runDiscoveryChild(t, dataDir)
		return
	}

	t.Run("one contender initializes and publishes", func(t *testing.T) {
		e, dataDir := newDiscoveryRoot(t)
		recordPath := filepath.Join(t.TempDir(), "discovery.json")
		firstMarker := filepath.Join(t.TempDir(), "marker")
		secondMarker := filepath.Join(t.TempDir(), "marker")
		first := startDiscoveryChild(t, e.home, dataDir, recordPath, firstMarker, discoveryModeOwner)
		second := startDiscoveryChild(t, e.home, dataDir, recordPath, secondMarker, discoveryModeOwner)

		firstToken := first.await("acquired", "owned")
		secondToken := second.await("acquired", "owned")
		if (firstToken == "owned") == (secondToken == "owned") {
			t.Fatalf("contender dispositions = (%q, %q), want exactly one owner and one ErrOwned loser", firstToken, secondToken)
		}
		winner, loser := first, second
		winnerMarker, loserMarker := firstMarker, secondMarker
		if firstToken == "owned" {
			winner, loser = second, first
			winnerMarker, loserMarker = secondMarker, firstMarker
		}
		winner.await("published")
		if err := loser.awaitExit(); err != nil {
			t.Fatalf("losing contender exit: %v output=%q", err, loser.log.text())
		}
		if _, err := os.Stat(winnerMarker); err != nil {
			t.Fatalf("the winner's Runtime factory marker: %v", err)
		}
		if _, err := os.Stat(loserMarker); !os.IsNotExist(err) {
			t.Fatalf("the losing contender invoked a factory: marker stat error = %v", err)
		}
		if _, err := os.Stat(filepath.Join(dataDir, "lightcode.db")); err != nil {
			t.Fatalf("the winner's SQLite derivation under the data root: %v", err)
		}

		record := readDiscoveryRecord(t, recordPath)
		health := healthSnapshot(t, record)
		if health.InstanceId != record.InstanceId || health.ProtocolVersion != protocol.N1 {
			t.Fatalf("authenticated health = %s/%s, want the record's %s/%s", health.InstanceId, health.ProtocolVersion, record.InstanceId, record.ProtocolVersion)
		}

		winner.shutdown()
		if after := readDiscoveryBytes(t, recordPath); after != nil {
			t.Fatalf("the record survived managed shutdown (%d bytes)", len(after))
		}
		e.assertLockReleased(t)
	})

	t.Run("a dead record never authorizes a replacement", func(t *testing.T) {
		e, dataDir := newDiscoveryRoot(t)
		recordPath := filepath.Join(t.TempDir(), "discovery.json")
		first := startDiscoveryChild(t, e.home, dataDir, recordPath, filepath.Join(t.TempDir(), "marker"), discoveryModeOwner)
		first.await("published")
		dead := readDiscoveryRecord(t, recordPath)
		before := readDiscoveryBytes(t, recordPath)

		if err := first.cmd.Process.Kill(); err != nil {
			t.Fatalf("kill the first owner: %v", err)
		}
		if err := first.awaitExit(); err == nil {
			t.Fatal("the hard-killed owner reported success")
		}
		if _, err := os.Stat(recordPath); err != nil {
			t.Fatalf("the hard-killed owner's record disappeared: %v", err)
		}
		if err := probeHealth(dead.Endpoint, dead.Credential); err == nil {
			t.Fatal("the health probe to the dead owner's endpoint succeeded")
		}

		incumbent := startDiscoveryChild(t, e.home, dataDir, recordPath, filepath.Join(t.TempDir(), "marker"), discoveryModeHold)
		incumbent.await("acquired")
		contenderMarker := filepath.Join(t.TempDir(), "marker")
		contender := startDiscoveryChild(t, e.home, dataDir, recordPath, contenderMarker, discoveryModeOwner)
		contender.await("owned")
		if err := contender.awaitExit(); err != nil {
			t.Fatalf("contender exit: %v output=%q", err, contender.log.text())
		}
		if _, err := os.Stat(contenderMarker); !os.IsNotExist(err) {
			t.Fatalf("the contender invoked a factory over a dead record: marker stat error = %v", err)
		}
		if err := incumbent.alive(); err != nil {
			t.Fatalf("the incumbent died after the contender attempt: %v", err)
		}
		if after := readDiscoveryBytes(t, recordPath); !bytes.Equal(after, before) {
			t.Fatalf("the dead record changed across the contender attempt")
		}

		incumbent.shutdown()
		if after := readDiscoveryBytes(t, recordPath); !bytes.Equal(after, before) {
			t.Fatalf("the incumbent shutdown changed a record it never published")
		}
	})

	t.Run("an incompatible live record never authorizes a replacement", func(t *testing.T) {
		e, dataDir := newDiscoveryRoot(t)
		recordPath := filepath.Join(t.TempDir(), "discovery.json")
		owner := startDiscoveryChild(t, e.home, dataDir, recordPath, filepath.Join(t.TempDir(), "marker"), discoveryModeIncompatible)
		owner.await("acquired")
		owner.await("published")
		owner.await("incompatible")
		record := readDiscoveryRecord(t, recordPath)
		before := readDiscoveryBytes(t, recordPath)
		first := healthSnapshot(t, record)
		if first.InstanceId != record.InstanceId || first.ProtocolVersion == "" || first.ProtocolVersion == protocol.N1 {
			t.Fatalf("incompatible health = %s/%q, want the record's instance and a different nonempty version", first.InstanceId, first.ProtocolVersion)
		}

		contenderMarker := filepath.Join(t.TempDir(), "marker")
		contender := startDiscoveryChild(t, e.home, dataDir, recordPath, contenderMarker, discoveryModeOwner)
		contender.await("owned")
		if err := contender.awaitExit(); err != nil {
			t.Fatalf("contender exit: %v output=%q", err, contender.log.text())
		}
		if _, err := os.Stat(contenderMarker); !os.IsNotExist(err) {
			t.Fatalf("the contender invoked a factory over an incompatible live record: marker stat error = %v", err)
		}
		if after := readDiscoveryBytes(t, recordPath); !bytes.Equal(after, before) {
			t.Fatalf("the incompatible record changed across the contender attempt")
		}
		if err := owner.alive(); err != nil {
			t.Fatalf("the incumbent died after the contender attempt: %v", err)
		}
		second := healthSnapshot(t, record)
		if second.InstanceId != first.InstanceId || second.ProtocolVersion != first.ProtocolVersion {
			t.Fatalf("the incumbent health changed after the contender attempt: %s/%s -> %s/%s", first.InstanceId, first.ProtocolVersion, second.InstanceId, second.ProtocolVersion)
		}

		owner.shutdown()
		if after := readDiscoveryBytes(t, recordPath); after != nil {
			t.Fatalf("the record survived managed shutdown (%d bytes)", len(after))
		}
	})

	t.Run("publication requires authenticated matching readiness", func(t *testing.T) {
		t.Run("incompatible health", func(t *testing.T) {
			e, dataDir := newDiscoveryRoot(t)
			recordPath := filepath.Join(t.TempDir(), "discovery.json")
			child := startDiscoveryChild(t, e.home, dataDir, recordPath, filepath.Join(t.TempDir(), "marker"), discoveryModeUnready)
			if token := child.await("refused", "published"); token != "refused" {
				t.Fatalf("publication while readiness reported an incompatible version = %q, want the refusal", token)
			}
			refuseDiscovery(t, recordPath)
			child.shutdown()
			refuseDiscovery(t, recordPath)
		})
		t.Run("cancellation during readiness", func(t *testing.T) {
			e, dataDir := newDiscoveryRoot(t)
			recordPath := filepath.Join(t.TempDir(), "discovery.json")
			child := startDiscoveryChild(t, e.home, dataDir, recordPath, filepath.Join(t.TempDir(), "marker"), discoveryModeCancel)
			if token := child.await("refused", "published"); token != "refused" {
				t.Fatalf("publication after cancellation during readiness = %q, want the refusal", token)
			}
			refuseDiscovery(t, recordPath)
			child.shutdown()
			refuseDiscovery(t, recordPath)
		})
	})
}

// newDiscoveryRoot prepares one isolated owner root and the child dotenv that
// keeps concurrent children from racing the first dotenv creation.
func newDiscoveryRoot(t *testing.T) (*ownerEnv, string) {
	t.Helper()
	home := t.TempDir()
	dataDir := t.TempDir()
	writeServiceFile(t, filepath.Join(home, ".lightcode", ".env"), "# isolated discovery child dotenv\n")
	e := newOwnerEnvIn(t, home, dataDir)
	return e, dataDir
}

// newDiscoveryChildEnv builds the child-process owner environment over the
// parent-prepared root. The parent wrote both service files before spawning,
// so concurrent children never rewrite them.
func newDiscoveryChildEnv(t *testing.T, home, dataDir string) *ownerEnv {
	t.Helper()
	t.Setenv("HOME", home)
	isolateBundledCredentials(t)
	e := &ownerEnv{
		t: t, home: home, dataDir: dataDir,
		configPath: filepath.Join(dataDir, "config.json"),
		events:     &traceLog{},
		prep:       newControlledPrep(),
	}
	e.scopeDataDir.Store("")
	e.scopeWorkspace.Store("")
	return e
}

// discoveryMarkerPlugin records one cross-process Runtime factory execution at
// the given marker path: the winner's marker exists while a losing
// contender's never does, the executable proof that no factory ran without
// the lock.
func discoveryMarkerPlugin(markerPath string) Plugin {
	return Plugin{
		ID:       "discovery-marker",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{Spec[any]("discovery.marker")},
		Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			if err := os.WriteFile(markerPath, []byte("open"), 0o600); err != nil {
				return Instance{}, err
			}
			return Instance{Values: map[string]any{"discovery.marker": "marker"}}, nil
		},
	}
}

// runDiscoveryChild executes one child role and prints only handshake tokens:
// "acquired", "published", "incompatible", "refused", "owned" or "closed".
func runDiscoveryChild(t *testing.T, dataDir string) {
	t.Helper()
	home := os.Getenv(discoveryChildHomeEnv)
	recordPath := os.Getenv(discoveryChildRecordEnv)
	markerPath := os.Getenv(discoveryChildMarkerEnv)
	mode := os.Getenv(discoveryChildModeEnv)
	e := newDiscoveryChildEnv(t, home, dataDir)
	r, err := e.open(context.Background(), e.sqliteDerivationPlugin(), discoveryMarkerPlugin(markerPath))
	if errors.Is(err, ErrOwned) {
		fmt.Println("owned")
		return
	}
	if err != nil {
		t.Fatalf("discovery child open: %v", err)
	}
	ps, err := r.OpenProtocol(context.Background())
	if err != nil {
		t.Fatalf("discovery child OpenProtocol: %v", err)
	}
	fmt.Println("acquired")
	switch mode {
	case discoveryModeOwner, discoveryModeIncompatible:
		if err := ps.PublishDiscovery(recordPath); err != nil {
			t.Fatalf("discovery child PublishDiscovery: %v", err)
		}
		fmt.Println("published")
		if mode == discoveryModeIncompatible {
			ps.server.Handler = incompatibleHealthHandler(ps, ps.server.Handler, "2")
			fmt.Println("incompatible")
		}
	case discoveryModeHold:
		// Ownership and the live listener only: the incumbent deliberately
		// publishes nothing, leaving any existing record untouched.
	case discoveryModeUnready:
		ps.server.Handler = incompatibleHealthHandler(ps, ps.server.Handler, "2")
		if err := ps.PublishDiscovery(recordPath); err == nil {
			t.Fatal("publication succeeded while authenticated health reported an incompatible version")
		}
		refuseDiscovery(t, recordPath)
		fmt.Println("refused")
	case discoveryModeCancel:
		cancelDuringReadiness(t, ps, r)
		refuseDiscovery(t, recordPath)
		fmt.Println("refused")
	default:
		t.Fatalf("unknown discovery child mode %q", mode)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("discovery child Close: %v", err)
	}
	fmt.Println("closed")
}

// cancelDuringReadiness parks the authenticated readiness request inside the
// live listener, cancels the owner work context while the admitted
// publication is in flight, and proves the publication fails without
// remembering a path.
func cancelDuringReadiness(t *testing.T, ps *ProtocolServer, r *Runtime) {
	t.Helper()
	original := ps.server.Handler
	arrived := make(chan struct{}, 1)
	gate := make(chan struct{})
	var once sync.Once
	ps.server.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v1/health" {
			once.Do(func() {
				select {
				case arrived <- struct{}{}:
				default:
				}
				<-gate
			})
		}
		original.ServeHTTP(w, req)
	})
	publishDone := make(chan error, 1)
	go func() { publishDone <- ps.PublishDiscovery(os.Getenv(discoveryChildRecordEnv)) }()
	select {
	case <-arrived:
	case <-time.After(30 * time.Second):
		t.Fatal("readiness verification never reached the parked health handler")
	}
	r.cancelWork()
	close(gate)
	if err := <-publishDone; err == nil {
		t.Fatal("publication succeeded after cancellation during readiness")
	}
	ps.pubMu.Lock()
	remembered := ps.discoveryPath
	ps.pubMu.Unlock()
	if remembered != "" {
		t.Fatalf("canceled publication remembered path %q", remembered)
	}
}

// incompatibleHealthHandler is a test-only native handler wrapper: an
// authenticated health request receives the server's own instance identity
// with a caller-supplied different nonempty version, while every other
// request is delegated unchanged. The fake response rides the production
// authentication gate, so the bearer check is not duplicated. It exists only
// to construct the incompatible-live and unready states and is absent from
// production.
func incompatibleHealthHandler(ps *ProtocolServer, original http.Handler, version string) http.Handler {
	fakeHealth := ps.authorize(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(protocol.Health{
			InstanceId:      ps.instance,
			ProtocolVersion: protocol.HealthProtocolVersion(version),
		})
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/health" {
			original.ServeHTTP(w, r)
			return
		}
		fakeHealth(w, r)
	})
}

// refuseDiscovery proves no usable discovery record exists at the path.
func refuseDiscovery(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("refused publication left a record at %s: %v", path, err)
	}
}

// readDiscoveryRecord reads the isolated 0600 record strictly: mode, one
// complete document with exactly the declared members, the closed version,
// the identity shapes, and the loopback endpoint.
func readDiscoveryRecord(t *testing.T, path string) discoveryRecord {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat discovery record: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("discovery record mode = %o, want 0600", info.Mode().Perm())
	}
	data := readDiscoveryBytes(t, path)
	var record discoveryRecord
	if err := decodeOneDocument(data, &record); err != nil {
		t.Fatalf("decode discovery record at %s: %v", path, err)
	}
	if record.ProtocolVersion != string(protocol.N1) {
		t.Fatalf("discovery record protocol version = %q, want %q", record.ProtocolVersion, protocol.N1)
	}
	if len(record.InstanceId) != 32 || strings.Trim(record.InstanceId, "0123456789abcdef") != "" {
		t.Fatalf("discovery instance %q is not 32 lowercase hex", record.InstanceId)
	}
	if len(record.Credential) != 64 || strings.Trim(record.Credential, "0123456789abcdef") != "" {
		t.Fatal("discovery credential is not 64 lowercase hex")
	}
	endpoint, err := url.Parse(record.Endpoint)
	if err != nil || endpoint.Scheme != "http" || endpoint.Hostname() != "127.0.0.1" || endpoint.Port() == "" {
		t.Fatalf("discovery endpoint %q is not an http loopback host:port", record.Endpoint)
	}
	return record
}

// readDiscoveryBytes reads one discovery record's exact bytes; a missing
// record reports nil so absence assertions stay one call.
func readDiscoveryBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read discovery record %s: %v", path, err)
	}
	return data
}

// healthSnapshot performs one authenticated readiness read using exactly the
// record's credential and returns the decoded body.
func healthSnapshot(t *testing.T, record discoveryRecord) protocol.Health {
	t.Helper()
	resp := rawProtocol(t, http.MethodGet, record.Endpoint+"/v1/health", record.Credential, "")
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read health body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d body=%q, want 200", resp.StatusCode, body)
	}
	var health protocol.Health
	if err := decodeOneDocument(body, &health); err != nil {
		t.Fatalf("decode health body %q: %v", body, err)
	}
	return health
}

// probeHealth performs one non-fatal health request and reports a transport
// failure or a non-200 status as a failed probe, which the dead-record row
// needs to observe; it implements no client connection algorithm.
func probeHealth(endpoint, credential string) error {
	req, err := http.NewRequest(http.MethodGet, endpoint+"/v1/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health probe returned %s", resp.Status)
	}
	return nil
}

// discoveryChild is one self-exec contender process: bounded handshake
// scanning, captured diagnostics, and an exactly-once join.
type discoveryChild struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	tokens chan string
	log    *discoveryLog
	done   chan struct{}
	err    error
}

// startDiscoveryChild launches one child role over the parent-prepared root
// and registers its join before any test root can be cleaned up.
func startDiscoveryChild(t *testing.T, home, dataDir, record, marker, mode string) *discoveryChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRuntimeDiscoveryOwnershipAcrossProcesses$", "-test.timeout=90s")
	cmd.Env = append(os.Environ(),
		discoveryChildDataEnv+"="+dataDir,
		discoveryChildHomeEnv+"="+home,
		discoveryChildRecordEnv+"="+record,
		discoveryChildMarkerEnv+"="+marker,
		discoveryChildModeEnv+"="+mode,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("discovery child StdinPipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("discovery child StdoutPipe: %v", err)
	}
	log := &discoveryLog{}
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatalf("discovery child Start: %v", err)
	}
	c := &discoveryChild{
		t: t, cmd: cmd, stdin: stdin,
		tokens: make(chan string, 64),
		log:    log,
		done:   make(chan struct{}),
	}
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			c.log.add(line)
			c.tokens <- line
		}
		close(c.tokens)
	}()
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	t.Cleanup(c.abort)
	return c
}

// await returns the first token line matching any wanted token, failing on
// child exit or timeout after killing and joining the child.
func (c *discoveryChild) await(tokens ...string) string {
	c.t.Helper()
	want := make(map[string]bool, len(tokens))
	for _, token := range tokens {
		want[token] = true
	}
	deadline := time.After(45 * time.Second)
	for {
		select {
		case line, ok := <-c.tokens:
			if !ok {
				c.abort()
				c.t.Fatalf("child %d exited before printing any of %v: output=%q", c.cmd.Process.Pid, tokens, c.log.text())
			}
			if want[line] {
				return line
			}
		case <-deadline:
			c.abort()
			c.t.Fatalf("child %d printed none of %v within 45s: output=%q", c.cmd.Process.Pid, tokens, c.log.text())
		}
	}
}

// shutdown closes the child's standard input, joins its managed shutdown and
// its process exit.
func (c *discoveryChild) shutdown() {
	c.t.Helper()
	_ = c.stdin.Close()
	c.await("closed")
	if err := c.awaitExit(); err != nil {
		c.t.Fatalf("child %d managed shutdown: %v output=%q", c.cmd.Process.Pid, err, c.log.text())
	}
}

// awaitExit joins the child's process exit, killing it after the deadline.
func (c *discoveryChild) awaitExit() error {
	c.t.Helper()
	select {
	case <-c.done:
		return c.err
	case <-time.After(45 * time.Second):
		_ = c.cmd.Process.Kill()
		<-c.done
		c.t.Fatalf("child %d did not exit", c.cmd.Process.Pid)
		return nil
	}
}

// abort is the registered join: it requests managed shutdown, then kills and
// joins a child that does not converge.
func (c *discoveryChild) abort() {
	_ = c.stdin.Close()
	select {
	case <-c.done:
	case <-time.After(15 * time.Second):
		_ = c.cmd.Process.Kill()
		select {
		case <-c.done:
		case <-time.After(15 * time.Second):
			c.t.Errorf("child %d never exited after kill", c.cmd.Process.Pid)
		}
	}
}

// alive reports whether the child process still exists.
func (c *discoveryChild) alive() error {
	return c.cmd.Process.Signal(syscall.Signal(0))
}

// discoveryLog collects one child's standard output and error lines for
// diagnostics, safely across the scanner and Wait goroutines.
type discoveryLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *discoveryLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *discoveryLog) add(line string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.WriteString(line)
	l.buf.WriteByte('\n')
}

func (l *discoveryLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
