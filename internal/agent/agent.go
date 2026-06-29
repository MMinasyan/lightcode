package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MMinasyan/lightcode/internal/adaptation"
	agentcfg "github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/cmdoutput"
	"github.com/MMinasyan/lightcode/internal/compact"
	"github.com/MMinasyan/lightcode/internal/config"
	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/engine/message"
	"github.com/MMinasyan/lightcode/internal/lsp"
	"github.com/MMinasyan/lightcode/internal/memory"
	"github.com/MMinasyan/lightcode/internal/pathutil"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/process"
	"github.com/MMinasyan/lightcode/internal/project"
	"github.com/MMinasyan/lightcode/internal/prompt"
	"github.com/MMinasyan/lightcode/internal/provider"
	"github.com/MMinasyan/lightcode/internal/safefs"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/tool"
)

const tokensFileName = "tokens.json"

// Config carries constructor parameters for New.
type Config struct {
	Cfg         *config.Config
	ConfigPath  string // absolute path the config was loaded from; used for reloads and writes
	ProjectRoot string
	Home        string
	Env         *config.ManagedEnv // live .env state; may be nil in tests
}

// Agent is the shared core used by all adapters (Wails, HTTP, ACP).
type Agent struct {
	rt *runtime

	cfg      *config.Config
	agents   *agentcfg.Config
	catalog  *catalog.Catalog
	store    *snapshot.Store
	projects *project.Resolver
	lp       *loop.Loop
	gate     *permission.Gate
	registry *tool.Registry

	projectRoot string
	home        string
	configPath  string // resolved config path (env override or default)
	agentsPath  string
	env         *config.ManagedEnv

	bundledModels map[string]map[string]struct{} // lazily loaded bundled provider→model id sets, for provenance

	currentRef        coremodel.ModelRef
	contextWindowSize int

	// activeAdapt is the adaptation resolved for the active model (nil = baseline);
	// resolveAdapt maps a bare model id to its adaptation (default adaptation.Match,
	// overridable in tests). Both are written only via setActiveModelLocked /
	// clearActiveModelLocked under rt.mu.
	activeAdapt  *adaptation.Adaptation
	resolveAdapt adaptation.Resolver

	tokensMu        sync.Mutex
	tokens          map[string]*TokenEntry
	lastContextUsed int

	assembler              *prompt.Assembler
	pendingPromptWarnings  []prompt.Warning
	pendingCatalogWarnings []prompt.Warning
	pendingAgentWarnings   []prompt.Warning
	pendingSetupWarnings   []prompt.Warning

	embedderDegraded bool // true when memory embedder failed to initialize

	memoryStore *memory.Store
	memoryHooks agentMemoryHooks

	lspManager     *lsp.Manager
	lspDiagnostics *tool.LSPDiagnostics

	taskToolInst *taskTool

	procMgr         *process.Manager
	pendingExecutor *tool.StagedExecutor

	fileTracker *tool.FileTracker

	warningsMu      sync.Mutex
	warningGroups   map[string][]PromptWarning
	warningSnapshot []PromptWarning
}

type agentSignalSink interface {
	AddSignal(loop.PendingSignal)
	HasWakeSignal() bool
	HasSignal() bool
}

type loopSignalSink struct {
	agent *Agent
}

func (s loopSignalSink) AddSignal(signal loop.PendingSignal) {
	if s.agent == nil || s.agent.lp == nil {
		return
	}
	s.agent.lp.AddPendingSignal(signal)
	if signal.Wake {
		s.agent.ensureRuntime().nudgeSignalScheduler()
	}
}

func (s loopSignalSink) HasWakeSignal() bool {
	return s.agent != nil && s.agent.lp != nil && s.agent.lp.HasPendingWakeSignal()
}

func (s loopSignalSink) HasSignal() bool {
	return s.agent != nil && s.agent.lp != nil && s.agent.lp.HasPendingSignal()
}

type agentMemoryHooks interface {
	Reconcile() error
	IndexSummary(sessionID, projectID, projectName, summary, createdAt, compactionPath string) error
	DeleteSessionSummaries(sessionID string) error
}

type agentUsageRecorder struct {
	agent *Agent
}

func (r agentUsageRecorder) RecordUsage(ev loop.Event) {
	if r.agent != nil {
		r.agent.recordUsage(ev)
	}
}

var newMemoryEmbedder = memory.NewEmbedder

// New constructs an Agent from the given config. It creates the
// provider client, tool registry, permission gate, snapshot store,
// and loop. Call Init after setting up the event handler.
func New(c Config) (*Agent, error) {
	configPath := c.ConfigPath
	if configPath == "" {
		configPath = agentConfigPath(c.Home)
	}
	modelLoader := catalog.NewLoaderWithConfigPath(c.Home, nil, configPath)
	modelLoader.AllowRefresh = func(_ string, prov *catalog.Provider) bool {
		return providerConnected(prov)
	}
	modelCatalog, catalogWarnings, err := modelLoader.Load()
	if err != nil {
		return nil, fmt.Errorf("load model catalog: %w", err)
	}
	agentsPath := agentcfg.PathForConfig(configPath)
	agentTypes, err := agentcfg.Load(agentsPath)
	if err != nil {
		return nil, fmt.Errorf("load agents config: %w", err)
	}

	resolver, err := project.NewResolver(c.Home, c.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("init project resolver: %w", err)
	}
	store, err := snapshot.NewForSessionsRoot("", resolver.Root(), "")
	if err != nil {
		return nil, fmt.Errorf("init snapshot store: %w", err)
	}

	a := &Agent{
		cfg:           c.Cfg,
		agents:        agentTypes,
		catalog:       modelCatalog,
		store:         store,
		projects:      resolver,
		projectRoot:   c.ProjectRoot,
		home:          c.Home,
		configPath:    configPath,
		agentsPath:    agentsPath,
		env:           c.Env,
		warningGroups: make(map[string][]PromptWarning),
		resolveAdapt:  adaptation.Match,
	}
	a.rt = newRuntime(a, runtimeOptions{WorkspaceRoot: c.ProjectRoot})
	rt := a.ensureRuntime()
	events := rt.loopEvents

	gate := permission.NewGate(func(ctx context.Context, req permission.Request) {
		ev := loop.Event{
			Kind:     loop.PermissionRequest,
			ToolName: req.ToolName,
			PermID:   req.ID,
			PermArg:  req.Arg,
			Metadata: map[string]any{
				"resolved_arg":         req.ResolvedArg,
				"can_allow_all":        req.CanAllowAll,
				"disable_project_save": req.DisableProjectSave,
				"batch_index":          req.BatchIndex,
				"batch_total":          req.BatchTotal,
				"batch_files":          req.BatchFiles,
				"batch_resolved_files": req.BatchResolvedFiles,
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
		a.ensureRuntime().mu.Lock()
		turnCtx := a.ensureRuntime().turnCtx
		a.ensureRuntime().mu.Unlock()
		if turnCtx == nil {
			return permission.ResponseDeny
		}
		return gate.AskRequest(turnCtx, req)
	})

	askActionFunc := tool.AskActionFunc(func(ctx context.Context, req permission.Request) permission.ResponseAction {
		return gate.AskRequest(ctx, req)
	})
	rt.permissionPolicy = runtimePermissionPolicy{Check: checkFunc, Ask: askFunc, AskAction: askActionFunc}
	checkPolicy := rt.permissionPolicy.checkFunc()
	askPolicy := rt.permissionPolicy.askFunc()
	askActionPolicy := rt.permissionPolicy.askActionFunc()

	fileTracker := tool.NewFileTracker()
	a.fileTracker = fileTracker

	registry := tool.NewRegistry()
	for _, tl := range tool.CoreToolList(store, fileTracker, c.Cfg.Tools, rt.workspaceRoot, checkPolicy, askPolicy) {
		registry.Register(tl)
	}
	registry.Register(tool.WrapWithPermission(tool.ExecutePending{}, checkPolicy, askPolicy))

	procMgr := process.NewManagerAtRoot(c.Cfg.Tools.MaxBackgroundProcesses, cmdoutput.Options{
		HomeDir:      c.Home,
		SpillPrefix:  "proc_output_",
		MaxBytes:     c.Cfg.Tools.MaxOutputBytes,
		MaxLineChars: c.Cfg.Tools.ReadLineMaxChars,
	}, rt.workspaceRoot)
	a.procMgr = procMgr

	procMgr.SetSessionProvider(func() string {
		if a.store == nil {
			return ""
		}
		return a.store.SessionID()
	})
	procMgr.SetExitHandler(func(event process.ExitEvent) {
		if a.lp != nil {
			a.ensureRuntime().mu.Lock()
			defer a.ensureRuntime().mu.Unlock()
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
			a.ensureRuntime().signalSink.AddSignal(loop.PendingSignal{
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
		}
	})

	// Re-create RunCommand with the process manager.
	rc := tool.NewRunCommandAtRoot(c.Cfg.Tools, c.Home, rt.workspaceRoot, procMgr)
	registry.Register(tool.WrapWithPermission(rc, checkPolicy, askPolicy))
	registry.Register(tool.WrapWithPermission(tool.NewProcessTool(procMgr), checkPolicy, askPolicy))
	registry.Register(tool.WrapWithPermission(tool.Sleep{}, checkPolicy, askPolicy))

	embedder, err := newMemoryEmbedder(c.Home)
	if err != nil {
		// Embedder failure is non-fatal: semantic memory search will be disabled.
		a.embedderDegraded = true
		embedder = nil
	}
	memStore := memory.NewStore(embedder, resolver.Root(), c.Home)
	a.memoryStore = memStore
	a.memoryHooks = memStore

	var projectID, memoriesDir string
	if proj, err := resolver.Current(); err == nil && proj != nil {
		projectID = proj.ID
		memoriesDir = filepath.Join(resolver.Root(), proj.ID, "memories")
	}
	registry.Register(tool.WrapWithPermission(tool.NewSaveMemory(memStore, memoriesDir), checkPolicy, askPolicy))
	registry.Register(tool.WrapWithPermission(tool.NewSearchMemory(memStore, projectID), checkPolicy, askPolicy))
	registry.Register(tool.WrapWithPermission(tool.NewSearchHistory(memStore, projectID), checkPolicy, askPolicy))

	lspMgr := lsp.NewManager(c.ProjectRoot, c.Home)
	a.lspManager = lspMgr

	lspClient := lsp.NewClient(lspMgr)
	diagAdapter := &snapshotDiagAdapter{store: store}
	lspDiag := tool.NewLSPDiagnostics(lspClient, diagAdapter)
	a.lspDiagnostics = lspDiag
	registry.Register(lspDiag)
	registry.Register(tool.NewWorkspaceSymbol(lspClient))

	taggedEvts := make(chan TaggedLoopEvent, 512)

	tt := newTaskTool(taskToolConfig{
		AgentTypes:    agentTypes,
		ParentStore:   store,
		ParentTracker: fileTracker,
		MaxConcurrent: c.Cfg.Subagents.MaxConcurrent,
		TaggedEvents:  taggedEvts,
		ModelCatalog:  modelCatalog,
		ToolsConfig:   c.Cfg.Tools,
		HomeDir:       c.Home,
		WorkspaceRoot: rt.workspaceRoot,
		ProcMgr:       procMgr,
		MemoryStore:   memStore,
		ProjectID:     projectID,
		MemoriesDir:   memoriesDir,
		LSPManager:    lspMgr,
		Check:         checkPolicy,
		Ask:           askPolicy,
		AskAction:     askActionPolicy,
		UsageRecorder: agentUsageRecorder{agent: a},
		ResolveAdapt:  adaptation.Match,
	})
	registry.Register(tt)
	rt.taggedEvents = taggedEvts
	a.taskToolInst = tt

	a.registry = registry

	asm := prompt.New(c.ProjectRoot, c.Home)
	a.assembler = asm
	res := a.assembleSystemPromptLocked()
	a.pendingPromptWarnings = res.Warnings
	a.pendingCatalogWarnings = catalogWarningsToPromptWarnings(catalogWarnings)
	a.pendingAgentWarnings = agentWarningsToPromptWarnings(agentTypes.Warnings())

	l := loop.New(nil, registry, res.Prompt)
	l.SetEvents(events)
	l.SetStore(store)
	l.SetContextTransformer(a)
	l.SetUsageRecorder(agentUsageRecorder{agent: a})
	pendingExecutor := tool.NewStagedExecutorAtRoot(store, fileTracker, c.Cfg.Tools, rt.workspaceRoot, checkPolicy, askActionPolicy)
	a.pendingExecutor = pendingExecutor
	l.SetPendingExecutor(pendingExecutor)
	registry.RegisterPendingCoordinator(tool.NewPendingCoordinator(pendingExecutor))
	a.lp = l

	return a, nil
}

// SetEventHandler sets the callback for agent events. Must be called
// before Init.
func (a *Agent) SetEventHandler(fn func(Event)) {
	a.ensureRuntime().setEventHandler(fn)
}

func (rt *runtime) setEventHandler(fn func(Event)) {
	rt.eventMu.Lock()
	defer rt.eventMu.Unlock()
	rt.onEvent = fn
}

// Init starts background goroutines, runs the session sweep, and
// resumes the most recent session if one exists. ctx controls the
// agent's lifetime.
func (a *Agent) Init(ctx context.Context) {
	a.ensureRuntime().init(ctx)
}

func (rt *runtime) init(ctx context.Context) {
	a := rt.agent
	go rt.drainLoopEvents(ctx)
	go rt.runSignalScheduler(ctx)
	go rt.runQueueDrainer(ctx)
	if a.memoryHooks != nil {
		_ = a.memoryHooks.Reconcile()
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
			a.ensureRuntime().signalSink.AddSignal(loop.PendingSignal{Payload: content, Persist: true})
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
	a.setWarningGroup("agents", a.pendingAgentWarnings)
	a.pendingAgentWarnings = nil
	a.ensureRuntime().mu.Lock()
	a.setWarningGroup("setup", a.setupWarningsLocked())
	a.ensureRuntime().mu.Unlock()
}

func (a *Agent) emitEvent(ev Event) {
	a.ensureRuntime().emitEvent(ev)
}

func (rt *runtime) emitEvent(ev Event) {
	rt.eventMu.RLock()
	fn := rt.onEvent
	rt.eventMu.RUnlock()
	if fn != nil {
		fn(ev)
	}
}

func (a *Agent) nudgeSignalScheduler() {
	a.ensureRuntime().nudgeSignalScheduler()
}

func (rt *runtime) nudgeSignalScheduler() {
	select {
	case rt.signalWake <- struct{}{}:
	default:
	}
}

func (rt *runtime) runSignalScheduler(ctx context.Context) {
	for {
		select {
		case <-rt.signalWake:
			rt.tryStartSignalTurn(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (rt *runtime) tryStartSignalTurn(ctx context.Context) {
	a := rt.agent
	if rt.signalSink == nil || !rt.signalSink.HasWakeSignal() {
		return
	}
	rt.mu.Lock()
	// Queued user input takes priority over a bare signal turn, and a session
	// change in flight must block it. The gate and the busy-claim share one
	// lock hold so nothing can slip a signal turn in between (claimTurnLocked
	// is called while still holding the runtime mutex).
	if rt.busy || rt.transitioning || len(rt.queue) > 0 || a.store == nil || !a.store.Active() || !rt.signalSink.HasWakeSignal() {
		rt.mu.Unlock()
		return
	}
	turnCtx, cancel, err := rt.claimTurnLocked(ctx)
	rt.mu.Unlock()
	if err != nil {
		if !strings.Contains(err.Error(), "turn is already in progress") {
			a.emitEvent(Event{Kind: EventError, Error: err.Error()})
		}
		return
	}
	rt.launchTurn(ctx, turnCtx, cancel, nil)
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
	for _, group := range []string{"setup", "prompt", "catalog", "agents", "lsp", "protocol"} {
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
	a.ensureRuntime().drainLoopEvents(ctx)
}

func (rt *runtime) drainLoopEvents(ctx context.Context) {
	for {
		if rt.drainOneTaggedEvent() {
			continue
		}
		select {
		case ev, ok := <-rt.loopEvents:
			if !ok {
				return
			}
			rt.dispatchLoopEvent(ev)
		case tev, ok := <-rt.taggedEvents:
			if !ok {
				continue
			}
			rt.dispatchTaggedEvent(tev)
		case done := <-rt.loopFlush:
			rt.drainPendingLoopEvents()
			close(done)
		case <-ctx.Done():
			return
		}
	}
}

func (rt *runtime) drainOneTaggedEvent() bool {
	select {
	case tev, ok := <-rt.taggedEvents:
		if !ok {
			return false
		}
		rt.dispatchTaggedEvent(tev)
		return true
	default:
		return false
	}
}

func (a *Agent) drainPendingLoopEvents() {
	a.ensureRuntime().drainPendingLoopEvents()
}

func (rt *runtime) drainPendingLoopEvents() {
	for {
		if rt.drainOneTaggedEvent() {
			continue
		}
		select {
		case ev := <-rt.loopEvents:
			rt.dispatchLoopEvent(ev)
		case tev, ok := <-rt.taggedEvents:
			if ok {
				rt.dispatchTaggedEvent(tev)
			}
		default:
			return
		}
	}
}

func (a *Agent) dispatchLoopEvent(ev loop.Event) {
	a.ensureRuntime().dispatchLoopEvent(ev)
}

func (rt *runtime) dispatchLoopEvent(ev loop.Event) {
	a := rt.agent
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
	case loop.UserMessageDisplay:
		a.emitEvent(Event{
			Kind:   EventUserMessageDisplay,
			Turn:   ev.Turn,
			Result: ev.Result,
		})
	case loop.GenericSystemSignalDisplay:
		a.emitEvent(Event{
			Kind:   EventGenericSystemSignal,
			Turn:   ev.Turn,
			Result: ev.Result,
		})
	case loop.PermissionRequest:
		canAllowAll, _ := ev.Metadata["can_allow_all"].(bool)
		disableProjectSave, _ := ev.Metadata["disable_project_save"].(bool)
		batchIndex, _ := ev.Metadata["batch_index"].(int)
		batchTotal, _ := ev.Metadata["batch_total"].(int)
		batchFiles, _ := ev.Metadata["batch_files"].([]string)
		batchResolvedFiles, _ := ev.Metadata["batch_resolved_files"].([]string)
		resolvedArg, _ := ev.Metadata["resolved_arg"].(string)
		a.emitEvent(Event{
			Kind: EventPermissionRequest,
			PermReq: &PermissionRequest{
				ID:                 ev.PermID,
				ToolName:           ev.ToolName,
				Arg:                ev.PermArg,
				ResolvedArg:        resolvedArg,
				CanAllowAll:        canAllowAll,
				DisableProjectSave: disableProjectSave,
				BatchIndex:         batchIndex,
				BatchTotal:         batchTotal,
				BatchFiles:         batchFiles,
				BatchResolvedFiles: batchResolvedFiles,
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
	a.ensureRuntime().dispatchTaggedEvent(tev)
}

func (rt *runtime) dispatchTaggedEvent(tev TaggedLoopEvent) {
	a := rt.agent
	rt.mu.Lock()
	if rt.seenSessions == nil {
		rt.seenSessions = make(map[string]bool)
	}
	isNew := tev.SessionID != "" && !rt.seenSessions[tev.SessionID]
	if isNew {
		rt.seenSessions[tev.SessionID] = true
	}
	rt.mu.Unlock()
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
	case loop.BackgroundProcessComplete:
		base.Kind = EventBackgroundProcessComplete
		base.Result = ev.Result
		base.IsError = ev.IsError
		if ev.BackgroundProcess != nil {
			base.BackgroundProcess = &BackgroundProcessDisplay{
				ID:       ev.BackgroundProcess.ID,
				Command:  ev.BackgroundProcess.Command,
				Reason:   ev.BackgroundProcess.Reason,
				ExitCode: ev.BackgroundProcess.ExitCode,
				Output:   ev.BackgroundProcess.Output,
			}
		}
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
	if !ev.ModelRef.IsZero() {
		ref = ev.ModelRef
	}
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
	if a.memoryHooks != nil {
		onDelete = func(sessionID string) { _ = a.memoryHooks.DeleteSessionSummaries(sessionID) }
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

// BeforeModelRequest implements the loop context-transform checkpoint.
func (a *Agent) BeforeModelRequest(ctx context.Context, checkpoint loop.ContextTransformCheckpoint) (loop.ContextTransformResult, error) {
	if !checkpoint.Force && !a.shouldAutoCompact() {
		return loop.ContextTransformResult{}, nil
	}
	activeStart, err := a.compactAtCheckpoint(ctx, checkpoint)
	if err != nil {
		if checkpoint.Force {
			return loop.ContextTransformResult{}, err
		}
		a.emitEvent(Event{Kind: EventError, Error: fmt.Sprintf("compaction: %v", err), Turn: checkpoint.Turn})
		return loop.ContextTransformResult{}, nil
	}
	return loop.ContextTransformResult{Transformed: true, ActiveTurnStart: activeStart}, nil
}

// runCompaction summarizes the current conversation through the same context
// transform hook used by model-request checkpoints. It is kept for existing
// focused tests and manual compaction plumbing.
func (a *Agent) runCompaction(ctx context.Context, turnInProgress bool) error {
	activeStart := len(a.lp.Messages())
	checkpoint := loop.ContextTransformCheckpoint{
		Turn:            a.store.CurrentTurn(),
		ActiveTurnStart: activeStart,
		Force:           true,
	}
	if turnInProgress && activeStart > 0 {
		checkpoint.ActiveTurnStart = activeStart
	}
	_, err := a.BeforeModelRequest(ctx, checkpoint)
	return err
}

func (a *Agent) compactAtCheckpoint(ctx context.Context, checkpoint loop.ContextTransformCheckpoint) (int, error) {
	a.emitEvent(Event{Kind: EventCompactionStart})
	refreshSessionNow := false
	defer func() {
		a.emitEvent(Event{Kind: EventCompactionEnd, RefreshSession: refreshSessionNow})
	}()

	messages := a.lp.Messages()
	activeStart := checkpoint.ActiveTurnStart
	if activeStart <= 0 || activeStart > len(messages) {
		activeStart = len(messages)
	}
	if activeStart <= 1 {
		return activeStart, fmt.Errorf("nothing to compact")
	}
	activeTail := activeStart < len(messages)
	toSummarize := append([]message.Message(nil), messages[1:activeStart]...)

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
		return activeStart, err
	}

	boundaryTurn := a.store.CurrentTurn()
	if !checkpoint.Force && checkpoint.Turn > 0 {
		boundaryTurn = checkpoint.Turn - 1
	} else if checkpoint.Force && activeStart < len(messages) && checkpoint.Turn > 0 {
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
		return activeStart, fmt.Errorf("save compaction: %w", err)
	}

	var activeReads []tool.ReadRecord
	if a.fileTracker != nil && activeStart < len(messages) {
		activeReads = activeTailReadRecords(messages[activeStart:], a.fileTracker.Snapshot(), a.cfg.Tools.ReadMaxLines, a.ensureRuntime().workspaceRoot)
	}

	if a.memoryHooks != nil {
		sessionID := a.store.SessionID()
		var projID, projName string
		if proj, pErr := a.projects.Current(); pErr == nil && proj != nil {
			projID = proj.ID
			projName = proj.Name
		}
		compactionPath := filepath.Join(a.store.Dir(), "compaction.json")
		if err := a.memoryHooks.IndexSummary(sessionID, projID, projName, result.Summary, rec.CompactedAt, compactionPath); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: memory index summary: %v\n", err)
		}
	}

	newActiveStart := a.lp.LoadHistoryWithSummaryAndActiveTail(result.Summary, result.SummarizerRef, activeStart)
	if a.fileTracker != nil {
		if len(activeReads) > 0 {
			a.fileTracker.Restore(activeReads)
		} else {
			a.fileTracker.Reset()
		}
	}
	if activeTail {
		a.ensureRuntime().deferSessionRefreshAfterTurn()
	} else {
		refreshSessionNow = true
	}

	a.tokensMu.Lock()
	a.lastContextUsed = 0
	a.tokensMu.Unlock()

	return newActiveStart, nil
}

func (rt *runtime) deferSessionRefreshAfterTurn() {
	rt.mu.Lock()
	rt.sessionRefreshAfterTurn = true
	rt.mu.Unlock()
}

func (rt *runtime) takeDeferredSessionRefreshAfterTurn() bool {
	rt.mu.Lock()
	refresh := rt.sessionRefreshAfterTurn
	rt.sessionRefreshAfterTurn = false
	rt.mu.Unlock()
	return refresh
}

func activeTailReadRecords(tail []message.Message, reads []tool.ReadRecord, defaultLimit int, workspaceRoot string) []tool.ReadRecord {
	if len(tail) == 0 || len(reads) == 0 {
		return nil
	}
	type readKey struct {
		path   string
		offset int
		limit  int
	}
	wanted := map[readKey]bool{}
	for _, msg := range tail {
		if msg.Role != message.RoleAssistant {
			continue
		}
		for _, tc := range msg.ToolCalls {
			if tc.Function.Name != "read_file" {
				continue
			}
			var params map[string]any
			if json.Unmarshal([]byte(tc.Function.Arguments), &params) != nil {
				continue
			}
			path, _ := params["path"].(string)
			if path == "" {
				continue
			}
			resolved, err := pathutil.ResolveFilePathFrom(workspaceRoot, path)
			if err != nil {
				continue
			}
			offset := 1
			if v, ok := params["offset"].(float64); ok {
				offset = int(v)
			}
			if offset < 1 {
				offset = 1
			}
			limit := defaultLimit
			if v, ok := params["limit"].(float64); ok {
				limit = int(v)
			}
			if limit < 1 {
				limit = defaultLimit
			}
			wanted[readKey{path: resolved.CanonicalPath, offset: offset, limit: limit}] = true
		}
	}
	if len(wanted) == 0 {
		return nil
	}
	reversed := make([]tool.ReadRecord, 0, len(wanted))
	seen := map[readKey]bool{}
	for i := len(reads) - 1; i >= 0; i-- {
		read := reads[i]
		key := readKey{path: read.Path, offset: read.Offset, limit: read.Limit}
		if wanted[key] && !seen[key] {
			reversed = append(reversed, read)
			seen[key] = true
		}
	}
	out := make([]tool.ReadRecord, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		out = append(out, reversed[i])
	}
	return out
}

func (a *Agent) summarizerClientAndWindow() (*provider.Adapter, int) {
	ref := a.currentRef
	if compactRef, _, ok := a.resolvedAgentModelLocked("compact"); ok {
		ref = compactRef
	}

	client, model, err := newProviderClient(a.catalog, ref)
	if err != nil && ref != a.currentRef {
		client, model, err = newProviderClient(a.catalog, a.currentRef)
	}
	if err != nil {
		return provider.NewAdapter(provider.New(nil, nil, "")), 0
	}
	return provider.NewAdapter(client), model.ContextWindow
}

// CompactNow triggers manual compaction. Must not be called while busy.
func (a *Agent) CompactNow(ctx context.Context) error {
	return a.ensureRuntime().compactNow(ctx)
}

func (rt *runtime) compactNow(ctx context.Context) error {
	a := rt.agent
	rt.mu.Lock()
	if rt.busy {
		rt.mu.Unlock()
		return fmt.Errorf("cannot compact while a turn is running")
	}
	if !a.store.Active() {
		rt.mu.Unlock()
		return fmt.Errorf("no session open")
	}
	rt.busy = true
	compactCtx, cancel := context.WithCancel(ctx)
	rt.turnCancel = cancel
	rt.turnCtx = compactCtx
	rt.mu.Unlock()

	defer func() {
		rt.mu.Lock()
		rt.busy = false
		rt.turnCancel = nil
		rt.turnCtx = nil
		rt.mu.Unlock()
		cancel()
		// Manual compaction uses the same busy gate as a turn. If input was
		// queued while compaction was running, wake the backend drainer now that
		// the gate is open again.
		rt.nudgeQueueDrainer()
		if rt.signalSink != nil && rt.signalSink.HasWakeSignal() {
			rt.nudgeSignalScheduler()
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
	// Restore the model under rt.mu so the currentRef / contextWindowSize / client
	// writes publish atomically with respect to the signal scheduler and queue
	// drainer started at construction (which read currentRef under the lock),
	// mirroring SessionSwitch. restoreModelFromSession never re-acquires rt.mu.
	a.ensureRuntime().mu.Lock()
	if err := a.reloadLocked(); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: reload config on resume: %v\n", err)
		a.restoreModelFromSession()
		a.ensureRuntime().mu.Unlock()
		return nil
	}
	a.restoreModelFromSession()
	a.ensureRuntime().mu.Unlock()
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

// resolveAdaptation maps a bare model id to its adaptation, defaulting to the
// production matcher when no resolver is injected.
func (a *Agent) resolveAdaptation(modelID string) *adaptation.Adaptation {
	if a.resolveAdapt != nil {
		return a.resolveAdapt(modelID)
	}
	return adaptation.Match(modelID)
}

// setActiveModelLocked publishes a newly active model and its adaptation. It is one
// of the two sole writers of currentRef/contextWindowSize/client/activeAdapt and
// must be called with rt.mu held. It resolves the adaptation, installs it on the
// loop, and reassembles the system prompt immediately so the advertised tools, the
// leak pattern, and the prompt all reflect the new model on its first turn. An
// unmatched model resolves to nil adaptation and stays on the baseline prompt.
func (a *Agent) setActiveModelLocked(ref coremodel.ModelRef, client *provider.Client, model *catalog.Model) {
	a.currentRef = ref
	a.contextWindowSize = model.ContextWindow
	a.activeAdapt = a.resolveAdaptation(ref.Model)
	if a.lp != nil {
		a.lp.SetClient(provider.NewAdapter(client))
		a.lp.SetActiveAdaptation(a.activeAdapt)
	}
	a.applyActiveAdaptationPromptLocked()
}

// clearActiveModelLocked clears the active model and its adaptation, reverting all
// three levers (tools, leak pattern, prompt) to baseline. The other sole writer;
// must be called with rt.mu held.
func (a *Agent) clearActiveModelLocked() {
	a.currentRef = coremodel.ModelRef{}
	a.contextWindowSize = 0
	a.activeAdapt = nil
	if a.lp != nil {
		a.lp.SetClient(nil)
		a.lp.SetActiveAdaptation(nil)
	}
	a.applyActiveAdaptationPromptLocked()
}

// applyActiveAdaptationPromptLocked reassembles the system prompt for the current
// activeAdapt and installs it when changed. Called by the two model chokepoints with
// rt.mu held; the assembler never re-acquires rt.mu. If the active adaptation is nil,
// the baseline prompt cache path avoids UpdateSystemPrompt churn.
func (a *Agent) applyActiveAdaptationPromptLocked() {
	if a.assembler == nil || a.lp == nil {
		return
	}
	if res := a.assembleSystemPromptLocked(); res.Rebuilt {
		a.lp.UpdateSystemPrompt(res.Prompt)
	}
}

// refreshSystemPrompt rebuilds the system prompt for the active model's adaptation
// and the current rules files, installing it when changed. It is the per-turn
// preamble — it runs without rt.mu, which is safe because the model-set paths are
// idle-gated, so activeAdapt is stable while a turn is in flight.
func (a *Agent) refreshSystemPrompt() {
	res := a.assembleSystemPromptLocked()
	if res.Rebuilt {
		a.lp.UpdateSystemPrompt(res.Prompt)
	}
	a.setWarningGroup("prompt", res.Warnings)
}

func (a *Agent) assembleSystemPromptLocked() prompt.Result {
	spec := prompt.Spec{Size: prompt.SizeFull, Memory: true, Adapt: a.activeAdapt}
	if resolved, err := a.resolvedAgentTypeLocked("primary"); err == nil {
		spec.Size = resolved.SystemPrompt
		spec.Body = resolved.Prompt
		spec.Memory = resolved.Memory
	}
	return a.assembler.AssembleForSpec(spec)
}

func (a *Agent) restoreModelFromSession() {
	meta, err := a.store.Meta()
	if err != nil || meta.Provider == "" || meta.Model == "" {
		return
	}
	ref := coremodel.ModelRef{Provider: meta.Provider, Model: meta.Model}
	client, model, err := newProviderClient(a.catalog, ref)
	if err != nil {
		return
	}
	a.setActiveModelLocked(ref, client, model)
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
		a.lp.LoadHistoryWithSummary(rec.Summary, coremodel.ModelRef{Provider: rec.SummarizerProvider, Model: rec.SummarizerModel}, decoded)
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
	a.clearActiveModelLocked()
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
	if a.currentRef.Provider != "" && a.currentRef.Model != "" {
		if err := a.store.SetModel(a.currentRef.Provider, a.currentRef.Model); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: store.SetModel: %v\n", err)
		}
	}
	a.lp.ResetHistory()
	if a.fileTracker != nil {
		a.fileTracker.Reset()
	}
	a.loadTokensFromDisk()
	return nil
}

// --- Public methods (the service API) ---

// Submit is the single entry point for new user input. If the agent is idle
// with an empty queue it starts a turn immediately; otherwise it appends to the
// backend-owned in-memory queue (drained automatically after the active turn
// ends). It rejects with an error while a session change is in flight rather
// than accepting input it would then discard. Any queue mutation emits a
// versioned EventQueueChanged.
func (a *Agent) Submit(ctx context.Context, content string) (SubmitResult, error) {
	return a.ensureRuntime().submit(ctx, content)
}

func (rt *runtime) submit(ctx context.Context, content string) (SubmitResult, error) {
	a := rt.agent
	rt.mu.Lock()
	if rt.transitioning {
		rt.mu.Unlock()
		return SubmitResult{}, fmt.Errorf("session is changing; retry")
	}
	if !rt.busy && len(rt.queue) == 0 {
		turnCtx, cancel, err := rt.claimTurnLocked(ctx)
		if err != nil {
			rt.mu.Unlock()
			return SubmitResult{}, err
		}
		version := rt.queueVersion
		rt.mu.Unlock()
		turn := rt.launchTurn(ctx, turnCtx, cancel, []string{content})
		return SubmitResult{Started: true, Turn: turn, Queue: emptyQueue(), Version: version}, nil
	}
	// Busy or queue non-empty: enqueue and let the drainer pick it up.
	rt.queueSeq++
	rt.queue = append(rt.queue, QueuedItem{ID: fmt.Sprintf("q-%d", rt.queueSeq), Content: content})
	rt.queueVersion++
	items := copyQueue(rt.queue)
	version := rt.queueVersion
	rt.mu.Unlock()
	rt.nudgeQueueDrainer()
	a.emitEvent(Event{Kind: EventQueueChanged, Queue: items, QueueVersion: version})
	return SubmitResult{Started: false, Queue: items, Version: version}, nil
}

// QueueSnapshot returns a versioned copy of the current queue, for adapter
// hydration (subscribe-then-GET: register the queue_changed handler first).
func (a *Agent) QueueSnapshot() QueueState {
	return a.ensureRuntime().queueSnapshot()
}

func (rt *runtime) queueSnapshot() QueueState {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return QueueState{Items: copyQueue(rt.queue), Version: rt.queueVersion}
}

// AppendUserMessage persists a user message as its own complete turn WITHOUT
// running the model. It is a history-seeding primitive (used to script/seed
// conversation state), not a user-input path — live input goes through Submit.
// It still routes through the loop's emit chokepoint, so it is display-ordered.
// Not exposed in the Wails layer.
func (a *Agent) AppendUserMessage(content string) (int, error) {
	return a.ensureRuntime().appendUserMessage(content)
}

func (rt *runtime) appendUserMessage(content string) (int, error) {
	a := rt.agent
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.busy {
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

// claimTurnLocked checks the busy gate and claims a turn (sets busy, builds the
// per-turn context). Caller must hold the runtime mutex. Returns a non-nil error if a turn
// is already in progress or ensureSession fails; on error it leaves busy
// unchanged (never half-claims). launchTurn must be called AFTER unlocking.
func (rt *runtime) claimTurnLocked(ctx context.Context) (context.Context, context.CancelFunc, error) {
	a := rt.agent
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if rt.busy {
		return nil, nil, fmt.Errorf("a turn is already in progress")
	}
	if err := a.ensureSession(); err != nil {
		return nil, nil, err
	}
	a.ensureActiveModelLocked()
	a.setWarningGroup("setup", a.setupWarningsLocked())
	rt.busy = true
	rt.seenSessions = nil
	turnCtx, cancel := context.WithCancel(ctx)
	rt.turnCancel = cancel
	rt.turnCtx = turnCtx
	return turnCtx, cancel, nil
}

// nudgeQueueDrainer wakes the drain goroutine (non-blocking; coalesced).
func (a *Agent) nudgeQueueDrainer() {
	a.ensureRuntime().nudgeQueueDrainer()
}

func (rt *runtime) nudgeQueueDrainer() {
	select {
	case rt.queueWake <- struct{}{}:
	default:
	}
}

func emptyQueue() []QueuedItem {
	return []QueuedItem{}
}

func copyQueue(items []QueuedItem) []QueuedItem {
	if len(items) == 0 {
		return emptyQueue()
	}
	return append([]QueuedItem(nil), items...)
}

// clearQueueLocked empties the queue and resets per-session item IDs, bumping
// the monotonic version when the queue was non-empty. Caller holds the runtime mutex.
// Returns the empty snapshot, its version, and whether the queue changed.
func (rt *runtime) clearQueueLocked() ([]QueuedItem, int, bool) {
	rt.queueSeq = 0
	if len(rt.queue) == 0 {
		return emptyQueue(), rt.queueVersion, false
	}
	rt.queue = nil
	rt.queueVersion++
	return emptyQueue(), rt.queueVersion, true
}

// beginTransition marks a cancel-and-wait session change in flight. While set,
// Submit rejects and the drainer/signal-scheduler will not start a turn, so no
// queued input can launch against the session being swapped. Must be paired
// with a deferred endTransition registered BEFORE cancelAndWaitIdle so it fires
// even on the pre-lock error return.
func (rt *runtime) beginTransition() {
	rt.mu.Lock()
	rt.transitioning = true
	rt.mu.Unlock()
}

// endTransition clears the transitioning flag and emits the current queue
// snapshot (adapters dedup by version, so this is harmless when unchanged and
// delivers the emptied snapshot when the swap cleared the queue). If the queue
// survived (a no-op or failed transition) and a session is active, it re-nudges
// the drainer — a nudge token may have been consumed while transitioning
// blocked the drain, which would otherwise strand the intact queue.
func (rt *runtime) endTransition() {
	a := rt.agent
	rt.mu.Lock()
	rt.transitioning = false
	items := copyQueue(rt.queue)
	version := rt.queueVersion
	active := a.store != nil && a.store.Active()
	rt.mu.Unlock()
	a.emitEvent(Event{Kind: EventQueueChanged, Queue: items, QueueVersion: version})
	if len(items) > 0 && active {
		rt.nudgeQueueDrainer()
	}
}

// runQueueDrainer drains the backend queue after a turn ends. It is woken by
// nudgeQueueDrainer (every Submit-append and every turn-end) and ctx is the
// agent lifetime context from Init.
func (rt *runtime) runQueueDrainer(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		select {
		case <-rt.queueWake:
			if ctx.Err() != nil {
				return
			}
			rt.tryDrainQueue(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// tryDrainQueue starts a turn from the whole queue when the agent is idle, not
// transitioning, and a session is active. All but the last queued message
// become user-only turns; the last starts the model turn. The gate + busy-claim
// share one lock hold so it can never double-start or launch against a session
// being swapped.
func (rt *runtime) tryDrainQueue(ctx context.Context) {
	a := rt.agent
	if ctx.Err() != nil {
		return
	}
	rt.mu.Lock()
	if ctx.Err() != nil || rt.busy || rt.transitioning || a.store == nil || !a.store.Active() || len(rt.queue) == 0 {
		rt.mu.Unlock()
		return
	}
	contents := make([]string, len(rt.queue))
	for i, it := range rt.queue {
		contents[i] = it.Content
	}
	rt.queue = nil
	rt.queueVersion++
	version := rt.queueVersion
	for _, content := range contents[:len(contents)-1] {
		if ctx.Err() != nil {
			rt.mu.Unlock()
			return
		}
		turn := a.store.BeginTurn()
		a.lp.AppendUserMessage(turn, content)
		_ = a.store.MarkTurnComplete(turn)
	}
	if ctx.Err() != nil {
		rt.mu.Unlock()
		return
	}
	rt.busy = true
	rt.seenSessions = nil
	turnCtx, cancel := context.WithCancel(ctx)
	rt.turnCancel = cancel
	rt.turnCtx = turnCtx
	rt.mu.Unlock()

	a.emitEvent(Event{Kind: EventQueueChanged, Queue: emptyQueue(), QueueVersion: version})
	rt.launchTurn(ctx, turnCtx, cancel, []string{contents[len(contents)-1]})
}

func (rt *runtime) launchTurn(ctx context.Context, turnCtx context.Context, cancel context.CancelFunc, contents []string) int {
	a := rt.agent
	turn := a.store.BeginTurn()

	a.emitEvent(Event{Kind: EventTurnStart, Turn: turn})

	rt.mu.Lock()
	a.ensureActiveModelLocked()
	a.setWarningGroup("setup", a.setupWarningsLocked())
	rt.mu.Unlock()

	if a.taskToolInst != nil {
		a.taskToolInst.updateParentState(cancel)
	}

	go func() {
		defer func() {
			rt.mu.Lock()
			rt.busy = false
			rt.turnCancel = nil
			rt.turnCtx = nil
			rt.mu.Unlock()
			cancel()
			// Unconditionally nudge the queue drainer after every turn end: it
			// no-ops on an empty queue, and the unconditional nudge is the
			// reliable retry that defeats cap-1 channel coalescing for items
			// queued mid-turn. The signal scheduler still defers to a non-empty
			// queue (see tryStartSignalTurn).
			rt.nudgeQueueDrainer()
			if rt.signalSink != nil && rt.signalSink.HasWakeSignal() {
				rt.nudgeSignalScheduler()
			}
		}()

		if ctx.Err() != nil {
			return
		}
		a.refreshSystemPrompt()

		if ctx.Err() != nil {
			return
		}
		_, err := a.lp.Run(turnCtx, contents...)

		done := make(chan struct{})
		select {
		case rt.loopFlush <- done:
			select {
			case <-done:
			case <-ctx.Done():
			}
		case <-ctx.Done():
		}

		if err != nil {
			a.emitEvent(Event{Kind: EventError, Error: a.turnErrorMessage(err), Turn: turn})
		}
		a.emitEvent(Event{Kind: EventTurnEnd, Turn: turn, Cancelled: turnCtx.Err() != nil, RefreshSession: rt.takeDeferredSessionRefreshAfterTurn()})
	}()

	return turn
}

func (a *Agent) turnErrorMessage(err error) string {
	if errors.Is(err, loop.ErrNoModelConfigured) {
		return "No model is configured. Select a model to get started."
	}
	return err.Error()
}

// Cancel aborts the current turn.
func (a *Agent) Cancel() error {
	cancel := a.ensureRuntime().turnCancelSnapshot()
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
	return a.ensureRuntime().busySnapshot()
}

func (rt *runtime) turnCancelSnapshot() context.CancelFunc {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.turnCancel
}

func (rt *runtime) busySnapshot() bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.busy
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
	if a.gate != nil {
		canSave, err := a.gate.CanSaveProjectPermission(id)
		if err == nil && !canSave {
			return errors.New("project permission save is disabled for this request")
		}
		if err != nil && !errors.Is(err, permission.ErrUnknownRequest) {
			return err
		}
	}
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
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().busy {
		return fmt.Errorf("cannot switch model while a turn is running")
	}
	ref, err := coremodel.Parse(refStr)
	if err != nil {
		return err
	}
	client, model, err := newProviderClient(a.catalog, ref)
	if err != nil {
		return err
	}
	if err := a.writePrimaryModelLocked(ref); err != nil {
		return err
	}
	a.setActiveModelLocked(ref, client, model)
	a.ensureRuntime().signalSink.AddSignal(loop.PendingSignal{Payload: fmt.Sprintf("Model switched to %s", ref.String()), Persist: true})
	if a.store.Active() {
		if err := a.store.SetModel(ref.Provider, ref.Model); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: store.SetModel: %v\n", err)
		}
	}
	return nil
}

// Reload reloads config and catalog state for future turns.
func (a *Agent) Reload() error {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().busy {
		return fmt.Errorf("cannot reload while a turn is running")
	}
	return a.reloadLocked()
}

func (a *Agent) reloadLocked() error {
	return a.reloadLockedWithRefresh(true)
}

func (a *Agent) reloadLockedNoRefresh() error {
	return a.reloadLockedWithRefresh(false)
}

func (a *Agent) reloadLockedWithRefresh(allowBackgroundDiscovery bool) error {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return err
	}
	agentTypes, err := agentcfg.Load(a.agentsPath)
	if err != nil {
		return fmt.Errorf("load agents config: %w", err)
	}
	modelLoader := catalog.NewLoaderWithConfigPath(a.home, nil, a.configPath)
	modelLoader.AllowRefresh = func(_ string, prov *catalog.Provider) bool {
		if !allowBackgroundDiscovery {
			return false
		}
		return providerConnected(prov)
	}
	modelCatalog, catalogWarnings, err := modelLoader.Load()
	if err != nil {
		return fmt.Errorf("load model catalog: %w", err)
	}

	a.cfg = cfg
	a.setAgentTypesLocked(agentTypes)
	a.catalog = modelCatalog
	if a.taskToolInst != nil {
		a.taskToolInst.setCatalog(modelCatalog)
		a.taskToolInst.setMaxConcurrent(cfg.Subagents.MaxConcurrent)
		a.taskToolInst.setToolsConfig(cfg.Tools)
	}
	if a.procMgr != nil {
		a.procMgr.SetLimits(cfg.Tools.MaxBackgroundProcesses, cmdoutput.Options{
			HomeDir:      a.home,
			SpillPrefix:  "proc_output_",
			MaxBytes:     cfg.Tools.MaxOutputBytes,
			MaxLineChars: cfg.Tools.ReadLineMaxChars,
		})
	}
	a.updateRegisteredToolsConfigLocked(cfg.Tools)
	if !a.modelRefConnected(a.currentRef) {
		a.clearActiveModelLocked()
	}
	a.ensureActiveModelLocked()
	a.setWarningGroup("catalog", catalogWarningsToPromptWarnings(catalogWarnings))
	a.setWarningGroup("setup", a.setupWarningsLocked())
	return nil
}

func (a *Agent) setAgentTypesLocked(agentTypes *agentcfg.Config) {
	a.agents = agentTypes
	if a.taskToolInst != nil {
		a.taskToolInst.setAgentTypes(agentTypes)
	}
	a.setWarningGroup("agents", agentWarningsToPromptWarnings(agentTypes.Warnings()))
}

type ModelCompletion struct {
	ContextWindow   int `json:"context_window"`
	MaxOutputTokens int `json:"max_output_tokens"`
}

func (a *Agent) CompleteModelEntry(refStr string, completion ModelCompletion) error {
	ref, err := coremodel.Parse(refStr)
	if err != nil {
		return err
	}
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().busy {
		return fmt.Errorf("cannot complete model entry while a turn is running")
	}
	if err := a.mutateModelConfig(ref, func(modelMap map[string]any) error {
		if completion.ContextWindow > 0 {
			modelMap["context_window"] = completion.ContextWindow
		}
		if completion.MaxOutputTokens > 0 {
			modelMap["max_output_tokens"] = completion.MaxOutputTokens
		}
		return nil
	}); err != nil {
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

// mutateProviderConfig reads the agent config, navigates to the specified
// provider map, calls mutate, and writes the config atomically.
// Caller must hold the runtime mutex.
func (a *Agent) mutateProviderConfig(providerID string, mutate func(providerMap map[string]any) error) error {
	path := a.configPath
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
	if err := mutate(providerMap); err != nil {
		return err
	}
	if prov := a.catalog.Providers[providerID]; prov != nil && !prov.Builtin {
		if err := validateRawProviderConfig(providerID, providerMap); err != nil {
			return err
		}
	}
	return writeAgentConfigAtomic(path, root)
}

func validateRawProviderConfig(providerID string, providerMap map[string]any) error {
	providerForValidation := map[string]any{}
	encoded, err := json.Marshal(providerMap)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(encoded, &providerForValidation); err != nil {
		return err
	}
	if errs := catalog.ValidateRaw(providerID, providerForValidation, true); len(errs) != 0 {
		return fmt.Errorf("invalid provider config: %s", errs[0].Error())
	}
	return nil
}

// mutateModelConfig reads the agent config, navigates to the specified
// model map, calls mutate, and writes the config atomically.
// Caller must hold the runtime mutex.
func (a *Agent) mutateModelConfig(ref coremodel.ModelRef, mutate func(modelMap map[string]any) error) error {
	return a.mutateProviderConfig(ref.Provider, func(providerMap map[string]any) error {
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
		return mutate(modelMap)
	})
}

// RefreshDiscovery refreshes live model discovery for one enabled provider.
func (a *Agent) RefreshDiscovery(provider string) error {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().busy {
		return fmt.Errorf("cannot refresh discovery while a turn is running")
	}
	return a.refreshDiscoveryLocked(provider)
}

func (a *Agent) refreshDiscoveryLocked(provider string) error {
	_, warnings := catalog.RefreshProviderDiscoveryWithConfigPath(context.Background(), a.home, a.configPath, a.catalog, provider)
	if len(warnings) == 0 {
		return nil
	}
	a.setWarningGroup("catalog", catalogWarningsToPromptWarnings(warnings))
	return fmt.Errorf("refresh discovery for %s: %s", provider, warnings[0].Message)
}

// CurrentModel returns the active model identity and catalog metadata.
func (a *Agent) CurrentModel() ModelInfo {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	a.ensureActiveModelLocked()
	a.setWarningGroup("setup", a.setupWarningsLocked())
	return a.modelInfo(a.currentRef)
}

// modelListFrom builds enriched model list entries from the given refs.
// Caller must hold the runtime mutex.
func (a *Agent) modelListFrom(refs []catalog.ModelRef) []ModelListEntry {
	bundled := a.bundledModelIDsLocked()
	disc := a.discoveryCacheLocked()
	_, primaryModel, _ := a.resolvedAgentModelLocked("primary")
	result := make([]ModelListEntry, 0, len(refs))
	for _, ref := range refs {
		prov, model, err := a.catalog.LookupOrIncomplete(ref)
		if err != nil {
			continue
		}
		if !providerConnected(prov) {
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
			Ref:             ref.String(),
			Provider:        ref.Provider,
			ProviderName:    providerName,
			Model:           ref.Model,
			DisplayName:     displayName,
			ContextWindow:   model.ContextWindow,
			MaxOutputTokens: model.MaxOutputTokens,
			Cost:            model.Cost,
			Hidden:          model.Hidden || prov.Hidden,
			ProviderHidden:  prov.Hidden,
			Incomplete:      incomplete,
			Default:         ref.String() == primaryModel,
			Source:          classifyModelSource(prov, ref.Model, bundled, disc),
		})
	}
	return result
}

// ModelList returns all visible catalog models as flat enriched entries.
func (a *Agent) ModelList() []ModelListEntry {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	return a.modelListFrom(a.catalog.VisibleModels())
}

func (a *Agent) AllModelList() []ModelListEntry {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	return a.modelListFrom(a.catalog.AllModels())
}

func (a *Agent) SetModelHidden(refStr string, hidden bool) error {
	ref, err := coremodel.Parse(refStr)
	if err != nil {
		return err
	}
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().busy {
		return fmt.Errorf("cannot change model visibility while a turn is running")
	}
	if err := a.mutateModelConfig(ref, func(modelMap map[string]any) error {
		modelMap["hidden"] = hidden
		return nil
	}); err != nil {
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
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().busy {
		return fmt.Errorf("cannot change provider visibility while a turn is running")
	}
	if err := a.mutateProviderConfig(providerID, func(providerMap map[string]any) error {
		providerMap["hidden"] = hidden
		return nil
	}); err != nil {
		return err
	}
	if prov := a.catalog.Providers[providerID]; prov != nil {
		prov.Hidden = hidden
	}
	return nil
}

type toolsConfigSetter interface {
	SetToolsConfig(config.ToolsConfig)
}

func (a *Agent) updateRegisteredToolsConfigLocked(cfg config.ToolsConfig) {
	if a.registry != nil {
		for _, name := range []string{"read_file", "write_file", "edit_file", "run_command"} {
			if t, ok := a.registry.Get(name); ok {
				setRegisteredToolConfig(t, cfg)
			}
		}
	}
	if a.pendingExecutor != nil {
		a.pendingExecutor.SetToolsConfig(cfg)
	}
}

func setRegisteredToolConfig(t tool.Tool, cfg config.ToolsConfig) {
	for t != nil {
		if setter, ok := t.(toolsConfigSetter); ok {
			setter.SetToolsConfig(cfg)
			return
		}
		wrapped, ok := t.(interface{ WrappedTool() tool.Tool })
		if !ok {
			return
		}
		t = wrapped.WrappedTool()
	}
}

func (a *Agent) GetRuntimeConfig() RuntimeConfigSettings {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	return runtimeConfigFromConfig(a.cfg)
}

func runtimeConfigFromConfig(cfg *config.Config) RuntimeConfigSettings {
	if cfg == nil {
		return RuntimeConfigSettings{}
	}
	return RuntimeConfigSettings{
		Sessions: RuntimeSessionsConfig{
			ArchiveAfterDays:       cfg.Sessions.ArchiveAfterDays,
			DeleteAfterArchiveDays: cfg.Sessions.DeleteAfterArchiveDays,
		},
		Compaction: RuntimeCompactionConfig{
			ThresholdPct: cfg.Compaction.ThresholdPct,
		},
		Subagents: RuntimeSubagentsConfig{
			MaxConcurrent: cfg.Subagents.MaxConcurrent,
		},
		Tools: RuntimeToolsConfig{
			MaxOutputBytes:         cfg.Tools.MaxOutputBytes,
			ReadMaxLines:           cfg.Tools.ReadMaxLines,
			ReadLineMaxChars:       cfg.Tools.ReadLineMaxChars,
			CommandTimeout:         cfg.Tools.CommandTimeout,
			MaxBackgroundProcesses: cfg.Tools.MaxBackgroundProcesses,
		},
	}
}

func (a *Agent) SetRuntimeConfig(settings RuntimeConfigSettings) error {
	if err := validateRuntimeConfig(settings); err != nil {
		return err
	}
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().busy {
		return fmt.Errorf("cannot change runtime config while a turn is running")
	}
	data, err := os.ReadFile(a.configPath)
	if err != nil {
		return err
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return fmt.Errorf("parse config %s: %w", a.configPath, err)
	}
	sessions := objectMap(root, "sessions")
	sessions["archive_after_days"] = settings.Sessions.ArchiveAfterDays
	sessions["delete_after_archive_days"] = settings.Sessions.DeleteAfterArchiveDays
	compaction := objectMap(root, "compaction")
	compaction["threshold_pct"] = settings.Compaction.ThresholdPct
	delete(compaction, "summarizer_model")
	subagents := objectMap(root, "subagents")
	subagents["max_concurrent"] = settings.Subagents.MaxConcurrent
	delete(subagents, "model")
	tools := objectMap(root, "tools")
	tools["max_output_bytes"] = settings.Tools.MaxOutputBytes
	tools["read_max_lines"] = settings.Tools.ReadMaxLines
	tools["read_line_max_chars"] = settings.Tools.ReadLineMaxChars
	tools["command_timeout"] = settings.Tools.CommandTimeout
	tools["max_background_processes"] = settings.Tools.MaxBackgroundProcesses
	if err := writeAgentConfigAtomic(a.configPath, root); err != nil {
		return err
	}
	return a.reloadLockedNoRefresh()
}

func objectMap(root map[string]any, key string) map[string]any {
	if raw, ok := root[key]; ok {
		if m, ok := raw.(map[string]any); ok {
			return m
		}
	}
	m := map[string]any{}
	root[key] = m
	return m
}

func validateRuntimeConfig(settings RuntimeConfigSettings) error {
	if settings.Sessions.ArchiveAfterDays < 1 || settings.Sessions.ArchiveAfterDays > 365 {
		return fmt.Errorf("sessions.archive_after_days must be between 1 and 365")
	}
	if settings.Sessions.DeleteAfterArchiveDays < 1 || settings.Sessions.DeleteAfterArchiveDays > 365 {
		return fmt.Errorf("sessions.delete_after_archive_days must be between 1 and 365")
	}
	if settings.Compaction.ThresholdPct < 0.1 || settings.Compaction.ThresholdPct > 0.99 {
		return fmt.Errorf("compaction.threshold_pct must be between 0.1 and 0.99")
	}
	if settings.Subagents.MaxConcurrent < 1 || settings.Subagents.MaxConcurrent > 20 {
		return fmt.Errorf("subagents.max_concurrent must be between 1 and 20")
	}
	if settings.Tools.MaxOutputBytes < 1024 || settings.Tools.MaxOutputBytes > 1048576 {
		return fmt.Errorf("tools.max_output_bytes must be between 1024 and 1048576")
	}
	if settings.Tools.ReadMaxLines < 10 || settings.Tools.ReadMaxLines > 10000 {
		return fmt.Errorf("tools.read_max_lines must be between 10 and 10000")
	}
	if settings.Tools.ReadLineMaxChars < 100 || settings.Tools.ReadLineMaxChars > 100000 {
		return fmt.Errorf("tools.read_line_max_chars must be between 100 and 100000")
	}
	if settings.Tools.CommandTimeout < 5 || settings.Tools.CommandTimeout > 600 {
		return fmt.Errorf("tools.command_timeout must be between 5 and 600")
	}
	if settings.Tools.MaxBackgroundProcesses < 1 || settings.Tools.MaxBackgroundProcesses > 50 {
		return fmt.Errorf("tools.max_background_processes must be between 1 and 50")
	}
	return nil
}

func (a *Agent) SetDefaultModel(refStr string) error {
	ref, err := coremodel.Parse(refStr)
	if err != nil {
		return err
	}
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().busy {
		return fmt.Errorf("cannot set default model while a turn is running")
	}
	if _, _, err := a.catalog.LookupOrIncomplete(ref); err != nil {
		return err
	}
	if err := a.writePrimaryModelLocked(ref); err != nil {
		return err
	}
	if a.currentRef.Provider == "" && a.currentRef.Model == "" {
		a.ensureActiveModelLocked()
	}
	a.setWarningGroup("setup", a.setupWarningsLocked())
	return nil
}

func (a *Agent) writePrimaryModelLocked(ref coremodel.ModelRef) error {
	if a.agentsPath == "" {
		a.agentsPath = agentcfg.PathForConfig(a.configPath)
	}
	if err := agentcfg.WriteModel(a.agentsPath, "primary", ref.String()); err != nil {
		return err
	}
	agentTypes, err := agentcfg.Load(a.agentsPath)
	if err != nil {
		return err
	}
	a.setAgentTypesLocked(agentTypes)
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
		ID:              meta.ID,
		CreatedAt:       meta.CreatedAt,
		LastActivity:    meta.LastActivity,
		State:           metaState(meta.State),
		ArchivedAt:      meta.ArchivedAt,
		ProjectPath:     meta.ProjectPath,
		ParentSessionID: meta.ParentSessionID,
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
			ID:              info.ID,
			CreatedAt:       info.CreatedAt,
			LastActivity:    info.LastActivity,
			State:           info.State,
			ArchivedAt:      info.ArchivedAt,
			ProjectPath:     info.ProjectPath,
			ParentSessionID: info.ParentSessionID,
		}
	}
	return out, nil
}

func (a *Agent) cancelAndWaitIdle() error {
	a.ensureRuntime().mu.Lock()
	cancel := a.ensureRuntime().turnCancel
	a.ensureRuntime().mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for i := 0; i < 200; i++ {
		a.ensureRuntime().mu.Lock()
		busy := a.ensureRuntime().busy
		a.ensureRuntime().mu.Unlock()
		if !busy {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for current turn to end")
}

// CloseForProjectSwitch cancels any active turn, closes the current session, and
// clears queued input under the backend transition guard before adapters relaunch
// the process in another project.
func (a *Agent) CloseForProjectSwitch() error {
	a.ensureRuntime().beginTransition()
	defer a.ensureRuntime().endTransition()
	if err := a.cancelAndWaitIdle(); err != nil {
		return err
	}

	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.store.Active() {
		if _, err := a.store.Close(); err != nil {
			return err
		}
	}
	a.ensureRuntime().clearQueueLocked()
	return nil
}

// SessionSwitch closes the current session and loads another.
func (a *Agent) SessionSwitch(id string) error {
	// Mark the transition and register the clear BEFORE cancelAndWaitIdle so it
	// fires on every return — including the pre-lock error return below — and
	// never leaves transitioning stuck true.
	a.ensureRuntime().beginTransition()
	defer a.ensureRuntime().endTransition()
	if err := a.cancelAndWaitIdle(); err != nil {
		return err
	}
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()

	if a.store.Active() && a.store.SessionID() == id {
		return nil // same-session no-op: queue is preserved
	}
	if _, err := a.store.Close(); err != nil {
		return err
	}
	// The old session is now irreversibly detached: clear the queue here, not
	// after LoadSession — a LoadSession failure must not leave the queue bound
	// to a closed (inactive) session, which endTransition could not drain.
	a.ensureRuntime().clearQueueLocked()
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

// resetCurrentSessionStateLocked resets loop history, file tracker, tokens,
// and LSP diagnostics. Caller must hold the runtime mutex.
func (a *Agent) resetCurrentSessionStateLocked() {
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
}

// resetCurrentSessionState acquires the runtime mutex and resets session state.
func (a *Agent) resetCurrentSessionState() {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	a.resetCurrentSessionStateLocked()
}

// SessionNew closes the current session and starts fresh.
func (a *Agent) SessionNew() error {
	// Registered before the lock so it emits after unlock (defer LIFO).
	var clearedVersion int
	var queueCleared bool
	defer func() {
		if queueCleared {
			a.emitEvent(Event{Kind: EventQueueChanged, Queue: emptyQueue(), QueueVersion: clearedVersion})
		}
	}()
	a.ensureRuntime().mu.Lock()
	if a.ensureRuntime().busy {
		a.ensureRuntime().mu.Unlock()
		return fmt.Errorf("cannot start new session while a turn is running")
	}
	defer a.ensureRuntime().mu.Unlock()

	if _, err := a.store.Close(); err != nil {
		return err
	}
	_, clearedVersion, queueCleared = a.ensureRuntime().clearQueueLocked()
	a.resetCurrentSessionStateLocked()
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
		a.resetCurrentSessionState()
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
	if a.memoryHooks != nil {
		_ = a.memoryHooks.DeleteSessionSummaries(id)
	}
	if err := snapshot.DeleteSession(sessionsRoot, id); err != nil {
		return false, err
	}
	if closedCurrent {
		a.resetCurrentSessionState()
	}
	return closedCurrent, nil
}

// SessionMessages returns the persisted messages for the current session.
func (a *Agent) SessionMessages() []DisplayMessage {
	if a.store == nil || !a.store.Active() {
		return nil
	}
	msgs, _ := a.messagesForFrontendForSession("")
	return msgs
}

// SessionMessagesFor returns persisted messages for a session without
// switching the active session.
func (a *Agent) SessionMessagesFor(id string) ([]DisplayMessage, error) {
	if a.store == nil {
		return nil, snapshot.ErrNoSession
	}
	if id == "" {
		return a.SessionMessages(), nil
	}
	return a.messagesForFrontendForSession(id)
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
		return false, nil // not the current session: not a transition, queue untouched
	}
	// Transition begins only once we've decided to actually close the current
	// session; clear registered before cancelAndWaitIdle covers its error path.
	a.ensureRuntime().beginTransition()
	defer a.ensureRuntime().endTransition()
	if err := a.cancelAndWaitIdle(); err != nil {
		return false, err
	}
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if _, err := a.store.Close(); err != nil {
		return false, err
	}
	// Close (no LoadSession follows for archive/delete) is the irreversible
	// change: clear the queue now.
	a.ensureRuntime().clearQueueLocked()
	return true, nil
}

func (a *Agent) messagesForFrontend() []DisplayMessage {
	msgs, _ := a.messagesForFrontendForSession("")
	return msgs
}

func (a *Agent) messagesForFrontendForSession(sessionID string) ([]DisplayMessage, error) {
	var rec *snapshot.CompactionRecord
	var err error
	if sessionID == "" {
		rec, err = a.store.LoadCompaction()
	} else {
		rec, err = a.store.LoadCompactionForSession(sessionID)
	}
	if err != nil {
		return nil, err
	}
	var raw []snapshot.TurnMessages
	if sessionID == "" {
		if rec != nil {
			raw, err = a.store.LoadCompleteTurnsAfterReadOnly(rec.BoundaryTurn)
		} else {
			raw, err = a.store.LoadCompleteTurnsReadOnly()
		}
	} else if rec != nil {
		raw, err = a.store.LoadCompleteTurnsAfterForSessionReadOnly(sessionID, rec.BoundaryTurn)
	} else {
		raw, err = a.store.LoadCompleteTurnsForSessionReadOnly(sessionID)
	}
	if err != nil {
		return nil, err
	}

	var out []DisplayMessage

	for _, t := range raw {
		toolStubs := make(map[string]int)
		legacyStagedFlushAllowed := false
		for _, line := range t.Messages {
			var m message.Message
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			switch m.Role {
			case message.RoleSystem:

			case message.RoleUser:
				c := m.TextContent()
				entries, ok := loop.ParseStagedFlushMessage(m)
				if !ok && legacyStagedFlushAllowed {
					// Compatibility for sessions persisted by the short-lived
					// unmarked wrapper format on this branch. Real typed user
					// prompts are turn-leading messages, so only allow the old
					// text marker after a staged tool result in the same turn.
					entries, ok = loop.ParseStagedFlush(c)
				}
				if ok {
					// <staged-flush> wrapper: overlay the real per-staged results
					// onto the tool stubs (which currently hold "Staged."), so
					// reload matches the live per-tool ToolCallEnd events. Produces
					// no transcript row. New wrappers carry metadata; old wrappers
					// fall back to registry-derived metadata from args+result.
					for _, e := range entries {
						idx, found := toolStubs[e.ID]
						if !found {
							continue
						}
						out[idx].Done = true
						out[idx].Success = !e.IsError
						out[idx].Result = e.Result
						if out[idx].Success && e.Metadata != nil {
							out[idx].Metadata = e.Metadata
						} else if out[idx].Success {
							out[idx].Metadata = a.displayMetadataForToolCall(out[idx].Name, out[idx].Args, e.Result)
						} else {
							out[idx].Metadata = nil
						}
					}
				} else if signal, ok := loop.ParseSystemSignalMessage(m); ok {
					if bg, ok := parseBackgroundTerminalSignal(signal); ok {
						out = append(out, DisplayMessage{
							Type:              "background_process",
							ID:                bg.ID,
							Done:              true,
							Success:           backgroundProcessSuccess(bg),
							Result:            bg.Output,
							BackgroundProcess: bg,
						})
					} else {
						out = append(out, DisplayMessage{
							Type:    "system",
							Content: "System: " + collapseOneLine(signal),
						})
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
					if content == "Staged." {
						legacyStagedFlushAllowed = true
					}
					out[idx].Success = !displayToolResultIsError(m, out[idx].Name, content)
					out[idx].Result = content
					if out[idx].Success {
						out[idx].Metadata = m.DisplayMetadata
						if out[idx].Metadata == nil {
							out[idx].Metadata = a.displayMetadataForToolCall(out[idx].Name, out[idx].Args, content)
						}
						out[idx].SubagentSessionIDs = subagentSessionLinksFromMetadata(out[idx].Metadata)
					}
				}
			}
		}
	}
	return out, nil
}

// collapseOneLine flattens runs of whitespace (including newlines) to single
// spaces and trims leading/trailing whitespace. Mirrors the loop helper so
// reload-derived system-signal strings match live-event strings byte for byte.
func collapseOneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func displayToolResultIsError(msg message.Message, toolName, content string) bool {
	if loop.IsToolResultErrorMessage(msg) {
		return true
	}
	if content == "denied by user" || strings.HasPrefix(content, "error: ") {
		return true
	}
	if toolName == "execute_pending" {
		return strings.HasPrefix(content, "Failed to apply ") ||
			(strings.HasPrefix(content, "Applied ") && strings.Contains(content, " failed."))
	}
	return false
}

func subagentSessionLinksFromMetadata(metadata map[string]any) []SubagentSessionLink {
	if len(metadata) == 0 {
		return nil
	}
	raw, ok := metadata["subagent_session_ids"]
	if !ok {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var links []SubagentSessionLink
	if err := json.Unmarshal(data, &links); err != nil {
		return nil
	}
	return links
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

func (a *Agent) displayMetadataForToolCall(name, args, result string) map[string]any {
	if a == nil || a.registry == nil {
		return nil
	}
	provider, ok := a.registry.DisplayMetadataProvider(name)
	if !ok {
		return nil
	}
	return provider.DisplayMetadata(context.Background(), json.RawMessage(args), result)
}

// --- Snapshot / revert operations ---

// ApplyTurnAction applies a revert/fork action selected from a user message.
// The turn argument is the clicked user turn; this method owns the conversion
// to the lower-level snapshot/history cut points so adapters do not duplicate it.
func (a *Agent) ApplyTurnAction(turn int, action string, alsoRevertCode bool) (TurnActionResult, error) {
	// fork / revert_history change the session; clear the queue at the
	// irreversible store mutation and emit after unlock (defer LIFO).
	var clearedVersion int
	var queueCleared bool
	defer func() {
		if queueCleared {
			a.emitEvent(Event{Kind: EventQueueChanged, Queue: emptyQueue(), QueueVersion: clearedVersion})
		}
	}()
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().busy {
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
		// History irreversibly truncated: the queued input no longer applies.
		_, clearedVersion, queueCleared = a.ensureRuntime().clearQueueLocked()
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
		// Old session irreversibly detached by the fork's Close: clear here,
		// before LoadSession (which may fail and leave no active session).
		_, clearedVersion, queueCleared = a.ensureRuntime().clearQueueLocked()
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
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().busy {
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
	var clearedVersion int
	var queueCleared bool
	defer func() {
		if queueCleared {
			a.emitEvent(Event{Kind: EventQueueChanged, Queue: emptyQueue(), QueueVersion: clearedVersion})
		}
	}()
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().busy {
		return fmt.Errorf("cannot revert while a turn is running")
	}
	if !a.store.Active() {
		return fmt.Errorf("no session open")
	}
	if err := a.store.RevertHistory(turn); err != nil {
		return err
	}
	_, clearedVersion, queueCleared = a.ensureRuntime().clearQueueLocked()
	if err := a.loadHistoryIntoLoop(); err != nil {
		return err
	}
	a.populateFileTracker()
	return nil
}

// ForkSession creates a new session branched from the given turn.
func (a *Agent) ForkSession(turn int) error {
	var clearedVersion int
	var queueCleared bool
	defer func() {
		if queueCleared {
			a.emitEvent(Event{Kind: EventQueueChanged, Queue: emptyQueue(), QueueVersion: clearedVersion})
		}
	}()
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().busy {
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
	_, clearedVersion, queueCleared = a.ensureRuntime().clearQueueLocked()
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

// ManagedEnv returns the live .env state for this agent. It is safe for
// concurrent use. May return nil in tests that did not wire one up.
func (a *Agent) ManagedEnv() *config.ManagedEnv {
	return a.env
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

func (a *Agent) modelInfo(ref coremodel.ModelRef) ModelInfo {
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

func newProviderClient(cat *catalog.Catalog, ref coremodel.ModelRef) (*provider.Client, *catalog.Model, error) {
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

func agentWarningsToPromptWarnings(warnings []agentcfg.Warning) []prompt.Warning {
	out := make([]prompt.Warning, 0, len(warnings))
	for _, w := range warnings {
		out = append(out, prompt.Warning{Kind: w.Kind, Message: w.Error()})
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

// providerConnected reports whether the catalog has at least one connected provider.
// It is a runtime predicate — there is no stored connection flag.
func (a *Agent) providerConnected() bool {
	if a.catalog == nil {
		return false
	}
	for _, prov := range a.catalog.Providers {
		if providerConnected(prov) {
			return true
		}
	}
	return false
}

// providerConnected applies the shared connection predicate with the agent's
// env semantics: LoadDotEnv already ran, so a plain non-empty Getenv covers
// both shell-exported and .env-managed keys.
func providerConnected(prov *catalog.Provider) bool {
	return catalog.ProviderConnected(prov, func(name string) bool {
		return os.Getenv(name) != ""
	})
}

func (a *Agent) modelRefConnected(ref coremodel.ModelRef) bool {
	if a.catalog == nil || ref.Provider == "" || ref.Model == "" {
		return false
	}
	prov, model, err := a.catalog.Lookup(ref)
	if err != nil || model == nil || model.ContextWindow <= 0 {
		return false
	}
	return providerConnected(prov)
}

func (a *Agent) agentResolveContextLocked() agentcfg.ResolveContext {
	ctx := agentcfg.ResolveContext{Home: a.home}
	if a.projects != nil {
		if proj, err := a.projects.Current(); err == nil && proj != nil {
			ctx.ProjectID = proj.ID
		}
	}
	return ctx
}

func (a *Agent) resolvedAgentTypeLocked(name string) (agentcfg.Resolved, error) {
	if a.agents == nil {
		return agentcfg.Resolved{}, fmt.Errorf("agents config is not loaded")
	}
	return a.agents.Resolve(name, a.agentResolveContextLocked())
}

func (a *Agent) resolvedAgentModelLocked(name string) (coremodel.ModelRef, string, bool) {
	resolved, err := a.resolvedAgentTypeLocked(name)
	if err != nil || resolved.Model == "" {
		return coremodel.ModelRef{}, "", false
	}
	ref, err := coremodel.Parse(resolved.Model)
	if err != nil {
		return coremodel.ModelRef{}, resolved.Model, false
	}
	return ref, resolved.Model, true
}

// ensureActiveModelLocked resolves the active model lazily from primary.model
// only if the provider is connected. Caller must hold the runtime mutex.
// If no active model can be resolved, it leaves currentRef empty and returns false.
func (a *Agent) ensureActiveModelLocked() bool {
	if a.modelRefConnected(a.currentRef) {
		return true
	}
	a.clearActiveModelLocked()
	ref, _, ok := a.resolvedAgentModelLocked("primary")
	if !ok || !a.modelRefConnected(ref) {
		return false
	}
	client, model, err := newProviderClient(a.catalog, ref)
	if err != nil {
		return false
	}
	a.setActiveModelLocked(ref, client, model)
	return true
}

// setupWarningsLocked returns the current setup-state warnings based on runtime state.
// Caller must hold the runtime mutex if concurrent mutation is possible.
func (a *Agent) setupWarningsLocked() []prompt.Warning {
	var out []prompt.Warning
	if !a.providerConnected() {
		out = append(out, prompt.Warning{
			Kind:    "setup_no_provider",
			Message: "No provider connected — configure a provider with credentials and at least one usable model.",
		})
	}
	ref, configuredModel, ok := a.resolvedAgentModelLocked("primary")
	if configuredModel == "" {
		out = append(out, prompt.Warning{
			Kind:    "setup_no_model",
			Message: "No model is configured. Select a model to get started.",
		})
	} else if !ok || !a.modelRefConnected(ref) {
		out = append(out, prompt.Warning{
			Kind:    "setup_model_unavailable",
			Message: fmt.Sprintf("Configured model %q is unavailable because its provider is not connected or the model is incomplete.", configuredModel),
		})
	}
	if a.embedderDegraded {
		out = append(out, prompt.Warning{
			Kind:    "setup_embedder_degraded",
			Message: "Memory embedder failed to initialize; semantic memory search is disabled.",
		})
	}
	return out
}
