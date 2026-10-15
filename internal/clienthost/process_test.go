package clienthost_test

// The clienthost subprocess fixture: one self-exec test binary plays every
// Runtime owner and contender role over isolated HOME/DataDir/ConfigPath and
// bounded marker rendezvous. The helper invokes the public
// Runtime.Open/OpenProtocol/PublishDiscovery path with the production sqlite
// plugin declaration whose factory callback is wrapped with a marker, so a
// losing attempt proves zero factories and zero storage opened. Every role
// flag rides PrefixArgs before the positional runtime command, exactly as a
// production host would pass them.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/client"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/clienthost"
	"github.com/MMinasyan/lightcode/internal/plugins/sqlite"
	"github.com/MMinasyan/lightcode/protocol"
	"github.com/MMinasyan/lightcode/runtime"
)

// The helper roles: owner starts a real owner and loses the lock cleanly when
// another process owns it; hold owns the lock but publishes only after its
// gate; fail exits nonzero before any Runtime work; incompatible and parked
// publish a real record, then answer readiness through a test-only shaped
// health server.
const (
	roleOwner        = "owner"
	roleHold         = "hold"
	roleFail         = "fail"
	roleIncompatible = "incompatible"
	roleParked       = "parked"
)

var (
	childRole       = flag.String("clienthost-child-role", "", "test-only helper role")
	childFactoryDir = flag.String("clienthost-child-factory-dir", "", "test-only factory marker directory")
	childAttemptDir = flag.String("clienthost-child-attempt-dir", "", "test-only attempt marker directory")
	childGate       = flag.String("clienthost-child-gate", "", "test-only gate marker path")
	childAcquired   = flag.String("clienthost-child-acquired", "", "test-only lock-acquired marker path")
	childReady      = flag.String("clienthost-child-ready", "", "test-only readiness marker path")
	childEndpoint   = flag.String("clienthost-child-endpoint", "", "test-only original endpoint file")
	childStop       = flag.String("clienthost-child-stop", "", "test-only stop marker path")
	childClosed     = flag.String("clienthost-child-closed", "", "test-only closed marker path")
)

// TestClientHostSubprocess is the one self-exec fixture entry: with a role
// flag it runs one helper child; an ordinary package run skips it.
func TestClientHostSubprocess(t *testing.T) {
	if *childRole == "" {
		t.Skip("subprocess fixture entry: runs only in a helper child")
	}
	runChild()
}

// runChild executes one helper role over the exact runtime argument tail the
// common startup helper appends after PrefixArgs.
func runChild() {
	args := flag.Args()
	if len(args) != 5 || args[0] != "runtime" || args[1] != "--data-dir" || args[3] != "--config" {
		childAbort("runtime argument tail %q, want exactly runtime --data-dir <dir> --config <path>", args)
	}
	dataDir, configPath := args[2], args[4]
	if !filepath.IsAbs(dataDir) || !filepath.IsAbs(configPath) {
		childAbort("runtime argument paths are not absolute: %q, %q", dataDir, configPath)
	}
	writeAttempt()
	switch *childRole {
	case roleOwner:
		runOwnerChild(dataDir, configPath)
	case roleHold:
		runHoldChild(dataDir, configPath)
	case roleFail:
		os.Exit(2)
	case roleIncompatible:
		runShapedChild(dataDir, configPath, "2", false)
	case roleParked:
		runShapedChild(dataDir, configPath, "", true)
	default:
		childAbort("unknown helper role %q", *childRole)
	}
}

// runOwnerChild starts a real owner, exits zero on contention loss, and
// otherwise publishes and serves until the stop marker.
func runOwnerChild(dataDir, configPath string) {
	awaitGate()
	r, err := openRuntime(dataDir, configPath)
	if errors.Is(err, runtime.ErrOwned) {
		writeLostMarker()
		os.Exit(0)
	}
	if err != nil {
		childAbort("runtime open: %v", err)
	}
	ps, err := r.OpenProtocol(context.Background())
	if err != nil {
		childAbort("OpenProtocol: %v", err)
	}
	if err := ps.PublishDiscovery(filepath.Join(dataDir, "runtime.json")); err != nil {
		childAbort("PublishDiscovery: %v", err)
	}
	writeMarker(*childReady, ps.Endpoint())
	awaitStopAndClose(r)
}

// runHoldChild owns the lock without publishing until its gate opens, so a
// contender started before publication loses the lock deterministically.
func runHoldChild(dataDir, configPath string) {
	r, err := openRuntime(dataDir, configPath)
	if err != nil {
		childAbort("runtime open: %v", err)
	}
	writeMarker(*childAcquired, "acquired")
	awaitGate()
	ps, err := r.OpenProtocol(context.Background())
	if err != nil {
		childAbort("OpenProtocol: %v", err)
	}
	if err := ps.PublishDiscovery(filepath.Join(dataDir, "runtime.json")); err != nil {
		childAbort("PublishDiscovery: %v", err)
	}
	writeMarker(*childReady, ps.Endpoint())
	awaitStopAndClose(r)
}

// runShapedChild publishes the real record first, then answers readiness
// through a test-only health server built from the published instance and
// credential, and rewrites only the record's endpoint. The real owner and
// its original endpoint stay live for survival checks.
func runShapedChild(dataDir, configPath, version string, park bool) {
	r, err := openRuntime(dataDir, configPath)
	if err != nil {
		childAbort("runtime open: %v", err)
	}
	ps, err := r.OpenProtocol(context.Background())
	if err != nil {
		childAbort("OpenProtocol: %v", err)
	}
	recordPath := filepath.Join(dataDir, "runtime.json")
	if err := ps.PublishDiscovery(recordPath); err != nil {
		childAbort("PublishDiscovery: %v", err)
	}
	shapedEndpoint, originalEndpoint := shapeHealth(recordPath, version, park)
	writeMarker(*childEndpoint, originalEndpoint)
	writeMarker(*childReady, shapedEndpoint)
	awaitStopAndClose(r)
}

// openRuntime opens the production sqlite plugin declaration through the
// public Runtime constructor, wrapping its test-only factory callback with the
// per-attempt marker. No controlled preparation or private constructor is
// used.
func openRuntime(dataDir, configPath string) (*runtime.Runtime, error) {
	plugins := []runtime.Plugin{sqlite.Plugin()}
	for index := range plugins {
		plugin := plugins[index]
		open := plugin.Open
		plugin.Open = func(ctx context.Context, info runtime.ScopeInfo, bindings runtime.Bindings) (runtime.Instance, error) {
			markFactory(plugin.ID)
			return open(ctx, info, bindings)
		}
		plugins[index] = plugin
	}
	return runtime.Open(context.Background(), runtime.Options{DataDir: dataDir, ConfigPath: configPath, Plugins: plugins})
}

// shapeHealth starts one test-only health server over the record's published
// instance and credential, verifies the bearer credential, parks or answers
// health with the supplied version, and rewrites only the endpoint member of
// the record as raw members.
func shapeHealth(recordPath, version string, park bool) (shaped, original string) {
	data, err := os.ReadFile(recordPath)
	if err != nil {
		childAbort("read published record: %v", err)
	}
	record := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &record); err != nil {
		childAbort("decode published record: %v", err)
	}
	var instance, credential string
	if err := json.Unmarshal(record["instance_id"], &instance); err != nil {
		childAbort("decode published instance: %v", err)
	}
	if err := json.Unmarshal(record["credential"], &credential); err != nil {
		childAbort("decode published credential: %v", err)
	}
	if err := json.Unmarshal(record["endpoint"], &original); err != nil {
		childAbort("decode published endpoint: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		childAbort("listen shaped health: %v", err)
	}
	shaped = "http://" + listener.Addr().String()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") != "Bearer "+credential {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if park {
			<-req.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"instance_id":      instance,
			"protocol_version": version,
		})
	})}
	go func() { _ = server.Serve(listener) }()
	record["endpoint"], err = json.Marshal(shaped)
	if err != nil {
		childAbort("encode shaped endpoint: %v", err)
	}
	shapedRecord, err := json.Marshal(record)
	if err != nil {
		childAbort("encode shaped record: %v", err)
	}
	if err := os.WriteFile(recordPath, shapedRecord, 0o600); err != nil {
		childAbort("rewrite record endpoint: %v", err)
	}
	return shaped, original
}

// writeAttempt records this attempt's exact runtime argument tail under the
// attempt directory, one file per process.
func writeAttempt() {
	if *childAttemptDir == "" {
		return
	}
	tail, err := json.Marshal(flag.Args())
	if err != nil {
		childAbort("encode runtime argument tail: %v", err)
	}
	name := filepath.Join(*childAttemptDir, fmt.Sprintf("attempt-%d", os.Getpid()))
	if err := os.WriteFile(name, tail, 0o600); err != nil {
		childAbort("write attempt marker: %v", err)
	}
}

// writeLostMarker records one clean contention loss before the zero exit.
func writeLostMarker() {
	if *childAttemptDir == "" {
		return
	}
	name := filepath.Join(*childAttemptDir, fmt.Sprintf("lost-%d", os.Getpid()))
	if err := os.WriteFile(name, []byte("lost"), 0o600); err != nil {
		childAbort("write lost marker: %v", err)
	}
}

// markFactory appends one factory execution to this attempt's own marker, so
// a losing attempt's zero factories and zero storage stay provable per
// process.
func markFactory(pluginID string) {
	if *childFactoryDir == "" {
		return
	}
	path := filepath.Join(*childFactoryDir, fmt.Sprintf("factory-%d", os.Getpid()))
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		childAbort("open factory marker: %v", err)
	}
	if _, err := io.WriteString(file, pluginID+"\n"); err != nil {
		childAbort("write factory marker: %v", err)
	}
	if err := file.Close(); err != nil {
		childAbort("close factory marker: %v", err)
	}
}

// writeMarker atomically publishes one rendezvous marker.
func writeMarker(path, content string) {
	if err := atomicfs.Write(path, []byte(content), 0o600); err != nil {
		childAbort("write marker %s: %v", path, err)
	}
}

// awaitGate waits for the role's gate marker when one is declared.
func awaitGate() {
	if *childGate == "" {
		return
	}
	for {
		if _, err := os.Stat(*childGate); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitStopAndClose waits for the stop marker, then performs the managed
// Runtime Close before the process exits.
func awaitStopAndClose(r *runtime.Runtime) {
	if *childStop == "" {
		childAbort("publishing helper role requires a stop marker")
	}
	for {
		if _, err := os.Stat(*childStop); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := r.Close(context.Background()); err != nil {
		childAbort("runtime close: %v", err)
	}
	writeMarker(*childClosed, "closed")
}

// childAbort reports a fixture failure and exits nonzero.
func childAbort(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "clienthost helper: "+format+"\n", args...)
	os.Exit(2)
}

// sandbox isolates one test's HOME, DataDir, ConfigPath, credentials, and
// rendezvous markers.
type sandbox struct {
	t          *testing.T
	dataDir    string
	configPath string
	dir        string
	stop       string
	closed     string
}

// newSandbox prepares the isolated roots and registers the hard cleanup that
// stops and reaps every identified helper child.
func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	home := t.TempDir()
	dataDir := t.TempDir()
	dir := t.TempDir()
	configPath := filepath.Join(dataDir, "config.json")
	if err := os.WriteFile(configPath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("prepare isolated config: %v", err)
	}
	t.Setenv("HOME", home)
	isolateBundledCredentials(t)
	s := &sandbox{
		t: t, dataDir: dataDir, configPath: configPath, dir: dir,
		stop: filepath.Join(dir, "stop"), closed: filepath.Join(dir, "closed"),
	}
	t.Cleanup(s.reapLaunched)
	return s
}

// wantTail is the exact runtime argument tail the common startup helper must
// append after PrefixArgs.
func (s *sandbox) wantTail() []string {
	return []string{"runtime", "--data-dir", s.dataDir, "--config", s.configPath}
}

// launched describes the marker paths one clienthost Connect call passes in
// PrefixArgs, so every child it starts is identified and reapable.
type launched struct {
	sandbox *sandbox
	dir     string
	gate    string
	ready   string
}

func (s *sandbox) launched(name string) *launched {
	s.t.Helper()
	dir := filepath.Join(s.dir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		s.t.Fatalf("prepare launched attempt directory: %v", err)
	}
	return &launched{
		sandbox: s, dir: dir,
		gate:  filepath.Join(dir, "gate"),
		ready: filepath.Join(dir, "ready"),
	}
}

// options builds one Connect input whose PrefixArgs carry the helper flags
// before the positional runtime command.
func (l *launched) options(executable, role string, gated bool) clienthost.Options {
	gate := ""
	if gated {
		gate = l.gate
	}
	return clienthost.Options{
		DataDir:    l.sandbox.dataDir,
		ConfigPath: l.sandbox.configPath,
		Executable: executable,
		PrefixArgs: []string{
			"-test.run=^TestClientHostSubprocess$",
			"-test.timeout=60s",
			"-clienthost-child-role=" + role,
			"-clienthost-child-factory-dir=" + l.dir,
			"-clienthost-child-attempt-dir=" + l.dir,
			"-clienthost-child-gate=" + gate,
			"-clienthost-child-ready=" + l.ready,
			"-clienthost-child-stop=" + l.sandbox.stop,
			"-clienthost-child-closed=" + l.sandbox.closed,
		},
	}
}

// directChild is one parent-started helper process with its captured
// diagnostics and exactly-once join.
type directChild struct {
	t        *testing.T
	role     string
	cmd      *exec.Cmd
	log      *lockedBuffer
	done     chan struct{}
	err      error
	gate     string
	acquired string
	ready    string
	endpoint string
	stop     string
}

// startDirect launches one parent-owned helper role over the sandbox roots.
func (s *sandbox) startDirect(t *testing.T, role string) *directChild {
	t.Helper()
	dir := filepath.Join(s.dir, role)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("prepare %s helper directory: %v", role, err)
	}
	c := &directChild{
		t: t, role: role,
		gate:     filepath.Join(dir, "gate"),
		acquired: filepath.Join(dir, "acquired"),
		ready:    filepath.Join(dir, "ready"),
		endpoint: filepath.Join(dir, "endpoint"),
		stop:     s.stop,
	}
	args := []string{
		"-test.run=^TestClientHostSubprocess$",
		"-test.timeout=60s",
		"-clienthost-child-role=" + role,
		"-clienthost-child-gate=" + c.gate,
		"-clienthost-child-acquired=" + c.acquired,
		"-clienthost-child-ready=" + c.ready,
		"-clienthost-child-endpoint=" + c.endpoint,
		"-clienthost-child-stop=" + c.stop,
		"-clienthost-child-closed=" + s.closed,
		"runtime", "--data-dir", s.dataDir, "--config", s.configPath,
	}
	cmd := exec.Command(os.Args[0], args...)
	log := &lockedBuffer{}
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s helper: %v", role, err)
	}
	c.cmd = cmd
	c.log = log
	c.done = make(chan struct{})
	go func() {
		c.err = cmd.Wait()
		close(c.done)
	}()
	t.Cleanup(c.killAndJoin)
	return c
}

// stopAndJoin performs the managed stop-marker shutdown and joins the exit.
func (c *directChild) stopAndJoin(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(c.stop, []byte("stop"), 0o600); err != nil {
		t.Fatalf("write stop marker: %v", err)
	}
	select {
	case <-c.done:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s helper did not stop; log=%s", c.role, c.log.text())
	}
	if c.err != nil {
		t.Fatalf("%s helper exit: %v log=%s", c.role, c.err, c.log.text())
	}
}

// killAndJoin is the registered hard cleanup: stop, then kill and join a
// helper that does not converge.
func (c *directChild) killAndJoin() {
	_ = os.WriteFile(c.stop, []byte("stop"), 0o600)
	select {
	case <-c.done:
		return
	case <-time.After(15 * time.Second):
	}
	_ = c.cmd.Process.Kill()
	select {
	case <-c.done:
	case <-time.After(15 * time.Second):
		c.t.Errorf("%s helper %d never exited after kill", c.role, c.cmd.Process.Pid)
	}
}

// reapLaunched is the sandbox hard cleanup for Connect-started children: the
// stop marker requests managed Close, and any child that does not converge is
// killed by its identified PID and waited for until the asynchronous join
// reaps it.
func (s *sandbox) reapLaunched() {
	_ = os.WriteFile(s.stop, []byte("stop"), 0o600)
	deadline := time.Now().Add(20 * time.Second)
	for _, pid := range s.attemptPIDs() {
		process, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		for aliveProcess(process) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if aliveProcess(process) {
			_ = process.Kill()
		}
		for aliveProcess(process) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if aliveProcess(process) {
			s.t.Errorf("launched child %d survived cleanup", pid)
		}
	}
}

// attemptPIDs returns every identified helper PID under the sandbox.
func (s *sandbox) attemptPIDs() []int {
	var pids []int
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		attempts, err := os.ReadDir(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			continue
		}
		for _, attempt := range attempts {
			if !strings.HasPrefix(attempt.Name(), "attempt-") {
				continue
			}
			if pid, err := strconv.Atoi(strings.TrimPrefix(attempt.Name(), "attempt-")); err == nil {
				pids = append(pids, pid)
			}
		}
	}
	return pids
}

// outcome is one Connect result, delivered through connectAsync's one-slot
// buffered channel.
type outcome struct {
	client *client.Client
	err    error
}

// connectAsync starts one Connect call on its own goroutine and returns the
// one-slot buffered channel carrying its outcome.
func connectAsync(ctx context.Context, opts clienthost.Options) <-chan outcome {
	results := make(chan outcome, 1)
	go func() {
		c, err := clienthost.Connect(ctx, opts)
		results <- outcome{client: c, err: err}
	}()
	return results
}

// TestClientHostConcurrentStartup proves two concurrent startup calls reach
// one compatible owner: exactly one contender wins the ownership lock and
// opens its factories, the other opens none, and both callers receive the
// same authenticated instance.
func TestClientHostConcurrentStartup(t *testing.T) {
	s := newSandbox(t)
	first := s.launched("first")
	second := s.launched("second")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	firstResults := connectAsync(ctx, first.options(os.Args[0], roleOwner, false))
	secondResults := connectAsync(ctx, second.options(os.Args[0], roleOwner, false))

	var clients []*client.Client
	for _, results := range []<-chan outcome{firstResults, secondResults} {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("concurrent Connect: %v", result.err)
			}
			clients = append(clients, result.client)
		case <-time.After(70 * time.Second):
			t.Fatal("concurrent Connect did not return")
		}
	}
	if clients[0].InstanceID() == "" || clients[0].InstanceID() != clients[1].InstanceID() {
		t.Fatalf("concurrent clients = %q/%q, want one authenticated owner", clients[0].InstanceID(), clients[1].InstanceID())
	}
	recordPath := filepath.Join(s.dataDir, "runtime.json")
	record := readRecord(t, recordPath)
	if record.InstanceID != clients[0].InstanceID() {
		t.Fatalf("client instance %q, record %q", clients[0].InstanceID(), record.InstanceID)
	}
	health := authenticatedHealth(t, record.Endpoint, record.Credential)
	if health.InstanceId != record.InstanceID || health.ProtocolVersion != protocol.N1 {
		t.Fatalf("authenticated health = %s/%s, want the record's instance and wire version", health.InstanceId, health.ProtocolVersion)
	}

	winners := 0
	for _, l := range []*launched{first, second} {
		for _, path := range readAttemptFiles(t, l.dir) {
			if tail := attemptTail(t, path); !slices.Equal(tail, s.wantTail()) {
				t.Fatalf("contender argument tail = %q, want %q", tail, s.wantTail())
			}
			plugins := readFactoryMarker(t, l.dir, attemptPID(t, path))
			if plugins == nil {
				continue
			}
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("factory-opening contender attempts = %d, want exactly one", winners)
	}

	if err := os.WriteFile(s.stop, []byte("stop"), 0o600); err != nil {
		t.Fatalf("write stop marker: %v", err)
	}
	awaitMarker(t, s.closed)
	if _, err := os.Stat(recordPath); !os.IsNotExist(err) {
		t.Fatalf("the record survived managed shutdown: %v", err)
	}
}

// TestClientHostIncompatibleIncumbent proves an authenticated owner speaking
// another wire version is reported immediately: no contender is started, the
// incumbent keeps its record and its real endpoint, and the caller sees the
// incompatible outcome, not a launch or probe failure.
func TestClientHostIncompatibleIncumbent(t *testing.T) {
	s := newSandbox(t)
	incumbent := s.startDirect(t, roleIncompatible)
	shapedEndpoint := strings.TrimSpace(awaitMarker(t, incumbent.ready, incumbent))
	originalEndpoint := strings.TrimSpace(awaitMarker(t, incumbent.endpoint, incumbent))
	if shapedEndpoint == originalEndpoint {
		t.Fatal("the shaped health server did not replace the record endpoint")
	}
	recordPath := filepath.Join(s.dataDir, "runtime.json")
	record := readRecord(t, recordPath)
	if record.Endpoint != shapedEndpoint {
		t.Fatalf("record endpoint = %q, want the shaped %q", record.Endpoint, shapedEndpoint)
	}
	before := readFile(t, recordPath)

	contender := s.launched("contender")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := clienthost.Connect(ctx, contender.options(filepath.Join(s.dir, "no-such-executable"), roleOwner, false))
	if !errors.Is(err, client.ErrIncompatibleProtocol) {
		t.Fatalf("Connect over an incompatible incumbent = %v, want ErrIncompatibleProtocol", err)
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("Connect did not return immediately: %v", err)
	}
	if attempts := readAttemptFiles(t, contender.dir); len(attempts) != 0 {
		t.Fatalf("an incompatible incumbent started %d contender attempts", len(attempts))
	}
	if after := readFile(t, recordPath); !bytes.Equal(after, before) {
		t.Fatal("the incompatible record changed across the probe")
	}
	health := authenticatedHealth(t, originalEndpoint, record.Credential)
	if health.InstanceId != record.InstanceID || health.ProtocolVersion != protocol.N1 {
		t.Fatalf("the incumbent's real health = %s/%s", health.InstanceId, health.ProtocolVersion)
	}
	incumbent.stopAndJoin(t)
}

// TestClientHostReadinessExitAndWinner proves a contender that exits before
// readiness is not classified as failure: the incumbent publishes after the
// contender loses the held lock, and Connect returns the winner instead of
// the local loser's outcome.
func TestClientHostReadinessExitAndWinner(t *testing.T) {
	s := newSandbox(t)
	incumbent := s.startDirect(t, roleHold)
	awaitMarker(t, incumbent.acquired, incumbent)

	contender := s.launched("contender")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	results := connectAsync(ctx, contender.options(os.Args[0], roleOwner, false))

	// The contender loses the held lock and exits before any owner publishes.
	awaitLost(t, contender)
	if err := os.WriteFile(incumbent.gate, []byte("publish"), 0o600); err != nil {
		t.Fatalf("open the incumbent's publish gate: %v", err)
	}
	awaitMarker(t, incumbent.ready, incumbent)

	var res outcome
	select {
	case res = <-results:
	case <-time.After(60 * time.Second):
		t.Fatal("Connect did not return after the contender's pre-ready exit")
	}
	if res.err != nil {
		t.Fatalf("Connect after the contender's pre-ready exit = %v, want the winner", res.err)
	}
	record := readRecord(t, filepath.Join(s.dataDir, "runtime.json"))
	if res.client.InstanceID() != record.InstanceID {
		t.Fatalf("Connect returned instance %q, winner %q", res.client.InstanceID(), record.InstanceID)
	}
	for _, path := range readAttemptFiles(t, contender.dir) {
		if plugins := readFactoryMarker(t, contender.dir, attemptPID(t, path)); plugins != nil {
			t.Fatalf("the losing contender opened factories %q", plugins)
		}
	}
	incumbent.stopAndJoin(t)
}

// TestClientHostCancellationDoesNotKillOwner proves caller cancellation ends
// only the probes and waits: the detached contender survives, becomes the
// owner after the canceled call, serves authenticated health, and is reaped
// through its managed stop.
func TestClientHostCancellationDoesNotKillOwner(t *testing.T) {
	s := newSandbox(t)
	contender := s.launched("owner")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := connectAsync(ctx, contender.options(os.Args[0], roleOwner, true))

	attempts := awaitAttemptCount(t, contender.dir, 1)
	cancel()
	var res outcome
	select {
	case res = <-results:
	case <-time.After(30 * time.Second):
		t.Fatal("canceled Connect did not return")
	}
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("canceled Connect = %v, want context.Canceled", res.err)
	}
	if res.client != nil {
		t.Fatal("canceled Connect returned a client")
	}

	if err := os.WriteFile(contender.gate, []byte("go"), 0o600); err != nil {
		t.Fatalf("release the surviving contender: %v", err)
	}
	endpoint := strings.TrimSpace(awaitMarker(t, contender.ready))
	record := readRecord(t, filepath.Join(s.dataDir, "runtime.json"))
	if record.Endpoint != endpoint {
		t.Fatalf("record endpoint = %q, want the surviving owner %q", record.Endpoint, endpoint)
	}
	health := authenticatedHealth(t, record.Endpoint, record.Credential)
	if health.InstanceId != record.InstanceID || health.ProtocolVersion != protocol.N1 {
		t.Fatalf("the surviving owner health = %s/%s", health.InstanceId, health.ProtocolVersion)
	}
	pid := attemptPID(t, attempts[0])
	if !aliveProcessByPID(pid) {
		t.Fatalf("the canceled Connect killed owner process %d", pid)
	}
	if plugins := readFactoryMarker(t, contender.dir, pid); plugins == nil {
		t.Fatal("the surviving owner opened no factories")
	}

	if err := os.WriteFile(s.stop, []byte("stop"), 0o600); err != nil {
		t.Fatalf("write stop marker: %v", err)
	}
	awaitMarker(t, s.closed)
	awaitReaped(t, pid)
}

// TestClientHostHealthTimeoutPreservesHolder proves a probe-only timeout is
// unusable discovery that never disturbs the holder: the parked holder keeps
// its record, lock, and real endpoint, and the loop ends only with the
// caller's deadline.
func TestClientHostHealthTimeoutPreservesHolder(t *testing.T) {
	s := newSandbox(t)
	incumbent := s.startDirect(t, roleParked)
	awaitMarker(t, incumbent.ready, incumbent)
	originalEndpoint := strings.TrimSpace(awaitMarker(t, incumbent.endpoint, incumbent))
	recordPath := filepath.Join(s.dataDir, "runtime.json")
	record := readRecord(t, recordPath)
	before := readFile(t, recordPath)

	contender := s.launched("contenders")
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	_, err := clienthost.Connect(ctx, contender.options(os.Args[0], roleOwner, false))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Connect against a parked health holder = %v, want the caller deadline", err)
	}

	health := authenticatedHealth(t, originalEndpoint, record.Credential)
	if health.InstanceId != record.InstanceID || health.ProtocolVersion != protocol.N1 {
		t.Fatalf("the preserved holder health = %s/%s", health.InstanceId, health.ProtocolVersion)
	}
	if after := readFile(t, recordPath); !bytes.Equal(after, before) {
		t.Fatal("the parked record changed across the failed probes")
	}
	contenders := readAttemptFiles(t, contender.dir)
	if len(contenders) == 0 {
		t.Fatal("the parked holder was never probed by a started contender")
	}
	for _, path := range contenders {
		if plugins := readFactoryMarker(t, contender.dir, attemptPID(t, path)); plugins != nil {
			t.Fatalf("a parked-health contender opened factories %q", plugins)
		}
	}
	incumbent.stopAndJoin(t)
}

// TestClientHostPreReadyFailureRestartsDiscovery proves a started contender
// whose initialization exits nonzero restarts discovery from scratch until
// caller cancellation, leaving no Runtime state or factory execution behind.
func TestClientHostPreReadyFailureRestartsDiscovery(t *testing.T) {
	s := newSandbox(t)
	failures := s.launched("failures")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	results := connectAsync(ctx, failures.options(os.Args[0], roleFail, false))

	attempts := awaitAttemptCount(t, failures.dir, 2)
	cancel()
	var res outcome
	select {
	case res = <-results:
	case <-time.After(30 * time.Second):
		t.Fatal("Connect did not return after cancellation")
	}
	if !errors.Is(res.err, context.Canceled) {
		t.Fatalf("canceled Connect = %v, want context.Canceled", res.err)
	}
	for _, path := range attempts {
		if tail := attemptTail(t, path); !slices.Equal(tail, s.wantTail()) {
			t.Fatalf("failed contender argument tail = %q, want %q", tail, s.wantTail())
		}
		if plugins := readFactoryMarker(t, failures.dir, attemptPID(t, path)); plugins != nil {
			t.Fatalf("a failing contender opened factories %q", plugins)
		}
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "runtime.json")); !os.IsNotExist(err) {
		t.Fatalf("a failing contender published discovery: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "lightcode.db")); !os.IsNotExist(err) {
		t.Fatalf("a failing contender opened storage: %v", err)
	}
}

// TestClientHostLaunchError proves a process that cannot be started is the
// separately stated launch error, not a probe outcome and not a started
// contender's exit.
func TestClientHostLaunchError(t *testing.T) {
	s := newSandbox(t)
	missing := filepath.Join(s.dir, "no-such-executable")
	_, err := clienthost.Connect(context.Background(), clienthost.Options{
		DataDir:    s.dataDir,
		ConfigPath: s.configPath,
		Executable: missing,
	})
	if err == nil {
		t.Fatal("Connect with a missing runtime executable reported success")
	}
	if errors.Is(err, client.ErrNoRuntime) || errors.Is(err, client.ErrIncompatibleProtocol) {
		t.Fatalf("launch failure classified as a probe outcome: %v", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("launch error = %v, want the missing executable reported", err)
	}
	if _, err := os.Stat(filepath.Join(s.dataDir, "runtime.json")); !os.IsNotExist(err) {
		t.Fatalf("launch failure left discovery state: %v", err)
	}
}

// recordView is the test-side read-only view of the published discovery
// record: the endpoint, the instance identity, and the bearer credential.
// client.Connect owns strict discovery validation; this view only supplies
// the members the fixture asserts on.
type recordView struct {
	Endpoint   string `json:"endpoint"`
	InstanceID string `json:"instance_id"`
	Credential string `json:"credential"`
}

// readRecord decodes one published discovery record through the read-only
// fixture view.
func readRecord(t *testing.T, path string) recordView {
	t.Helper()
	var record recordView
	if err := json.Unmarshal(readFile(t, path), &record); err != nil {
		t.Fatalf("decode discovery record: %v", err)
	}
	return record
}

// readFile reads one file or fails the test.
func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

// authenticatedHealth performs one authenticated readiness read.
func authenticatedHealth(t *testing.T, endpoint, credential string) protocol.Health {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, endpoint+"/v1/health", nil)
	if err != nil {
		t.Fatalf("build health request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("authenticated health: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read health body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated health = %d body=%q", resp.StatusCode, body)
	}
	var health protocol.Health
	if err := json.Unmarshal(body, &health); err != nil {
		t.Fatalf("decode health body %q: %v", body, err)
	}
	return health
}

// awaitMarker waits for one rendezvous marker file and returns its content.
func awaitMarker(t *testing.T, path string, children ...*directChild) string {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data)
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read marker %s: %v", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, child := range children {
		t.Logf("%s helper log: %s", child.role, child.log.text())
	}
	t.Fatalf("marker %s did not appear", path)
	return ""
}

// awaitLost waits for one launched contender's clean contention-loss marker.
func awaitLost(t *testing.T, l *launched) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		entries, err := os.ReadDir(l.dir)
		if err == nil {
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), "lost-") {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no losing contender exited under %s", l.dir)
}

// awaitAttemptCount waits for at least want started helper attempts and
// returns their marker paths.
func awaitAttemptCount(t *testing.T, dir string, want int) []string {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if attempts := readAttemptFiles(t, dir); len(attempts) >= want {
			return attempts
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("only %d helper attempts started under %s, want %d", len(readAttemptFiles(t, dir)), dir, want)
	return nil
}

// readAttemptFiles lists one directory's attempt marker files.
func readAttemptFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var paths []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "attempt-") {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	return paths
}

// attemptTail decodes one attempt marker's exact runtime argument tail.
func attemptTail(t *testing.T, path string) []string {
	t.Helper()
	var tail []string
	if err := json.Unmarshal(readFile(t, path), &tail); err != nil {
		t.Fatalf("decode attempt marker %s: %v", path, err)
	}
	return tail
}

// attemptPID extracts the identified helper PID from an attempt marker path.
func attemptPID(t *testing.T, path string) int {
	t.Helper()
	pid, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(path), "attempt-"))
	if err != nil {
		t.Fatalf("attempt marker %s carries no PID: %v", path, err)
	}
	return pid
}

// readFactoryMarker returns the plugin IDs one identified attempt opened, or
// nil when that attempt never reached a factory.
func readFactoryMarker(t *testing.T, dir string, pid int) []string {
	t.Helper()
	path := filepath.Join(dir, fmt.Sprintf("factory-%d", pid))
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read factory marker %s: %v", path, err)
	}
	var plugins []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line != "" {
			plugins = append(plugins, line)
		}
	}
	return plugins
}

// aliveProcess reports whether one process still exists.
func aliveProcess(process *os.Process) bool {
	return process.Signal(syscall.Signal(0)) == nil
}

// aliveProcessByPID reports whether one PID still exists.
func aliveProcessByPID(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return aliveProcess(process)
}

// awaitReaped waits until one identified helper PID is gone, proving the
// asynchronous join reaped it.
func awaitReaped(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for aliveProcessByPID(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if aliveProcessByPID(pid) {
		t.Fatalf("helper %d was never reaped", pid)
	}
}

// lockedBuffer collects one helper child's diagnostics safely.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lockedBuffer) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// isolateBundledCredentials empties every api_key_env declared by the bundled
// catalog so a captured build in these tests never starts a provider
// discovery attempt from an ambient shell key.
func isolateBundledCredentials(t *testing.T) {
	t.Helper()
	catalogDir := filepath.Join("..", "..", "internal", "catalog", "builtin")
	entries, err := os.ReadDir(catalogDir)
	if err != nil {
		t.Fatalf("read bundled catalog: %v", err)
	}
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
			t.Fatalf("decode bundled provider %s: %v", entry.Name(), err)
		}
		if provider.Transport.APIKeyEnv != "" {
			t.Setenv(provider.Transport.APIKeyEnv, "")
		}
	}
}
