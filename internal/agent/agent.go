package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/cmdoutput"
	"github.com/MMinasyan/lightcode/internal/compact"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/editpreview"
	"github.com/MMinasyan/lightcode/internal/loop"
	"github.com/MMinasyan/lightcode/internal/lsp"
	"github.com/MMinasyan/lightcode/internal/memory"
	"github.com/MMinasyan/lightcode/internal/message"
	"github.com/MMinasyan/lightcode/internal/pathutil"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/process"
	"github.com/MMinasyan/lightcode/internal/project"
	"github.com/MMinasyan/lightcode/internal/prompt"
	"github.com/MMinasyan/lightcode/internal/provider"
	"github.com/MMinasyan/lightcode/internal/safefs"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/subagent"
	"github.com/MMinasyan/lightcode/internal/tool"
)

const tokensFileName = "tokens.json"

// Config carries constructor parameters for New.
type Config struct {
	Cfg         *config.Config
	ProjectRoot string
	Home        string
}

// Agent is the shared core used by all adapters (Wails, HTTP, ACP).
type Agent struct {
	cfg      *config.Config
	catalog  *catalog.Catalog
	store    *snapshot.Store
	projects *project.Resolver
	lp       *loop.Loop
	gate     *permission.Gate
	registry *tool.Registry

	projectRoot string
	home        string

	loopEvents chan loop.Event
	onEvent    func(Event)

	mu         sync.Mutex
	busy       bool
	turnCancel context.CancelFunc
	turnCtx    context.Context

	currentRef        catalog.ModelRef
	contextWindowSize int

	tokensMu        sync.Mutex
	tokens          map[string]*TokenEntry
	lastContextUsed int

	assembler              *prompt.Assembler
	pendingPromptWarnings  []prompt.Warning
	pendingCatalogWarnings []prompt.Warning

	memoryStore *memory.Store

	lspManager     *lsp.Manager
	lspDiagnostics *tool.LSPDiagnostics

	subagentLoader *subagent.Loader
	taggedEvents   chan TaggedLoopEvent
	taskToolInst   *taskTool
	seenSessions   map[string]bool

	procMgr *process.Manager

	fileTracker *tool.FileTracker

	loopFlush  chan chan struct{}
	signalWake chan struct{}

	warningsMu      sync.Mutex
	warningGroups   map[string][]PromptWarning
	warningSnapshot []PromptWarning
}

// New constructs an Agent from the given config. It creates the
// provider client, tool registry, permission gate, snapshot store,
// and loop. Call Init after setting up the event handler.
func New(c Config) (*Agent, error) {
	modelCatalog, catalogWarnings, err := catalog.NewLoader(c.Home, nil).Load()
	if err != nil {
		return nil, fmt.Errorf("load model catalog: %w", err)
	}

	defaultModelStr := c.Cfg.DefaultModel
	if defaultModelStr == "" {
		if ref, ok := autoSelectModel(modelCatalog); ok {
			defaultModelStr = ref.String()
		}
	}

	var defaultRef catalog.ModelRef
	var client *provider.Client
	var defaultModel *catalog.Model

	if defaultModelStr != "" {
		defaultRef, err = catalog.ParseModelRef(defaultModelStr)
		if err != nil {
			return nil, fmt.Errorf("default_model: %w", err)
		}
		client, defaultModel, err = newProviderClient(modelCatalog, defaultRef)
		if err != nil {
			return nil, err
		}
	}

	resolver, err := project.NewResolver(c.Home, c.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("init project resolver: %w", err)
	}
	store, err := snapshot.NewForSessionsRoot("", resolver.Root(), "")
	if err != nil {
		return nil, fmt.Errorf("init snapshot store: %w", err)
	}

	events := make(chan loop.Event, 256)

	var contextWindowSize int
	if defaultModel != nil {
		contextWindowSize = defaultModel.ContextWindow
	}

	a := &Agent{
		cfg:               c.Cfg,
		catalog:           modelCatalog,
		store:             store,
		projects:          resolver,
		projectRoot:       c.ProjectRoot,
		home:              c.Home,
		loopEvents:        events,
		currentRef:        defaultRef,
		contextWindowSize: contextWindowSize,
		loopFlush:         make(chan chan struct{}, 1),
		signalWake:        make(chan struct{}, 1),
		warningGroups:     make(map[string][]PromptWarning),
	}

	gate := permission.NewGate(func(ctx context.Context, req permission.Request) {
		ev := loop.Event{
			Kind:     loop.PermissionRequest,
			ToolName: req.ToolName,
			PermID:   req.ID,
			PermArg:  req.Arg,
			Metadata: map[string]any{
				"resolved_arg":  req.ResolvedArg,
				"can_allow_all": req.CanAllowAll,
				"batch_index":   req.BatchIndex,
				"batch_total":   req.BatchTotal,
				"batch_files":   req.BatchFiles,
			},
		}
		select {
		case events <- ev:
		case <-ctx.Done():
		}
	})
	a.gate = gate

	checkFunc := tool.CheckFunc(func(toolName, arg string) permission.Decision {
		var local permission.Rules
		if proj, err := resolver.Current(); err == nil && proj != nil {
			local, _ = permission.LoadLocal(resolver.Root(), proj.ID)
		}
		return permission.Check(local, c.Cfg.Permissions, toolName, arg, c.ProjectRoot, c.Home, c.ProjectRoot)
	})

	askFunc := tool.AskFunc(func(ctx context.Context, req permission.Request) permission.ResponseAction {
		a.mu.Lock()
		turnCtx := a.turnCtx
		a.mu.Unlock()
		if turnCtx == nil {
			return permission.ResponseDeny
		}
		return gate.AskRequest(turnCtx, req)
	})

	askActionFunc := tool.AskActionFunc(func(ctx context.Context, req permission.Request) permission.ResponseAction {
		return gate.AskRequest(ctx, req)
	})

	fileTracker := tool.NewFileTracker()
	a.fileTracker = fileTracker

	registry := tool.NewRegistry()
	registry.Register(tool.WrapWithPermission(tool.NewReadFile(c.Cfg.Tools, fileTracker), checkFunc, askFunc))
	registry.Register(tool.WrapWithPermission(tool.NewWriteFileWithSnapshot(store, fileTracker, c.Cfg.Tools), checkFunc, askFunc))
	registry.Register(tool.WrapWithPermission(tool.NewEditFileWithSnapshot(store, fileTracker, c.Cfg.Tools), checkFunc, askFunc))
	registry.Register(tool.WrapWithPermission(tool.ExecutePending{}, checkFunc, askFunc))

	procMgr := process.NewManager(c.Cfg.Tools.MaxBackgroundProcesses, cmdoutput.Options{
		HomeDir:      c.Home,
		SpillPrefix:  "proc_output_",
		MaxBytes:     c.Cfg.Tools.MaxOutputBytes,
		MaxLineChars: c.Cfg.Tools.ReadLineMaxChars,
	})
	a.procMgr = procMgr

	procMgr.SetSessionProvider(func() string {
		if a.store == nil {
			return ""
		}
		return a.store.SessionID()
	})
	procMgr.SetExitHandler(func(event process.ExitEvent) {
		if a.lp != nil {
			a.mu.Lock()
			defer a.mu.Unlock()
			if event.SessionID != "" && a.store.SessionID() != event.SessionID {
				return
			}
			output := ""
			if event.FormatOutput != nil {
				output = event.FormatOutput()
			}
			if output == "" {
				output = "(No output)"
			}
			reason := event.Reason
			if reason == "" {
				reason = process.ExitReasonCompleted
			}
			payload := backgroundTerminalPayload(event, output)
			a.lp.AddPendingSignal(loop.PendingSignal{
				Payload: payload,
				Wake:    true,
				Persist: true,
				BackgroundProcess: &loop.BackgroundProcessDisplay{
					ID:       event.ID,
					Command:  event.Command,
					Reason:   string(reason),
					ExitCode: event.ExitCode,
					Output:   output,
				},
			})
			a.nudgeSignalScheduler()
		}
	})

	// Re-create RunCommand with the process manager.
	rc := tool.NewRunCommand(c.Cfg.Tools, c.Home, procMgr)
	registry.Register(tool.WrapWithPermission(rc, checkFunc, askFunc))
	registry.Register(tool.WrapWithPermission(tool.NewProcessTool(procMgr), checkFunc, askFunc))
	registry.Register(tool.WrapWithPermission(tool.Sleep{}, checkFunc, askFunc))

	embedder, err := memory.NewEmbedder()
	if err != nil {
		return nil, fmt.Errorf("init embedder: %w", err)
	}
	memStore := memory.NewStore(embedder, resolver.Root(), c.Home)
	a.memoryStore = memStore

	var projectID, memoriesDir string
	if proj, err := resolver.Current(); err == nil && proj != nil {
		projectID = proj.ID
		memoriesDir = filepath.Join(resolver.Root(), proj.ID, "memories")
	}
	registry.Register(tool.WrapWithPermission(tool.NewSaveMemory(memStore, memoriesDir), checkFunc, askFunc))
	registry.Register(tool.WrapWithPermission(tool.NewSearchMemory(memStore, projectID), checkFunc, askFunc))
	registry.Register(tool.WrapWithPermission(tool.NewSearchHistory(memStore, projectID), checkFunc, askFunc))

	lspMgr := lsp.NewManager(c.ProjectRoot, c.Home)
	a.lspManager = lspMgr

	lspClient := lsp.NewClient(lspMgr)
	diagAdapter := &snapshotDiagAdapter{store: store}
	lspDiag := tool.NewLSPDiagnostics(lspClient, diagAdapter)
	a.lspDiagnostics = lspDiag
	registry.Register(lspDiag)
	registry.Register(tool.NewWorkspaceSymbol(lspClient))

	loader := subagent.NewLoader(c.ProjectRoot, c.Home)
	taggedEvts := make(chan TaggedLoopEvent, 512)

	subModel := c.Cfg.Subagents.Model

	tt := newTaskTool(taskToolConfig{
		Loader:        loader,
		ParentStore:   store,
		BaseRegistry:  registry,
		MaxConcurrent: c.Cfg.Subagents.MaxConcurrent,
		TaggedEvents:  taggedEvts,
		ModelCatalog:  modelCatalog,
		ProviderName:  defaultRef.Provider,
		Model:         defaultRef.Model,
		SubModel:      subModel,
		ToolsConfig:   c.Cfg.Tools,
		HomeDir:       c.Home,
		ProcMgr:       procMgr,
		Check:         checkFunc,
		Ask:           askFunc,
	})
	registry.Register(tt)
	a.subagentLoader = loader
	a.taggedEvents = taggedEvts
	a.taskToolInst = tt

	a.registry = registry

	asm := prompt.New(c.ProjectRoot, c.Home)
	res := asm.Assemble()
	a.assembler = asm
	a.pendingPromptWarnings = res.Warnings
	a.pendingCatalogWarnings = catalogWarningsToPromptWarnings(catalogWarnings)

	l := loop.New(client, registry, res.Prompt)
	l.SetEvents(events)
	l.SetStore(store)
	l.SetPendingExecutor(tool.NewStagedExecutor(store, fileTracker, c.Cfg.Tools, checkFunc, askActionFunc))
	a.lp = l

	return a, nil
}

// SetEventHandler sets the callback for agent events. Must be called
// before Init.
func (a *Agent) SetEventHandler(fn func(Event)) {
	a.onEvent = fn
}

// Init starts background goroutines, runs the session sweep, and
// resumes the most recent session if one exists. ctx controls the
// agent's lifetime.
func (a *Agent) Init(ctx context.Context) {
	go a.drainLoopEvents(ctx)
	go a.runSignalScheduler(ctx)
	if a.memoryStore != nil {
		_ = a.memoryStore.Reconcile()
	}
	a.runSweep()
	if err := a.resumeMostRecent(); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: resume session: %v\n", err)
	}
	go a.periodicSweep(ctx)

	if a.procMgr != nil {
		go func() {
			<-ctx.Done()
			a.procMgr.KillAll()
		}()
	}

	if a.lspManager != nil {
		a.lspManager.SetWarningHandler(func(kind, message string) {
			a.addWarning("lsp", prompt.Warning{Kind: kind, Message: message})
		})
		a.lspManager.SetSignalHandler(func(content string) {
			if a.lp != nil {
				a.lp.AddPendingSignal(loop.PendingSignal{Payload: content})
			}
		})
		go a.lspManager.Detect(ctx)
		go func() {
			<-ctx.Done()
			a.lspManager.ShutdownAll()
		}()
	}

	a.setWarningGroup("prompt", a.pendingPromptWarnings)
	a.pendingPromptWarnings = nil
	a.setWarningGroup("catalog", a.pendingCatalogWarnings)
	a.pendingCatalogWarnings = nil
}

func (a *Agent) emitEvent(ev Event) {
	if a.onEvent != nil {
		a.onEvent(ev)
	}
}

func (a *Agent) nudgeSignalScheduler() {
	select {
	case a.signalWake <- struct{}{}:
	default:
	}
}

func (a *Agent) runSignalScheduler(ctx context.Context) {
	for {
		select {
		case <-a.signalWake:
			a.tryStartSignalTurn(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (a *Agent) tryStartSignalTurn(ctx context.Context) {
	if a.lp == nil || !a.lp.HasPendingWakeSignal() {
		return
	}
	a.mu.Lock()
	busy := a.busy
	active := a.store != nil && a.store.Active()
	a.mu.Unlock()
	if busy || !active {
		return
	}
	if _, err := a.startTurn(ctx, nil); err != nil && !strings.Contains(err.Error(), "turn is already in progress") {
		a.emitEvent(Event{Kind: EventError, Error: err.Error()})
	}
}

func (a *Agent) setWarningGroup(group string, warnings []prompt.Warning) {
	a.updateWarningGroup(group, promptWarnings(warnings), false)
}

func (a *Agent) addWarning(group string, warning prompt.Warning) {
	a.updateWarningGroup(group, []PromptWarning{{Kind: warning.Kind, Message: warning.Message}}, true)
}

func (a *Agent) updateWarningGroup(group string, warnings []PromptWarning, appendOnly bool) {
	a.warningsMu.Lock()
	if a.warningGroups == nil {
		a.warningGroups = make(map[string][]PromptWarning)
	}
	if appendOnly {
		warnings = appendUniquePromptWarnings(a.warningGroups[group], warnings)
	}
	sort.Slice(warnings, func(i, j int) bool {
		return promptWarningSortKey(warnings[i]) < promptWarningSortKey(warnings[j])
	})
	if len(warnings) == 0 {
		delete(a.warningGroups, group)
	} else {
		a.warningGroups[group] = append([]PromptWarning(nil), warnings...)
	}
	next := a.warningSnapshotLocked()
	if promptWarningsEqual(a.warningSnapshot, next) {
		a.warningsMu.Unlock()
		return
	}
	a.warningSnapshot = append([]PromptWarning(nil), next...)
	a.warningsMu.Unlock()

	a.emitEvent(Event{Kind: EventWarning, Warnings: next})
}

// CurrentWarnings returns the current warning snapshot for adapters that need
// to hydrate UI state after startup events may already have fired.
func (a *Agent) CurrentWarnings() []PromptWarning {
	a.warningsMu.Lock()
	defer a.warningsMu.Unlock()
	return append([]PromptWarning(nil), a.warningSnapshot...)
}

func (a *Agent) warningSnapshotLocked() []PromptWarning {
	var out []PromptWarning
	for _, group := range []string{"prompt", "catalog", "lsp", "protocol"} {
		out = append(out, a.warningGroups[group]...)
	}
	return out
}

func promptWarnings(warnings []prompt.Warning) []PromptWarning {
	out := make([]PromptWarning, len(warnings))
	for i, w := range warnings {
		out[i] = PromptWarning{Kind: w.Kind, Message: w.Message}
	}
	return out
}

func appendUniquePromptWarnings(dst, src []PromptWarning) []PromptWarning {
	out := append([]PromptWarning(nil), dst...)
	for _, w := range src {
		seen := false
		for _, existing := range out {
			if existing == w {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, w)
		}
	}
	return out
}

func promptWarningSortKey(w PromptWarning) string {
	return w.Kind + "\x00" + w.Message
}

func promptWarningsEqual(a, b []PromptWarning) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func backgroundTerminalPayload(event process.ExitEvent, output string) string {
	reason := event.Reason
	if reason == "" {
		reason = process.ExitReasonCompleted
	}
	return fmt.Sprintf("Background process %s (%q) finished: %s, exit code %d.\nOutput:\n%s", event.ID, event.Command, reason, event.ExitCode, output)
}

func agentBackgroundProcessDisplay(bg *loop.BackgroundProcessDisplay) *BackgroundProcessDisplay {
	if bg == nil {
		return nil
	}
	return &BackgroundProcessDisplay{
		ID:       bg.ID,
		Command:  bg.Command,
		Reason:   bg.Reason,
		ExitCode: bg.ExitCode,
		Output:   bg.Output,
	}
}

func (a *Agent) drainLoopEvents(ctx context.Context) {
	for {
		select {
		case ev, ok := <-a.loopEvents:
			if !ok {
				return
			}
			a.dispatchLoopEvent(ev)
		case tev, ok := <-a.taggedEvents:
			if !ok {
				continue
			}
			a.dispatchTaggedEvent(tev)
		case done := <-a.loopFlush:
			a.drainPendingLoopEvents()
			close(done)
		case <-ctx.Done():
			return
		}
	}
}

func (a *Agent) drainPendingLoopEvents() {
	for {
		select {
		case ev := <-a.loopEvents:
			a.dispatchLoopEvent(ev)
		default:
			return
		}
	}
}

func (a *Agent) dispatchLoopEvent(ev loop.Event) {
	switch ev.Kind {
	case loop.TextDelta:
		a.emitEvent(Event{Kind: EventTextDelta, Result: ev.Result})
	case loop.ToolCallStart:
		a.emitEvent(Event{
			Kind:       EventToolCallStart,
			ToolCallID: ev.ToolCallID,
			ToolName:   ev.ToolName,
			Args:       ev.Args,
		})
	case loop.ToolCallEnd:
		a.emitEvent(Event{
			Kind:       EventToolCallEnd,
			ToolCallID: ev.ToolCallID,
			ToolName:   ev.ToolName,
			Args:       ev.Args,
			IsError:    ev.IsError,
			Result:     ev.Result,
			Metadata:   ev.Metadata,
		})
	case loop.BackgroundProcessComplete:
		a.emitEvent(Event{
			Kind:              EventBackgroundProcessComplete,
			Result:            ev.Result,
			IsError:           ev.IsError,
			Turn:              ev.Turn,
			BackgroundProcess: agentBackgroundProcessDisplay(ev.BackgroundProcess),
		})
	case loop.PermissionRequest:
		canAllowAll, _ := ev.Metadata["can_allow_all"].(bool)
		batchIndex, _ := ev.Metadata["batch_index"].(int)
		batchTotal, _ := ev.Metadata["batch_total"].(int)
		batchFiles, _ := ev.Metadata["batch_files"].([]string)
		resolvedArg, _ := ev.Metadata["resolved_arg"].(string)
		a.emitEvent(Event{
			Kind: EventPermissionRequest,
			PermReq: &PermissionRequest{
				ID:          ev.PermID,
				ToolName:    ev.ToolName,
				Arg:         ev.PermArg,
				ResolvedArg: resolvedArg,
				CanAllowAll: canAllowAll,
				BatchIndex:  batchIndex,
				BatchTotal:  batchTotal,
				BatchFiles:  batchFiles,
			},
		})
	case loop.Usage:
		a.recordUsage(ev)
	case loop.Warning:
		kind, _ := ev.Metadata["kind"].(string)
		if kind == "" {
			kind = "protocol_warning"
		}
		a.addWarning("protocol", prompt.Warning{Kind: kind, Message: ev.Result})
	}
}

func (a *Agent) dispatchTaggedEvent(tev TaggedLoopEvent) {
	a.mu.Lock()
	if a.seenSessions == nil {
		a.seenSessions = make(map[string]bool)
	}
	isNew := tev.SessionID != "" && !a.seenSessions[tev.SessionID]
	if isNew {
		a.seenSessions[tev.SessionID] = true
	}
	a.mu.Unlock()
	if isNew {
		a.emitEvent(Event{
			Kind:              EventSubagentStart,
			SubagentSessionID: tev.SessionID,
			TaskIndex:         tev.TaskIndex,
			ToolCallID:        tev.ToolCallID,
		})
	}

	ev := tev.Event
	base := Event{
		SubagentSessionID: tev.SessionID,
		TaskIndex:         tev.TaskIndex,
		ToolCallID:        tev.ToolCallID,
	}
	switch ev.Kind {
	case loop.TextDelta:
		base.Kind = EventTextDelta
		base.Result = ev.Result
	case loop.ToolCallStart:
		base.Kind = EventToolCallStart
		base.ToolCallID = ev.ToolCallID
		base.ToolName = ev.ToolName
		base.Args = ev.Args
	case loop.ToolCallEnd:
		base.Kind = EventToolCallEnd
		base.ToolCallID = ev.ToolCallID
		base.ToolName = ev.ToolName
		base.Args = ev.Args
		base.IsError = ev.IsError
		base.Result = ev.Result
		base.Metadata = ev.Metadata
	case loop.Usage:
		a.recordUsage(ev)
		return
	case loop.Warning:
		kind, _ := ev.Metadata["kind"].(string)
		if kind == "" {
			kind = "protocol_warning"
		}
		a.addWarning("protocol", prompt.Warning{Kind: kind, Message: ev.Result})
		return
	default:
		return
	}
	a.emitEvent(base)
}

func (a *Agent) recordUsage(ev loop.Event) {
	ref := a.currentRef
	prov := ref.Provider
	model := ev.Model
	if model == "" {
		model = ref.Model
	}
	key := prov + "/" + model

	a.tokensMu.Lock()
	if a.tokens == nil {
		a.tokens = map[string]*TokenEntry{}
	}
	entry, ok := a.tokens[key]
	if !ok {
		entry = &TokenEntry{Provider: prov, Model: model, Known: true}
		a.tokens[key] = entry
	}
	entry.Cache += ev.Cache
	entry.Input += ev.Input
	entry.Output += ev.Output
	if ev.UsageKnown {
		a.lastContextUsed = ev.Cache + ev.Input
	}
	a.persistTokensLocked()
	a.tokensMu.Unlock()

	a.emitEvent(Event{
		Kind:       EventUsage,
		Model:      model,
		Cache:      ev.Cache,
		Input:      ev.Input,
		Output:     ev.Output,
		UsageKnown: ev.UsageKnown,
	})
}

func (a *Agent) buildReportLocked() TokenReport {
	total := TokenEntry{Known: true}
	per := make([]TokenEntry, 0, len(a.tokens))
	for _, e := range a.tokens {
		per = append(per, *e)
		total.Cache += e.Cache
		total.Input += e.Input
		total.Output += e.Output
	}
	return TokenReport{
		Total:         total,
		PerModel:      per,
		ContextUsed:   a.lastContextUsed,
		ContextWindow: a.contextWindowSize,
	}
}

func (a *Agent) persistTokensLocked() {
	if a.store == nil || !a.store.Active() {
		return
	}
	entries := make([]TokenEntry, 0, len(a.tokens))
	for _, e := range a.tokens {
		entries = append(entries, *e)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	data = append(data, '\n')
	_ = os.WriteFile(filepath.Join(a.store.Dir(), tokensFileName), data, 0o600)
}

func (a *Agent) runSweep() {
	if a.projects == nil {
		return
	}
	cfg := snapshot.LifecycleConfig{
		Enabled:                a.cfg.Sessions.AutoArchive,
		ArchiveAfterDays:       a.cfg.Sessions.ArchiveAfterDays,
		DeleteAfterArchiveDays: a.cfg.Sessions.DeleteAfterArchiveDays,
	}
	var onDelete func(string)
	if a.memoryStore != nil {
		onDelete = func(sessionID string) { _ = a.memoryStore.DeleteSessionSummaries(sessionID) }
	}
	if _, _, err := snapshot.SweepAllProjects(a.projects.Root(), cfg, onDelete); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: sweep: %v\n", err)
	}
}

func (a *Agent) periodicSweep(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.runSweep()
		}
	}
}

func (a *Agent) shouldAutoCompact() bool {
	if !a.cfg.Compaction.Enabled {
		return false
	}
	a.tokensMu.Lock()
	used := a.lastContextUsed
	window := a.contextWindowSize
	a.tokensMu.Unlock()
	if window <= 0 || used <= 0 {
		return false
	}
	return float64(used)/float64(window) >= a.cfg.Compaction.ThresholdPct
}

// runCompaction summarizes the current conversation. turnInProgress
// should be true when called from inside SendPrompt (a BeginTurn was
// just issued for a not-yet-executed turn), false for manual compaction.
func (a *Agent) runCompaction(ctx context.Context, turnInProgress bool) error {
	a.emitEvent(Event{Kind: EventCompactionStart})
	defer a.emitEvent(Event{Kind: EventCompactionEnd})

	messages := a.lp.Messages()
	if len(messages) <= 1 {
		return fmt.Errorf("nothing to compact")
	}
	// Skip system prompt at index 0.
	toSummarize := messages[1:]

	client, summarizerWindow := a.summarizerClientAndWindow()
	if summarizerWindow <= 0 {
		summarizerWindow = a.contextWindowSize
	}

	prompt := compact.DefaultSummarizerPrompt

	result, err := compact.Run(ctx, toSummarize, compact.Config{
		SummarizerClient: client,
		ContextWindow:    summarizerWindow,
		SummarizerPrompt: prompt,
	})
	if err != nil {
		return err
	}

	// When called from SendPrompt, CurrentTurn() is the just-begun
	// empty turn; the boundary is the previous (last completed) turn.
	boundaryTurn := a.store.CurrentTurn()
	if turnInProgress {
		boundaryTurn--
	}
	rec := snapshot.CompactionRecord{
		Summary:            result.Summary,
		BoundaryTurn:       boundaryTurn,
		CompactedAt:        time.Now().UTC().Format(time.RFC3339),
		SummarizerModel:    result.SummarizerModel,
		SummarizerProvider: result.SummarizerRef.Provider,
	}
	if err := a.store.SaveCompaction(rec); err != nil {
		return fmt.Errorf("save compaction: %w", err)
	}

	if a.memoryStore != nil {
		sessionID := a.store.SessionID()
		var projID, projName string
		if proj, pErr := a.projects.Current(); pErr == nil && proj != nil {
			projID = proj.ID
			projName = proj.Name
		}
		compactionPath := filepath.Join(a.store.Dir(), "compaction.json")
		if err := a.memoryStore.IndexSummary(sessionID, projID, projName, result.Summary, rec.CompactedAt, compactionPath); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: memory index summary: %v\n", err)
		}
	}

	a.lp.LoadHistoryWithSummaryPreservePending(result.Summary, result.SummarizerRef, nil)
	if a.fileTracker != nil {
		a.fileTracker.Reset()
	}

	a.tokensMu.Lock()
	a.lastContextUsed = 0
	a.tokensMu.Unlock()

	return nil
}

func (a *Agent) summarizerClientAndWindow() (*provider.Client, int) {
	ref := a.currentRef
	if a.cfg.Compaction.SummarizerModel != "" {
		parsed, err := catalog.ParseModelRef(a.cfg.Compaction.SummarizerModel)
		if err == nil {
			ref = parsed
		}
	}

	client, model, err := newProviderClient(a.catalog, ref)
	if err != nil && ref != a.currentRef {
		client, model, err = newProviderClient(a.catalog, a.currentRef)
	}
	if err != nil {
		return provider.New(nil, nil, ""), 0
	}
	return client, model.ContextWindow
}

// CompactNow triggers manual compaction. Must not be called while busy.
func (a *Agent) CompactNow(ctx context.Context) error {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return fmt.Errorf("cannot compact while a turn is running")
	}
	if !a.store.Active() {
		a.mu.Unlock()
		return fmt.Errorf("no session open")
	}
	a.busy = true
	compactCtx, cancel := context.WithCancel(ctx)
	a.turnCancel = cancel
	a.turnCtx = compactCtx
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		a.busy = false
		a.turnCancel = nil
		a.turnCtx = nil
		a.mu.Unlock()
		cancel()
		if a.lp != nil && a.lp.HasPendingWakeSignal() {
			a.nudgeSignalScheduler()
		}
	}()

	return a.runCompaction(compactCtx, false)
}

func (a *Agent) resumeMostRecent() error {
	proj, err := a.projects.Current()
	if err != nil || proj == nil {
		return err
	}
	sessionsRoot := a.projects.SessionsRoot(proj.ID)
	if err := a.store.AttachSessionsRoot(sessionsRoot, a.projects.Root(), proj.ID); err != nil {
		return err
	}
	id, err := snapshot.LoadMostRecent(sessionsRoot, "")
	if err != nil || id == "" {
		return err
	}
	if err := a.store.LoadSession(id); err != nil {
		return err
	}
	if err := a.loadHistoryIntoLoop(); err != nil {
		return err
	}
	a.populateFileTracker()
	a.loadTokensFromDisk()
	a.mu.Lock()
	if err := a.reloadLocked(); err != nil {
		a.mu.Unlock()
		fmt.Fprintf(os.Stderr, "lightcode: reload config on resume: %v\n", err)
		a.restoreModelFromSession()
		return nil
	}
	a.mu.Unlock()
	a.restoreModelFromSession()
	return nil
}

func (a *Agent) populateFileTracker() {
	if a.fileTracker == nil {
		return
	}
	a.fileTracker.Reset()
	if a.store == nil {
		return
	}
	rec, _ := a.store.LoadCompaction()
	var raw []snapshot.TurnMessages
	var err error
	if rec != nil {
		raw, err = a.store.LoadCompleteTurnsAfter(rec.BoundaryTurn)
	} else {
		raw, err = a.store.LoadCompleteTurns()
	}
	if err != nil || len(raw) == 0 {
		return
	}
	var msgs []tool.PersistedMessage
	// We extract paths from assistant messages' tool_call args,
	// since tool result messages don't contain the file path.
	for _, t := range raw {
		for _, line := range t.Messages {
			var rawMsg map[string]any
			if json.Unmarshal(line, &rawMsg) != nil {
				continue
			}
			role, _ := rawMsg["role"].(string)
			if role != "assistant" {
				continue
			}
			toolCalls, ok := rawMsg["tool_calls"].([]any)
			if !ok {
				continue
			}
			for _, tc := range toolCalls {
				tcMap, ok := tc.(map[string]any)
				if !ok {
					continue
				}
				fn, ok := tcMap["function"].(map[string]any)
				if !ok {
					continue
				}
				fnName, _ := fn["name"].(string)
				if fnName != "read_file" {
					continue
				}
				argsStr, _ := fn["arguments"].(string)
				var args map[string]any
				if json.Unmarshal([]byte(argsStr), &args) != nil {
					continue
				}
				path, _ := args["path"].(string)
				if path != "" {
					msgs = append(msgs, tool.PersistedMessage{
						Role:     "tool",
						ToolName: "read_file",
						Path:     path,
					})
				}
			}
		}
	}
	a.fileTracker.PopulateFromMessages(msgs)
}

func (a *Agent) restoreModelFromSession() {
	meta, err := a.store.Meta()
	if err != nil || meta.Provider == "" || meta.Model == "" {
		return
	}
	ref := catalog.ModelRef{Provider: meta.Provider, Model: meta.Model}
	client, model, err := newProviderClient(a.catalog, ref)
	if err != nil {
		return
	}
	a.lp.SetClient(client)
	a.currentRef = ref
	a.contextWindowSize = model.ContextWindow
}

func (a *Agent) loadHistoryIntoLoop() error {
	rec, err := a.store.LoadCompaction()
	if err != nil {
		return err
	}

	var raw []snapshot.TurnMessages
	if rec != nil {
		raw, err = a.store.LoadCompleteTurnsAfter(rec.BoundaryTurn)
	} else {
		raw, err = a.store.LoadCompleteTurns()
	}
	if err != nil {
		return err
	}

	decoded := make([][]message.Message, 0, len(raw))
	for _, t := range raw {
		var turnMsgs []message.Message
		for _, line := range t.Messages {
			var m message.Message
			if err := json.Unmarshal(line, &m); err != nil {
				continue
			}
			turnMsgs = append(turnMsgs, m)
		}
		if len(turnMsgs) > 0 {
			decoded = append(decoded, turnMsgs)
		}
	}

	if rec != nil {
		a.lp.LoadHistoryWithSummary(rec.Summary, catalog.ModelRef{Provider: rec.SummarizerProvider, Model: rec.SummarizerModel}, decoded)
	} else {
		a.lp.LoadHistory(decoded)
	}
	return nil
}

func (a *Agent) loadTokensFromDisk() {
	a.tokensMu.Lock()
	defer a.tokensMu.Unlock()
	a.tokens = map[string]*TokenEntry{}
	if a.store == nil || !a.store.Active() {
		return
	}
	data, err := os.ReadFile(filepath.Join(a.store.Dir(), tokensFileName))
	if err != nil {
		return
	}
	var entries []TokenEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return
	}
	for i := range entries {
		e := entries[i]
		e.Known = true
		a.tokens[e.Provider+"/"+e.Model] = &e
	}
}

func (a *Agent) ensureSession() error {
	if a.store.Active() {
		return nil
	}
	if err := a.reloadLocked(); err != nil {
		return err
	}
	proj, err := a.projects.Ensure()
	if err != nil {
		return err
	}
	if err := a.store.AttachSessionsRoot(a.projects.SessionsRoot(proj.ID), a.projects.Root(), proj.ID); err != nil {
		return err
	}
	if err := a.store.BeginNewSession(a.projectRoot); err != nil {
		return err
	}
	if err := a.store.SetModel(a.currentRef.Provider, a.currentRef.Model); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: store.SetModel: %v\n", err)
	}
	a.lp.ResetHistory()
	if a.fileTracker != nil {
		a.fileTracker.Reset()
	}
	a.loadTokensFromDisk()
	return nil
}

// --- Public methods (the service API) ---

// SendPrompt starts a turn with a single user message.
func (a *Agent) SendPrompt(ctx context.Context, content string) (int, error) {
	return a.sendMessages(ctx, []string{content})
}

// SendQueuedMessages flushes messages submitted while a turn was busy: all
// but the last are persisted as user-only turns, then the last starts the
// next model turn.
func (a *Agent) SendQueuedMessages(ctx context.Context, contents []string) (QueuedMessagesResult, error) {
	var result QueuedMessagesResult
	if len(contents) == 0 {
		return result, fmt.Errorf("no queued messages")
	}
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return result, fmt.Errorf("a turn is already in progress")
	}
	if err := a.ensureSession(); err != nil {
		a.mu.Unlock()
		return result, err
	}
	for _, content := range contents[:len(contents)-1] {
		turn := a.store.BeginTurn()
		a.lp.AppendUserMessage(turn, content)
		_ = a.store.MarkTurnComplete(turn)
		result.Appended = append(result.Appended, QueuedMessageTurn{Content: content, Turn: turn})
	}
	a.busy = true
	a.seenSessions = nil
	turnCtx, cancel := context.WithCancel(ctx)
	a.turnCancel = cancel
	a.turnCtx = turnCtx
	a.mu.Unlock()

	last := contents[len(contents)-1]
	turn := a.launchTurn(ctx, turnCtx, cancel, []string{last})
	result.Started = QueuedMessageTurn{Content: last, Turn: turn}
	return result, nil
}

// AppendUserMessage persists a user message as its own complete turn
// without running the model.
func (a *Agent) AppendUserMessage(content string) (int, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return 0, fmt.Errorf("a turn is already in progress")
	}
	if err := a.ensureSession(); err != nil {
		return 0, err
	}
	turn := a.store.BeginTurn()
	a.lp.AppendUserMessage(turn, content)
	_ = a.store.MarkTurnComplete(turn)
	return turn, nil
}

func (a *Agent) sendMessages(ctx context.Context, contents []string) (int, error) {
	return a.startTurn(ctx, contents)
}

func (a *Agent) startTurn(ctx context.Context, contents []string) (int, error) {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return 0, fmt.Errorf("a turn is already in progress")
	}
	if err := a.ensureSession(); err != nil {
		a.mu.Unlock()
		return 0, err
	}
	a.busy = true
	a.seenSessions = nil
	turnCtx, cancel := context.WithCancel(ctx)
	a.turnCancel = cancel
	a.turnCtx = turnCtx
	a.mu.Unlock()

	return a.launchTurn(ctx, turnCtx, cancel, contents), nil
}

func (a *Agent) launchTurn(ctx context.Context, turnCtx context.Context, cancel context.CancelFunc, contents []string) int {
	turn := a.store.BeginTurn()

	a.emitEvent(Event{Kind: EventTurnStart, Turn: turn})

	if a.taskToolInst != nil {
		a.taskToolInst.updateParentState(a.currentRef.Provider, a.currentRef.Model, cancel)
	}

	go func() {
		defer func() {
			a.mu.Lock()
			a.busy = false
			a.turnCancel = nil
			a.turnCtx = nil
			a.mu.Unlock()
			cancel()
			if a.lp != nil && a.lp.HasPendingWakeSignal() {
				a.nudgeSignalScheduler()
			}
		}()

		res := a.assembler.Assemble()
		if res.Rebuilt {
			a.lp.UpdateSystemPrompt(res.Prompt)
		}
		a.setWarningGroup("prompt", res.Warnings)

		if a.shouldAutoCompact() {
			if err := a.runCompaction(turnCtx, true); err != nil {
				a.emitEvent(Event{Kind: EventError, Error: fmt.Sprintf("compaction: %v", err), Turn: turn})
			}
		}

		_, err := a.lp.Run(turnCtx, contents...)

		done := make(chan struct{})
		select {
		case a.loopFlush <- done:
			select {
			case <-done:
			case <-ctx.Done():
			}
		case <-ctx.Done():
		}

		if err != nil {
			a.emitEvent(Event{Kind: EventError, Error: err.Error(), Turn: turn})
		}
		a.emitEvent(Event{Kind: EventTurnEnd, Turn: turn, Cancelled: turnCtx.Err() != nil})
	}()

	return turn
}

// Cancel aborts the current turn.
func (a *Agent) Cancel() error {
	a.mu.Lock()
	cancel := a.turnCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if a.gate != nil {
		a.gate.CancelAll()
	}
	return nil
}

// Busy reports whether a turn is in progress.
func (a *Agent) Busy() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.busy
}

// RespondPermission answers a pending permission prompt.
func (a *Agent) RespondPermission(id string, allow bool) error {
	return a.gate.Respond(id, allow)
}

// RespondPermissionAction answers a pending permission prompt with an action.
func (a *Agent) RespondPermissionAction(id string, action string) error {
	return a.gate.RespondAction(id, action)
}

// PermissionSuggest returns pattern suggestions for the "Allow for project" UI.
func (a *Agent) PermissionSuggest(toolName, arg string) []PermissionSuggestion {
	return permission.Suggest(toolName, arg, a.projectRoot)
}

// SaveProjectPermission appends patterns to the project's local
// permissions.json, then allows the pending request.
func (a *Agent) SaveProjectPermission(id string, patterns []string) error {
	proj, err := a.projects.Ensure()
	if err != nil {
		return err
	}
	add := permission.Rules{Allow: patterns}
	if err := permission.SaveLocal(a.projects.Root(), proj.ID, add); err != nil {
		return err
	}
	if err := a.gate.Respond(id, true); err != nil {
		if errors.Is(err, permission.ErrUnknownRequest) {
			return nil
		}
		return err
	}
	return nil
}

// SwitchModel changes the active model by provider-prefixed catalog ref.
func (a *Agent) SwitchModel(refStr string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return fmt.Errorf("cannot switch model while a turn is running")
	}
	ref, err := catalog.ParseModelRef(refStr)
	if err != nil {
		return err
	}
	client, model, err := newProviderClient(a.catalog, ref)
	if err != nil {
		return err
	}
	a.lp.SetClient(client)
	a.currentRef = ref
	a.contextWindowSize = model.ContextWindow
	a.lp.AddPendingSignal(loop.PendingSignal{Payload: fmt.Sprintf("Model switched to %s", ref.String())})
	if a.store.Active() {
		if err := a.store.SetModel(ref.Provider, ref.Model); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: store.SetModel: %v\n", err)
		}
	}
	return nil
}

// Reload reloads config and catalog state for future turns.
func (a *Agent) Reload() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return fmt.Errorf("cannot reload while a turn is running")
	}
	return a.reloadLocked()
}

func (a *Agent) reloadLocked() error {
	cfg, err := config.Load(agentConfigPath(a.home))
	if err != nil {
		return err
	}
	modelCatalog, catalogWarnings, err := catalog.NewLoader(a.home, nil).Load()
	if err != nil {
		return fmt.Errorf("load model catalog: %w", err)
	}

	ref := a.currentRef
	if _, _, err := modelCatalog.Lookup(ref); err != nil {
		ref, err = catalog.ParseModelRef(cfg.DefaultModel)
		if err != nil {
			return fmt.Errorf("default_model: %w", err)
		}
	}
	client, model, err := newProviderClient(modelCatalog, ref)
	if err != nil {
		return err
	}

	a.cfg = cfg
	a.catalog = modelCatalog
	if a.taskToolInst != nil {
		a.taskToolInst.setCatalog(modelCatalog)
		a.taskToolInst.setSubModel(cfg.Subagents.Model)
	}
	a.lp.SetClient(client)
	a.currentRef = ref
	a.contextWindowSize = model.ContextWindow
	a.setWarningGroup("catalog", catalogWarningsToPromptWarnings(catalogWarnings))
	return nil
}

type ModelCompletion struct {
	ContextWindow   int `json:"context_window"`
	MaxOutputTokens int `json:"max_output_tokens"`
}

func (a *Agent) CompleteModelEntry(refStr string, completion ModelCompletion) error {
	ref, err := catalog.ParseModelRef(refStr)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return fmt.Errorf("cannot complete model entry while a turn is running")
	}

	path := agentConfigPath(a.home)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	providers, ok := root["providers"].(map[string]any)
	if !ok {
		providers = map[string]any{}
		root["providers"] = providers
	}
	providerRaw, ok := providers[ref.Provider]
	if !ok {
		providerRaw = map[string]any{}
		providers[ref.Provider] = providerRaw
	}
	providerMap, ok := providerRaw.(map[string]any)
	if !ok {
		return fmt.Errorf("providers.%s must be an object", ref.Provider)
	}
	modelsRaw, ok := providerMap["models"]
	if !ok {
		modelsRaw = map[string]any{}
		providerMap["models"] = modelsRaw
	}
	modelsMap, ok := modelsRaw.(map[string]any)
	if !ok {
		return fmt.Errorf("providers.%s.models must be an object", ref.Provider)
	}
	modelRaw, ok := modelsMap[ref.Model]
	if !ok {
		modelRaw = map[string]any{}
		modelsMap[ref.Model] = modelRaw
	}
	modelMap, ok := modelRaw.(map[string]any)
	if !ok {
		return fmt.Errorf("providers.%s.models.%s must be an object", ref.Provider, ref.Model)
	}
	if completion.ContextWindow > 0 {
		modelMap["context_window"] = completion.ContextWindow
	}
	if completion.MaxOutputTokens > 0 {
		modelMap["max_output_tokens"] = completion.MaxOutputTokens
	}
	if err := writeAgentConfigAtomic(path, root); err != nil {
		return err
	}
	return a.reloadLocked()
}

func agentConfigPath(home string) string {
	return filepath.Join(home, ".lightcode", "config.json")
}

func writeAgentConfigAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// RefreshDiscovery refreshes live model discovery for one enabled provider.
func (a *Agent) RefreshDiscovery(provider string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return fmt.Errorf("cannot refresh discovery while a turn is running")
	}
	return a.refreshDiscoveryLocked(provider)
}

func (a *Agent) refreshDiscoveryLocked(provider string) error {
	_, warnings := catalog.RefreshProviderDiscovery(context.Background(), a.home, a.catalog, provider)
	if len(warnings) == 0 {
		return nil
	}
	a.setWarningGroup("catalog", catalogWarningsToPromptWarnings(warnings))
	return fmt.Errorf("refresh discovery for %s: %s", provider, warnings[0].Message)
}

// CurrentModel returns the active model identity and catalog metadata.
func (a *Agent) CurrentModel() ModelInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.modelInfo(a.currentRef)
}

// ModelList returns all visible catalog models as flat enriched entries.
func (a *Agent) ModelList() []ModelListEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	refs := a.catalog.VisibleModels()
	result := make([]ModelListEntry, 0, len(refs))
	for _, ref := range refs {
		prov, model, err := a.catalog.LookupOrIncomplete(ref)
		if err != nil {
			continue
		}
		displayName := model.Name
		if displayName == "" {
			displayName = model.ID
		}
		providerName := prov.Name
		if providerName == "" {
			providerName = prov.ID
		}
		_, incomplete := model.Incomplete()
		result = append(result, ModelListEntry{
			Ref:            ref.String(),
			Provider:       ref.Provider,
			ProviderName:   providerName,
			Model:          ref.Model,
			DisplayName:    displayName,
			ContextWindow:  model.ContextWindow,
			Cost:           model.Cost,
			Hidden:         model.Hidden || prov.Hidden,
			ProviderHidden: prov.Hidden,
			Incomplete:     incomplete,
		})
	}
	return result
}

func (a *Agent) AllModelList() []ModelListEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	refs := a.catalog.AllModels()
	result := make([]ModelListEntry, 0, len(refs))
	for _, ref := range refs {
		prov, model, err := a.catalog.LookupOrIncomplete(ref)
		if err != nil {
			continue
		}
		displayName := model.Name
		if displayName == "" {
			displayName = model.ID
		}
		providerName := prov.Name
		if providerName == "" {
			providerName = prov.ID
		}
		_, incomplete := model.Incomplete()
		result = append(result, ModelListEntry{
			Ref:            ref.String(),
			Provider:       ref.Provider,
			ProviderName:   providerName,
			Model:          ref.Model,
			DisplayName:    displayName,
			ContextWindow:  model.ContextWindow,
			Cost:           model.Cost,
			Hidden:         model.Hidden || prov.Hidden,
			ProviderHidden: prov.Hidden,
			Incomplete:     incomplete,
		})
	}
	return result
}

func (a *Agent) SetModelHidden(refStr string, hidden bool) error {
	ref, err := catalog.ParseModelRef(refStr)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return fmt.Errorf("cannot change model visibility while a turn is running")
	}
	path := agentConfigPath(a.home)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	providers, ok := root["providers"].(map[string]any)
	if !ok {
		providers = map[string]any{}
		root["providers"] = providers
	}
	providerRaw, ok := providers[ref.Provider]
	if !ok {
		providerRaw = map[string]any{}
		providers[ref.Provider] = providerRaw
	}
	providerMap, ok := providerRaw.(map[string]any)
	if !ok {
		return fmt.Errorf("providers.%s must be an object", ref.Provider)
	}
	modelsRaw, ok := providerMap["models"]
	if !ok {
		modelsRaw = map[string]any{}
		providerMap["models"] = modelsRaw
	}
	modelsMap, ok := modelsRaw.(map[string]any)
	if !ok {
		return fmt.Errorf("providers.%s.models must be an object", ref.Provider)
	}
	modelRaw, ok := modelsMap[ref.Model]
	if !ok {
		modelRaw = map[string]any{}
		modelsMap[ref.Model] = modelRaw
	}
	modelMap, ok := modelRaw.(map[string]any)
	if !ok {
		return fmt.Errorf("providers.%s.models.%s must be an object", ref.Provider, ref.Model)
	}
	modelMap["hidden"] = hidden
	if err := writeAgentConfigAtomic(path, root); err != nil {
		return err
	}
	if prov := a.catalog.Providers[ref.Provider]; prov != nil {
		if model := prov.Models[ref.Model]; model != nil {
			model.Hidden = hidden
		}
	}
	return nil
}

func (a *Agent) SetProviderHidden(providerID string, hidden bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return fmt.Errorf("cannot change provider visibility while a turn is running")
	}
	path := agentConfigPath(a.home)
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse config %s: %w", path, err)
	}
	providers, ok := root["providers"].(map[string]any)
	if !ok {
		providers = map[string]any{}
		root["providers"] = providers
	}
	providerRaw, ok := providers[providerID]
	if !ok {
		providerRaw = map[string]any{}
		providers[providerID] = providerRaw
	}
	providerMap, ok := providerRaw.(map[string]any)
	if !ok {
		return fmt.Errorf("providers.%s must be an object", providerID)
	}
	providerMap["hidden"] = hidden
	if err := writeAgentConfigAtomic(path, root); err != nil {
		return err
	}
	if prov := a.catalog.Providers[providerID]; prov != nil {
		prov.Hidden = hidden
	}
	return nil
}

// TokenUsage returns cumulative token usage for the session.
func (a *Agent) TokenUsage() TokenReport {
	a.tokensMu.Lock()
	defer a.tokensMu.Unlock()
	return a.buildReportLocked()
}

// --- Session operations ---

// SessionCurrent returns the active session, or zero-value if none is open.
func (a *Agent) SessionCurrent() SessionSummary {
	if !a.store.Active() {
		return SessionSummary{}
	}
	meta, err := a.store.Meta()
	if err != nil {
		return SessionSummary{ID: a.store.SessionID()}
	}
	return SessionSummary{
		ID:           meta.ID,
		CreatedAt:    meta.CreatedAt,
		LastActivity: meta.LastActivity,
		State:        metaState(meta.State),
		ArchivedAt:   meta.ArchivedAt,
		ProjectPath:  meta.ProjectPath,
	}
}

// SessionList returns sessions for the current project filtered by state.
func (a *Agent) SessionList(state string) ([]SessionSummary, error) {
	if state != snapshot.StateActive && state != snapshot.StateArchived {
		return nil, fmt.Errorf("invalid state %q", state)
	}
	infos, err := snapshot.List(a.store.Root(), a.projectRoot, state)
	if err != nil {
		return nil, err
	}
	out := make([]SessionSummary, len(infos))
	for i, info := range infos {
		out[i] = SessionSummary{
			ID:           info.ID,
			CreatedAt:    info.CreatedAt,
			LastActivity: info.LastActivity,
			State:        info.State,
			ArchivedAt:   info.ArchivedAt,
			ProjectPath:  info.ProjectPath,
		}
	}
	return out, nil
}

func (a *Agent) cancelAndWaitIdle() error {
	a.mu.Lock()
	cancel := a.turnCancel
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for i := 0; i < 200; i++ {
		a.mu.Lock()
		busy := a.busy
		a.mu.Unlock()
		if !busy {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for current turn to end")
}

// SessionSwitch closes the current session and loads another.
func (a *Agent) SessionSwitch(id string) error {
	if err := a.cancelAndWaitIdle(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.store.Active() && a.store.SessionID() == id {
		return nil
	}
	if _, err := a.store.Close(); err != nil {
		return err
	}
	if err := a.store.LoadSession(id); err != nil {
		return err
	}
	meta, err := a.store.Meta()
	if err == nil && metaState(meta.State) == snapshot.StateArchived {
		_ = a.store.SetState(snapshot.StateActive)
		_ = a.store.TouchActivity()
	}
	if err := a.loadHistoryIntoLoop(); err != nil {
		return err
	}
	a.populateFileTracker()
	a.loadTokensFromDisk()
	if err := a.reloadLocked(); err != nil {
		return err
	}
	a.restoreModelFromSession()
	a.tokensMu.Lock()
	a.lastContextUsed = 0
	a.tokensMu.Unlock()
	if a.lspDiagnostics != nil {
		a.lspDiagnostics.Reset()
	}
	return nil
}

// SessionNew closes the current session and starts fresh.
func (a *Agent) SessionNew() error {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return fmt.Errorf("cannot start new session while a turn is running")
	}
	defer a.mu.Unlock()

	if _, err := a.store.Close(); err != nil {
		return err
	}
	a.lp.ResetHistory()
	if a.fileTracker != nil {
		a.fileTracker.Reset()
	}
	a.tokensMu.Lock()
	a.tokens = map[string]*TokenEntry{}
	a.tokensMu.Unlock()
	if a.lspDiagnostics != nil {
		a.lspDiagnostics.Reset()
	}
	return nil
}

// SessionArchive archives a session. If it's the current session, close
// first. Returns true if the current session was closed.
func (a *Agent) SessionArchive(id string) (bool, error) {
	sessionsRoot, err := a.currentSessionsRoot()
	if err != nil {
		return false, err
	}
	closedCurrent, err := a.closeIfCurrent(id)
	if err != nil {
		return false, err
	}
	if err := snapshot.ArchiveSession(sessionsRoot, id); err != nil {
		return false, err
	}
	if closedCurrent {
		a.lp.ResetHistory()
		if a.fileTracker != nil {
			a.fileTracker.Reset()
		}
		a.tokensMu.Lock()
		a.tokens = map[string]*TokenEntry{}
		a.tokensMu.Unlock()
	}
	return closedCurrent, nil
}

// SessionDelete removes a session from disk. Returns true if the
// current session was closed.
func (a *Agent) SessionDelete(id string) (bool, error) {
	sessionsRoot, err := a.currentSessionsRoot()
	if err != nil {
		return false, err
	}
	closedCurrent, err := a.closeIfCurrent(id)
	if err != nil {
		return false, err
	}
	if a.memoryStore != nil {
		_ = a.memoryStore.DeleteSessionSummaries(id)
	}
	if err := snapshot.DeleteSession(sessionsRoot, id); err != nil {
		return false, err
	}
	if closedCurrent {
		a.lp.ResetHistory()
		if a.fileTracker != nil {
			a.fileTracker.Reset()
		}
		a.tokensMu.Lock()
		a.tokens = map[string]*TokenEntry{}
		a.tokensMu.Unlock()
	}
	return closedCurrent, nil
}

// SessionMessages returns the persisted messages for the current session.
func (a *Agent) SessionMessages() []DisplayMessage {
	if a.store == nil || !a.store.Active() {
		return nil
	}
	return a.messagesForFrontend()
}

func (a *Agent) currentSessionsRoot() (string, error) {
	proj, err := a.projects.Current()
	if err != nil {
		return "", err
	}
	if proj == nil {
		return "", fmt.Errorf("no project for current directory")
	}
	return a.projects.SessionsRoot(proj.ID), nil
}

func (a *Agent) closeIfCurrent(id string) (bool, error) {
	if !a.store.Active() || a.store.SessionID() != id {
		return false, nil
	}
	if err := a.cancelAndWaitIdle(); err != nil {
		return false, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, err := a.store.Close(); err != nil {
		return false, err
	}
	return true, nil
}

func (a *Agent) messagesForFrontend() []DisplayMessage {
	rec, _ := a.store.LoadCompaction()
	var raw []snapshot.TurnMessages
	if rec != nil {
		raw, _ = a.store.LoadCompleteTurnsAfter(rec.BoundaryTurn)
	} else {
		raw, _ = a.store.LoadCompleteTurns()
	}

	var out []DisplayMessage
	toolStubs := make(map[string]int)

	for _, t := range raw {
		for _, line := range t.Messages {
			var m message.Message
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			switch m.Role {
			case message.RoleSystem:

			case message.RoleUser:
				c := m.TextContent()
				if strings.HasPrefix(c, "<system-signal>") && strings.HasSuffix(c, "</system-signal>") {
					signal := c[len("<system-signal>") : len(c)-len("</system-signal>")]
					if bg, ok := parseBackgroundTerminalSignal(html.UnescapeString(signal)); ok {
						out = append(out, DisplayMessage{
							Type:              "background_process",
							ID:                bg.ID,
							Done:              true,
							Success:           backgroundProcessSuccess(bg),
							Result:            bg.Output,
							BackgroundProcess: bg,
						})
					} else if strings.Contains(signal, "interrupted") {
						out = append(out, DisplayMessage{Type: "system", Content: "interrupted"})
					} else if strings.HasPrefix(signal, "Model switched") {
						out = append(out, DisplayMessage{Type: "system", Content: signal})
					}
				} else {
					out = append(out, DisplayMessage{Type: "user", Content: c, Turn: t.Turn})
				}

			case message.RoleAssistant:
				content := m.TextContent()
				if content == "" {
					content = m.Refusal
				}
				if content != "" {
					out = append(out, DisplayMessage{Type: "assistant", Content: content, Turn: t.Turn})
				}
				for _, tc := range m.ToolCalls {
					toolStubs[tc.ID] = len(out)
					out = append(out, DisplayMessage{
						Type: "tool",
						ID:   tc.ID,
						Name: tc.Function.Name,
						Args: tc.Function.Arguments,
					})
				}

			case message.RoleTool:
				if idx, ok := toolStubs[m.ToolCallID]; ok {
					out[idx].Done = true
					content := m.TextContent()
					out[idx].Success = content != "denied by user" && !strings.HasPrefix(content, "error: ")
					out[idx].Result = content
					if out[idx].Success {
						out[idx].Metadata = displayMetadataForToolCall(out[idx].Name, out[idx].Args, content)
					}
				}
			}
		}
	}
	return out
}

func parseBackgroundTerminalSignal(payload string) (*BackgroundProcessDisplay, bool) {
	const prefix = "Background process "
	if !strings.HasPrefix(payload, prefix) {
		return nil, false
	}
	rest := strings.TrimPrefix(payload, prefix)
	idEnd := strings.Index(rest, " (")
	if idEnd <= 0 {
		return nil, false
	}
	id := rest[:idEnd]
	rest = rest[idEnd+2:]
	if !strings.HasPrefix(rest, "\"") {
		return nil, false
	}
	quoteEnd := endQuotedString(rest)
	if quoteEnd < 0 {
		return nil, false
	}
	command, err := strconv.Unquote(rest[:quoteEnd+1])
	if err != nil {
		return nil, false
	}
	rest = rest[quoteEnd+1:]
	const afterCommand = ") finished: "
	if !strings.HasPrefix(rest, afterCommand) {
		return nil, false
	}
	rest = strings.TrimPrefix(rest, afterCommand)
	const exitMarker = ", exit code "
	reasonEnd := strings.Index(rest, exitMarker)
	if reasonEnd <= 0 {
		return nil, false
	}
	reason := rest[:reasonEnd]
	rest = rest[reasonEnd+len(exitMarker):]
	const outputMarker = ".\nOutput:\n"
	codeEnd := strings.Index(rest, outputMarker)
	if codeEnd <= 0 {
		return nil, false
	}
	exitCode, err := strconv.Atoi(rest[:codeEnd])
	if err != nil {
		return nil, false
	}
	output := rest[codeEnd+len(outputMarker):]
	return &BackgroundProcessDisplay{
		ID:       id,
		Command:  command,
		Reason:   reason,
		ExitCode: exitCode,
		Output:   output,
	}, true
}

func endQuotedString(s string) int {
	for i := 1; i < len(s); i++ {
		if s[i] != '"' {
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 == 0 {
			return i
		}
	}
	return -1
}

func backgroundProcessSuccess(bg *BackgroundProcessDisplay) bool {
	return bg != nil && bg.Reason == string(process.ExitReasonCompleted) && bg.ExitCode == 0
}

func displayMetadataForToolCall(name, args, result string) map[string]any {
	if name != "edit_file" {
		return nil
	}
	if !strings.Contains(result, "lines ") {
		return nil
	}
	return editpreview.MetadataFromArgs(args, result)
}

// --- Snapshot / revert operations ---

// ApplyTurnAction applies a revert/fork action selected from a user message.
// The turn argument is the clicked user turn; this method owns the conversion
// to the lower-level snapshot/history cut points so adapters do not duplicate it.
func (a *Agent) ApplyTurnAction(turn int, action string, alsoRevertCode bool) (TurnActionResult, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return TurnActionResult{}, fmt.Errorf("cannot %s while a turn is running", turnActionVerb(action))
	}
	if !a.store.Active() {
		return TurnActionResult{}, fmt.Errorf("no session open")
	}
	if turn < 1 {
		return TurnActionResult{}, fmt.Errorf("turn must be >= 1")
	}

	prefill := a.userMessageContentForTurn(turn)
	result := TurnActionResult{Action: action, Turn: turn}

	switch action {
	case TurnActionRevertCode:
		target := turn - 1
		result.TargetTurn = target
		if _, err := a.store.RevertCode(target); err != nil {
			return TurnActionResult{}, err
		}
		a.populateFileTracker()
		return result, nil

	case TurnActionRevertHistory:
		target := turn - 1
		result.TargetTurn = target
		result.Prefill = prefill
		result.SessionChanged = true
		if alsoRevertCode {
			if _, err := a.store.RevertCode(target); err != nil {
				return TurnActionResult{}, err
			}
		}
		if err := a.store.RevertHistory(target); err != nil {
			return TurnActionResult{}, err
		}
		if err := a.loadHistoryIntoLoop(); err != nil {
			return TurnActionResult{}, err
		}
		a.populateFileTracker()
		return a.populateTurnActionResult(result), nil

	case TurnActionFork:
		target := turn
		result.TargetTurn = target
		result.SessionChanged = true
		if alsoRevertCode {
			if _, err := a.store.RevertCode(target); err != nil {
				return TurnActionResult{}, err
			}
		}
		newID, _, err := a.store.ForkInto(target)
		if err != nil {
			return TurnActionResult{}, err
		}
		if _, err := a.store.Close(); err != nil {
			return TurnActionResult{}, err
		}
		if err := a.store.LoadSession(newID); err != nil {
			return TurnActionResult{}, err
		}
		if err := a.loadHistoryIntoLoop(); err != nil {
			return TurnActionResult{}, err
		}
		a.populateFileTracker()
		a.loadTokensFromDisk()
		return a.populateTurnActionResult(result), nil

	default:
		return TurnActionResult{}, fmt.Errorf("unknown turn action %q", action)
	}
}

func turnActionVerb(action string) string {
	switch action {
	case TurnActionRevertCode, TurnActionRevertHistory:
		return "revert"
	case TurnActionFork:
		return "fork"
	default:
		return "apply turn action"
	}
}

func (a *Agent) populateTurnActionResult(result TurnActionResult) TurnActionResult {
	result.Session = a.SessionCurrent()
	result.Messages = a.messagesForFrontend()
	result.Tokens = a.TokenUsage()
	return result
}

func (a *Agent) userMessageContentForTurn(turn int) string {
	for _, msg := range a.messagesForFrontend() {
		if msg.Type == "user" && msg.Turn == turn {
			return msg.Content
		}
	}
	return ""
}

// RevertCode restores files to their state at the given turn. After the
// store revert lands the file tracker is repopulated from conversation
// history: stale per-path identities are cleared while paths visible in
// messages keep their "read happened" marker, forcing a re-read on the
// next edit until read_file observes the current disk state. Symmetric
// with RevertHistory.
func (a *Agent) RevertCode(turn int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return fmt.Errorf("cannot revert while a turn is running")
	}
	if !a.store.Active() {
		return fmt.Errorf("no session open")
	}
	if _, err := a.store.RevertCode(turn); err != nil {
		return err
	}
	a.populateFileTracker()
	return nil
}

// RevertHistory truncates conversation after the given turn.
func (a *Agent) RevertHistory(turn int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return fmt.Errorf("cannot revert while a turn is running")
	}
	if !a.store.Active() {
		return fmt.Errorf("no session open")
	}
	if err := a.store.RevertHistory(turn); err != nil {
		return err
	}
	if err := a.loadHistoryIntoLoop(); err != nil {
		return err
	}
	a.populateFileTracker()
	return nil
}

// ForkSession creates a new session branched from the given turn.
func (a *Agent) ForkSession(turn int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return fmt.Errorf("cannot fork while a turn is running")
	}
	if !a.store.Active() {
		return fmt.Errorf("no session open")
	}
	newID, _, err := a.store.ForkInto(turn)
	if err != nil {
		return err
	}
	if _, err := a.store.Close(); err != nil {
		return err
	}
	if err := a.store.LoadSession(newID); err != nil {
		return err
	}
	if err := a.loadHistoryIntoLoop(); err != nil {
		return err
	}
	a.populateFileTracker()
	a.loadTokensFromDisk()
	return nil
}

// SnapshotList returns the timeline of all snapshots in the session.
func (a *Agent) SnapshotList() ([]Snapshot, error) {
	if !a.store.Active() {
		return nil, nil
	}
	turns, err := a.store.ListTurns()
	if err != nil {
		return nil, err
	}
	result := make([]Snapshot, len(turns))
	for i, t := range turns {
		files := make([]SnapshotFile, len(t.Files))
		for j, f := range t.Files {
			files[j] = SnapshotFile{Path: f.OriginalPath, Existed: f.Existed}
		}
		result[i] = Snapshot{Turn: t.Turn, Files: files}
	}
	return result, nil
}

// --- File / project operations ---

// viewerReadAllowed validates that path is safe to read for the inline
// viewer. Returns the canonical path on success, or an error explaining
// the boundary violation.
//
// Boundary rules: (a) only canonical project root is allowed; (b)
// relative paths resolve against project root; (c) absolute paths must
// canonical-resolve inside; (d) ".." allowed only if cleaning +
// canonical resolution stays inside; (e) symlinks resolved and must
// stay inside; (f) hardlinks rejected at safefs FD layer via
// requireRegularFD; (g) sensitive-name leaves rejected.
func (a *Agent) viewerReadAllowed(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.projectRoot, path)
	}
	canonicalRoot, _, err := pathutil.ResolveAbsPath(a.projectRoot)
	if err != nil {
		return "", fmt.Errorf("resolve project root: %w", err)
	}
	canonicalPath, _, err := pathutil.ResolveAbsPath(path)
	if err != nil {
		return "", fmt.Errorf("resolve viewer path: %w", err)
	}
	rel, err := filepath.Rel(canonicalRoot, canonicalPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("viewer path outside project root")
	}
	if permission.IsSensitivePath(canonicalPath) {
		return "", fmt.Errorf("viewer path is sensitive")
	}
	return canonicalPath, nil
}

// ReadFileContent reads a file for the inline viewer. Enforces the
// viewer file-read boundary: paths outside canonical project root,
// escaping symlinks, hardlinks to outside-boundary inodes, and
// sensitive-name leaves are refused before any byte is read.
func (a *Agent) ReadFileContent(path string) (string, error) {
	canonicalPath, err := a.viewerReadAllowed(path)
	if err != nil {
		return "", err
	}
	f, err := safefs.OpenExisting(canonicalPath, os.O_RDONLY)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ProjectName returns the basename of the project directory.
func (a *Agent) ProjectName() string {
	return filepath.Base(a.projectRoot)
}

// ProjectRoot returns the absolute project directory path.
func (a *Agent) ProjectRoot() string {
	return a.projectRoot
}

// Projects returns the project resolver (needed by Wails adapter for
// project switching).
func (a *Agent) Projects() *project.Resolver {
	return a.projects
}

// Store returns the snapshot store (needed by Wails adapter for
// session-changed events).
func (a *Agent) Store() *snapshot.Store {
	return a.store
}

// ProjectCurrent returns the project record for the current cwd.
func (a *Agent) ProjectCurrent() ProjectSummary {
	p, err := a.projects.Current()
	if err != nil || p == nil {
		return ProjectSummary{Path: a.projectRoot, Name: filepath.Base(a.projectRoot)}
	}
	return ProjectSummary{
		ID:           p.ID,
		Name:         p.Name,
		Path:         p.Path,
		CreatedAt:    p.CreatedAt,
		LastActivity: p.LastActivity,
	}
}

// ProjectList returns every known project sorted by last activity.
func (a *Agent) ProjectList() ([]ProjectSummary, error) {
	projects, err := project.ListSortedByActivity(a.projects.Root())
	if err != nil {
		return nil, err
	}
	out := make([]ProjectSummary, len(projects))
	for i, p := range projects {
		out[i] = ProjectSummary{
			ID:           p.ID,
			Name:         p.Name,
			Path:         p.Path,
			CreatedAt:    p.CreatedAt,
			LastActivity: p.LastActivity,
		}
	}
	return out, nil
}

func (a *Agent) modelInfo(ref catalog.ModelRef) ModelInfo {
	info := ModelInfo{Ref: ref.String(), Provider: ref.Provider, Model: ref.Model, Incomplete: true}
	_, model, err := a.catalog.LookupOrIncomplete(ref)
	if err != nil {
		return info
	}
	info.DisplayName = model.Name
	if info.DisplayName == "" {
		info.DisplayName = model.ID
	}
	info.ContextWindow = model.ContextWindow
	info.Cost = model.Cost
	_, info.Incomplete = model.Incomplete()
	return info
}

func newProviderClient(cat *catalog.Catalog, ref catalog.ModelRef) (*provider.Client, *catalog.Model, error) {
	prov, model, err := cat.Lookup(ref)
	if err != nil {
		return nil, nil, err
	}
	var apiKey string
	if prov.Transport.APIKeyEnv != "" {
		apiKey = os.Getenv(prov.Transport.APIKeyEnv)
		if apiKey == "" {
			return nil, nil, fmt.Errorf("%w: %s (for provider %q)", config.ErrMissingEnvVar, prov.Transport.APIKeyEnv, ref.Provider)
		}
	}
	return provider.New(prov, model, apiKey), model, nil
}

func catalogWarningsToPromptWarnings(warnings []catalog.Warning) []prompt.Warning {
	out := make([]prompt.Warning, 0, len(warnings))
	for _, w := range warnings {
		message := w.Message
		if message == "" {
			message = w.Kind
		}
		if w.Provider != "" && w.Model != "" {
			message = fmt.Sprintf("%s/%s: %s", w.Provider, w.Model, message)
		} else if w.Provider != "" {
			message = fmt.Sprintf("%s: %s", w.Provider, message)
		}
		out = append(out, prompt.Warning{Kind: "catalog_" + w.Kind, Message: message})
	}
	return out
}

func metaState(s string) string {
	if s == "" {
		return snapshot.StateActive
	}
	return s
}

type snapshotDiagAdapter struct {
	store *snapshot.Store
}

func (a *snapshotDiagAdapter) CurrentTurn() int {
	return a.store.CurrentTurn()
}

func (a *snapshotDiagAdapter) ListTurns() ([]tool.DiagTurnEntry, error) {
	turns, err := a.store.ListTurns()
	if err != nil {
		return nil, err
	}
	out := make([]tool.DiagTurnEntry, len(turns))
	for i, t := range turns {
		files := make([]tool.DiagFileMeta, len(t.Files))
		for j, f := range t.Files {
			files[j] = tool.DiagFileMeta{OriginalPath: f.OriginalPath}
		}
		out[i] = tool.DiagTurnEntry{Turn: t.Turn, Files: files}
	}
	return out, nil
}

func autoSelectModel(cat *catalog.Catalog) (catalog.ModelRef, bool) {
	refs := cat.VisibleModels()
	if len(refs) == 0 {
		refs = cat.AllModels()
	}
	if len(refs) == 0 {
		return catalog.ModelRef{}, false
	}
	return refs[0], true
}
