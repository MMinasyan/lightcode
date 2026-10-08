package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/protocol"
)

// The mutation suite: one private configurationService.mutate is the
// single validated config writer for the settings PUT and the Agent-model
// PUT. Every row below pins one axis of the semantic matrix over real file
// bytes, generation, warning, and event oracles.

// mutationConfigDocument is the settings-mutation fixture document: an
// unowned providers member with an exact big-integer lexeme, an unowned
// permissions member, and an unowned unknown top-level member.
const mutationConfigDocument = `{"providers":{"prov":{"transport":{"base_url":"https://prov.test/v1","api_key_env":""},"discovery":false,"models":{"m":{"name":"One","context_window":9007199254740993}}}},"permissions":{"rules":[{"permission":"file.write","target":"*","access":"allow"}]},"custom_flag":true}`

// mutationSettings is one complete settings shape the tests write: the
// sessions policy beside one opaque tools document string.
func mutationSettings() protocol.Settings {
	return protocol.Settings{
		Sessions: protocol.SessionsSettings{AutoArchive: false, ArchiveAfterDays: 5, DeleteAfterArchiveDays: 2},
		Plugins: protocol.PluginsSettings{
			"tools": `{"command_timeout":60,"max_output_bytes":2048,"read_line_max_chars":3000,"read_max_lines":100}`,
		},
	}
}

// acceptValidator is one plugin settings validator that accepts anything.
func acceptValidator(json.RawMessage) error { return nil }

// compactJSON renders one JSON value's canonical compacted form so raw
// member bytes can be compared across re-serializations.
func compactJSON(t *testing.T, data []byte) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, data); err != nil {
		t.Fatalf("compact %s: %v", data, err)
	}
	return buf.String()
}

// opaqueSettingsDocument is the settings round-trip fixture: two compiled
// plugin sections carrying fields common Runtime does not know plus one
// int64 lexeme beyond JS number precision, one empty compiled section, and
// one unowned provider model carrying the same kind of lexeme.
const opaqueSettingsDocument = `{
  "providers": {"prov": {"transport": {"base_url": "https://prov.test/v1", "api_key_env": ""}, "discovery": false, "models": {"m": {"name": "One", "context_window": 9007199254740993}}}},
  "sessions": {"archive_after_days": 3},
  "plugins": {
    "alpha": {"kept": 1, "exact": 9007199254740993},
    "tools": {"max_output_bytes": 42, "exact": 9007199254740993},
    "tasks": {}
  }
}`

// TestRuntimeSettingsReadSaveRoundTripPreservesOwnedPluginDocuments proves
// the representable-domain rule across a read-to-save round trip: every
// compiled plugin section returns as its exact owned document string —
// unknown fields and the int64 lexeme included — and writing the returned
// view back preserves every section while unowned numeric lexemes survive.
func TestRuntimeSettingsReadSaveRoundTripPreservesOwnedPluginDocuments(t *testing.T) {
	store := storage.NewMemory()
	var opens atomic.Int64
	r, e := openConfigurationRuntime(t, store, opaqueSettingsDocument, configurationAgentsDocument,
		append(settingsPlugins(), servicePlugin("alpha", &opens, acceptValidator))...)
	defer closeProjectionRuntime(r)
	ctx := context.Background()
	sub, err := r.Subscribe(8)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	view, err := r.getConfiguration(ctx)
	if err != nil {
		t.Fatalf("getConfiguration: %v", err)
	}
	read := view.Settings.Plugins
	want := map[string]string{
		"alpha": `{"kept":1,"exact":9007199254740993}`,
		"tools": `{"max_output_bytes":42,"exact":9007199254740993}`,
		"tasks": `{}`,
	}
	if len(read) != len(want) {
		t.Fatalf("settings plugins = %v, want exactly the owned sections %v", read, want)
	}
	for id, wantDoc := range want {
		got, ok := read[id]
		if !ok {
			t.Fatalf("plugins.%s is absent from the read; want the owned document %s", id, wantDoc)
		}
		if got := compactJSON(t, []byte(got)); got != wantDoc {
			t.Fatalf("plugins.%s read document = %s, want the owned %s", id, got, wantDoc)
		}
	}

	// Read-to-save: the whole plugins member round-trips its documents
	// and the unowned provider lexeme survives the owning write.
	if _, err := r.updateSettings(ctx, view.Settings); err != nil {
		t.Fatalf("updateSettings: %v", err)
	}
	nextConfigurationEvent(t, sub, "2")
	root := fileRoot(t, e.configPath)
	var plugins map[string]json.RawMessage
	if err := json.Unmarshal(root["plugins"], &plugins); err != nil {
		t.Fatalf("decode written plugins member: %v", err)
	}
	if len(plugins) != len(want) {
		t.Fatalf("written plugins = %v, want exactly %v", plugins, want)
	}
	for id, wantDoc := range want {
		if got := compactJSON(t, plugins[id]); got != wantDoc {
			t.Fatalf("written plugins.%s = %s, want the round-tripped %s", id, got, wantDoc)
		}
	}
	if got := string(root["providers"]); !strings.Contains(got, "9007199254740993") {
		t.Fatalf("written providers = %s, want the unowned int64 lexeme preserved", got)
	}
}

// assertNoEvent proves one subscription is silent without blocking.
func assertNoEvent(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case event, ok := <-sub.Events():
		if ok {
			t.Fatalf("published event %+v, want silence", event)
		}
	default:
	}
}

// fileRoot decodes one on-disk document into its raw root object.
func fileRoot(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return root
}

// writeSyncProbe is the process-global atomicfs.SyncFileFunc seam scoped to
// exactly one owning file's temp write (atomicfs names its temps
// <base>.tmp-<random> in the destination directory). Every other file sync
// delegates to the real f.Sync. The probe parks each matching sync after the
// write has begun when park is armed, and can inject a pre-rename failure.
type writeSyncProbe struct {
	mu      sync.Mutex
	base    string
	dir     string
	park    bool
	fail    error
	syncs   int
	arrive  chan struct{}
	release chan struct{}
	restore func()
}

// installOwningSyncProbe installs the probe for one owning path and returns
// it. The caller must defer releaseProbe-then-restore so the seam is
// restored before the owner's deferred disposal, and the tests using it
// never run in parallel (the seam is process-global).
func installOwningSyncProbe(t *testing.T, owningPath string) *writeSyncProbe {
	t.Helper()
	p := &writeSyncProbe{
		base:    filepath.Base(owningPath) + ".tmp-",
		dir:     filepath.Dir(owningPath),
		arrive:  make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	previous := atomicfs.SyncFileFunc
	p.restore = func() { atomicfs.SyncFileFunc = previous }
	atomicfs.SyncFileFunc = func(f *os.File) error {
		p.mu.Lock()
		matched := filepath.Dir(f.Name()) == p.dir && strings.HasPrefix(filepath.Base(f.Name()), p.base)
		if !matched {
			p.mu.Unlock()
			return f.Sync() // every other file sync delegates normally
		}
		p.syncs++
		park, fail, arrive, release := p.park, p.fail, p.arrive, p.release
		p.mu.Unlock()
		if arrive != nil {
			select {
			case arrive <- struct{}{}:
			default:
			}
		}
		if park {
			select {
			case <-release:
			case <-time.After(10 * time.Second):
				return errors.New("probe gate budget elapsed")
			}
		}
		if fail != nil {
			return fail
		}
		return nil
	}
	t.Cleanup(p.restore)
	return p
}

func (p *writeSyncProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.syncs
}

// releaseProbe unblocks a parked owning write exactly once: every consumer
// parks at most one write and guards this call, so the close is one-shot.
func (p *writeSyncProbe) releaseProbe() {
	p.mu.Lock()
	defer p.mu.Unlock()
	close(p.release)
}

// awaitWriteStarted waits for the owning write's sync point — after the
// write has begun, past every cancellation checkpoint.
func (p *writeSyncProbe) awaitWriteStarted(t *testing.T) {
	t.Helper()
	select {
	case <-p.arrive:
	case <-time.After(10 * time.Second):
		t.Fatal("the owning write never reached its sync point")
	}
}

// --- settings edit: whole shape, publication, retention ---

// TestConfigurationMutateSettingsReplacesWholeShapeAndRetainsUnowned pins
// the settings PUT over real bytes: the sessions section and the owned
// optional plugin sections are replaced wholesale, unowned root members and
// their exact numeric lexemes survive untouched, the candidate publishes as
// the next generation with one event, and the published settings are the
// written shape.
func TestConfigurationMutateSettingsReplacesWholeShapeAndRetainsUnowned(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument)
	svc := h.service(context.Background(), servicePlugin("tools", &h.opens, acceptValidator))
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	sub, err := svc.obs.subscribe(4)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)

	wantSettings := mutationSettings()
	candidate, err := svc.mutate(context.Background(), editSettings(wantSettings))
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
	if candidate.snapshot.generation != 2 || svc.current() != candidate.snapshot {
		t.Fatalf("mutate = generation %d (current published %v), want generation 2 published", candidate.snapshot.generation, svc.current() == candidate.snapshot)
	}
	if got := projectSettings(candidate.snapshot.sessions, candidate.snapshot.plugins).Sessions; got != wantSettings.Sessions {
		t.Fatalf("published sessions = %+v, want the written shape", got)
	}

	// The owning file: the written members are the marshaled shape, the
	// unowned members and their exact numeric lexeme survive.
	root := fileRoot(t, h.configPath)
	if got, want := compactJSON(t, root["sessions"]), compactJSON(t, mustMarshal(t, wantSettings.Sessions)); got != want {
		t.Fatalf("sessions member = %s, want %s", got, want)
	}
	var plugins map[string]json.RawMessage
	if err := json.Unmarshal(root["plugins"], &plugins); err != nil {
		t.Fatalf("decode plugins member: %v", err)
	}
	if got, want := compactJSON(t, plugins["tools"]), compactJSON(t, []byte(wantSettings.Plugins["tools"])); got != want {
		t.Fatalf("plugins.tools member = %s, want %s", got, want)
	}
	for _, omitted := range []string{"jobs", "tasks"} {
		if _, ok := plugins[omitted]; ok {
			t.Fatalf("plugins.%s member = %s, want it removed by the omitted optional section", omitted, plugins[omitted])
		}
	}
	var providers struct {
		Prov struct {
			Models map[string]struct {
				ContextWindow json.Number `json:"context_window"`
			} `json:"models"`
		} `json:"prov"`
	}
	decoder := json.NewDecoder(bytes.NewReader(root["providers"]))
	decoder.UseNumber()
	if err := decoder.Decode(&providers); err != nil {
		t.Fatalf("decode providers member: %v", err)
	}
	if got := providers.Prov.Models["m"].ContextWindow.String(); got != "9007199254740993" {
		t.Fatalf("unowned numeric lexeme = %s, want 9007199254740993 preserved", got)
	}
	if got, want := compactJSON(t, root["permissions"]), `{"rules":[{"permission":"file.write","target":"*","access":"allow"}]}`; got != want {
		t.Fatalf("unowned permissions member = %s, want %s", got, want)
	}
	if got := root["custom_flag"]; string(got) != "true" {
		t.Fatalf("unowned unknown member = %s, want true", got)
	}

	// One event for the successful edit, at the written generation.
	nextConfigurationEvent(t, sub, "2")
	assertNoEvent(t, sub)
}

// TestConfigurationMutateIdenticalBytesStillWriteAndPublish proves the
// no-byte-comparison rule: an edit whose complete bytes equal the current
// file — whether from an identical client request or because an external
// editor already wrote the requested bytes without a Runtime reload — still
// performs the owning write and publishes the next generation with its
// event.
func TestConfigurationMutateIdenticalBytesStillWriteAndPublish(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument)
	svc := h.service(context.Background(), servicePlugin("tools", &h.opens, acceptValidator))
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	sub, err := svc.obs.subscribe(8)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	probe := installOwningSyncProbe(t, h.configPath)
	defer probe.restore()

	settings := mutationSettings()
	if _, err := svc.mutate(context.Background(), editSettings(settings)); err != nil {
		t.Fatalf("first mutate: %v", err)
	}
	nextConfigurationEvent(t, sub, "2")
	if syncs := probe.count(); syncs != 1 {
		t.Fatalf("owning writes so far = %d, want 1", syncs)
	}
	before := fileRoot(t, h.configPath)

	// Identical settings bytes: still a real owning write and publication.
	candidate, err := svc.mutate(context.Background(), editSettings(settings))
	if err != nil {
		t.Fatalf("identical mutate: %v", err)
	}
	if candidate.snapshot.generation != 3 || svc.current() != candidate.snapshot {
		t.Fatalf("identical mutate = generation %d, want 3 published", candidate.snapshot.generation)
	}
	nextConfigurationEvent(t, sub, "3")
	if syncs := probe.count(); syncs != 2 {
		t.Fatalf("owning writes after the identical edit = %d, want 2", syncs)
	}
	if got := fileRoot(t, h.configPath); compactJSON(t, got["sessions"]) != compactJSON(t, before["sessions"]) {
		t.Fatal("the identical edit changed the written sessions member")
	}

	// An external editor writing the requested bytes without a Runtime
	// reload must not let the next valid edit skip the write or publication.
	data, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatalf("read owning file: %v", err)
	}
	writeServiceFile(t, h.configPath, string(data))
	candidate, err = svc.mutate(context.Background(), editSettings(settings))
	if err != nil {
		t.Fatalf("mutate over externally matching bytes: %v", err)
	}
	if candidate.snapshot.generation != 4 {
		t.Fatalf("generation = %d, want 4 — external byte equality must not skip publication", candidate.snapshot.generation)
	}
	nextConfigurationEvent(t, sub, "4")
	if syncs := probe.count(); syncs != 3 {
		t.Fatalf("owning writes after the external-editor edit = %d, want 3", syncs)
	}
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}

// TestConfigurationMutateSettingsOwnsOpaquePluginDocuments pins the
// whole-plugins rule under the opaque-document domain: the PUT replaces both
// whole sessions and whole plugins members with the generated target shape —
// each document string becomes its plugin's owned raw section, an old
// compiled plugin's valid section absent from the target is gone (its
// validator receives nil, defaults govern), an unknown target ID or a
// non-object/invalid inner document refuses the complete candidate before
// any write, and an empty complete map is valid. No per-key merge or
// preserve logic inside plugins; unowned top-level members and the agents
// file stay untouched.
func TestConfigurationMutateSettingsOwnsOpaquePluginDocuments(t *testing.T) {
	h := newServiceHarness(t)
	var received []json.RawMessage
	var mu sync.Mutex
	// The fixture document carries the old alpha section the stale save must
	// remove wholesale.
	writeServiceFile(t, h.configPath, strings.TrimSuffix(mutationConfigDocument, "}")+`,"plugins":{"alpha":{"kept":1},"tools":{"max_output_bytes":42}}}`)
	if err := os.MkdirAll(h.dataDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	writeServiceFile(t, filepath.Join(h.dataDir, "agents.json"), `{"worker":{"system_prompt":"simple"}}`)
	svc := h.service(context.Background(),
		servicePlugin("alpha", &h.opens, func(raw json.RawMessage) error {
			mu.Lock()
			received = append(received, append(json.RawMessage(nil), raw...))
			mu.Unlock()
			return nil
		}),
		servicePlugin("tools", &h.opens, acceptValidator),
	)
	svc.attachWarnings(newWarningStore())
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	sub, err := svc.obs.subscribe(8)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	agentsPath := filepath.Join(h.dataDir, "agents.json")
	agentsBefore, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read agents file: %v", err)
	}

	// The omission domain: the target carries only the tools document; the
	// old alpha section is removed wholesale even though it is valid.
	stale := protocol.Settings{
		Sessions: protocol.SessionsSettings{AutoArchive: true, ArchiveAfterDays: 7, DeleteAfterArchiveDays: 7},
		Plugins: protocol.PluginsSettings{
			"tools": `{"command_timeout":30,"max_output_bytes":1024,"read_line_max_chars":2000,"read_max_lines":50}`,
		},
	}
	candidate, err := svc.mutate(context.Background(), editSettings(stale))
	if err != nil {
		t.Fatalf("mutate: %v", err)
	}
	published := projectSettings(candidate.snapshot.sessions, candidate.snapshot.plugins)
	if len(published.Plugins) != 1 || compactJSON(t, []byte(published.Plugins["tools"])) != compactJSON(t, []byte(stale.Plugins["tools"])) {
		t.Fatalf("published plugins = %v, want only the written tools document", published.Plugins)
	}
	root := fileRoot(t, h.configPath)
	var plugins map[string]json.RawMessage
	if err := json.Unmarshal(root["plugins"], &plugins); err != nil {
		t.Fatalf("decode plugins member: %v", err)
	}
	if got, want := compactJSON(t, plugins["tools"]), compactJSON(t, []byte(stale.Plugins["tools"])); got != want {
		t.Fatalf("plugins.tools member = %s, want %s", got, want)
	}
	if _, ok := plugins["alpha"]; ok {
		t.Fatalf("plugins.alpha member = %s, want it removed by the omitted section", plugins["alpha"])
	}
	if got, want := compactJSON(t, root["sessions"]), compactJSON(t, mustMarshal(t, stale.Sessions)); got != want {
		t.Fatalf("sessions member = %s, want %s", got, want)
	}
	// Unowned top-level members survive; the agents file is untouched.
	if got, want := compactJSON(t, root["permissions"]), `{"rules":[{"permission":"file.write","target":"*","access":"allow"}]}`; got != want {
		t.Fatalf("unowned permissions member = %s, want %s", got, want)
	}
	if got := root["custom_flag"]; string(got) != "true" {
		t.Fatalf("unowned unknown member = %s, want true", got)
	}
	if string(root["providers"]) == "" {
		t.Fatal("unowned providers member missing")
	}
	if _, ok := root["alpha"]; ok {
		t.Fatal("a removed plugin section leaked to the root")
	}
	agentsAfter, err := os.ReadFile(agentsPath)
	if err != nil || string(agentsBefore) != string(agentsAfter) {
		t.Fatalf("the settings edit changed the agents file (%v)", err)
	}
	// The removed section is gone for its plugin too: the edit build's alpha
	// validator received nil, so defaults govern.
	mu.Lock()
	alphaCalls := len(received)
	var alphaEditRaw json.RawMessage
	if alphaCalls == 2 {
		alphaEditRaw = received[1]
	}
	mu.Unlock()
	if alphaCalls != 2 || alphaEditRaw != nil {
		t.Fatalf("alpha validator calls = %d (edit build received %s), want nil defaults on the omitted section", alphaCalls, alphaEditRaw)
	}
	nextConfigurationEvent(t, sub, "2")

	// The refusal domain: an unknown ID, invalid inner JSON, and a valid but
	// non-object document each fail the complete candidate with no file,
	// generation, warning, or event change.
	before, first, warnRev := metadataBaseline(t, svc, h.configPath)
	for _, row := range []struct {
		name    string
		plugins protocol.PluginsSettings
	}{
		{"unknown id", protocol.PluginsSettings{"ghost": `{}`}},
		{"invalid document", protocol.PluginsSettings{"alpha": `{not json`}},
		{"non-object document", protocol.PluginsSettings{"alpha": `1`}},
		{"null document", protocol.PluginsSettings{"alpha": `null`}},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := svc.mutate(context.Background(), editSettings(protocol.Settings{Sessions: stale.Sessions, Plugins: row.plugins}))
			assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, ErrConfiguration)
		})
	}

	// An empty complete map is valid: it removes every owned section.
	candidate, err = svc.mutate(context.Background(), editSettings(protocol.Settings{Sessions: stale.Sessions, Plugins: protocol.PluginsSettings{}}))
	if err != nil {
		t.Fatalf("empty plugins mutate: %v", err)
	}
	if got := projectSettings(candidate.snapshot.sessions, candidate.snapshot.plugins).Plugins; len(got) != 0 {
		t.Fatalf("published plugins after the empty map = %v, want none", got)
	}
	root = fileRoot(t, h.configPath)
	if got := compactJSON(t, root["plugins"]); got != `{}` {
		t.Fatalf("plugins member = %s, want the empty complete map", got)
	}
	nextConfigurationEvent(t, sub, "3")
}

// TestConfigurationMutateEverySuccessfulEditWritesAndPublishes pins the one
// shared successful-edit rule that replaced the old changed/no-edit branch:
// an apply that reports the owning file still performs the owning write and
// publishes the next generation with its event even when it changed no
// member.
func TestConfigurationMutateEverySuccessfulEditWritesAndPublishes(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument)
	svc := h.service(context.Background(), servicePlugin("tools", &h.opens, acceptValidator))
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	sub, err := svc.obs.subscribe(4)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	probe := installOwningSyncProbe(t, h.configPath)
	defer probe.restore()

	noChange := configurationEdit{apply: func(rawRoots, configurationCapture) (editedFile, error) {
		return editMainConfig, nil
	}}
	candidate, err := svc.mutate(context.Background(), noChange)
	if err != nil {
		t.Fatalf("no-change mutate: %v", err)
	}
	if candidate.snapshot.generation != 2 || svc.current() != candidate.snapshot {
		t.Fatalf("no-change mutate = generation %d, want 2 published", candidate.snapshot.generation)
	}
	nextConfigurationEvent(t, sub, "2")
	if syncs := probe.count(); syncs != 1 {
		t.Fatalf("owning writes = %d, want the shared successful edit to write", syncs)
	}
}

// --- owning file identity, mode, and no side effects ---

// TestConfigurationMutateWritesOwningFileOnly0600 proves the write contract:
// exactly the owning file is atomically rewritten at mode 0600, a missing
// input file contributes its skeleton bytes in memory without creating it,
// and the companion file is never written as a side effect.
func TestConfigurationMutateWritesOwningFileOnly0600(t *testing.T) {
	h := newServiceHarness(t)
	// No input files exist yet: the mutation must not create the non-owning
	// file, and the owning file is born at mode 0600.
	svc := h.service(context.Background(), servicePlugin("tools", &h.opens, acceptValidator))
	agentsPath := filepath.Join(h.dataDir, "agents.json")

	candidate, err := svc.mutate(context.Background(), svc.editAgentModel("primary", "prov/m"))
	if err != nil {
		t.Fatalf("agent mutate: %v", err)
	}
	if candidate.snapshot.generation != 1 {
		t.Fatalf("mutation generation = %d, want 1 from the empty prior publication", candidate.snapshot.generation)
	}
	info, err := os.Stat(agentsPath)
	if err != nil {
		t.Fatalf("owning agents file missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("owning file mode = %o, want 600", info.Mode().Perm())
	}
	if _, err := os.Stat(h.configPath); !os.IsNotExist(err) {
		t.Fatalf("the companion main configuration was created as a side effect: %v", err)
	}

	// The settings PUT owns the main configuration only: agents.json keeps
	// its bytes and the main file is born at 0600 from the skeleton root.
	agentsBefore, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read agents file: %v", err)
	}
	if _, err := svc.mutate(context.Background(), editSettings(mutationSettings())); err != nil {
		t.Fatalf("settings mutate: %v", err)
	}
	info, err = os.Stat(h.configPath)
	if err != nil {
		t.Fatalf("owning config file missing: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("owning file mode = %o, want 600", info.Mode().Perm())
	}
	agentsAfter, err := os.ReadFile(agentsPath)
	if err != nil || string(agentsBefore) != string(agentsAfter) {
		t.Fatalf("the companion agents file changed: %v", err)
	}
}

// --- agent model edit ---

// TestConfigurationMutateAgentModelEditsLatestRoot pins the Agent-model
// mutation semantics copied from the retained WriteModel: a nonempty ref
// replaces the user override (identical values included), an empty ref
// clears it while preserving the entry's other members, a builtin without a
// user entry gains only its model member, and every successful set or clear
// — absent override included, identical bytes included — is a real edit
// that writes the owning file and publishes the next generation.
func TestConfigurationMutateAgentModelEditsLatestRoot(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument)
	writeServiceFile(t, filepath.Join(h.dataDir, "agents.json"), `{"worker":{"system_prompt":"simple"}}`)
	svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, acceptValidator))
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	sub, err := svc.obs.subscribe(16)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	agentsPath := filepath.Join(h.dataDir, "agents.json")

	// Set: the override lands on the existing entry and resolves.
	candidate, err := svc.mutate(context.Background(), svc.editAgentModel("worker", "prov/m"))
	if err != nil {
		t.Fatalf("set model: %v", err)
	}
	if found, model := hasDefinition(candidate.snapshot, "worker"); !found || model != "prov/m" {
		t.Fatalf("worker definition = (%v, %q), want the new override", found, model)
	}
	nextConfigurationEvent(t, sub, "2")
	if root := fileRoot(t, agentsPath); compactJSON(t, root["worker"]) != `{"model":"prov/m","system_prompt":"simple"}` {
		t.Fatalf("worker entry = %s, want the model added beside the kept member", root["worker"])
	}

	// Identical value: still a real edit and publication.
	candidate, err = svc.mutate(context.Background(), svc.editAgentModel("worker", "prov/m"))
	if err != nil || candidate.snapshot.generation != 3 {
		t.Fatalf("identical model edit = (%v, generation %d), want generation 3", err, candidate.snapshot.generation)
	}
	nextConfigurationEvent(t, sub, "3")

	// Clear: the override is removed, the entry's other member kept.
	candidate, err = svc.mutate(context.Background(), svc.editAgentModel("worker", ""))
	if err != nil {
		t.Fatalf("clear model: %v", err)
	}
	if found, model := hasDefinition(candidate.snapshot, "worker"); !found || model != "" {
		t.Fatalf("worker definition = (%v, %q), want the cleared override", found, model)
	}
	nextConfigurationEvent(t, sub, "4")
	if root := fileRoot(t, agentsPath); compactJSON(t, root["worker"]) != `{"system_prompt":"simple"}` {
		t.Fatalf("worker entry = %s, want the model member removed", root["worker"])
	}

	// Clearing again with no user override present is still a successful
	// edit: it writes the owning file and publishes the next generation —
	// the no-override exception belongs to the retained public field reset
	// operator, not to this client. The rewritten bytes are identical
	// (deterministic remarshal of a no-op clear), so the write itself is
	// proven by the sync-seam probe.
	probe := installOwningSyncProbe(t, agentsPath)
	candidate, err = svc.mutate(context.Background(), svc.editAgentModel("worker", ""))
	if err != nil || candidate.snapshot.generation != 5 {
		t.Fatalf("absent-override clear = (%v, generation %d), want a successful edit at generation 5", err, candidate.snapshot.generation)
	}
	nextConfigurationEvent(t, sub, "5")
	if syncs := probe.count(); syncs != 1 {
		t.Fatalf("owning writes for the absent-override clear = %d, want a real write", syncs)
	}
	probe.restore()
	if found, model := hasDefinition(candidate.snapshot, "worker"); !found || model != "" {
		t.Fatalf("worker definition = (%v, %q), want the resolved inherited state", found, model)
	}

	// A builtin without a user entry is addressable: the edit creates only
	// its model member.
	candidate, err = svc.mutate(context.Background(), svc.editAgentModel("primary", "prov/m"))
	if err != nil {
		t.Fatalf("builtin model edit: %v", err)
	}
	if found, model := hasDefinition(candidate.snapshot, "primary"); !found || model != "prov/m" {
		t.Fatalf("primary definition = (%v, %q), want the new override", found, model)
	}
	nextConfigurationEvent(t, sub, "6")
	if root := fileRoot(t, agentsPath); compactJSON(t, root["primary"]) != `{"model":"prov/m"}` {
		t.Fatalf("primary entry = %s, want the bare model-only overlay", root["primary"])
	}
}

// TestConfigurationMutateAgentModelClearAlwaysWritesAndPublishes proves the
// nearest sibling of the retained public absent-override reset: a known
// builtin with NO user model override and an empty-ref request is still a
// successful edit — it writes the owning agents file, publishes the next
// generation with its event, and repeating the same empty-ref bytes advances
// again instead of behaving as a reset.
func TestConfigurationMutateAgentModelClearAlwaysWritesAndPublishes(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument)
	writeServiceFile(t, filepath.Join(h.dataDir, "agents.json"), "{}\n")
	svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, acceptValidator))
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	sub, err := svc.obs.subscribe(8)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	agentsPath := filepath.Join(h.dataDir, "agents.json")
	probe := installOwningSyncProbe(t, agentsPath)
	defer probe.restore()

	for _, generation := range []string{"2", "3"} {
		candidate, err := svc.mutate(context.Background(), svc.editAgentModel("primary", ""))
		if err != nil {
			t.Fatalf("empty-ref clear: %v", err)
		}
		if got := fmt.Sprint(candidate.snapshot.generation); got != generation {
			t.Fatalf("empty-ref clear generation = %s, want %s — the repeat must advance again", got, generation)
		}
		nextConfigurationEvent(t, sub, generation)
	}
	if syncs := probe.count(); syncs != 2 {
		t.Fatalf("owning writes = %d, want a real write per request", syncs)
	}
	if root := fileRoot(t, agentsPath); compactJSON(t, root["primary"]) != `{}` {
		t.Fatalf("primary entry = %s, want the retained WriteModel bare-entry semantics", root["primary"])
	}
}

// TestConfigurationMutateUnknownAgentTypeFailsWithoutBareEntry proves the
// nearest forbidden sibling of the retained create-on-missing loader path:
// an unknown or dropped/invalid type fails harness.ErrInvalid before any
// bare definition is created, and nothing is written, published, or
// notified.
func TestConfigurationMutateUnknownAgentTypeFailsWithoutBareEntry(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument)
	writeServiceFile(t, filepath.Join(h.dataDir, "agents.json"), `{"broken":{"tools":["ghost_tool"]},"badmodel":{"model":"noslash"}}`)
	svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil))
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	sub, err := svc.obs.subscribe(4)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	agentsPath := filepath.Join(h.dataDir, "agents.json")
	before, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read agents file: %v", err)
	}

	for _, agentType := range []string{"ghost", "broken", "badmodel"} {
		if _, err := svc.mutate(context.Background(), svc.editAgentModel(agentType, "prov/m")); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("agent type %q mutate = %v, want harness.ErrInvalid", agentType, err)
		}
	}
	after, err := os.ReadFile(agentsPath)
	if err != nil || string(before) != string(after) {
		t.Fatalf("a failed agent mutate changed the owning file (%v)", err)
	}
	if svc.current().generation != 1 {
		t.Fatalf("generation = %d, want a failed edit to consume nothing", svc.current().generation)
	}
	assertNoEvent(t, sub)
}

// TestConfigurationMutateMalformedModelRefFailsWithoutWrite proves a
// nonempty ref is validated by the retained model.Parse before any edit.
func TestConfigurationMutateMalformedModelRefFailsWithoutWrite(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument)
	writeServiceFile(t, filepath.Join(h.dataDir, "agents.json"), `{"worker":{"model":"prov/m"}}`)
	svc := h.service(context.Background(), servicePlugin("alpha", &h.opens, nil))
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	agentsPath := filepath.Join(h.dataDir, "agents.json")
	before, err := os.ReadFile(agentsPath)
	if err != nil {
		t.Fatalf("read agents file: %v", err)
	}
	if _, err := svc.mutate(context.Background(), svc.editAgentModel("worker", "noslash")); !errors.Is(err, harness.ErrInvalid) {
		t.Fatalf("malformed ref mutate = %v, want harness.ErrInvalid", err)
	}
	after, err := os.ReadFile(agentsPath)
	if err != nil || string(before) != string(after) {
		t.Fatalf("a malformed ref changed the owning file (%v)", err)
	}
	if svc.current().generation != 1 {
		t.Fatalf("generation = %d, want no consumed generation", svc.current().generation)
	}
}

// --- invalid candidate: nothing changes ---

// TestConfigurationMutateInvalidCandidateLeavesEverythingUnchanged proves
// the candidate-failure rule over the latest bytes: a validator rejection of
// the written section, an externally malformed document, and an externally
// trailing document each fail the mutation with no file write, no
// generation, no warning change, and no event. (Foreign sections inside the
// plugins member are removed by the whole-shape write instead — the
// whole-plugins regression owns that axis.)
func TestConfigurationMutateInvalidCandidateLeavesEverythingUnchanged(t *testing.T) {
	sentinel := errors.New("tools settings invalid")
	// rejectWritten accepts the absent section but rejects the mutation's
	// written tools section (MaxOutputBytes 2048).
	rejectWritten := func(raw json.RawMessage) error {
		if strings.Contains(string(raw), "2048") {
			return sentinel
		}
		return nil
	}
	rows := []struct {
		name       string
		validate   func(json.RawMessage) error
		external   string // the document an external editor writes before the edit
		wantSource string
	}{
		{
			name:       "validator rejects the written section",
			validate:   rejectWritten,
			external:   mutationConfigDocument,
			wantSource: "tools settings invalid",
		},
		{
			name:       "externally malformed document",
			validate:   acceptValidator,
			external:   `{not json`,
			wantSource: "decode",
		},
		{
			name:       "externally trailing document",
			validate:   acceptValidator,
			external:   mutationConfigDocument + `{}`,
			wantSource: "after top-level value",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := newServiceHarness(t)
			writeServiceFile(t, h.configPath, mutationConfigDocument)
			svc := h.service(context.Background(),
				servicePlugin("tools", &h.opens, row.validate),
				servicePlugin("alpha", &h.opens, acceptValidator))
			first, err := svc.publish(context.Background())
			if err != nil {
				t.Fatalf("initial publish: %v", err)
			}
			sub, err := svc.obs.subscribe(4)
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			t.Cleanup(sub.Close)
			warnings := newWarningStore()
			svc.attachWarnings(warnings)
			revision, _ := warnings.snapshot()

			// The external editor's whole-file last-writer-wins state is what
			// the edit consumes as the latest raw layer.
			writeServiceFile(t, h.configPath, row.external)
			data, err := os.ReadFile(h.configPath)
			if err != nil {
				t.Fatalf("read owning file: %v", err)
			}
			candidate, err := svc.mutate(context.Background(), editSettings(mutationSettings()))
			if candidate.snapshot != nil || !errors.Is(err, ErrConfiguration) {
				t.Fatalf("mutate = (%v, %v), want a rejected candidate", candidate, err)
			}
			if row.wantSource != "" && !strings.Contains(err.Error(), row.wantSource) {
				t.Fatalf("error %v does not preserve %q", err, row.wantSource)
			}
			after, rerr := os.ReadFile(h.configPath)
			if rerr != nil || string(data) != string(after) {
				t.Fatalf("a failed candidate changed the owning file (%v)", rerr)
			}
			if gotRevision, _ := warnings.snapshot(); gotRevision != revision {
				t.Fatalf("warning revision advanced on a failed candidate: %d → %d", revision, gotRevision)
			}
			assertNoEvent(t, sub)
			if svc.current() != first.snapshot {
				t.Fatal("a failed candidate replaced the prior publication")
			}
		})
	}
}

// TestConfigurationMutateAgentEditInvalidCandidateLeavesEverythingUnchanged
// pins the same candidate-failure rule through the Agent-model edit, whose
// owning file is agents.json and whose main configuration is only the
// companion: an external editor's bad unowned members — a wrong-typed
// providers or sessions member, an unknown plugin section with no compiled
// plugin — stay in the latest raw layer and reject the candidate with the
// typed failure, both files byte-unchanged from the post-bad-write baseline,
// the same published pointer, generation, and warning revision, and event
// silence. The inverse probe proves the oracles flip: restoring a valid
// companion makes the same edit succeed with its generation and event.
func TestConfigurationMutateAgentEditInvalidCandidateLeavesEverythingUnchanged(t *testing.T) {
	rows := []struct {
		name       string
		config     string // the valid-JSON companion an external editor writes
		wantSource string
	}{
		{
			name:       "wrong-typed unowned providers member",
			config:     `{"providers":5,"sessions":{"auto_archive":true}}`,
			wantSource: "decode",
		},
		{
			name:       "wrong-typed unowned sessions member",
			config:     `{"providers":{},"sessions":"x"}`,
			wantSource: "sessions",
		},
		{
			name:       "unknown plugin section",
			config:     `{"providers":{},"plugins":{"ghost":{}}}`,
			wantSource: "ghost",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := newServiceHarness(t)
			writeServiceFile(t, h.configPath, mutationConfigDocument)
			writeServiceFile(t, filepath.Join(h.dataDir, "agents.json"), `{"worker":{"system_prompt":"simple"}}`)
			svc := h.service(context.Background(),
				servicePlugin("tools", &h.opens, acceptValidator),
				servicePlugin("alpha", &h.opens, acceptValidator))
			first, err := svc.publish(context.Background())
			if err != nil {
				t.Fatalf("initial publish: %v", err)
			}
			sub, err := svc.obs.subscribe(4)
			if err != nil {
				t.Fatalf("subscribe: %v", err)
			}
			t.Cleanup(sub.Close)
			warnings := newWarningStore()
			svc.attachWarnings(warnings)
			revision, _ := warnings.snapshot()

			// The external bad state is the baseline: the oracles compare
			// against the latest bytes on disk, not the last valid file.
			writeServiceFile(t, h.configPath, row.config)
			configBefore, err := os.ReadFile(h.configPath)
			if err != nil {
				t.Fatalf("read companion file: %v", err)
			}
			agentsPath := filepath.Join(h.dataDir, "agents.json")
			agentsBefore, err := os.ReadFile(agentsPath)
			if err != nil {
				t.Fatalf("read owning file: %v", err)
			}
			candidate, err := svc.mutate(context.Background(), svc.editAgentModel("worker", "prov/m"))
			if candidate.snapshot != nil || !errors.Is(err, ErrConfiguration) {
				t.Fatalf("agent edit = (%v, %v), want a rejected candidate", candidate, err)
			}
			if !strings.Contains(err.Error(), row.wantSource) {
				t.Fatalf("error %v does not preserve %q", err, row.wantSource)
			}
			configAfter, err := os.ReadFile(h.configPath)
			if err != nil || string(configBefore) != string(configAfter) {
				t.Fatalf("a failed candidate changed the companion file (%v)", err)
			}
			agentsAfter, err := os.ReadFile(agentsPath)
			if err != nil || string(agentsBefore) != string(agentsAfter) {
				t.Fatalf("a failed candidate changed the owning file (%v)", err)
			}
			if gotRevision, _ := warnings.snapshot(); gotRevision != revision {
				t.Fatalf("warning revision advanced on a failed candidate: %d → %d", revision, gotRevision)
			}
			assertNoEvent(t, sub)
			if svc.current() != first.snapshot || first.snapshot.generation != 1 {
				t.Fatal("a failed candidate replaced the prior publication")
			}

			// The inverse probe: with the bad member gone the same edit
			// succeeds, proving the file/generation/event oracles flip.
			writeServiceFile(t, h.configPath, mutationConfigDocument)
			candidate, err = svc.mutate(context.Background(), svc.editAgentModel("worker", "prov/m"))
			if err != nil || candidate.snapshot.generation != 2 || svc.current() != candidate.snapshot {
				t.Fatalf("inverse edit = (%v, generation %d), want a successful publication at 2", err, candidate.snapshot.generation)
			}
			nextConfigurationEvent(t, sub, "2")
		})
	}
}

// TestConfigurationMutateMalformedPermissionsFallbackPublishes proves a
// hand-edited malformed Workspace permissions.json is captured and resolved
// to the built-in fallback without failing the mutation's publication.
func TestConfigurationMutateMalformedPermissionsFallbackPublishes(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument)
	svc := h.service(context.Background(), servicePlugin("tools", &h.opens, acceptValidator))
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	dirID, err := config.WorkspacePermissionDirID("/ws")
	if err != nil {
		t.Fatalf("dir id: %v", err)
	}
	permissionPath := filepath.Join(h.home, ".lightcode", "projects", dirID, "permissions.json")
	writeServiceFile(t, permissionPath, `NOT JSON`)
	candidate, err := svc.mutate(context.Background(), editSettings(mutationSettings()))
	if err != nil {
		t.Fatalf("mutate with a malformed permission file: %v", err)
	}
	var builtin harness.PermissionPolicy
	if got := candidate.snapshot.permissionPolicy("/ws"); fmt.Sprint(got) != fmt.Sprint(builtin) {
		t.Fatalf("malformed permission policy = %#v, want the built-in fallback %#v", got, builtin)
	}
}

// --- cancellation brackets around the write ---

// TestConfigurationMutateCancelsBeforeWrite proves the pre-write barrier: a
// cancellation observed after the candidate validates but before the owning
// write leaves the file, generation, and event stream unchanged, and the
// context error stays unwrapped.
func TestConfigurationMutateCancelsBeforeWrite(t *testing.T) {
	t.Run("caller canceled after validation", func(t *testing.T) {
		h := newServiceHarness(t)
		writeServiceFile(t, h.configPath, mutationConfigDocument)
		ctx, cancel := context.WithCancel(context.Background())
		svc := h.service(context.Background(), servicePlugin("tools", &h.opens, func(json.RawMessage) error {
			cancel()
			return nil
		}))
		if _, err := svc.publish(context.Background()); err != nil {
			t.Fatalf("initial publish: %v", err)
		}
		sub, err := svc.obs.subscribe(4)
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		t.Cleanup(sub.Close)
		data, err := os.ReadFile(h.configPath)
		if err != nil {
			t.Fatalf("read owning file: %v", err)
		}
		candidate, err := svc.mutate(ctx, editSettings(mutationSettings()))
		if candidate.snapshot != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("mutate = (%v, %v), want the caller context error before the write", candidate, err)
		}
		after, rerr := os.ReadFile(h.configPath)
		if rerr != nil || string(data) != string(after) {
			t.Fatalf("the canceled mutation changed the owning file (%v)", rerr)
		}
		assertNoEvent(t, sub)
	})
	t.Run("owner canceled before waiting", func(t *testing.T) {
		owner, cancelOwner := context.WithCancel(context.Background())
		h := newServiceHarness(t)
		writeServiceFile(t, h.configPath, mutationConfigDocument)
		svc := h.service(owner, servicePlugin("tools", &h.opens, acceptValidator))
		if _, err := svc.publish(context.Background()); err != nil {
			t.Fatalf("initial publish: %v", err)
		}
		sub, err := svc.obs.subscribe(4)
		if err != nil {
			t.Fatalf("subscribe: %v", err)
		}
		t.Cleanup(sub.Close)
		warnings := newWarningStore()
		svc.attachWarnings(warnings)
		revision, _ := warnings.snapshot()
		data, err := os.ReadFile(h.configPath)
		if err != nil {
			t.Fatalf("read owning file: %v", err)
		}
		cancelOwner()
		candidate, err := svc.mutate(context.Background(), editSettings(mutationSettings()))
		if candidate.snapshot != nil || !errors.Is(err, ErrClosed) {
			t.Fatalf("mutate = (%v, %v), want ErrClosed before any write", candidate, err)
		}
		after, rerr := os.ReadFile(h.configPath)
		if rerr != nil || string(data) != string(after) {
			t.Fatalf("the owner-canceled mutation changed the owning file (%v)", rerr)
		}
		if gotRevision, _ := warnings.snapshot(); gotRevision != revision {
			t.Fatalf("warning revision advanced on the owner-canceled mutation: %d → %d", revision, gotRevision)
		}
		assertNoEvent(t, sub)
	})
}

// TestConfigurationMutateCompletesPublicationAfterWriteStarts proves the
// post-write barrier: once the owning write has begun, a caller
// cancellation no longer stops anything — the admitted call completes the
// write, the ready snapshot publication, and its one event, with no
// half-published generation.
func TestConfigurationMutateCompletesPublicationAfterWriteStarts(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument)
	svc := h.service(context.Background(), servicePlugin("tools", &h.opens, acceptValidator))
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	sub, err := svc.obs.subscribe(4)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	probe := installOwningSyncProbe(t, h.configPath)
	probe.mu.Lock()
	probe.park = true
	probe.mu.Unlock()
	// Failure cleanup releases the parked owning write and restores the seam
	// before the fixture disposal, deterministically, on every exit.
	releasePark := sync.OnceFunc(probe.releaseProbe)
	defer func() {
		releasePark()
		probe.restore()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		cfg configurationCapture
		err error
	}
	done := make(chan result, 1)
	go func() {
		cfg, err := svc.mutate(ctx, editSettings(mutationSettings()))
		done <- result{cfg, err}
	}()
	probe.awaitWriteStarted(t)
	cancel() // the caller disconnects after the write has begun
	releasePark()

	got := <-done
	if got.err != nil {
		t.Fatalf("mutate after write start = %v, want the admitted call to complete publication", got.err)
	}
	if got.cfg.snapshot.generation != 2 || svc.current() != got.cfg.snapshot {
		t.Fatalf("post-cancel publication = generation %d, want 2 published", got.cfg.snapshot.generation)
	}
	nextConfigurationEvent(t, sub, "2")
	root := fileRoot(t, h.configPath)
	if !strings.Contains(compactJSON(t, root["sessions"]), `"archive_after_days":5`) {
		t.Fatalf("owning file = %s, want the written sessions member", root["sessions"])
	}
}

// TestConfigurationMutateWriteFailureLeavesEverythingUnchanged proves a
// pre-rename write failure publishes nothing: the error is classified, the
// file is unchanged, and no generation, warning refresh, or event follows.
func TestConfigurationMutateWriteFailureLeavesEverythingUnchanged(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument)
	svc := h.service(context.Background(), servicePlugin("tools", &h.opens, acceptValidator))
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	sub, err := svc.obs.subscribe(4)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)
	injected := errors.New("injected sync failure")
	probe := installOwningSyncProbe(t, h.configPath)
	probe.mu.Lock()
	probe.fail = injected
	probe.mu.Unlock()
	defer probe.restore()
	data, err := os.ReadFile(h.configPath)
	if err != nil {
		t.Fatalf("read owning file: %v", err)
	}
	candidate, err := svc.mutate(context.Background(), editSettings(mutationSettings()))
	if candidate.snapshot != nil || !errors.Is(err, ErrConfiguration) || !errors.Is(err, injected) {
		t.Fatalf("mutate = (%v, %v), want the injected write failure preserved", candidate, err)
	}
	after, rerr := os.ReadFile(h.configPath)
	if rerr != nil || string(data) != string(after) {
		t.Fatalf("a failed write changed the owning file (%v)", rerr)
	}
	if svc.current().generation != 1 {
		t.Fatalf("generation = %d, want no consumed generation", svc.current().generation)
	}
	assertNoEvent(t, sub)
}

// --- one shared writer serializes mutations and Reload ---

// TestConfigurationMutateSerializesWithReload proves one build mutex over
// both entry points: a Reload waits for the in-flight mutation and completes
// with the next generation, and the events keep publication order.
func TestConfigurationMutateSerializesWithReload(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument)
	var builds atomic.Int64
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	svc := h.service(context.Background(), servicePlugin("tools", &h.opens, func(json.RawMessage) error {
		if builds.Add(1) == 2 { // the second build — the mutation — parks
			entered <- struct{}{}
			<-gate
		}
		return nil
	}))
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	sub, err := svc.obs.subscribe(8)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)

	type result struct {
		revision string
		err      error
	}
	mutation := make(chan result, 1)
	go func() {
		cfg, err := svc.mutate(context.Background(), editSettings(mutationSettings()))
		if err != nil {
			mutation <- result{err: err}
			return
		}
		mutation <- result{revision: fmt.Sprint(cfg.snapshot.generation)}
	}()
	<-entered
	reload := make(chan result, 1)
	go func() {
		snapshot, err := svc.publish(context.Background())
		if err != nil {
			reload <- result{err: err}
			return
		}
		reload <- result{revision: fmt.Sprint(snapshot.snapshot.generation)}
	}()
	// The Reload cannot finish while the mutation holds the shared writer.
	select {
	case r := <-reload:
		t.Fatalf("Reload overtook the in-flight mutation: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
	close(gate)
	if got := <-mutation; got.err != nil || got.revision != "2" {
		t.Fatalf("mutation = %+v, want generation 2", got)
	}
	if got := <-reload; got.err != nil || got.revision != "3" {
		t.Fatalf("reload = %+v, want generation 3 after the serialized mutation", got)
	}
	nextConfigurationEvent(t, sub, "2")
	nextConfigurationEvent(t, sub, "3")
}

// --- Runtime clients ---

// TestRuntimeSettingsMutationLastWriterWins proves the settings client's
// whole-shape rule at the Runtime boundary: two clients reading revision N
// both succeed with sequential next generations — the second, older
// whole-section document overwrites the first write and a fresh read
// observes the winner.
func TestRuntimeSettingsMutationLastWriterWins(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, _ := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}
		defer sub.Close()

		view, err := r.getConfiguration(ctx)
		if err != nil {
			t.Fatalf("getConfiguration: %v", err)
		}
		if view.ConfigurationRevision.Generation != "1" {
			t.Fatalf("initial revision = %+v, want generation 1", view.ConfigurationRevision)
		}
		stale := view.Settings // value copy: client 2's older whole-section document

		first := view.Settings
		first.Sessions.ArchiveAfterDays = 9
		mutation, err := r.updateSettings(ctx, first)
		if err != nil {
			t.Fatalf("client 1 updateSettings: %v", err)
		}
		if mutation.ConfigurationRevision.Generation != "2" || mutation.Result.Sessions.ArchiveAfterDays != 9 {
			t.Fatalf("client 1 mutation = %+v, want generation 2 with the written value", mutation)
		}

		// Client 2 submits its older whole-section document:
		// last-writer-wins, not a conflict.
		mutation, err = r.updateSettings(ctx, stale)
		if err != nil {
			t.Fatalf("client 2 stale updateSettings: %v", err)
		}
		if mutation.ConfigurationRevision.Generation != "3" || mutation.Result.Sessions.ArchiveAfterDays != 3 {
			t.Fatalf("client 2 mutation = %+v, want generation 3 with the stale value", mutation)
		}
		fresh, err := r.getConfiguration(ctx)
		if err != nil {
			t.Fatalf("fresh read: %v", err)
		}
		if fresh.ConfigurationRevision.Generation != "3" || fresh.Settings.Sessions.ArchiveAfterDays != 3 {
			t.Fatalf("fresh settings = rev %+v sessions %+v, want the stale writer's value at generation 3", fresh.ConfigurationRevision, fresh.Settings.Sessions)
		}
		for _, want := range []string{"2", "3"} {
			nextConfigurationEvent(t, sub, want)
		}
	})
}

// TestRuntimeSettingsMutationOwnershipAndResult pins the Runtime client's
// result projection: the mutation result projects the returned candidate
// with the empty pre-server instance identity, and the returned settings are
// owned against a serialized baseline.
func TestRuntimeSettingsMutationOwnershipAndResult(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, _ := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		mutation, err := r.updateSettings(ctx, mutationSettings())
		if err != nil {
			t.Fatalf("updateSettings: %v", err)
		}
		if mutation.ConfigurationRevision.Generation != "2" || mutation.ConfigurationRevision.InstanceId != "" {
			t.Fatalf("mutation revision = %+v, want generation 2 with the empty pre-server instance", mutation.ConfigurationRevision)
		}
		mutation.Result.Sessions.ArchiveAfterDays = 999
		mutation.Result.Plugins["tools"] = `{"mutated":true}`
		fresh, err := r.getConfiguration(ctx)
		if err != nil {
			t.Fatalf("getConfiguration: %v", err)
		}
		if fresh.Settings.Sessions.ArchiveAfterDays != 5 || compactJSON(t, []byte(fresh.Settings.Plugins["tools"])) != compactJSON(t, []byte(mutationSettings().Plugins["tools"])) {
			t.Fatalf("a mutation of the returned result reached the published settings: %+v", fresh.Settings)
		}
	})
}

// TestRuntimeAgentModelMutationAffectsNextAdmissions proves the Agent-model
// PUT's effect boundary over the real admission path: two Sessions selecting
// the edited type both prepare their next Operations with the new model at
// the new generation, while the durably running Operation keeps its original
// committed capture — no re-preparation and no capture change.
func TestRuntimeAgentModelMutationAffectsNextAdmissions(t *testing.T) {
	eachPrepStore(t, func(t *testing.T, store harness.Storage) {
		e := newOwnerEnv(t)
		writeServiceFile(t, e.configPath, `{"providers":{"prov":{"transport":{"base_url":"`+e.server.URL+`","api_key_env":""},"discovery":false,"models":{"m":{"name":"M","context_window":262144,"max_output_tokens":4096},"wide":{"name":"W","context_window":262144,"max_output_tokens":4096}}}}}`)
		writeServiceFile(t, agents.PathForConfig(e.configPath), `{"solo":{"model":"prov/m","system_prompt":"simple"},"worker":{"model":"prov/m","system_prompt":"simple"}}`)
		r, err := e.open(context.Background(), e.storagePlugin(store))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		defer closeProjectionRuntime(r)
		ctx := context.Background()

		a := projectionSession(t, r, "/tmp/mutation-a", "worker")
		b := projectionSession(t, r, "/tmp/mutation-b", "worker")

		// One durably running Operation on session A holds the original
		// capture while the mutation lands.
		gate := make(chan struct{})
		e.server.setHold(gate)
		submitThroughRuntime(t, r, a.Identity.SessionID, "op-active", "running")
		awaitModelArrival(t, e)

		mutation, err := r.setAgentTypeModel(ctx, "worker", "prov/wide")
		if err != nil {
			t.Fatalf("setAgentTypeModel: %v", err)
		}
		if mutation.ConfigurationRevision.Generation != "2" || mutation.Result.Model != "prov/wide" || mutation.Result.Name != "worker" {
			t.Fatalf("mutation = %+v, want the new model projected from the returned candidate at generation 2", mutation)
		}

		// Session B is idle: its next admission commits the new model at the
		// new generation, and the active Operation has not re-captured. The
		// held HTTP request parks every execution, so B's Operation runs up
		// to its model request and both settle once the gate opens.
		submitThroughRuntime(t, r, b.Identity.SessionID, "op-b1", "b turn")
		if got := committedAdmission(t, r, b.Identity.SessionID, "op-b1"); got != "worker=prov/wide@2" {
			t.Fatalf("B's committed capture = %q, want the new selection at generation 2", got)
		}
		if got := committedAdmission(t, r, a.Identity.SessionID, "op-active"); got != "worker=prov/m@1" {
			t.Fatalf("A's committed capture = %q, want the original capture kept by the running Operation", got)
		}

		// The active captures settle with their original selections; A's
		// next admission then commits the new model too.
		close(gate)
		awaitIdleSession(t, r, a.Identity.SessionID)
		awaitIdleSession(t, r, b.Identity.SessionID)
		submitConvergedThroughRuntime(t, r, a.Identity.SessionID, "op-a2", "a turn", harness.OperationSuccess)
		if got := committedAdmission(t, r, a.Identity.SessionID, "op-a2"); got != "worker=prov/wide@2" {
			t.Fatalf("A's next committed capture = %q, want the selection under the mutated publication", got)
		}

		// The forbidden siblings: an unknown type and a malformed ref fail
		// invalid and never touch the owning file.
		agentsPath := agents.PathForConfig(e.configPath)
		before, err := os.ReadFile(agentsPath)
		if err != nil {
			t.Fatalf("read agents file: %v", err)
		}
		if _, err := r.setAgentTypeModel(ctx, "ghost", "prov/m"); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("unknown type = %v, want harness.ErrInvalid", err)
		}
		if _, err := r.setAgentTypeModel(ctx, "worker", "noslash"); !errors.Is(err, harness.ErrInvalid) {
			t.Fatalf("malformed ref = %v, want harness.ErrInvalid", err)
		}
		after, err := os.ReadFile(agentsPath)
		if err != nil || string(before) != string(after) {
			t.Fatalf("a failed agent mutation changed the owning file (%v)", err)
		}
	})
}

// TestRuntimeOwnerCloseAfterWriteBeginsFinishesPublication proves the owner
// side of the post-write barrier: an owner Close racing a mutation whose
// owning write has begun waits for the admitted call, the publication
// completes, and Close joins only after it.
func TestRuntimeOwnerCloseAfterWriteBeginsFinishesPublication(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, e := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)
		probe := installOwningSyncProbe(t, e.configPath)
		probe.mu.Lock()
		probe.park = true
		probe.mu.Unlock()
		// Teardown order: defers run LIFO, so this cleanup — registered
		// AFTER closeProjectionRuntime's — runs FIRST on every exit. It
		// releases the parked owning write (unblocking the mutation worker
		// so the owner Close below can join it even on a failed assertion)
		// and then restores the seam; both are idempotent.
		releasePark := sync.OnceFunc(probe.releaseProbe)
		defer func() {
			releasePark()
			probe.restore()
		}()
		ctx := context.Background()

		type result struct {
			mutation protocol.SettingsMutation
			err      error
		}
		done := make(chan result, 1)
		go func() {
			mutation, err := r.updateSettings(ctx, mutationSettings())
			done <- result{mutation, err}
		}()
		probe.awaitWriteStarted(t)

		closed := make(chan error, 1)
		go func() { closed <- r.Close(ctx) }()
		// Close cannot join while the mutation holds its admission; the
		// parked write guarantees the mutation is still in flight.
		select {
		case err := <-closed:
			t.Fatalf("Close overtook the admitted mutation: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
		releasePark()

		got := <-done
		if got.err != nil {
			t.Fatalf("updateSettings after write start = %v, want the admitted call to complete", got.err)
		}
		if got.mutation.ConfigurationRevision.Generation != "2" {
			t.Fatalf("mutation revision = %+v, want generation 2 published", got.mutation.ConfigurationRevision)
		}
		if err := <-closed; err != nil {
			t.Fatalf("Close: %v", err)
		}
		root := fileRoot(t, e.configPath)
		if !strings.Contains(compactJSON(t, root["sessions"]), `"archive_after_days":5`) {
			t.Fatalf("owning file = %s, want the written sessions member", root["sessions"])
		}
	})
}

// TestConfigurationMutateExternalEditorRaceLastWriterWins proves the
// last-writer-wins rule against a REAL concurrent external editor: the
// mutation is parked in its validation stage — after the raw inputs and the
// complete owning bytes are captured, before the final cancellation check
// and the atomic write — the main test goroutine performs a valid OS write
// to the same owning file in that window, and the mutation still wins with
// its complete earlier candidate bytes. A mid-window reread or merge would
// be discriminated: the external document carries a changed provider value
// and a new unowned top-level member absent from the captured root. No CAS,
// no mtime checks, no second writer.
func TestConfigurationMutateExternalEditorRaceLastWriterWins(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument)
	var parked int32 // the validator parks only the edit build
	arrive := make(chan struct{}, 1)
	gate := make(chan struct{})
	svc := h.service(context.Background(), servicePlugin("tools", &h.opens, func(json.RawMessage) error {
		if atomic.LoadInt32(&parked) == 1 {
			arrive <- struct{}{} // signal from the validation stage; no I/O here
			<-gate
		}
		return nil
	}))
	if _, err := svc.publish(context.Background()); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	sub, err := svc.obs.subscribe(4)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(sub.Close)

	atomic.StoreInt32(&parked, 1)
	type result struct {
		cfg configurationCapture
		err error
	}
	done := make(chan result, 1)
	go func() {
		cfg, err := svc.mutate(context.Background(), editSettings(mutationSettings()))
		done <- result{cfg, err}
	}()
	// Failure cleanup: release the parked validation and join the worker
	// before the fixture disposal, on every exit, with no sleep inference.
	// The join is conditional — on the success path the single result was
	// already consumed below.
	release := sync.OnceFunc(func() { close(gate) })
	received := false
	defer func() {
		release()
		if !received {
			<-done
		}
	}()
	<-arrive // the mutation captured its inputs and complete owning bytes

	// The real concurrent external editor: a valid whole-file OS write in
	// the open window, with a changed provider value and a new unowned
	// top-level member the captured root never saw.
	external := strings.Replace(mutationConfigDocument, `"name":"One"`, `"name":"Two"`, 1)
	external = strings.TrimSuffix(external, "}") + `,"external_note":"added mid-window"}`
	writeServiceFile(t, h.configPath, external)
	release()

	got := <-done
	received = true
	if got.err != nil {
		t.Fatalf("mutation after the concurrent external write: %v", got.err)
	}
	if got.cfg.snapshot.generation != 2 || svc.current() != got.cfg.snapshot {
		t.Fatalf("race mutation = generation %d, want 2 published", got.cfg.snapshot.generation)
	}
	nextConfigurationEvent(t, sub, "2")

	// The file: the complete earlier candidate bytes won — the captured old
	// provider value is preserved, the external member is gone, and the
	// requested settings are new — pinned against independent expected
	// values.
	root := fileRoot(t, h.configPath)
	if _, ok := root["external_note"]; ok {
		t.Fatalf("the external editor's mid-window member survived: %s", root["external_note"])
	}
	var providers struct {
		Prov struct {
			Models map[string]struct {
				Name string `json:"name"`
			} `json:"models"`
		} `json:"prov"`
	}
	if err := json.Unmarshal(root["providers"], &providers); err != nil {
		t.Fatalf("decode providers member: %v", err)
	}
	if got := providers.Prov.Models["m"].Name; got != "One" {
		t.Fatalf("captured provider value = %q, want the captured \"One\" over the external \"Two\"", got)
	}
	if got, want := compactJSON(t, root["sessions"]), compactJSON(t, mustMarshal(t, mutationSettings().Sessions)); got != want {
		t.Fatalf("sessions member = %s, want the requested %s", got, want)
	}
	var writtenPlugins map[string]json.RawMessage
	if err := json.Unmarshal(root["plugins"], &writtenPlugins); err != nil {
		t.Fatalf("decode plugins member: %v", err)
	}
	if got, want := compactJSON(t, writtenPlugins["tools"]), compactJSON(t, []byte(mutationSettings().Plugins["tools"])); got != want {
		t.Fatalf("plugins member = %s, want the requested %s", got, want)
	}

	// The published projection: the same winning candidate's settings.
	view := projectSettings(svc.current().sessions, svc.current().plugins)
	if view.Sessions != mutationSettings().Sessions || compactJSON(t, []byte(view.Plugins["tools"])) != compactJSON(t, []byte(mutationSettings().Plugins["tools"])) {
		t.Fatalf("published settings = %+v, want the requested shape", view)
	}
}

// nextConfigurationEvent reads one event and proves it is the configuration
// publication of the given generation.
func nextConfigurationEvent(t *testing.T, sub *Subscription, generation string) {
	t.Helper()
	event, ok := nextEvent(t, sub)
	if !ok || eventKind(t, event) != "configuration_changed" || eventGeneration(t, event) != generation {
		t.Fatalf("event = %s (ok=%v), want the generation %s configuration event", eventJSON(t, event), ok, generation)
	}
}

// drainMutationEventWithWarning consumes the events of one successful
// warning-changing edit: the generation's configuration event plus the one
// REQUIRED global warning event — its revision must be a real store state,
// nonzero and never beyond the store's current value — and then pins the
// stream silent.
func drainMutationEventWithWarning(t *testing.T, sub *Subscription, generation string, store *warningStore) {
	t.Helper()
	nextConfigurationEvent(t, sub, generation)
	event, ok := nextEvent(t, sub)
	if !ok || eventKind(t, event) != "warning_changed" {
		t.Fatalf("warning-changing edit event = %s (ok=%v), want the global warning event", eventJSON(t, event), ok)
	}
	requireRealWarningRevision(t, event, store)
}

// consumeOptionalWarningEvent consumes the global warning event when one
// immediately follows the just-read configuration event, requiring its
// revision to be a real store value. It asserts no silence — the batched
// sequence's later publications are still queued — so the caller's final
// silence check closes the sequence.
func consumeOptionalWarningEvent(t *testing.T, sub *Subscription, store *warningStore) {
	t.Helper()
	select {
	case event, ok := <-sub.Events():
		if !ok {
			t.Fatal("the subscription closed before the end of its batch")
		}
		if eventKind(t, event) != "warning_changed" {
			t.Fatalf("batched publication event = %s, want the optional global warning event", eventJSON(t, event))
		}
		requireRealWarningRevision(t, event, store)
	default:
	}
}

// requireRealWarningRevision proves one warning event carries a real store
// revision: nonzero and never beyond the store's current value — in a
// batched sequence each event names the revision of its own publication.
func requireRealWarningRevision(t *testing.T, event Event, store *warningStore) {
	t.Helper()
	body, err := event.AsWarningChangedEvent()
	if err != nil {
		t.Fatalf("warning event body: %v", err)
	}
	if scopeKind(t, body.Scope) != "runtime" {
		t.Fatalf("warning event scope kind = %q, want the runtime scope", scopeKind(t, body.Scope))
	}
	revision, err := strconv.ParseUint(body.WarningsRevision.Revision, 10, 64)
	if err != nil || revision == 0 {
		t.Fatalf("warning event revision = %q, want a real advanced value", body.WarningsRevision.Revision)
	}
	current, _ := store.snapshot()
	if revision > current {
		t.Fatalf("warning event revision = %d, beyond the store's current value %d", revision, current)
	}
}
