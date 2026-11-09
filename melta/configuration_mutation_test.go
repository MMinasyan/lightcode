package melta

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/extension"
	taskplugin "github.com/MMinasyan/lightcode/extensions/agents"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

// The mutation suite: one private configurationService.mutate is the
// single validated config writer for the settings PUT and the Agent-model
// PUT. Every row below pins one axis of the semantic matrix over real file
// bytes, generation, warning, and event oracles.

// mutationConfigDocument is the settings-mutation fixture document: an
// unowned providers member with an exact big-integer lexeme, an unowned
// permissions member, and an unowned unknown top-level member.
func mutationConfigDocument() string {
	return `{"providers":{"prov":` + nominalProvMember("https://prov.test/v1", "",
		`{"m":{"name":"One","context_window":9007199254740993}}`) +
		`},"permissions":{"rules":[{"permission":"file.write","target":"*","access":"allow"}]},"custom_flag":true}`
}

// mutationSettings is one complete settings shape the tests write: the
// sessions policy beside the top-level tools object and no plugin documents.
func mutationSettings() protocol.Settings {
	return protocol.Settings{
		Sessions:   protocol.SessionsSettings{AutoArchive: false, ArchiveAfterDays: 5, DeleteAfterArchiveDays: 2},
		Tools:      json.RawMessage(`{"command_timeout":60,"max_output_bytes":2048,"read_line_max_chars":3000,"read_max_lines":100}`),
		Extensions: protocol.ExtensionsSettings{},
	}
}

// acceptValidator is one plugin settings validator that accepts anything.
func acceptValidator(json.RawMessage) error { return nil }

// compactJSON renders one JSON value's canonical compacted form so raw
// member bytes can be compared across re-serializations.
func compactJSON(t *testing.T, data []byte) string {
	t.Helper()
	var buf bytes.Buffer
	{
		err := json.Compact(&buf, data)
		testassert.Require(t, err == nil, "compact %s: %v", data, err)
	}
	return buf.String()
}

// opaqueSettingsDocument is the settings round-trip fixture: one compiled
// plugin section carrying fields common Melta does not know plus one int64
// lexeme beyond JS number precision, one empty compiled section, the
// top-level tools section carrying the same kind of lexeme, and one unowned
// provider model carrying the same kind of lexeme.
func opaqueSettingsDocument() string {
	return `{"providers":{"prov":` + nominalProvMember("https://prov.test/v1", "",
		`{"m":{"name":"One","context_window":9007199254740993}}`) +
		`},"sessions":{"archive_after_days":3},` +
		`"tools":{"max_output_bytes":42,"exact":9007199254740993},` +
		`"extensions":{"alpha":{"enabled":true,"kept":1,"exact":9007199254740993},"tasks":{}}}`
}

// TestRuntimeSettingsReadSaveRoundTripPreservesOwnedPluginDocuments proves
// the representable-domain rule across a read-to-save round trip: every
// compiled plugin section returns as its exact owned document string —
// unknown fields and the int64 lexeme included — and writing the returned
// view back preserves every section while unowned numeric lexemes survive.
func TestRuntimeSettingsReadSaveRoundTripPreservesOwnedPluginDocuments(t *testing.T) {
	store := storage.NewMemory()
	var opens atomic.Int64
	r, e := openConfigurationRuntime(t, store, opaqueSettingsDocument(), configurationAgentsDocument,
		append(settingsPlugins(), servicePlugin("alpha", &opens, acceptValidator))...)
	defer closeProjectionRuntime(r)
	ctx := context.Background()
	sub, err := r.Subscribe(8)
	testassert.NoError(t, err)
	defer sub.Close()

	view, err := r.GetConfiguration(ctx, testCaller())
	testassert.NoError(t, err)
	read := view.Settings.Extensions
	want := map[string]string{
		"alpha":  `{"enabled":true,"kept":1,"exact":9007199254740993}`,
		"tasks":  `{}`,
		"agents": `{"definitions":` + compactJSON(t, []byte(configurationAgentsDocument)) + `,"enabled":true}`,
	}
	testassert.Require(t, len(read) == len(want), "settings plugins = %v, want exactly the owned sections %v", read, want)
	for id, wantDoc := range want {
		got, ok := read[id]
		testassert.Require(t, ok, "plugins.%s is absent from the read; want the owned document %s", id, wantDoc)
		{
			got := compactJSON(t, []byte(got))
			testassert.Require(t, got == wantDoc, "plugins.%s read document = %s, want the owned %s", id, got, wantDoc)
		}
	}

	// Read-to-save: the whole plugins member round-trips its documents
	// and the unowned provider lexeme survives the owning write.
	{
		_, err := updateSettingsOnly(r, ctx, view.Settings)
		testassert.NoError(t, err)
	}
	nextConfigurationEvent(t, sub, "2")
	root := fileRoot(t, e.configPath)
	var plugins map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(root["extensions"], &plugins))
	testassert.Require(t, len(plugins) == len(want), "written plugins = %v, want exactly %v", plugins, want)
	for id, wantDoc := range want {
		{
			got := compactJSON(t, plugins[id])
			testassert.Require(t, got == wantDoc, "written plugins.%s = %s, want the round-tripped %s", id, got, wantDoc)
		}
	}
	{
		got := string(root["providers"])
		testassert.Require(t, strings.Contains(got, "9007199254740993"), "written providers = %s, want the unowned int64 lexeme preserved", got)
	}
}

// assertNoEvent proves one subscription is silent without blocking.
func assertNoEvent(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case event, ok := <-sub.Events():
		testassert.Require(t, !ok, "published event %+v, want silence", event)
	default:
	}
}

// fileRoot decodes one on-disk document into its raw root object.
func fileRoot(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	testassert.Require(t, err == nil, "read %s: %v", path, err)
	var root map[string]json.RawMessage
	{
		err := json.Unmarshal(data, &root)
		testassert.Require(t, err == nil, "decode %s: %v", path, err)
	}
	return root
}

func definitionsRoot(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	root := fileRoot(t, path)
	var extensions, agents, definitions map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(root["extensions"], &extensions))
	testassert.NoError(t, json.Unmarshal(extensions["agents"], &agents))
	testassert.NoError(t, json.Unmarshal(agents["definitions"], &definitions))
	return definitions
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
	writeServiceFile(t, h.configPath, mutationConfigDocument())
	svc := h.service(context.Background(), servicePlugin("capowner", &h.opens, acceptValidator))
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	sub, err := svc.obs.subscribe(4)
	testassert.NoError(t, err)
	t.Cleanup(sub.Close)

	wantSettings := mutationSettings()
	candidate, err := svc.mutate(context.Background(), editSettings(wantSettings))
	testassert.NoError(t, err)
	if candidate.snapshot.generation != 2 || svc.current() != candidate.snapshot {
		t.Fatalf("mutate = generation %d (current published %v), want generation 2 published", candidate.snapshot.generation, svc.current() == candidate.snapshot)
	}
	testassert.Equal(t, projectSettings(candidate.snapshot.sessions, candidate.snapshot.tools, candidate.snapshot.jobs, candidate.snapshot.compaction, candidate.snapshot.extensions).Sessions, wantSettings.Sessions)

	// The owning file: the written members are the marshaled shape, the
	// unowned members and their exact numeric lexeme survive.
	root := fileRoot(t, h.configPath)
	{
		got, want := compactJSON(t, root["sessions"]), compactJSON(t, mustMarshal(t, wantSettings.Sessions))
		testassert.Equal(t, got, want)
	}
	var plugins map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(root["extensions"], &plugins))
	{
		got, want := compactJSON(t, root["tools"]), compactJSON(t, wantSettings.Tools)
		testassert.Equal(t, got, want)
	}
	for _, omitted := range []string{"jobs", "tasks"} {
		{
			_, ok := plugins[omitted]
			testassert.Require(t, !ok, "plugins.%s member = %s, want it removed by the omitted optional section", omitted, plugins[omitted])
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
	testassert.NoError(t, decoder.Decode(&providers))
	testassert.Equal(t, providers.Prov.Models["m"].ContextWindow.String(), "9007199254740993")
	{
		got, want := compactJSON(t, root["permissions"]), `{"rules":[{"permission":"file.write","target":"*","access":"allow"}]}`
		testassert.Equal(t, got, want)
	}
	{
		got := root["custom_flag"]
		testassert.Require(t, string(got) == "true", "unowned unknown member = %s, want true", got)
	}

	// One event for the successful edit, at the written generation.
	nextConfigurationEvent(t, sub, "2")
	assertNoEvent(t, sub)
}

// TestConfigurationMutateIdenticalBytesStillWriteAndPublish proves the
// no-byte-comparison rule: an edit whose complete bytes equal the current
// file — whether from an identical client request or because an external
// editor already wrote the requested bytes without a Melta reload — still
// performs the owning write and publishes the next generation with its
// event.
func TestConfigurationMutateIdenticalBytesStillWriteAndPublish(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument())
	svc := h.service(context.Background(), servicePlugin("capowner", &h.opens, acceptValidator))
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	sub, err := svc.obs.subscribe(8)
	testassert.NoError(t, err)
	t.Cleanup(sub.Close)
	probe := installOwningSyncProbe(t, h.configPath)
	defer probe.restore()

	settings := mutationSettings()
	{
		_, err := svc.mutate(context.Background(), editSettings(settings))
		testassert.NoError(t, err)
	}
	nextConfigurationEvent(t, sub, "2")
	testassert.Equal(t, probe.count(), 1)
	before := fileRoot(t, h.configPath)

	// Identical settings bytes: still a real owning write and publication.
	candidate, err := svc.mutate(context.Background(), editSettings(settings))
	testassert.NoError(t, err)
	testassert.Require(t, candidate.snapshot.generation == 3 && svc.current() == candidate.snapshot, "identical mutate = generation %d, want 3 published", candidate.snapshot.generation)
	nextConfigurationEvent(t, sub, "3")
	testassert.Equal(t, probe.count(), 2)
	{
		got := fileRoot(t, h.configPath)
		testassert.Require(t, compactJSON(t, got["sessions"]) == compactJSON(t, before["sessions"]), "the identical edit changed the written sessions member")
	}

	// An external editor writing the requested bytes without a Melta
	// reload must not let the next valid edit skip the write or publication.
	data, err := os.ReadFile(h.configPath)
	testassert.NoError(t, err)
	writeServiceFile(t, h.configPath, string(data))
	candidate, err = svc.mutate(context.Background(), editSettings(settings))
	testassert.NoError(t, err)
	testassert.Equal(t, candidate.snapshot.generation, 4)
	nextConfigurationEvent(t, sub, "4")
	testassert.Equal(t, probe.count(), 3)
}

func mustMarshal(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	testassert.NoError(t, err)
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
	// The fixture document carries the old alpha section the stale save must
	// remove wholesale.
	writeServiceFile(t, h.configPath, strings.TrimSuffix(mutationConfigDocument(), "}")+`,"tools":{"max_output_bytes":42},"plugins":{"legacy":{"value":9007199254740993}},"extensions":{"alpha":{"enabled":true,"kept":1}}}`)
	svc := h.service(context.Background(),
		servicePlugin("alpha", &h.opens, acceptValidator),
		servicePlugin("capowner", &h.opens, acceptValidator),
	)
	svc.attachWarnings(newWarningStore())
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	sub, err := svc.obs.subscribe(8)
	testassert.NoError(t, err)
	t.Cleanup(sub.Close)

	// The omission domain: the target carries the tools object and no plugin
	// documents; the old alpha section is removed wholesale even though it is
	// valid.
	stale := protocol.Settings{
		Sessions:   protocol.SessionsSettings{AutoArchive: true, ArchiveAfterDays: 7, DeleteAfterArchiveDays: 7},
		Tools:      json.RawMessage(`{"command_timeout":30,"max_output_bytes":1024,"read_line_max_chars":2000,"read_max_lines":50}`),
		Extensions: protocol.ExtensionsSettings{},
	}
	candidate, err := svc.mutate(context.Background(), editSettings(stale))
	testassert.NoError(t, err)
	published := projectSettings(candidate.snapshot.sessions, candidate.snapshot.tools, candidate.snapshot.jobs, candidate.snapshot.compaction, candidate.snapshot.extensions)
	testassert.Require(t, len(published.Extensions) == 0 && compactJSON(t, published.Tools) == compactJSON(t, stale.Tools), "published extensions = %v, tools = %s, want no extension documents and the written tools object", published.Extensions, published.Tools)
	root := fileRoot(t, h.configPath)
	var plugins map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(root["extensions"], &plugins))
	{
		got, want := compactJSON(t, root["tools"]), compactJSON(t, stale.Tools)
		testassert.Equal(t, got, want)
	}
	{
		_, ok := plugins["alpha"]
		testassert.Require(t, !ok, "plugins.alpha member = %s, want it removed by the omitted section", plugins["alpha"])
	}
	{
		got, want := compactJSON(t, root["sessions"]), compactJSON(t, mustMarshal(t, stale.Sessions))
		testassert.Equal(t, got, want)
	}
	// Unowned top-level members survive; the agents file is untouched.
	{
		got, want := compactJSON(t, root["permissions"]), `{"rules":[{"permission":"file.write","target":"*","access":"allow"}]}`
		testassert.Equal(t, got, want)
	}
	{
		got := root["custom_flag"]
		testassert.Require(t, string(got) == "true", "unowned unknown member = %s, want true", got)
	}
	testassert.Require(t, string(root["providers"]) != "", "unowned providers member missing")
	testassert.Equal(t, compactJSON(t, root["plugins"]), `{"legacy":{"value":9007199254740993}}`)
	{
		_, ok := root["alpha"]
		testassert.Require(t, !ok, "a removed plugin section leaked to the root")
	}
	nextConfigurationEvent(t, sub, "2")

	// The refusal domain: an unknown ID, invalid inner JSON, and a valid but
	// non-object document each fail the complete candidate with no file,
	// generation, warning, or event change.
	before, first, warnRev := metadataBaseline(t, svc, h.configPath)
	for _, row := range []struct {
		name    string
		plugins protocol.ExtensionsSettings
	}{
		{"invalid document", protocol.ExtensionsSettings{"alpha": `{not json`}},
	} {
		t.Run(row.name, func(t *testing.T) {
			_, err := svc.mutate(context.Background(), editSettings(protocol.Settings{Sessions: stale.Sessions, Extensions: row.plugins}))
			assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, ErrConfiguration)
		})
	}

	// An empty complete map is valid: it removes every owned section.
	candidate, err = svc.mutate(context.Background(), editSettings(protocol.Settings{Sessions: stale.Sessions, Extensions: protocol.ExtensionsSettings{}}))
	testassert.NoError(t, err)
	{
		got := projectSettings(candidate.snapshot.sessions, candidate.snapshot.tools, candidate.snapshot.jobs, candidate.snapshot.compaction, candidate.snapshot.extensions).Extensions
		testassert.Require(t, len(got) == 0, "published plugins after the empty map = %v, want none", got)
	}
	root = fileRoot(t, h.configPath)
	testassert.Equal(t, compactJSON(t, root["extensions"]), `{}`)
	nextConfigurationEvent(t, sub, "3")

	retained := protocol.ExtensionsSettings{"ghost": `{"enabled":true,"unknown":1}`, "alpha": `1`}
	candidate, err = svc.mutate(context.Background(), editSettings(protocol.Settings{Sessions: stale.Sessions, Extensions: retained}))
	testassert.NoError(t, err)
	got := projectSettings(candidate.snapshot.sessions, candidate.snapshot.tools, candidate.snapshot.jobs, candidate.snapshot.compaction, candidate.snapshot.extensions).Extensions
	testassert.Require(t, reflect.DeepEqual(got, retained), "inactive documents = %v, want %v", got, retained)
	nextConfigurationEvent(t, sub, "4")
}

// TestConfigurationMutateEverySuccessfulEditWritesAndPublishes pins the one
// shared successful-edit rule that replaced the old changed/no-edit branch:
// an apply that reports the owning file still performs the owning write and
// publishes the next generation with its event even when it changed no
// member.
func TestConfigurationMutateEverySuccessfulEditWritesAndPublishes(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument())
	svc := h.service(context.Background(), servicePlugin("capowner", &h.opens, acceptValidator))
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	sub, err := svc.obs.subscribe(4)
	testassert.NoError(t, err)
	t.Cleanup(sub.Close)
	probe := installOwningSyncProbe(t, h.configPath)
	defer probe.restore()

	noChange := configurationEdit{apply: func(rawRoots, configurationCapture) (editedFile, error) {
		return editMainConfig, nil
	}}
	candidate, err := svc.mutate(context.Background(), noChange)
	testassert.NoError(t, err)
	testassert.Require(t, candidate.snapshot.generation == 2 && svc.current() == candidate.snapshot, "no-change mutate = generation %d, want 2 published", candidate.snapshot.generation)
	nextConfigurationEvent(t, sub, "2")
	testassert.Equal(t, probe.count(), 1)
}

// --- owning file identity, mode, and no side effects ---

// TestConfigurationMutateWritesOwningFileOnly0600 proves the write contract:
// the main configuration is atomically rewritten at mode 0600 and no
// separate Agent file is created.
func TestConfigurationMutateWritesOwningFileOnly0600(t *testing.T) {
	h := newServiceHarness(t)
	svc := h.preparedService(context.Background(), fixtureAgentPlugin())
	agentsPath := filepath.Join(h.dataDir, "agents.json")
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}

	candidate, err := svc.mutate(context.Background(), svc.editAgentModel("primary", "prov/m"))
	testassert.NoError(t, err)
	testassert.Equal(t, candidate.snapshot.generation, 2)
	info, err := os.Stat(h.configPath)
	testassert.NoError(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("owning file mode = %o, want 600", info.Mode().Perm())
	}
	{
		_, err := os.Stat(agentsPath)
		testassert.Require(t, os.IsNotExist(err), "agents.json was created: %v", err)
	}

	// The settings PUT retains the Agent document carried by the caller.
	settings := projectSettings(candidate.snapshot.sessions, candidate.snapshot.tools, candidate.snapshot.jobs, candidate.snapshot.compaction, candidate.snapshot.extensions)
	settings.Sessions = mutationSettings().Sessions
	{
		_, err := svc.mutate(context.Background(), editSettings(settings))
		testassert.NoError(t, err)
	}
	info, err = os.Stat(h.configPath)
	testassert.NoError(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("owning file mode = %o, want 600", info.Mode().Perm())
	}
	root := definitionsRoot(t, h.configPath)
	testassert.Require(t, compactJSON(t, root["primary"]) == `{"model":"prov/m"}`, "settings lost the agent model: %s", root["primary"])
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
	writeServiceFile(t, h.configPath, mutationConfigDocument())
	writeAgentDefinitions(t, h.configPath, workerAgentsDocument)
	svc := h.preparedService(context.Background(), fixtureAgentPlugin())
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	sub, err := svc.obs.subscribe(16)
	testassert.NoError(t, err)
	t.Cleanup(sub.Close)
	agentsPath := h.configPath

	// Set: the override lands on the existing entry and resolves.
	candidate, err := svc.mutate(context.Background(), svc.editAgentModel("worker", "prov/m"))
	testassert.NoError(t, err)
	{
		found, model := hasDefinition(candidate.snapshot, "worker")
		testassert.Require(t, found && model == "prov/m", "worker definition = (%v, %q), want the new override", found, model)
	}
	nextConfigurationEvent(t, sub, "2")
	{
		root := definitionsRoot(t, agentsPath)
		testassert.Require(t, compactJSON(t, root["worker"]) == `{"model":"prov/m","system_prompt":"simple"}`, "worker entry = %s, want the model added beside the kept member", root["worker"])
	}

	// Identical value: still a real edit and publication.
	candidate, err = svc.mutate(context.Background(), svc.editAgentModel("worker", "prov/m"))
	testassert.Require(t, err == nil && candidate.snapshot.generation == 3, "identical model edit = (%v, generation %d), want generation 3", err, candidate.snapshot.generation)
	nextConfigurationEvent(t, sub, "3")

	// Clear: the override is removed, the entry's other member kept.
	candidate, err = svc.mutate(context.Background(), svc.editAgentModel("worker", ""))
	testassert.NoError(t, err)
	{
		found, model := hasDefinition(candidate.snapshot, "worker")
		testassert.Require(t, found && model == "", "worker definition = (%v, %q), want the cleared override", found, model)
	}
	nextConfigurationEvent(t, sub, "4")
	{
		root := definitionsRoot(t, agentsPath)
		testassert.Require(t, compactJSON(t, root["worker"]) == `{"system_prompt":"simple"}`, "worker entry = %s, want the model member removed", root["worker"])
	}

	// Clearing again with no user override present is still a successful
	// edit: it writes the owning file and publishes the next generation —
	// the no-override exception belongs to the retained public field reset
	// operator, not to this client. The rewritten bytes are identical
	// (deterministic remarshal of a no-op clear), so the write itself is
	// proven by the sync-seam probe.
	probe := installOwningSyncProbe(t, agentsPath)
	candidate, err = svc.mutate(context.Background(), svc.editAgentModel("worker", ""))
	testassert.Require(t, err == nil && candidate.snapshot.generation == 5, "absent-override clear = (%v, generation %d), want a successful edit at generation 5", err, candidate.snapshot.generation)
	nextConfigurationEvent(t, sub, "5")
	testassert.Equal(t, probe.count(), 1)
	probe.restore()
	{
		found, model := hasDefinition(candidate.snapshot, "worker")
		testassert.Require(t, found && model == "", "worker definition = (%v, %q), want the resolved inherited state", found, model)
	}

	// A builtin without a user entry is addressable: the edit creates only
	// its model member.
	candidate, err = svc.mutate(context.Background(), svc.editAgentModel("primary", "prov/m"))
	testassert.NoError(t, err)
	{
		found, model := hasDefinition(candidate.snapshot, "primary")
		testassert.Require(t, found && model == "prov/m", "primary definition = (%v, %q), want the new override", found, model)
	}
	nextConfigurationEvent(t, sub, "6")
	{
		root := definitionsRoot(t, agentsPath)
		testassert.Require(t, compactJSON(t, root["primary"]) == `{"model":"prov/m"}`, "primary entry = %s, want the bare model-only overlay", root["primary"])
	}
}

func TestConfigurationMutateAgentModelPreservesConcurrentSettings(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument())
	writeAgentDefinitions(t, h.configPath, workerAgentsDocument)
	var armed atomic.Bool
	entered, release := make(chan struct{}), make(chan struct{})
	candidateBarrier = func() {
		if armed.CompareAndSwap(true, false) {
			close(entered)
			<-release
		}
	}
	t.Cleanup(func() { candidateBarrier = nil })
	alpha := servicePlugin("alpha", &h.opens, acceptValidator)
	svc := h.preparedService(context.Background(), alpha, fixtureAgentPlugin(alpha))
	initial, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	settings := projectSettings(initial.snapshot.sessions, initial.snapshot.tools, initial.snapshot.jobs, initial.snapshot.compaction, initial.snapshot.extensions)
	settings.Extensions["agents"] = `{"enabled":true,"task":{"max_concurrent":7},"definitions":{"worker":{"system_prompt":"simple","description":"latest","tools":[]}}}`
	settings.Extensions["uninstalled"] = `{"enabled":true,"unknown":9007199254740993}`
	armed.Store(true)
	settingsDone := make(chan error, 1)
	go func() { _, err := svc.mutate(context.Background(), editSettings(settings)); settingsDone <- err }()
	<-entered
	modelStarted, modelDone := make(chan struct{}), make(chan error, 1)
	go func() {
		close(modelStarted)
		_, err := svc.mutate(context.Background(), svc.editAgentModel("worker", "prov/m"))
		modelDone <- err
	}()
	<-modelStarted
	select {
	case err := <-modelDone:
		t.Fatalf("model edit escaped the held settings build: %v", err)
	default:
	}
	close(release)
	testassert.NoError(t, <-settingsDone)
	testassert.NoError(t, <-modelDone)
	definitions := definitionsRoot(t, h.configPath)
	testassert.Equal(t, compactJSON(t, definitions["worker"]), `{"description":"latest","model":"prov/m","system_prompt":"simple","tools":[]}`)
	root := fileRoot(t, h.configPath)
	var extensions map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(root["extensions"], &extensions))
	testassert.Equal(t, compactJSON(t, extensions["uninstalled"]), `{"enabled":true,"unknown":9007199254740993}`)
	var section map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(extensions["agents"], &section))
	testassert.Equal(t, compactJSON(t, section["task"]), `{"max_concurrent":7}`)
	testassert.Equal(t, svc.current().generation, uint64(3))
}

// TestConfigurationMutateAgentModelClearAlwaysWritesAndPublishes proves the
// nearest sibling of the retained public absent-override reset: a known
// builtin with NO user model override and an empty-ref request is still a
// successful edit — it writes the owning agents file, publishes the next
// generation with its event, and repeating the same empty-ref bytes advances
// again instead of behaving as a reset.
func TestConfigurationMutateAgentModelClearAlwaysWritesAndPublishes(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument())
	writeAgentDefinitions(t, h.configPath, `{}`)
	svc := h.preparedService(context.Background(), fixtureAgentPlugin())
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	sub, err := svc.obs.subscribe(8)
	testassert.NoError(t, err)
	t.Cleanup(sub.Close)
	agentsPath := h.configPath
	probe := installOwningSyncProbe(t, agentsPath)
	defer probe.restore()

	for _, generation := range []string{"2", "3"} {
		candidate, err := svc.mutate(context.Background(), svc.editAgentModel("primary", ""))
		testassert.NoError(t, err)
		testassert.Equal(t, fmt.Sprint(candidate.snapshot.generation), generation)
		nextConfigurationEvent(t, sub, generation)
	}
	testassert.Equal(t, probe.count(), 2)
	{
		root := definitionsRoot(t, agentsPath)
		testassert.Require(t, compactJSON(t, root["primary"]) == `{}`, "primary entry = %s, want the retained WriteModel bare-entry semantics", root["primary"])
	}
}

// TestConfigurationMutateUnknownAgentTypeFailsWithoutBareEntry proves the
// nearest forbidden sibling of the retained create-on-missing loader path:
// an unknown or dropped/invalid type fails harness.ErrInvalid before any
// bare definition is created, and nothing is written, published, or
// notified.
func TestConfigurationMutateUnknownAgentTypeFailsWithoutBareEntry(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument())
	writeAgentDefinitions(t, h.configPath, `{}`)
	svc := h.preparedService(context.Background(), fixtureAgentPlugin())
	svc.attachWarnings(newWarningStore())
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	sub, err := svc.obs.subscribe(4)
	testassert.NoError(t, err)
	t.Cleanup(sub.Close)
	agentsPath := h.configPath
	before, published, warnRev := metadataBaseline(t, svc, h.configPath)
	agentsBefore := mustRead(t, agentsPath)

	for _, agentType := range []string{"ghost"} {
		_, err := svc.mutate(context.Background(), svc.editAgentModel(agentType, "prov/m"))
		assertMetadataRefused(t, svc, sub, h.configPath, before, published, warnRev, err, harness.ErrInvalid)
	}
	testassert.Require(t, string(mustRead(t, agentsPath)) == string(agentsBefore), "a failed agent mutate changed the owning agents file")
	for _, invalid := range []string{`{"broken":{"tools":["ghost_tool"]}}`, `{"badmodel":{"model":"noslash"}}`} {
		writeAgentDefinitions(t, h.configPath, invalid)
		invalidBytes := mustRead(t, h.configPath)
		_, err := svc.publish(context.Background())
		testassert.Require(t, errors.Is(err, ErrConfiguration), "invalid active definitions = %v", err)
		testassert.Require(t, svc.current() == published && string(mustRead(t, h.configPath)) == string(invalidBytes), "invalid reload changed publication or source")
		assertNoEvent(t, sub)
	}
}

// TestConfigurationMutateMalformedModelRefFailsWithoutWrite proves a
// nonempty ref is validated by the retained model.Parse before any edit.
func TestConfigurationMutateMalformedModelRefFailsWithoutWrite(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument())
	writeAgentDefinitions(t, h.configPath, `{"worker":{"model":"prov/m"}}`)
	svc := h.preparedService(context.Background(), fixtureAgentPlugin())
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	agentsPath := h.configPath
	before, err := os.ReadFile(agentsPath)
	testassert.NoError(t, err)
	{
		_, err := svc.mutate(context.Background(), svc.editAgentModel("worker", "noslash"))
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "malformed ref mutate = %v, want harness.ErrInvalid", err)
	}
	after, err := os.ReadFile(agentsPath)
	testassert.Require(t, err == nil && string(before) == string(after), "a malformed ref changed the owning file (%v)", err)
	testassert.Equal(t, svc.current().generation, 1)
}

func TestConfigurationMutateAgentModelRejectsInactiveOwner(t *testing.T) {
	for _, row := range []struct {
		name      string
		config    string
		installed bool
	}{
		{"disabled", `{"extensions":{"agents":{"enabled":false,"definitions":{"primary":{"model":"p/old"}}}}}`, true},
		{"uninstalled", `{"extensions":{"agents":{"enabled":true,"definitions":{"primary":{"model":"p/old"}}}}}`, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newServiceHarness(t)
			writeServiceFile(t, h.configPath, row.config)
			var plugins []extension.Native
			if row.installed {
				plugins = append(plugins, fixtureAgentPlugin())
			}
			svc := h.service(context.Background(), plugins...)
			before, err := svc.publish(context.Background())
			testassert.NoError(t, err)
			bytesBefore := mustRead(t, h.configPath)
			_, err = svc.mutate(context.Background(), svc.editAgentModel("primary", "p/new"))
			testassert.Require(t, errors.Is(err, harness.ErrInvalid) && svc.current() == before.snapshot && bytes.Equal(bytesBefore, mustRead(t, h.configPath)), "inactive edit = %v, want no write or publication", err)
		})
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
	// withSection returns the shared written settings plus one extra owned
	// plugin section, so a row can target one specific plugin settings owner.
	withSection := func(id, document string) protocol.Settings {
		settings := mutationSettings()
		settings.Extensions[id] = document
		return settings
	}
	// withTools replaces the written top-level tools object.
	withTools := func(document string) protocol.Settings {
		settings := mutationSettings()
		settings.Tools = json.RawMessage(document)
		return settings
	}
	rows := []struct {
		name       string
		validate   func(json.RawMessage) error
		schema     json.RawMessage
		external   string // the document an external editor writes before the edit
		settings   protocol.Settings
		wantSource string
	}{
		{
			name:       "the baseline settings owner rejects the written section",
			validate:   acceptValidator,
			external:   mutationConfigDocument(),
			settings:   withTools(`{"max_output_bytes":0}`),
			wantSource: "tools: max_output_bytes must be positive",
		},
		{
			name:       "an unrelated plugin validator rejects its own written section",
			validate:   acceptValidator,
			schema:     json.RawMessage("false"),
			external:   mutationConfigDocument(),
			settings:   withSection("capowner", `{"enabled":true,"kept":2048}`),
			wantSource: "jsonschema validation failed",
		},
		{
			name:       "externally malformed document",
			validate:   acceptValidator,
			external:   `{not json`,
			settings:   mutationSettings(),
			wantSource: "decode",
		},
		{
			name:       "externally trailing document",
			validate:   acceptValidator,
			external:   mutationConfigDocument() + `{}`,
			settings:   mutationSettings(),
			wantSource: "after top-level value",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := newServiceHarness(t)
			writeServiceFile(t, h.configPath, mutationConfigDocument())
			capowner := servicePlugin("capowner", &h.opens, row.validate)
			if row.schema != nil {
				capowner.Declaration.ConfigSchema = row.schema
			}
			svc := h.service(context.Background(), capowner, servicePlugin("alpha", &h.opens, acceptValidator))
			{
				_, err := svc.publish(context.Background())
				testassert.NoError(t, err)
			}
			sub, err := svc.obs.subscribe(4)
			testassert.NoError(t, err)
			t.Cleanup(sub.Close)
			svc.attachWarnings(newWarningStore())

			// The external editor's whole-file last-writer-wins state is what
			// the edit consumes as the latest raw layer: the refusal baseline
			// is the state that write left.
			writeServiceFile(t, h.configPath, row.external)
			before, published, warnRev := metadataBaseline(t, svc, h.configPath)
			candidate, err := svc.mutate(context.Background(), editSettings(row.settings))
			testassert.Require(t, candidate.snapshot == nil, "mutate = %+v, want a rejected candidate", candidate)
			testassert.Require(t, row.wantSource == "" || strings.Contains(err.Error(), row.wantSource), "error %v does not preserve %q", err, row.wantSource)
			assertMetadataRefused(t, svc, sub, h.configPath, before, published, warnRev, err, ErrConfiguration)
		})
	}
}

// TestConfigurationMutateAgentEditInvalidCandidateLeavesEverythingUnchanged
// pins the same candidate-failure rule through the Agent-model edit: an
// external editor's bad provider, session or enabled-extension member stays
// in the latest raw layer and rejects the candidate with the typed failure,
// with the owning file byte-unchanged from the post-bad-write baseline,
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
			name:       "invalid active extension",
			config:     `{"providers":{},"extensions":{"alpha":{"enabled":true,"invalid":true}}}`,
			wantSource: "jsonschema validation failed",
		},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			h := newServiceHarness(t)
			writeServiceFile(t, h.configPath, mutationConfigDocument())
			writeAgentDefinitions(t, h.configPath, workerAgentsDocument)
			capowner := servicePlugin("capowner", &h.opens, acceptValidator)
			alpha := servicePlugin("alpha", &h.opens, acceptValidator)
			alpha.Declaration.ConfigSchema = json.RawMessage(`{"type":"object","properties":{"enabled":{"type":"boolean"},"invalid":{"not":{"const":true}}},"required":["enabled"]}`)
			svc := h.preparedService(context.Background(), capowner, alpha, fixtureAgentPlugin(capowner, alpha))
			{
				_, err := svc.publish(context.Background())
				testassert.NoError(t, err)
			}
			sub, err := svc.obs.subscribe(4)
			testassert.NoError(t, err)
			t.Cleanup(sub.Close)
			svc.attachWarnings(newWarningStore())

			// The external bad state is the baseline: the oracles compare
			// against the latest bytes on disk, not the last valid file.
			writeServiceFile(t, h.configPath, row.config)
			writeAgentDefinitions(t, h.configPath, workerAgentsDocument)
			before, published, warnRev := metadataBaseline(t, svc, h.configPath)
			agentsPath := h.configPath
			agentsBefore := mustRead(t, agentsPath)
			candidate, err := svc.mutate(context.Background(), svc.editAgentModel("worker", "prov/m"))
			testassert.Require(t, candidate.snapshot == nil, "agent edit = %+v, want a rejected candidate", candidate)
			testassert.Require(t, strings.Contains(err.Error(), row.wantSource), "error %v does not preserve %q", err, row.wantSource)
			assertMetadataRefused(t, svc, sub, h.configPath, before, published, warnRev, err, ErrConfiguration)
			testassert.Require(t, string(mustRead(t, agentsPath)) == string(agentsBefore), "a failed candidate changed the owning agents file")

			// The inverse probe: with the bad member gone the same edit
			// succeeds, proving the file/generation/event oracles flip.
			writeServiceFile(t, h.configPath, mutationConfigDocument())
			writeAgentDefinitions(t, h.configPath, workerAgentsDocument)
			candidate, err = svc.mutate(context.Background(), svc.editAgentModel("worker", "prov/m"))
			testassert.Require(t, err == nil && candidate.snapshot.generation == 2 && svc.current() == candidate.snapshot, "inverse edit = (%v, generation %d), want a successful publication at 2", err, candidate.snapshot.generation)
			nextConfigurationEvent(t, sub, "2")
		})
	}
}

// TestConfigurationMutateMalformedPermissionsFallbackPublishes proves a
// hand-edited malformed Workspace permissions.json is captured and resolved
// to the built-in fallback without failing the mutation's publication.
func TestConfigurationMutateMalformedPermissionsFallbackPublishes(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument())
	svc := h.service(context.Background(), servicePlugin("capowner", &h.opens, acceptValidator))
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	dirID, err := config.WorkspacePermissionDirID("/ws")
	testassert.NoError(t, err)
	permissionPath := filepath.Join(h.home, ".lightcode", "projects", dirID, "permissions.json")
	writeServiceFile(t, permissionPath, `NOT JSON`)
	candidate, err := svc.mutate(context.Background(), editSettings(mutationSettings()))
	testassert.NoError(t, err)
	var builtin harness.PermissionPolicy
	{
		got := candidate.snapshot.permissionPolicy("/ws")
		testassert.Require(t, fmt.Sprint(got) == fmt.Sprint(builtin), "malformed permission policy = %#v, want the built-in fallback %#v", got, builtin)
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
		writeServiceFile(t, h.configPath, strings.TrimSuffix(mutationConfigDocument(), "}")+`,"extensions":{"capowner":{"enabled":true}}}`)
		ctx, cancel := context.WithCancel(context.Background())
		candidateBarrier = cancel
		t.Cleanup(func() { candidateBarrier = nil })
		svc := h.service(context.Background(), servicePlugin("capowner", &h.opens, acceptValidator))
		{
			_, err := svc.publish(context.Background())
			testassert.NoError(t, err)
		}
		sub, err := svc.obs.subscribe(4)
		testassert.NoError(t, err)
		t.Cleanup(sub.Close)
		data, err := os.ReadFile(h.configPath)
		testassert.NoError(t, err)
		settings := mutationSettings()
		settings.Extensions["capowner"] = `{"enabled":true}`
		candidate, err := svc.mutate(ctx, editSettings(settings))
		testassert.Require(t, candidate.snapshot == nil && errors.Is(err, context.Canceled), "mutate = (%v, %v), want the caller context error before the write", candidate, err)
		after, rerr := os.ReadFile(h.configPath)
		testassert.Require(t, rerr == nil && string(data) == string(after), "the canceled mutation changed the owning file (%v)", rerr)
		assertNoEvent(t, sub)
	})
	t.Run("owner canceled before waiting", func(t *testing.T) {
		owner, cancelOwner := context.WithCancel(context.Background())
		h := newServiceHarness(t)
		writeServiceFile(t, h.configPath, mutationConfigDocument())
		svc := h.service(owner, servicePlugin("capowner", &h.opens, acceptValidator))
		{
			_, err := svc.publish(context.Background())
			testassert.NoError(t, err)
		}
		sub, err := svc.obs.subscribe(4)
		testassert.NoError(t, err)
		t.Cleanup(sub.Close)
		svc.attachWarnings(newWarningStore())
		before, first, warnRev := metadataBaseline(t, svc, h.configPath)
		cancelOwner()
		candidate, err := svc.mutate(context.Background(), editSettings(mutationSettings()))
		testassert.Require(t, candidate.snapshot == nil, "mutate = %+v, want no candidate", candidate)
		assertMetadataRefused(t, svc, sub, h.configPath, before, first, warnRev, err, ErrClosed)
	})
}

// TestConfigurationMutateCompletesPublicationAfterWriteStarts proves the
// post-write barrier: once the owning write has begun, a caller
// cancellation no longer stops anything — the admitted call completes the
// write, the ready snapshot publication, and its one event, with no
// half-published generation.
func TestConfigurationMutateCompletesPublicationAfterWriteStarts(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument())
	svc := h.service(context.Background(), servicePlugin("capowner", &h.opens, acceptValidator))
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	sub, err := svc.obs.subscribe(4)
	testassert.NoError(t, err)
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
	testassert.Require(t, got.err == nil && got.cfg.snapshot.generation == 2 && svc.current() == got.cfg.snapshot, "mutate after write start = %v, want the admitted call to complete publication; post-cancel publication = generation %d, want 2 published", got.err, got.cfg.snapshot.generation)
	nextConfigurationEvent(t, sub, "2")
	root := fileRoot(t, h.configPath)
	testassert.Require(t, strings.Contains(compactJSON(t, root["sessions"]), `"archive_after_days":5`), "owning file = %s, want the written sessions member", root["sessions"])
}

// TestConfigurationMutateWriteFailureLeavesEverythingUnchanged proves a
// pre-rename write failure publishes nothing: the error is classified, the
// file is unchanged, and no generation, warning refresh, or event follows.
func TestConfigurationMutateWriteFailureLeavesEverythingUnchanged(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, mutationConfigDocument())
	svc := h.service(context.Background(), servicePlugin("capowner", &h.opens, acceptValidator))
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	sub, err := svc.obs.subscribe(4)
	testassert.NoError(t, err)
	t.Cleanup(sub.Close)
	injected := errors.New("injected sync failure")
	probe := installOwningSyncProbe(t, h.configPath)
	probe.mu.Lock()
	probe.fail = injected
	probe.mu.Unlock()
	defer probe.restore()
	data, err := os.ReadFile(h.configPath)
	testassert.NoError(t, err)
	candidate, err := svc.mutate(context.Background(), editSettings(mutationSettings()))
	testassert.Require(t, candidate.snapshot == nil && errors.Is(err, ErrConfiguration) && errors.Is(err, injected), "mutate = (%v, %v), want the injected write failure preserved", candidate, err)
	after, rerr := os.ReadFile(h.configPath)
	testassert.Require(t, rerr == nil && string(data) == string(after), "a failed write changed the owning file (%v)", rerr)
	testassert.Equal(t, svc.current().generation, 1)
	assertNoEvent(t, sub)
}

// --- one shared writer serializes mutations and Reload ---

// TestConfigurationMutateSerializesWithReload proves one build mutex over
// both entry points: a Reload waits for the in-flight mutation and completes
// with the next generation, and the events keep publication order.
func TestConfigurationMutateSerializesWithReload(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, strings.TrimSuffix(mutationConfigDocument(), "}")+`,"extensions":{"capowner":{"enabled":true}}}`)
	var builds atomic.Int64
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	candidateBarrier = func() {
		if builds.Add(1) == 2 { // the second build — the mutation — parks
			entered <- struct{}{}
			<-gate
		}
	}
	t.Cleanup(func() { candidateBarrier = nil })
	svc := h.service(context.Background(), servicePlugin("capowner", &h.opens, acceptValidator))
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	sub, err := svc.obs.subscribe(8)
	testassert.NoError(t, err)
	t.Cleanup(sub.Close)

	type result struct {
		revision string
		err      error
	}
	mutation := make(chan result, 1)
	go func() {
		settings := mutationSettings()
		settings.Extensions["capowner"] = `{"enabled":true}`
		cfg, err := svc.mutate(context.Background(), editSettings(settings))
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
	{
		got := <-mutation
		testassert.Require(t, got.err == nil && got.revision == "2", "mutation = %+v, want generation 2", got)
	}
	{
		got := <-reload
		testassert.Require(t, got.err == nil && got.revision == "3", "reload = %+v, want generation 3 after the serialized mutation", got)
	}
	nextConfigurationEvent(t, sub, "2")
	nextConfigurationEvent(t, sub, "3")
}

// --- Melta clients ---

// TestRuntimeSettingsMutationLastWriterWins proves the settings client's
// whole-shape rule at the Melta boundary: two clients reading revision N
// both succeed with sequential next generations — the second, older
// whole-section document overwrites the first write and a fresh read
// observes the winner.
func TestRuntimeSettingsMutationLastWriterWins(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, _ := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		sub, err := r.Subscribe(8)
		testassert.NoError(t, err)
		defer sub.Close()

		view, err := r.GetConfiguration(ctx, testCaller())
		testassert.NoError(t, err)
		testassert.Require(t, view.ConfigurationRevision.Generation == "1", "initial revision = %+v, want generation 1", view.ConfigurationRevision)
		stale := view.Settings // value copy: client 2's older whole-section document

		first := view.Settings
		first.Sessions.ArchiveAfterDays = 9
		mutation, err := updateSettingsOnly(r, ctx, first)
		testassert.NoError(t, err)
		testassert.Require(t, mutation.ConfigurationRevision.Generation == "2" && mutation.Result.Sessions.ArchiveAfterDays == 9, "client 1 mutation = %+v, want generation 2 with the written value", mutation)

		// Client 2 submits its older whole-section document:
		// last-writer-wins, not a conflict.
		mutation, err = updateSettingsOnly(r, ctx, stale)
		testassert.NoError(t, err)
		testassert.Require(t, mutation.ConfigurationRevision.Generation == "3" && mutation.Result.Sessions.ArchiveAfterDays == 3, "client 2 mutation = %+v, want generation 3 with the stale value", mutation)
		fresh, err := r.GetConfiguration(ctx, testCaller())
		testassert.NoError(t, err)
		testassert.Require(t, fresh.ConfigurationRevision.Generation == "3" && fresh.Settings.Sessions.ArchiveAfterDays == 3, "fresh settings = rev %+v sessions %+v, want the stale writer's value at generation 3", fresh.ConfigurationRevision, fresh.Settings.Sessions)
		for _, want := range []string{"2", "3"} {
			nextConfigurationEvent(t, sub, want)
		}
	})
}

// TestRuntimeSettingsMutationOwnershipAndResult pins the Melta client's
// result projection: the mutation result projects the returned candidate
// with the owner's instance identity, and the returned settings are owned
// against a serialized baseline.
func TestRuntimeSettingsMutationOwnershipAndResult(t *testing.T) {
	eachPrepStoreOnce(t, func(t *testing.T, store harness.Storage) {
		r, _ := openConfigurationRuntime(t, store, configurationProvidersDocument, configurationAgentsDocument, settingsPlugins()...)
		defer closeProjectionRuntime(r)
		ctx := context.Background()
		mutation, err := updateSettingsOnly(r, ctx, mutationSettings())
		testassert.NoError(t, err)
		testassert.Require(t, mutation.ConfigurationRevision.Generation == "2" && mutation.ConfigurationRevision.InstanceId == r.instance, "mutation revision = %+v, want generation 2 with the owner instance", mutation.ConfigurationRevision)
		mutation.Result.Sessions.ArchiveAfterDays = 999
		mutation.Result.Tools = json.RawMessage(`{"mutated":true}`)
		fresh, err := r.GetConfiguration(ctx, testCaller())
		testassert.NoError(t, err)
		testassert.Require(t, fresh.Settings.Sessions.ArchiveAfterDays == 5 && compactJSON(t, fresh.Settings.Tools) == compactJSON(t, mutationSettings().Tools), "a mutation of the returned result reached the published settings: %+v", fresh.Settings)
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
		writeServiceFile(t, e.configPath, `{"providers":{"prov":`+nominalProvMember(e.server.URL, "",
			`{"m":{"name":"M","context_window":262144,"max_output_tokens":4096},"wide":{"name":"W","context_window":262144,"max_output_tokens":4096}}`)+`}}`)
		writeAgentDefinitions(t, e.configPath, `{"solo":{"model":"prov/m","system_prompt":"simple"},"worker":{"model":"prov/m","system_prompt":"simple"}}`)
		e.fixtureStore(store)
		r, err := e.open(context.Background())
		testassert.NoError(t, err)
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
		testassert.NoError(t, err)
		testassert.Require(t, mutation.ConfigurationRevision.Generation == "2", "mutation = %+v, want the new model at generation 2", mutation)

		// Session B is idle: its next admission commits the new model at the
		// new generation, and the active Operation has not re-captured. The
		// held HTTP request parks every execution, so B's Operation runs up
		// to its model request and both settle once the gate opens.
		submitThroughRuntime(t, r, b.Identity.SessionID, "op-b1", "b turn")
		testassert.Equal(t, committedAdmission(t, r, b.Identity.SessionID, "op-b1"), "worker=prov/wide@2")
		testassert.Equal(t, committedAdmission(t, r, a.Identity.SessionID, "op-active"), "worker=prov/m@1")

		// The active captures settle with their original selections; A's
		// next admission then commits the new model too.
		close(gate)
		awaitIdleSession(t, r, a.Identity.SessionID)
		awaitIdleSession(t, r, b.Identity.SessionID)
		submitConvergedThroughRuntime(t, r, a.Identity.SessionID, "op-a2", "a turn", harness.OperationSuccess)
		testassert.Equal(t, committedAdmission(t, r, a.Identity.SessionID, "op-a2"), "worker=prov/wide@2")

		// The forbidden siblings: an unknown type and a malformed ref fail
		// invalid and never touch the owning file.
		agentsPath := e.configPath
		before, err := os.ReadFile(agentsPath)
		testassert.NoError(t, err)
		{
			_, err := r.setAgentTypeModel(ctx, "ghost", "prov/m")
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "unknown type = %v, want harness.ErrInvalid", err)
		}
		{
			_, err := r.setAgentTypeModel(ctx, "worker", "noslash")
			testassert.Require(t, errors.Is(err, harness.ErrInvalid), "malformed ref = %v, want harness.ErrInvalid", err)
		}
		after, err := os.ReadFile(agentsPath)
		testassert.Require(t, err == nil && string(before) == string(after), "a failed agent mutation changed the owning file (%v)", err)
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
			mutation, err := updateSettingsOnly(r, ctx, mutationSettings())
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
		testassert.Require(t, got.err == nil && got.mutation.ConfigurationRevision.Generation == "2", "updateSettings after write start = %v, want the admitted call to complete; mutation revision = %+v, want generation 2 published", got.err, got.mutation.ConfigurationRevision)
		testassert.NoError(t, <-closed)
		root := fileRoot(t, e.configPath)
		testassert.Require(t, strings.Contains(compactJSON(t, root["sessions"]), `"archive_after_days":5`), "owning file = %s, want the written sessions member", root["sessions"])
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
	writeServiceFile(t, h.configPath, mutationConfigDocument())
	var parked int32 // the validator parks only the edit build
	arrive := make(chan struct{}, 1)
	gate := make(chan struct{})
	candidateBarrier = func() {
		if atomic.LoadInt32(&parked) == 1 {
			arrive <- struct{}{}
			<-gate
		}
	}
	t.Cleanup(func() { candidateBarrier = nil })
	svc := h.service(context.Background(), servicePlugin("capowner", &h.opens, acceptValidator))
	{
		_, err := svc.publish(context.Background())
		testassert.NoError(t, err)
	}
	sub, err := svc.obs.subscribe(4)
	testassert.NoError(t, err)
	t.Cleanup(sub.Close)

	atomic.StoreInt32(&parked, 1)
	type result struct {
		cfg configurationCapture
		err error
	}
	done := make(chan result, 1)
	go func() {
		settings := mutationSettings()
		settings.Extensions["capowner"] = `{"enabled":true}`
		cfg, err := svc.mutate(context.Background(), editSettings(settings))
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
	external := strings.Replace(mutationConfigDocument(), `"name":"One"`, `"name":"Two"`, 1)
	external = strings.TrimSuffix(external, "}") + `,"external_note":"added mid-window"}`
	writeServiceFile(t, h.configPath, external)
	release()

	got := <-done
	received = true
	testassert.Require(t, got.err == nil && got.cfg.snapshot.generation == 2 && svc.current() == got.cfg.snapshot, "mutation after the concurrent external write: %v; race mutation = generation %d, want 2 published", got.err, got.cfg.snapshot.generation)
	nextConfigurationEvent(t, sub, "2")

	// The file: the complete earlier candidate bytes won — the captured old
	// provider value is preserved, the external member is gone, and the
	// requested settings are new — pinned against independent expected
	// values.
	root := fileRoot(t, h.configPath)
	{
		_, ok := root["external_note"]
		testassert.Require(t, !ok, "the external editor's mid-window member survived: %s", root["external_note"])
	}
	var providers struct {
		Prov struct {
			Models map[string]struct {
				Name string `json:"name"`
			} `json:"models"`
		} `json:"prov"`
	}
	testassert.NoError(t, json.Unmarshal(root["providers"], &providers))
	testassert.Equal(t, providers.Prov.Models["m"].Name, "One")
	{
		got, want := compactJSON(t, root["sessions"]), compactJSON(t, mustMarshal(t, mutationSettings().Sessions))
		testassert.Equal(t, got, want)
	}
	{
		got, want := compactJSON(t, root["tools"]), compactJSON(t, mutationSettings().Tools)
		testassert.Equal(t, got, want)
	}

	// The published projection: the same winning candidate's settings.
	view := projectSettings(svc.current().sessions, svc.current().tools, svc.current().jobs, svc.current().compaction, svc.current().extensions)
	testassert.Require(t, view.Sessions == mutationSettings().Sessions && compactJSON(t, view.Tools) == compactJSON(t, mutationSettings().Tools), "published settings = %+v, want the requested shape", view)
}

// nextConfigurationEvent reads one event and proves it is the configuration
// publication of the given generation.
func nextConfigurationEvent(t *testing.T, sub *Subscription, generation string) {
	t.Helper()
	for {
		event, ok := nextEvent(t, sub)
		if !ok {
			t.Fatalf("stream closed, want the generation %s configuration event", generation)
		}
		// Candidate and retired resource sets publish their own scope events
		// ahead of the configuration event.
		if strings.HasPrefix(eventKind(t, event), "scope_") {
			continue
		}
		if eventKind(t, event) != "configuration_changed" || eventGeneration(t, event) != generation {
			t.Fatalf("event = %s (ok=%v), want the generation %s configuration event", eventJSON(t, event), ok, generation)
		}
		return
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
		testassert.Require(t, ok, "the subscription closed before the end of its batch")
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
	testassert.NoError(t, err)
	if scopeKind(t, body.Scope) != "runtime" {
		t.Fatalf("warning event scope kind = %q, want the runtime scope", scopeKind(t, body.Scope))
	}
	revision, err := strconv.ParseUint(body.WarningsRevision.Revision, 10, 64)
	testassert.Require(t, err == nil && revision != 0, "warning event revision = %q, want a real advanced value", body.WarningsRevision.Revision)
	current, _ := store.snapshot()
	testassert.Require(t, revision <= current, "warning event revision = %d, beyond the store's current value %d", revision, current)
}

// TestConfigurationSettingsRejectNonIntegerTaskLexemes proves the settings and
// publication path: null/default/integer task forms publish, while JSON Schema
// integer lexemes that Go's decoder cannot represent as int are rejected at
// candidate Open before any owning byte or revision changes.
func TestConfigurationSettingsRejectNonIntegerTaskLexemes(t *testing.T) {
	for _, row := range []struct {
		name     string
		task     string
		accepted bool
	}{
		{"null task", `null`, true},
		{"absent members", `{}`, true},
		{"null members", `{"max_concurrent":null,"max_output_bytes":null}`, true},
		{"integer members", `{"max_concurrent":7,"max_output_bytes":2048}`, true},
		{"decimal lexeme", `{"max_concurrent":1.0}`, false},
		{"exponent lexeme", `{"max_concurrent":1e0}`, false},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newServiceHarness(t)
			writeServiceFile(t, h.configPath, mutationConfigDocument())
			svc := h.service(context.Background(), taskplugin.Native(taskplugin.Options{}))
			// The isolated service harness normally skips resource opening; this
			// row owns the real publication boundary, so attach the owner graph
			// and data root the Melta wires in production.
			graph, err := newNativeComposition([]extension.Native{taskplugin.Native(taskplugin.Options{})})
			testassert.NoError(t, err)
			svc.composition = graph
			svc.dataDir = t.TempDir()
			initial, err := svc.publish(context.Background())
			testassert.NoError(t, err)
			before, err := os.ReadFile(h.configPath)
			testassert.NoError(t, err)
			generation := svc.current().generation
			settings := projectSettings(initial.snapshot.sessions, initial.snapshot.tools, initial.snapshot.jobs, initial.snapshot.compaction, initial.snapshot.extensions)
			settings.Extensions["agents"] = `{"enabled":true,"task":` + row.task + `}`
			_, err = svc.mutate(context.Background(), editSettings(settings))
			if row.accepted {
				testassert.NoError(t, err)
				testassert.Require(t, svc.current().generation != generation, "accepted settings did not advance the generation")
				return
			}
			testassert.Require(t, errors.Is(err, ErrConfiguration), "settings write = %v, want the configuration refusal", err)
			after, err := os.ReadFile(h.configPath)
			testassert.NoError(t, err)
			testassert.Require(t, string(after) == string(before), "rejected settings changed the owning bytes")
			testassert.Equal(t, svc.current().generation, generation)
		})
	}
}

// TestConfigurationMutateExtensionEditRequiresPreparedProvider proves the
// nearest sibling of an inactive owner: an externally enabled section whose
// provider was not prepared in the published revision refuses the edit before
// any write instead of opening a provider on demand.
func TestConfigurationMutateExtensionEditRequiresPreparedProvider(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"extensions":{"agents":{"enabled":false}}}`)
	svc := h.preparedService(context.Background(), fixtureAgentPlugin())
	before, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	testassert.Require(t, before.snapshot.resources.runtime.capabilities["agents.configuration"] == nil, "disabled Agents prepared a configuration provider")

	// An external writer enables the section without a reload; the latest raw
	// state says enabled but the published resource set has no provider.
	writeServiceFile(t, h.configPath, `{"extensions":{"agents":{"enabled":true,"definitions":{"worker":{"system_prompt":"simple"}}}}}`)
	bytesBefore := mustRead(t, h.configPath)
	_, err = svc.mutate(context.Background(), svc.editAgentModel("worker", "prov/m"))
	testassert.Require(t, err != nil && svc.current() == before.snapshot && bytes.Equal(bytesBefore, mustRead(t, h.configPath)), "unprepared provider edit = %v, want no write or publication", err)
}

// TestConfigurationMutateExtensionEditRejectsMalformedArguments proves the
// declared edit's argument document is caller input: a non-JSON or
// wrong-shaped document fails invalid before any write.
func TestConfigurationMutateExtensionEditRejectsMalformedArguments(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, `{"extensions":{"agents":{"enabled":true,"definitions":{"worker":{"system_prompt":"simple"}}}}}`)
	svc := h.preparedService(context.Background(), fixtureAgentPlugin())
	before, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	bytesBefore := mustRead(t, h.configPath)
	for _, arguments := range []string{`{`, `{"definition_id":"worker"}`, `[]`} {
		_, err := svc.mutate(context.Background(), svc.editExtension(context.Background(), "agents", "set_model", json.RawMessage(arguments)))
		testassert.Require(t, errors.Is(err, harness.ErrInvalid), "arguments %s = %v, want harness.ErrInvalid", arguments, err)
		testassert.Require(t, svc.current() == before.snapshot && bytes.Equal(bytesBefore, mustRead(t, h.configPath)), "arguments %s changed publication or source", arguments)
	}
}
