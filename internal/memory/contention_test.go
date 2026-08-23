package memory

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

// memoryContentionRole selects the child half of the two-process memory rows
// below. Only the role rides the flag; every other parameter rides the
// environment, so one child entry point serves every row.
var memoryContentionRole = flag.String("lightcode.memory-contention-child", "", "run as a memory contention child: write-md, save, reconcile or create")

const (
	memoryContentionDirEnv     = "LIGHTCODE_MEMORY_CONTENTION_DIR"
	memoryContentionRootEnv    = "LIGHTCODE_MEMORY_CONTENTION_ROOT"
	memoryContentionTitleEnv   = "LIGHTCODE_MEMORY_CONTENTION_TITLE"
	memoryContentionContentEnv = "LIGHTCODE_MEMORY_CONTENTION_CONTENT"
	memoryContentionPathEnv    = "LIGHTCODE_MEMORY_CONTENTION_PATH"
	memoryContentionDataEnv    = "LIGHTCODE_MEMORY_CONTENTION_DATA"
	memoryContentionModeEnv    = "LIGHTCODE_MEMORY_CONTENTION_MODE"
	memoryContentionTargetEnv  = "LIGHTCODE_MEMORY_CONTENTION_TARGET"
	// memoryContentionScriptEnv carries the child's nonce script as
	// comma-separated 8-hex values. It is the test-process input that makes
	// the production retry loop's names predictable; production keeps no
	// nonce seam of its own.
	memoryContentionScriptEnv = "LIGHTCODE_MEMORY_CONTENTION_NONCES"
)

const (
	roleWriteMD   = "write-md"
	roleSave      = "save"
	roleReconcile = "reconcile"
	roleCreate    = "create"
)

// Child modes, applied at the temp sync whose one-based index matches the
// target. complete performs every real write. fail turns that one sync into
// an ordinary handled failure. park blocks in it until the process is killed.
// hold blocks in it until the parent closes stdin, so two processes can both
// be inside their real write before either is released.
const (
	modeComplete = "complete"
	modeFail     = "fail"
	modePark     = "park"
	modeHold     = "hold"
)

// The child announces markers on stdout. writeEntryMarker fires on every
// entry into a real temp sync, so the parent can require that a process
// actually wrote rather than returning through a skip branch. resultPrefix
// carries the operation's outcome, and doneMarker closes the run.
const (
	writeEntryMarker     = "write-entry"
	candidateReadyMarker = "candidate_ready"
	resultPrefix         = "result="
	doneMarker           = "done"
)

// contentionBound bounds every child, marker wait and reader loop, so no
// regression that blocks instead of returning can hang the package.
const contentionBound = 30 * time.Second

// scriptedNonceReader is the test-process replacement for crypto/rand.Reader.
// It serves a fixed cycle of nonces, always fills the whole buffer, and never
// returns io.EOF or an error, so the production retry loop sees exactly the
// scripted sequence of candidate names and nothing else changes.
type scriptedNonceReader struct {
	script   [][]byte
	released <-chan struct{}
	next     int
	ready    bool
}

func (r *scriptedNonceReader) Read(p []byte) (int, error) {
	if !r.ready {
		r.ready = true
		fmt.Println(candidateReadyMarker)
		<-r.released
	}
	for n := 0; n < len(p); {
		chunk := r.script[r.next%len(r.script)]
		r.next++
		n += copy(p[n:], chunk)
	}
	return len(p), nil
}

func parseNonceScript(t *testing.T, script string) [][]byte {
	t.Helper()
	var out [][]byte
	for _, field := range strings.Split(script, ",") {
		raw, err := hex.DecodeString(field)
		if err != nil || len(raw) != 4 {
			t.Fatalf("nonce %q is not four hex-encoded bytes: %v", field, err)
		}
		out = append(out, raw)
	}
	return out
}

// memoryContentionChild runs the child half of a row and never returns: it is
// the first statement of every parent test, and the parent selects it with
// the role flag.
func memoryContentionChild(t *testing.T) {
	role := *memoryContentionRole
	if role == "" {
		return
	}
	released := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(released)
	}()
	if script := os.Getenv(memoryContentionScriptEnv); script != "" {
		original := rand.Reader
		rand.Reader = &scriptedNonceReader{script: parseNonceScript(t, script), released: released}
		t.Cleanup(func() { rand.Reader = original })
	}

	dir := os.Getenv(memoryContentionDirEnv)
	root := os.Getenv(memoryContentionRootEnv)
	title := os.Getenv(memoryContentionTitleEnv)
	content := os.Getenv(memoryContentionContentEnv)
	mode := os.Getenv(memoryContentionModeEnv)
	target := 1
	if raw := os.Getenv(memoryContentionTargetEnv); raw != "" {
		if _, err := fmt.Sscanf(raw, "%d", &target); err != nil {
			fmt.Fprintf(os.Stderr, "bad sync target %q: %v\n", raw, err)
			os.Exit(2)
		}
	}

	injected := errors.New("injected memory temp sync failure")
	parked := make(chan struct{})
	calls := 0
	atomicfs.SyncFileFunc = func(f *os.File) error {
		calls++
		fmt.Println(writeEntryMarker)
		if calls != target {
			return f.Sync()
		}
		switch mode {
		case modeFail:
			return injected
		case modePark:
			<-parked // released only by this process being killed
		case modeHold:
			<-released
		}
		return f.Sync()
	}

	var result string
	var err error
	switch role {
	case roleWriteMD:
		var path string
		path, err = WriteMemoryFile(dir, title, content)
		result = path
	case roleSave:
		var path string
		path, err = NewStoreWithEmbedder(&fakeMemoryEmbedder{}, root, t.TempDir()).SaveMemory(dir, title, content)
		result = path
	case roleReconcile:
		err = NewStoreWithEmbedder(&fakeMemoryEmbedder{}, root, t.TempDir()).Reconcile()
		result = "ok"
	case roleCreate:
		var created bool
		created, err = atomicfs.CreateExclusive(os.Getenv(memoryContentionPathEnv), []byte(os.Getenv(memoryContentionDataEnv)), 0o644)
		result = "exists"
		if created {
			result = "created"
		}
	default:
		fmt.Fprintf(os.Stderr, "unknown memory contention role %q\n", role)
		os.Exit(2)
	}

	if mode == modePark {
		fmt.Fprintf(os.Stderr, "%s returned (%q, %v); the parked temp sync must never complete\n", role, result, err)
		os.Exit(4)
	}
	if err != nil {
		if !errors.Is(err, injected) && mode == modeFail {
			fmt.Fprintf(os.Stderr, "%s error = %v, want the injected temp sync failure\n", role, err)
			os.Exit(3)
		}
		result = "error:" + err.Error()
	}
	fmt.Println(resultPrefix + result)
	fmt.Println(doneMarker)
	os.Exit(0)
}

// contentionChild is one self-exec child process of a two-process row. It
// owns a single reap path: the deadline kills a child that hangs, the marker
// scanner is joined at pipe EOF before the wait, and stderr is read only
// after the child can no longer write to it.
type contentionChild struct {
	name    string
	cmd     *exec.Cmd
	cancel  context.CancelFunc
	stdin   io.WriteCloser
	markers chan string
	scanEnd chan struct{}
	stderr  *bytes.Buffer
	seen    []string
	once    sync.Once
	waitErr error
}

// startContentionChild self-execs testName with the given role and
// environment. The marker channel is far larger than the markers a child
// emits, so the scanner never blocks on a parent that has not drained it yet.
func startContentionChild(t *testing.T, name, testName, role string, env ...string) *contentionChild {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), contentionBound)
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^"+testName+"$",
		"-lightcode.memory-contention-child="+role,
	)
	cmd.Env = append(os.Environ(), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatalf("%s stdin pipe: %v", name, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatalf("%s stdout pipe: %v", name, err)
	}
	c := &contentionChild{
		name:    name,
		cmd:     cmd,
		cancel:  cancel,
		stdin:   stdin,
		markers: make(chan string, 256),
		scanEnd: make(chan struct{}),
		stderr:  &bytes.Buffer{},
	}
	cmd.Stderr = c.stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start %s: %v", name, err)
	}
	go func() {
		defer close(c.scanEnd)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			c.markers <- scanner.Text()
		}
		close(c.markers)
	}()
	t.Cleanup(func() { c.cancel(); _ = c.reap() })
	return c
}

// reap joins the marker scanner at pipe EOF and then waits for the process,
// exactly once, so every failure path can reap without racing the scanner.
func (c *contentionChild) reap() error {
	c.once.Do(func() {
		<-c.scanEnd
		for line := range c.markers {
			c.seen = append(c.seen, line)
		}
		c.waitErr = c.cmd.Wait()
	})
	return c.waitErr
}

// diagnose kills and reaps the child and only then returns its stderr: the
// buffer is never read while the child can still write to it.
func (c *contentionChild) diagnose() string {
	c.cancel()
	_ = c.reap()
	return c.stderr.String()
}

// await blocks until the child announces a line with the given prefix,
// returning the remainder, and fails the test when the child exits or the
// bound elapses first.
func (c *contentionChild) await(t *testing.T, prefix string) string {
	t.Helper()
	deadline := time.After(contentionBound)
	for {
		select {
		case line, ok := <-c.markers:
			if !ok {
				t.Fatalf("%s exited before announcing %q; markers=%v\n%s", c.name, prefix, c.seen, c.diagnose())
			}
			c.seen = append(c.seen, line)
			if strings.HasPrefix(line, prefix) {
				return strings.TrimPrefix(line, prefix)
			}
		case <-deadline:
			t.Fatalf("%s did not announce %q within %v; markers=%v\n%s", c.name, prefix, contentionBound, c.seen, c.diagnose())
		}
	}
}

// release closes the child's stdin, letting a held temp sync proceed.
func (c *contentionChild) release() { _ = c.stdin.Close() }

// finish releases the child and requires a clean exit.
func (c *contentionChild) finish(t *testing.T) {
	t.Helper()
	c.release()
	if err := c.reap(); err != nil {
		t.Fatalf("%s exited with %v\n%s", c.name, err, c.stderr.String())
	}
}

// kill terminates a parked child, standing in for a process that dies
// between a complete temp and its publication. It proves process and reader
// behavior at that point; it is not a power-loss simulation.
func (c *contentionChild) kill(t *testing.T) {
	t.Helper()
	c.cancel()
	_ = c.reap()
}

// count returns how many times the child announced marker.
func (c *contentionChild) count(marker string) int {
	n := 0
	for _, line := range c.seen {
		if line == marker {
			n++
		}
	}
	return n
}

// runOK awaits the child's result and requires a clean exit.
func (c *contentionChild) runOK(t *testing.T) string {
	t.Helper()
	result := c.await(t, resultPrefix)
	c.finish(t)
	return result
}

type memoryObservations struct {
	reads      int
	violations []string
}

type memoryReaderLoop struct {
	stop         chan struct{}
	checkpointCh chan chan struct{}
	out          chan memoryObservations
}

// startMemoryReaders runs check in a loop for the whole life of a row,
// taking no lock, so it observes exactly what a concurrent reader process
// would. check returns the violations of one pass.
func startMemoryReaders(check func() []string) *memoryReaderLoop {
	l := &memoryReaderLoop{
		stop:         make(chan struct{}),
		checkpointCh: make(chan chan struct{}),
		out:          make(chan memoryObservations, 1),
	}
	go func() {
		var obs memoryObservations
		deadline := time.Now().Add(contentionBound)
		for {
			select {
			case <-l.stop:
				l.out <- obs
				return
			case ack := <-l.checkpointCh:
				obs.reads++
				obs.violations = append(obs.violations, check()...)
				close(ack)
				continue
			default:
			}
			if time.Now().After(deadline) {
				obs.violations = append(obs.violations, "reader loop passed its deadline")
				l.out <- obs
				return
			}
			obs.reads++
			obs.violations = append(obs.violations, check()...)
			time.Sleep(time.Millisecond)
		}
	}()
	return l
}

// acknowledge waits until the reader loop has completed one check after the
// caller-established state. This prevents a row from passing only on reads
// that happened before its failure or crash transition.
func (l *memoryReaderLoop) acknowledge(t *testing.T) {
	t.Helper()
	ack := make(chan struct{})
	timer := time.NewTimer(contentionBound)
	defer timer.Stop()
	select {
	case l.checkpointCh <- ack:
	case <-timer.C:
		t.Fatal("reader checkpoint was not acknowledged")
	}
	select {
	case <-ack:
	case <-timer.C:
		t.Fatal("reader checkpoint did not complete")
	}
}

func (l *memoryReaderLoop) finish(t *testing.T) memoryObservations {
	t.Helper()
	close(l.stop)
	obs := <-l.out
	if len(obs.violations) > 0 {
		t.Fatalf("lock-free readers observed disallowed memory states: %s", strings.Join(obs.violations, "; "))
	}
	if obs.reads == 0 {
		t.Fatal("the lock-free reader loop performed no read")
	}
	return obs
}

// publishedMarkdownCheck reads every published .md name in dir and rejects
// anything that is not a complete frontmatter-plus-body record. Producer temp
// orphans do not carry the .md suffix, so a reader never surfaces one.
func publishedMarkdownCheck(dir, wantTitle string) func() []string {
	return func() []string {
		var violations []string
		matches, err := filepath.Glob(filepath.Join(dir, "*.md"))
		if err != nil {
			return []string{"glob: " + err.Error()}
		}
		for _, path := range matches {
			title, content, createdAt, err := ReadMemoryFile(path)
			switch {
			case err != nil:
				violations = append(violations, filepath.Base(path)+": "+err.Error())
			case title != wantTitle:
				violations = append(violations, fmt.Sprintf("%s: title = %q, want %q", filepath.Base(path), title, wantTitle))
			case strings.TrimSpace(content) == "":
				violations = append(violations, filepath.Base(path)+": empty body")
			case createdAt == "":
				violations = append(violations, filepath.Base(path)+": empty created_at")
			}
		}
		return violations
	}
}

// pairReaderCheck is the lock-free reader for an absent-vector row. Every
// published .md must be a complete record; its .vec may be absent — the pair
// is not committed yet — but when present it must be a complete vector.
func pairReaderCheck(dir string, dims int) func() []string {
	return func() []string {
		var violations []string
		matches, err := filepath.Glob(filepath.Join(dir, "*.md"))
		if err != nil {
			return []string{"glob: " + err.Error()}
		}
		for _, mdPath := range matches {
			title, content, createdAt, err := ReadMemoryFile(mdPath)
			if err != nil || title == "" || strings.TrimSpace(content) == "" || createdAt == "" {
				violations = append(violations, fmt.Sprintf("%s = %q/%q/%q, %v; want a complete record", filepath.Base(mdPath), title, content, createdAt, err))
				continue
			}
			vec, err := ReadVec(strings.TrimSuffix(mdPath, ".md") + ".vec")
			switch {
			case err != nil && !os.IsNotExist(err):
				violations = append(violations, filepath.Base(mdPath)+" vector: "+err.Error())
			case err == nil && len(vec) != dims:
				violations = append(violations, fmt.Sprintf("%s vector = %v, want a complete %d-dimension value", filepath.Base(mdPath), vec, dims))
			}
		}
		return violations
	}
}

// exactBytesCheck accepts an absent target or one that holds exactly one of
// the candidate payloads, so a partially visible publication is a violation.
func exactBytesCheck(path string, allowed ...string) func() []string {
	return func() []string {
		got, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return []string{"read: " + err.Error()}
		}
		for _, want := range allowed {
			if string(got) == want {
				return nil
			}
		}
		return []string{fmt.Sprintf("published bytes = %q, want one of %v", got, allowed)}
	}
}

func sameVector(got, want []float32) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// vectorReaderCheck is the lock-free reader for a present-vector row. ReadVec
// accepts an absent vector — the pair is not committed yet — or a value that
// is byte-for-byte one of the allowed complete ones; a short or torn read is
// a violation. Search must either skip the record or return it whole.
func vectorReaderCheck(store *Store, projectID, vecPath, title string, allowed ...[]float32) func() []string {
	return func() []string {
		var violations []string
		got, err := ReadVec(vecPath)
		switch {
		case err != nil && !os.IsNotExist(err):
			violations = append(violations, "ReadVec: "+err.Error())
		case err == nil:
			complete := false
			for _, want := range allowed {
				if sameVector(got, want) {
					complete = true
					break
				}
			}
			if !complete {
				violations = append(violations, fmt.Sprintf("ReadVec = %v, want one of the complete vectors %v", got, allowed))
			}
		}
		results, err := store.SearchMemory(title, projectID, false, 5)
		if err != nil {
			return append(violations, "SearchMemory: "+err.Error())
		}
		for _, r := range results {
			if r.Title != title || strings.TrimSpace(r.Content) == "" {
				violations = append(violations, fmt.Sprintf("SearchMemory returned %q/%q, want the whole record", r.Title, r.Content))
			}
		}
		return violations
	}
}

func envDir(dir string) string     { return memoryContentionDirEnv + "=" + dir }
func envRoot(root string) string   { return memoryContentionRootEnv + "=" + root }
func envTitle(t string) string     { return memoryContentionTitleEnv + "=" + t }
func envContent(c string) string   { return memoryContentionContentEnv + "=" + c }
func envMode(m string) string      { return memoryContentionModeEnv + "=" + m }
func envTarget(n int) string       { return fmt.Sprintf("%s=%d", memoryContentionTargetEnv, n) }
func envScript(s ...string) string { return memoryContentionScriptEnv + "=" + strings.Join(s, ",") }
func envPath(p string) string      { return memoryContentionPathEnv + "=" + p }
func envData(d string) string      { return memoryContentionDataEnv + "=" + d }

// nonceWindowSeconds covers the bounded child lifetime plus a one-second
// margin.
const nonceWindowSeconds = int(contentionBound/time.Second) + 2

// preplantNonces writes a complete memory file for every scripted nonce over
// a window of UTC seconds around now, using the production timestamp and slug
// format, so the child's first candidate names already exist.
func preplantNonces(t *testing.T, dir, title string, nonces []string) map[string][]byte {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	slug := slugify(title)
	if slug == "" {
		slug = "untitled"
	}
	if len(slug) > 40 {
		slug = slug[:40]
	}
	planted := make(map[string][]byte)
	start := time.Now().UTC().Add(-(contentionBound + time.Second))
	for i := 0; i < nonceWindowSeconds; i++ {
		second := start.Add(time.Duration(i) * time.Second)
		ts := second.Format("20060102-150405")
		for _, nonce := range nonces {
			path := filepath.Join(dir, fmt.Sprintf("%s-%s-%s.md", ts, slug, nonce))
			body := []byte(fmt.Sprintf("---\ntitle: %s\ncreated_at: %s\n---\n\npreplanted %s\n",
				yamlQuote(title), second.Format(time.RFC3339), nonce))
			if err := os.WriteFile(path, body, 0o644); err != nil {
				t.Fatal(err)
			}
			planted[path] = body
		}
	}
	return planted
}

// requirePreplantedIntact proves CreateExclusive never replaced a name that
// already existed: every preplanted file still holds its own bytes.
func requirePreplantedIntact(t *testing.T, planted map[string][]byte) {
	t.Helper()
	for path, want := range planted {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("preplanted %s: %v", filepath.Base(path), err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("preplanted %s was replaced: %q", filepath.Base(path), got)
		}
	}
}

func nonceOf(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".md")
	idx := strings.LastIndex(base, "-")
	if idx < 0 {
		return ""
	}
	return base[idx+1:]
}

func stampOf(path string) string {
	base := filepath.Base(path)
	if len(base) < len("20060102-150405") {
		return ""
	}
	return base[:len("20060102-150405")]
}

func memoryTemps(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "*.tmp-*"))
	if err != nil {
		t.Fatalf("glob temps: %v", err)
	}
	return matches
}

// requireCompleteMarkdown parses the orphan through the production reader and
// then requires the exact production bytes for the expected record. This keeps
// a malformed frontmatter file from passing as a plain-text fallback.
func requireCompleteMarkdown(t *testing.T, path, wantTitle, wantBody string) {
	t.Helper()
	title, body, createdAt, err := ReadMemoryFile(path)
	if err != nil {
		t.Fatalf("read markdown orphan: %v", err)
	}
	if title != wantTitle {
		t.Fatalf("markdown orphan title = %q, want %q", title, wantTitle)
	}
	if body != wantBody+"\n" {
		t.Fatalf("markdown orphan body = %q, want %q", body, wantBody+"\n")
	}
	if createdAt == "" {
		t.Fatal("markdown orphan created_at is empty")
	}
	if _, err := time.Parse(time.RFC3339, createdAt); err != nil {
		t.Fatalf("markdown orphan created_at = %q: %v", createdAt, err)
	}
	want := fmt.Sprintf("---\ntitle: %s\ncreated_at: %s\n---\n\n%s\n", yamlQuote(wantTitle), createdAt, wantBody)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read markdown orphan bytes: %v", err)
	}
	if string(data) != want {
		t.Fatalf("markdown orphan bytes = %q, want %q", data, want)
	}
}

// requireCompleteVectorTemp requires the one crash orphan to be the complete
// vector temp belonging to the producer's published markdown, not a stale or
// unrelated temp from another record.
func requireCompleteVectorTemp(t *testing.T, dir, wantPath string, want []float32) {
	t.Helper()
	temps := memoryTemps(t, dir)
	if len(temps) != 1 {
		t.Fatalf("vector temps = %v, want exactly the killed producer's orphan", temps)
	}
	if !strings.HasPrefix(temps[0], wantPath+".tmp-") {
		t.Fatalf("vector temp = %q, want an orphan for %q", temps[0], wantPath)
	}
	got, err := ReadVec(temps[0])
	if err != nil {
		t.Fatalf("read vector orphan: %v", err)
	}
	if !sameVector(got, want) {
		t.Fatalf("vector orphan = %v, want complete %v", got, want)
	}
}

// requireNoProducerTemps proves every producer temp has been removed, while
// also checking the published paths directly so a successful writer cannot
// leave residue hidden behind a broad directory count.
func requireNoProducerTemps(t *testing.T, dir string, published ...string) {
	t.Helper()
	for _, path := range published {
		matches, err := filepath.Glob(path + ".tmp-*")
		if err != nil {
			t.Fatalf("glob producer temps for %s: %v", filepath.Base(path), err)
		}
		if len(matches) != 0 {
			t.Fatalf("producer temps for %s = %v, want none", filepath.Base(path), matches)
		}
	}
	if temps := memoryTemps(t, dir); len(temps) != 0 {
		t.Fatalf("producer temps = %v, want none", temps)
	}
}

func countSuffix(t *testing.T, dir, suffix string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			n++
		}
	}
	return n
}

// TestWriteMemoryFileRetriesPastACollidingName drives the production retry
// loop with a scripted nonce cycle: the first candidate name already exists,
// so the writer must publish under its second candidate instead of replacing
// the existing record. Both attempts reach a real temp sync, so the retry is
// observed rather than inferred.
func TestWriteMemoryFileRetriesPastACollidingName(t *testing.T) {
	memoryContentionChild(t)

	const title = "Retry Row"
	const first = "aa000001"
	const second = "bb000002"

	dir := filepath.Join(t.TempDir(), "memories")
	planted := preplantNonces(t, dir, title, []string{first})
	readers := startMemoryReaders(publishedMarkdownCheck(dir, title))
	child := startContentionChild(t, "writer", "TestWriteMemoryFileRetriesPastACollidingName", roleWriteMD,
		envDir(dir), envTitle(title), envContent("retry body"), envMode(modeComplete),
		envScript(first, second))
	child.await(t, candidateReadyMarker)
	child.release()
	result := child.runOK(t)
	readers.acknowledge(t)
	readers.finish(t)
	if strings.HasPrefix(result, "error:") {
		t.Fatalf("WriteMemoryFile = %s", result)
	}
	if nonceOf(result) != second {
		t.Fatalf("published nonce = %q, want the scripted retry %q", nonceOf(result), second)
	}
	if got := child.count(writeEntryMarker); got != 2 {
		t.Fatalf("temp syncs = %d, want one per attempt across the collision and the retry", got)
	}
	title2, content, createdAt, err := ReadMemoryFile(result)
	if err != nil || title2 != title || strings.TrimSpace(content) != "retry body" || createdAt == "" {
		t.Fatalf("published record = %q/%q/%q, %v", title2, content, createdAt, err)
	}
	if got := countSuffix(t, dir, ".md"); got != len(planted)+1 {
		t.Fatalf(".md files = %d, want the fixtures plus one published record", got)
	}
	requirePreplantedIntact(t, planted)
	requireNoProducerTemps(t, dir, result)
}

// TestWriteMemoryFileExhaustsItsRetryBudget preplants every name the writer's
// eight scripted attempts can produce, so all eight collide and the writer
// reports that it could not allocate a unique file. Nothing is published and
// no temp is left behind.
func TestWriteMemoryFileExhaustsItsRetryBudget(t *testing.T) {
	memoryContentionChild(t)

	const title = "Exhaustion Row"
	nonces := []string{"a1000001", "a2000002", "a3000003", "a4000004", "a5000005", "a6000006", "a7000007", "a8000008"}

	dir := filepath.Join(t.TempDir(), "memories")
	planted := preplantNonces(t, dir, title, nonces)
	readers := startMemoryReaders(publishedMarkdownCheck(dir, title))
	child := startContentionChild(t, "writer", "TestWriteMemoryFileExhaustsItsRetryBudget", roleWriteMD,
		envDir(dir), envTitle(title), envContent("exhaustion body"), envMode(modeComplete),
		envScript(nonces...))
	child.await(t, candidateReadyMarker)
	child.release()
	result := child.runOK(t)
	readers.acknowledge(t)
	readers.finish(t)
	if !strings.Contains(result, "could not allocate a unique file") {
		t.Fatalf("WriteMemoryFile = %s, want the exhausted-retry report", result)
	}
	if got := child.count(writeEntryMarker); got != len(nonces) {
		t.Fatalf("temp syncs = %d, want one per scripted attempt (%d)", got, len(nonces))
	}
	if got := countSuffix(t, dir, ".md"); got != len(planted) {
		t.Fatalf(".md files = %d, want the %d preplanted records and nothing new", got, len(planted))
	}
	if temps := memoryTemps(t, dir); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after the exhausted retry", temps)
	}
	requirePreplantedIntact(t, planted)
}

// TestTwoProcessMemoryWritersPublishDistinctRetryNames runs two real
// WriteMemoryFile processes whose scripted first candidate is already taken.
// Both must retry and both must publish their own complete record. When both
// land in the same UTC second their second candidates are the same name, so
// exclusive creation arbitrates and the loser retries once more; when they
// land in different seconds the same second candidate is free for both. The
// row asserts whichever of the two the run actually produced.
func TestTwoProcessMemoryWritersPublishDistinctRetryNames(t *testing.T) {
	memoryContentionChild(t)

	const title = "Two Writer Row"
	const first = "c1000001"
	const second = "c2000002"
	const third = "c3000003"

	dir := filepath.Join(t.TempDir(), "memories")
	planted := preplantNonces(t, dir, title, []string{first})
	readers := startMemoryReaders(publishedMarkdownCheck(dir, title))
	env := []string{envDir(dir), envTitle(title), envContent("two writer body"), envMode(modeComplete), envScript(first, second, third)}
	a := startContentionChild(t, "writer-a", "TestTwoProcessMemoryWritersPublishDistinctRetryNames", roleWriteMD, env...)
	b := startContentionChild(t, "writer-b", "TestTwoProcessMemoryWritersPublishDistinctRetryNames", roleWriteMD, env...)
	a.await(t, candidateReadyMarker)
	b.await(t, candidateReadyMarker)
	a.release()
	b.release()
	pathA := a.await(t, resultPrefix)
	pathB := b.await(t, resultPrefix)
	a.finish(t)
	b.finish(t)
	readers.acknowledge(t)
	readers.finish(t)

	if strings.HasPrefix(pathA, "error:") || strings.HasPrefix(pathB, "error:") {
		t.Fatalf("writers = %s / %s, want two published records", pathA, pathB)
	}
	if nonceOf(pathA) == first || nonceOf(pathB) == first {
		t.Fatalf("published first candidate: %q / %q", pathA, pathB)
	}
	if pathA == pathB {
		t.Fatalf("both writers published %s; exclusive creation must give each its own name", pathA)
	}
	for _, writer := range []*contentionChild{a, b} {
		if got := writer.count(writeEntryMarker); got < 2 {
			t.Fatalf("%s temp syncs = %d, want at least the collision and the retry", writer.name, got)
		}
	}
	if stampOf(pathA) == stampOf(pathB) {
		got := []string{nonceOf(pathA), nonceOf(pathB)}
		if !(got[0] == second && got[1] == third) && !(got[0] == third && got[1] == second) {
			t.Fatalf("same-second nonces = %v, want the arbitrated %q and %q", got, second, third)
		}
	} else {
		if nonceOf(pathA) != second || nonceOf(pathB) != second {
			t.Fatalf("distinct-second nonces = %q/%q, want both at the free second candidate %q", nonceOf(pathA), nonceOf(pathB), second)
		}
	}
	for _, path := range []string{pathA, pathB} {
		gotTitle, content, createdAt, err := ReadMemoryFile(path)
		if err != nil || gotTitle != title || strings.TrimSpace(content) != "two writer body" || createdAt == "" {
			t.Fatalf("published %s = %q/%q/%q, %v", filepath.Base(path), gotTitle, content, createdAt, err)
		}
	}
	if got := countSuffix(t, dir, ".md"); got != len(planted)+2 {
		t.Fatalf(".md files = %d, want the fixtures plus two published records", got)
	}
	requirePreplantedIntact(t, planted)
	requireNoProducerTemps(t, dir, pathA, pathB)
}

// TestExclusiveCreateArbitratesOneWinnerAtAnExactPath removes the timestamp
// from the race: two processes create the same explicit path at once, so
// exactly one gets (true, nil) and the other (false, nil), neither errors,
// and the published bytes are the winner's.
func TestExclusiveCreateArbitratesOneWinnerAtAnExactPath(t *testing.T) {
	memoryContentionChild(t)

	dir := filepath.Join(t.TempDir(), "memories")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "exact.md")
	readers := startMemoryReaders(exactBytesCheck(path, "from-a", "from-b"))
	a := startContentionChild(t, "create-a", "TestExclusiveCreateArbitratesOneWinnerAtAnExactPath", roleCreate,
		envPath(path), envData("from-a"), envMode(modeComplete))
	b := startContentionChild(t, "create-b", "TestExclusiveCreateArbitratesOneWinnerAtAnExactPath", roleCreate,
		envPath(path), envData("from-b"), envMode(modeComplete))
	resultA := a.await(t, resultPrefix)
	resultB := b.await(t, resultPrefix)
	a.finish(t)
	b.finish(t)
	readers.finish(t)

	created := map[string]string{"create-a": resultA, "create-b": resultB}
	winners := 0
	losers := 0
	for name, result := range created {
		switch result {
		case "created":
			winners++
		case "exists":
			losers++
		default:
			t.Fatalf("%s = %s, want created or exists with no error", name, result)
		}
	}
	if winners != 1 || losers != 1 {
		t.Fatalf("exclusive create produced %d winners and %d losers, want exactly one of each", winners, losers)
	}
	for _, creator := range []*contentionChild{a, b} {
		if got := creator.count(writeEntryMarker); got != 1 {
			t.Fatalf("%s temp syncs = %d, want the one real create attempt", creator.name, got)
		}
	}
	want := "from-a"
	if resultA != "created" {
		want = "from-b"
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read published path: %v", err)
	}
	if string(got) != want {
		t.Fatalf("published bytes = %q, want the winner's %q", got, want)
	}
	if temps := memoryTemps(t, dir); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after both processes returned", temps)
	}
}

// TestMemoryMarkdownHandledSyncFailurePublishesNothing fails the writer at
// its pre-publication temp sync: nothing is published, no temp is left, and a
// second process publishes its own complete record.
func TestMemoryMarkdownHandledSyncFailurePublishesNothing(t *testing.T) {
	memoryContentionChild(t)

	const title = "Markdown Failure Row"
	dir := filepath.Join(t.TempDir(), "memories")
	readers := startMemoryReaders(publishedMarkdownCheck(dir, title))

	failing := startContentionChild(t, "writer-fail", "TestMemoryMarkdownHandledSyncFailurePublishesNothing", roleWriteMD,
		envDir(dir), envTitle(title), envContent("failure body"), envMode(modeFail), envTarget(1))
	result := failing.runOK(t)
	if !strings.HasPrefix(result, "error:") {
		t.Fatalf("WriteMemoryFile = %s, want the handled temp sync failure", result)
	}
	// An absent target alone is not evidence of a handled failure; the writer
	// must be shown to have reached its real temp sync.
	if got := failing.count(writeEntryMarker); got != 1 {
		t.Fatalf("temp syncs = %d, want the one real write the failure aborted", got)
	}
	if got := countSuffix(t, dir, ".md"); got != 0 {
		t.Fatalf(".md files = %d, want none after the handled failure", got)
	}
	if temps := memoryTemps(t, dir); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after the handled failure", temps)
	}

	second := startContentionChild(t, "writer-ok", "TestMemoryMarkdownHandledSyncFailurePublishesNothing", roleWriteMD,
		envDir(dir), envTitle(title), envContent("published body"), envMode(modeComplete))
	published := second.runOK(t)
	readers.finish(t)

	if strings.HasPrefix(published, "error:") {
		t.Fatalf("second WriteMemoryFile = %s", published)
	}
	if got := second.count(writeEntryMarker); got != 1 {
		t.Fatalf("second process temp syncs = %d, want its one real write", got)
	}
	gotTitle, content, _, err := ReadMemoryFile(published)
	if err != nil || gotTitle != title || strings.TrimSpace(content) != "published body" {
		t.Fatalf("published record = %q/%q, %v", gotTitle, content, err)
	}
	if temps := memoryTemps(t, dir); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after the second published", temps)
	}
}

// TestMemoryMarkdownKilledWriterLeavesOnlyAnIgnoredTemp kills the writer
// while it is parked inside its real temp sync: the temp holds the complete
// record but was never linked into place. Readers only ever see published
// .md names, so the orphan is invisible to them, and a second process
// publishes normally.
func TestMemoryMarkdownKilledWriterLeavesOnlyAnIgnoredTemp(t *testing.T) {
	memoryContentionChild(t)

	const title = "Markdown Crash Row"
	dir := filepath.Join(t.TempDir(), "memories")
	readers := startMemoryReaders(publishedMarkdownCheck(dir, title))

	parked := startContentionChild(t, "writer-parked", "TestMemoryMarkdownKilledWriterLeavesOnlyAnIgnoredTemp", roleWriteMD,
		envDir(dir), envTitle(title), envContent("parked body"), envMode(modePark), envTarget(1))
	parked.await(t, writeEntryMarker)
	parked.kill(t)

	if got := countSuffix(t, dir, ".md"); got != 0 {
		t.Fatalf(".md files = %d, want none after the killed writer", got)
	}
	temps := memoryTemps(t, dir)
	if len(temps) != 1 {
		t.Fatalf("temps = %v, want exactly the killed writer's orphan", temps)
	}
	requireCompleteMarkdown(t, temps[0], title, "parked body")

	second := startContentionChild(t, "writer-ok", "TestMemoryMarkdownKilledWriterLeavesOnlyAnIgnoredTemp", roleWriteMD,
		envDir(dir), envTitle(title), envContent("published body"), envMode(modeComplete))
	published := second.runOK(t)
	readers.finish(t)

	if strings.HasPrefix(published, "error:") {
		t.Fatalf("second WriteMemoryFile = %s", published)
	}
	if got := second.count(writeEntryMarker); got != 1 {
		t.Fatalf("second process temp syncs = %d, want its one real write", got)
	}
	if got := countSuffix(t, dir, ".md"); got != 1 {
		t.Fatalf(".md files = %d, want the one published record", got)
	}
	if got := memoryTemps(t, dir); len(got) != 1 {
		t.Fatalf("temps = %v, want only the killed writer's orphan", got)
	}
}

// memoriesDirFor builds the projects-root layout the store scans.
func memoriesDirFor(t *testing.T, root, projectID string) string {
	t.Helper()
	dir := filepath.Join(root, projectID, "memories")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, projectID, "meta.json"), []byte(`{"name":"Project"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestTwoProcessSaveMemoryPublishesTwoCompletePairs runs two real SaveMemory
// processes against an absent record set: each publishes its own complete
// .md plus the matching .vec, and no temp survives.
func TestTwoProcessSaveMemoryPublishesTwoCompletePairs(t *testing.T) {
	memoryContentionChild(t)

	const projectID = "p-vec-absent"
	root := t.TempDir()
	dir := memoriesDirFor(t, root, projectID)

	readers := startMemoryReaders(pairReaderCheck(dir, 3))
	a := startContentionChild(t, "save-a", "TestTwoProcessSaveMemoryPublishesTwoCompletePairs", roleSave,
		envDir(dir), envRoot(root), envTitle("Pair A"), envContent("content a"), envMode(modeComplete))
	b := startContentionChild(t, "save-b", "TestTwoProcessSaveMemoryPublishesTwoCompletePairs", roleSave,
		envDir(dir), envRoot(root), envTitle("Pair B"), envContent("content b"), envMode(modeComplete))
	pathA := a.await(t, resultPrefix)
	pathB := b.await(t, resultPrefix)
	a.finish(t)
	b.finish(t)
	readers.finish(t)

	for _, saver := range []*contentionChild{a, b} {
		if got := saver.count(writeEntryMarker); got != 2 {
			t.Fatalf("%s temp syncs = %d, want its own markdown then its own vector", saver.name, got)
		}
	}
	for _, path := range []string{pathA, pathB} {
		if strings.HasPrefix(path, "error:") {
			t.Fatalf("SaveMemory = %s", path)
		}
		if _, _, _, err := ReadMemoryFile(path); err != nil {
			t.Fatalf("published %s: %v", filepath.Base(path), err)
		}
		vec, err := ReadVec(strings.TrimSuffix(path, ".md") + ".vec")
		if err != nil || len(vec) != 3 {
			t.Fatalf("vector for %s = %v, %v; want the complete pair", filepath.Base(path), vec, err)
		}
	}
	if got := countSuffix(t, dir, ".md"); got != 2 {
		t.Fatalf(".md files = %d, want two", got)
	}
	if got := countSuffix(t, dir, ".vec"); got != 2 {
		t.Fatalf(".vec files = %d, want two", got)
	}
	if temps := memoryTemps(t, dir); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after two completed pairs", temps)
	}
}

// TestSaveMemoryVectorFailureRemovesOnlyItsOwnMarkdown fails the vector half
// of a save: the pair is not committed, so the save removes the markdown it
// had just published, leaves no temp, and touches no other record. A second
// process then completes its own pair.
func TestSaveMemoryVectorFailureRemovesOnlyItsOwnMarkdown(t *testing.T) {
	memoryContentionChild(t)

	const projectID = "p-vec-absent-failure"
	root := t.TempDir()
	dir := memoriesDirFor(t, root, projectID)

	readers := startMemoryReaders(pairReaderCheck(dir, 3))
	seed := startContentionChild(t, "save-seed", "TestSaveMemoryVectorFailureRemovesOnlyItsOwnMarkdown", roleSave,
		envDir(dir), envRoot(root), envTitle("Seed Pair"), envContent("seed content"), envMode(modeComplete))
	seedPath := seed.runOK(t)
	if strings.HasPrefix(seedPath, "error:") {
		t.Fatalf("seed SaveMemory = %s", seedPath)
	}
	seedVec := strings.TrimSuffix(seedPath, ".md") + ".vec"

	// The markdown publishes on the first temp sync and the vector on the
	// second, so failing the second is exactly a failed vector half.
	failing := startContentionChild(t, "save-fail", "TestSaveMemoryVectorFailureRemovesOnlyItsOwnMarkdown", roleSave,
		envDir(dir), envRoot(root), envTitle("Failing Pair"), envContent("failing content"), envMode(modeFail), envTarget(2))
	result := failing.runOK(t)
	if !strings.HasPrefix(result, "error:") {
		t.Fatalf("SaveMemory = %s, want the handled vector failure", result)
	}
	if got := failing.count(writeEntryMarker); got != 2 {
		t.Fatalf("temp syncs = %d, want the markdown then the vector", got)
	}
	if got := countSuffix(t, dir, ".md"); got != 1 {
		t.Fatalf(".md files = %d, want only the untouched seed pair's", got)
	}
	if got := countSuffix(t, dir, ".vec"); got != 1 {
		t.Fatalf(".vec files = %d, want only the untouched seed pair's", got)
	}
	if temps := memoryTemps(t, dir); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after the handled failure", temps)
	}
	if _, _, _, err := ReadMemoryFile(seedPath); err != nil {
		t.Fatalf("seed markdown after the failed save: %v", err)
	}
	if vec, err := ReadVec(seedVec); err != nil || len(vec) != 3 {
		t.Fatalf("seed vector after the failed save = %v, %v", vec, err)
	}

	second := startContentionChild(t, "save-ok", "TestSaveMemoryVectorFailureRemovesOnlyItsOwnMarkdown", roleSave,
		envDir(dir), envRoot(root), envTitle("Second Pair"), envContent("second content"), envMode(modeComplete))
	secondPath := second.runOK(t)
	readers.finish(t)
	if strings.HasPrefix(secondPath, "error:") {
		t.Fatalf("second SaveMemory = %s", secondPath)
	}
	if got := second.count(writeEntryMarker); got != 2 {
		t.Fatalf("second process temp syncs = %d, want its markdown then its vector", got)
	}
	if got := countSuffix(t, dir, ".md"); got != 2 {
		t.Fatalf(".md files = %d, want the seed pair plus the second", got)
	}
	if got := countSuffix(t, dir, ".vec"); got != 2 {
		t.Fatalf(".vec files = %d, want the seed pair plus the second", got)
	}
}

// TestSaveMemoryKilledBeforeVectorLeavesAHealableMarkdown kills a save while
// it is parked inside the vector's temp sync: the markdown is already
// published but the pair is not committed. Search ignores a markdown with no
// vector, Reconcile heals it into a complete pair, and a second process
// completes its own pair.
func TestSaveMemoryKilledBeforeVectorLeavesAHealableMarkdown(t *testing.T) {
	memoryContentionChild(t)

	const projectID = "p-vec-absent-crash"
	root := t.TempDir()
	dir := memoriesDirFor(t, root, projectID)
	store := NewStoreWithEmbedder(&fakeMemoryEmbedder{}, root, t.TempDir())

	readers := startMemoryReaders(pairReaderCheck(dir, 3))
	parked := startContentionChild(t, "save-parked", "TestSaveMemoryKilledBeforeVectorLeavesAHealableMarkdown", roleSave,
		envDir(dir), envRoot(root), envTitle("Lone Markdown"), envContent("lone content"), envMode(modePark), envTarget(2))
	parked.await(t, writeEntryMarker)
	parked.await(t, writeEntryMarker)
	parked.kill(t)

	if got := countSuffix(t, dir, ".md"); got != 1 {
		t.Fatalf(".md files = %d, want the published markdown of the killed save", got)
	}
	if got := countSuffix(t, dir, ".vec"); got != 0 {
		t.Fatalf(".vec files = %d, want none: the pair was never committed", got)
	}
	mdPaths, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		t.Fatalf("glob markdown after crash: %v", err)
	}
	if len(mdPaths) != 1 {
		t.Fatalf("markdown after crash = %v, want the killed producer's one record", mdPaths)
	}
	requireCompleteMarkdown(t, mdPaths[0], "Lone Markdown", "lone content")
	healedVecPath := strings.TrimSuffix(mdPaths[0], ".md") + ".vec"
	requireCompleteVectorTemp(t, dir, healedVecPath, []float32{1, 0, 0})
	results, err := store.SearchMemory("lone content", projectID, false, 5)
	if err != nil {
		t.Fatalf("SearchMemory: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("SearchMemory returned %d results from a markdown with no vector", len(results))
	}

	if err := store.Reconcile(); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if got := countSuffix(t, dir, ".vec"); got != 1 {
		t.Fatalf(".vec files after Reconcile = %d, want the healed pair", got)
	}
	requireCompleteVectorTemp(t, dir, healedVecPath, []float32{1, 0, 0})
	healed, err := ReadVec(healedVecPath)
	if err != nil || !sameVector(healed, []float32{1, 0, 0}) {
		t.Fatalf("healed vector = %v, %v; want the complete deterministic vector", healed, err)
	}
	results, err = store.SearchMemory("lone content", projectID, false, 5)
	if err != nil {
		t.Fatalf("SearchMemory after Reconcile: %v", err)
	}
	if len(results) != 1 || results[0].Title != "Lone Markdown" {
		t.Fatalf("SearchMemory after Reconcile = %+v, want the healed record", results)
	}

	second := startContentionChild(t, "save-ok", "TestSaveMemoryKilledBeforeVectorLeavesAHealableMarkdown", roleSave,
		envDir(dir), envRoot(root), envTitle("Second Pair"), envContent("second content"), envMode(modeComplete))
	secondPath := second.runOK(t)
	readers.finish(t)
	if strings.HasPrefix(secondPath, "error:") {
		t.Fatalf("second SaveMemory = %s", secondPath)
	}
	if got := second.count(writeEntryMarker); got != 2 {
		t.Fatalf("second process temp syncs = %d, want its markdown then its vector", got)
	}
	if got := countSuffix(t, dir, ".md"); got != 2 {
		t.Fatalf(".md files = %d, want the healed record plus the second pair", got)
	}
	if got := countSuffix(t, dir, ".vec"); got != 2 {
		t.Fatalf(".vec files = %d, want the healed record plus the second pair", got)
	}
	requireCompleteVectorTemp(t, dir, healedVecPath, []float32{1, 0, 0})
}

// seedStaleVector publishes a markdown with a vector older than it, which is
// exactly the state Reconcile rewrites.
func seedStaleVector(t *testing.T, dir, title, content string, stale []float32) (string, string) {
	t.Helper()
	mdPath, err := WriteMemoryFile(dir, title, content)
	if err != nil {
		t.Fatalf("seed markdown: %v", err)
	}
	vecPath := strings.TrimSuffix(mdPath, ".md") + ".vec"
	if err := WriteVec(vecPath, stale); err != nil {
		t.Fatalf("seed vector: %v", err)
	}
	mdInfo, err := os.Stat(mdPath)
	if err != nil {
		t.Fatal(err)
	}
	old := mdInfo.ModTime().Add(-time.Hour)
	if err := os.Chtimes(vecPath, old, old); err != nil {
		t.Fatal(err)
	}
	return mdPath, vecPath
}

// TestTwoProcessReconcileBothEnterTheirRealVectorWrite proves the present
// vector row cannot pass through a skip branch. Reconcile rewrites only a
// vector older than its markdown, and it decides that before it writes, so
// both processes are held inside their own real temp sync before either is
// released. Both write markers are required; which write survives is a
// last-writer-wins outcome and is not asserted.
func TestTwoProcessReconcileBothEnterTheirRealVectorWrite(t *testing.T) {
	memoryContentionChild(t)

	const projectID = "p-vec-present"
	root := t.TempDir()
	dir := memoriesDirFor(t, root, projectID)
	stale := []float32{0, 1, 0}
	fresh := []float32{1, 0, 0}
	_, vecPath := seedStaleVector(t, dir, "Present Vector", "present content", stale)
	reader := NewStoreWithEmbedder(&fakeMemoryEmbedder{}, root, t.TempDir())

	readers := startMemoryReaders(vectorReaderCheck(reader, projectID, vecPath, "Present Vector", stale, fresh))
	env := []string{envDir(dir), envRoot(root), envMode(modeHold), envTarget(1)}
	a := startContentionChild(t, "reconcile-a", "TestTwoProcessReconcileBothEnterTheirRealVectorWrite", roleReconcile, env...)
	b := startContentionChild(t, "reconcile-b", "TestTwoProcessReconcileBothEnterTheirRealVectorWrite", roleReconcile, env...)
	a.await(t, writeEntryMarker)
	b.await(t, writeEntryMarker)
	// Both are inside their real vector write; only now is either released,
	// so neither could have skipped the rewrite by observing the other's.
	a.release()
	b.release()
	resultA := a.await(t, resultPrefix)
	resultB := b.await(t, resultPrefix)
	a.finish(t)
	b.finish(t)
	readers.finish(t)

	if resultA != "ok" || resultB != "ok" {
		t.Fatalf("Reconcile results = %s / %s, want both complete", resultA, resultB)
	}
	if got := a.count(writeEntryMarker); got != 1 {
		t.Fatalf("first process vector writes = %d, want exactly one real write", got)
	}
	if got := b.count(writeEntryMarker); got != 1 {
		t.Fatalf("second process vector writes = %d, want exactly one real write", got)
	}
	final, err := ReadVec(vecPath)
	if err != nil || len(final) != len(fresh) {
		t.Fatalf("final vector = %v, %v; want a complete replacement", final, err)
	}
	if temps := memoryTemps(t, dir); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after both writes completed", temps)
	}
}

// TestReconcileVectorSyncFailurePreservesTheOldVector fails Reconcile's
// vector write at its pre-publication temp sync. Reconcile reports success
// because a single unhealed record is not a Reconcile failure, the old
// vector is preserved untouched, no temp is left, and a second process's
// Reconcile completes the rewrite.
func TestReconcileVectorSyncFailurePreservesTheOldVector(t *testing.T) {
	memoryContentionChild(t)

	const projectID = "p-vec-present-failure"
	root := t.TempDir()
	dir := memoriesDirFor(t, root, projectID)
	stale := []float32{0, 1, 0}
	fresh := []float32{1, 0, 0}
	_, vecPath := seedStaleVector(t, dir, "Present Vector", "present content", stale)
	reader := NewStoreWithEmbedder(&fakeMemoryEmbedder{}, root, t.TempDir())

	readers := startMemoryReaders(vectorReaderCheck(reader, projectID, vecPath, "Present Vector", stale, fresh))
	failing := startContentionChild(t, "reconcile-fail", "TestReconcileVectorSyncFailurePreservesTheOldVector", roleReconcile,
		envDir(dir), envRoot(root), envMode(modeFail), envTarget(1))
	result := failing.runOK(t)
	if result != "ok" {
		t.Fatalf("Reconcile = %s, want success: a dropped vector rewrite is not a Reconcile failure", result)
	}
	if got := failing.count(writeEntryMarker); got != 1 {
		t.Fatalf("vector writes = %d, want the one attempted rewrite", got)
	}
	preserved, err := ReadVec(vecPath)
	if err != nil || !sameVector(preserved, stale) {
		t.Fatalf("vector after the handled failure = %v, %v; want the old one intact", preserved, err)
	}
	if temps := memoryTemps(t, dir); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after the handled failure", temps)
	}
	readers.acknowledge(t)

	second := startContentionChild(t, "reconcile-ok", "TestReconcileVectorSyncFailurePreservesTheOldVector", roleReconcile,
		envDir(dir), envRoot(root), envMode(modeComplete), envTarget(1))
	if got := second.runOK(t); got != "ok" {
		t.Fatalf("second Reconcile = %s", got)
	}
	readers.finish(t)

	if got := second.count(writeEntryMarker); got != 1 {
		t.Fatalf("second process vector writes = %d, want exactly one real write", got)
	}
	final, err := ReadVec(vecPath)
	if err != nil || !sameVector(final, fresh) {
		t.Fatalf("final vector = %v, %v; want the second process's rewrite", final, err)
	}
	if temps := memoryTemps(t, dir); len(temps) != 0 {
		t.Fatalf("temps = %v, want none after the second rewrite", temps)
	}
}

// TestReconcileKilledWriterKeepsTheOldVectorReadable kills Reconcile while it
// is parked inside its real vector write: the old vector stays readable
// throughout, the only residue is that one complete temp, and a second
// process completes the rewrite.
func TestReconcileKilledWriterKeepsTheOldVectorReadable(t *testing.T) {
	memoryContentionChild(t)

	const projectID = "p-vec-present-crash"
	root := t.TempDir()
	dir := memoriesDirFor(t, root, projectID)
	stale := []float32{0, 1, 0}
	fresh := []float32{1, 0, 0}
	_, vecPath := seedStaleVector(t, dir, "Present Vector", "present content", stale)
	reader := NewStoreWithEmbedder(&fakeMemoryEmbedder{}, root, t.TempDir())

	readers := startMemoryReaders(vectorReaderCheck(reader, projectID, vecPath, "Present Vector", stale, fresh))
	parked := startContentionChild(t, "reconcile-parked", "TestReconcileKilledWriterKeepsTheOldVectorReadable", roleReconcile,
		envDir(dir), envRoot(root), envMode(modePark), envTarget(1))
	parked.await(t, writeEntryMarker)
	held, err := ReadVec(vecPath)
	if err != nil || !sameVector(held, stale) {
		t.Fatalf("vector while the writer is parked = %v, %v; want the old one", held, err)
	}
	parked.kill(t)

	temps := memoryTemps(t, dir)
	if len(temps) != 1 {
		t.Fatalf("temps = %v, want exactly the killed writer's orphan", temps)
	}
	orphan, err := ReadVec(temps[0])
	if err != nil || !sameVector(orphan, fresh) {
		t.Fatalf("orphan vector = %v, %v; want the complete value its producer was about to publish", orphan, err)
	}
	readers.acknowledge(t)

	second := startContentionChild(t, "reconcile-ok", "TestReconcileKilledWriterKeepsTheOldVectorReadable", roleReconcile,
		envDir(dir), envRoot(root), envMode(modeComplete), envTarget(1))
	if got := second.runOK(t); got != "ok" {
		t.Fatalf("second Reconcile = %s", got)
	}
	readers.finish(t)

	final, err := ReadVec(vecPath)
	if err != nil || !sameVector(final, fresh) {
		t.Fatalf("final vector = %v, %v; want the second process's rewrite", final, err)
	}
	if got := memoryTemps(t, dir); len(got) != 1 {
		t.Fatalf("temps = %v, want only the killed writer's orphan", got)
	}
}
