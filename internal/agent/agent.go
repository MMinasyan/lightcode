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
	"github.com/MMinasyan/lightcode/internal/engine/modelclient"
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

// session holds the mutable state for one live conversation.
type session struct {
	rt *runtime

	store      *snapshot.Store
	lp         *loop.Loop
	registry   *tool.Registry
	transcript *transcript

	activeAgentType   string
	projectID         string
	projectName       string
	projectRoot       string
	currentRef        coremodel.ModelRef
	contextWindowSize int
	activeAdapt       *adaptation.Adaptation
	sessionStart      time.Time
	installedPrompt   string

	busy       bool
	turnCancel context.CancelFunc
	turnCtx    context.Context

	queue         []QueuedItem
	queueVersion  int
	queueSeq      int
	transitioning bool
	seenSessions  map[string]bool

	sessionRefreshAfterTurn bool

	tokensMu        sync.Mutex
	tokens          map[string]*TokenEntry
	lastContextUsed int

	taskToolInst    *taskTool
	pendingExecutor *tool.StagedExecutor
	fileTracker     *tool.FileTracker
	lspDiagnostics  *tool.LSPDiagnostics
}

// Agent is the shared service facade used by all adapters (Wails, HTTP, ACP).
type Agent struct {
	*session
	rt *runtime

	cfg      *config.Config
	agents   *agentcfg.Config
	catalog  *catalog.Catalog
	projects *project.Resolver
	gate     *permission.Gate

	projectRoot string
	home        string
	configPath  string // resolved config path (env override or default)
	agentsPath  string
	env         *config.ManagedEnv

	bundledModels map[string]map[string]struct{} // lazily loaded bundled provider→model id sets, for provenance

	// resolveAdapt maps a bare model id to its adaptation (default
	// adaptation.Match, overridable in tests). activeAdapt lives on the session.
	resolveAdapt adaptation.Resolver

	promptSvc              *prompt.Service
	pendingPromptWarnings  []prompt.Warning
	pendingCatalogWarnings []prompt.Warning
	pendingAgentWarnings   []prompt.Warning
	pendingSetupWarnings   []prompt.Warning

	embedderDegraded bool // true when memory embedder failed to initialize

	memoryStore *memory.Store
	memoryHooks agentMemoryHooks
	// embedder is the one shared embedding model. Memory stores borrow it; the
	// owner closes it exactly once at shutdown.
	embedder *memory.Embedder

	// servicesMu guards lspManagers and detectCtx. Each project owns one LSP
	// manager, keyed by canonical project root and bound to it; the shared
	// process manager and memory store remain owner-wide. detectCtx is set once
	// the owner is running, so detection starts exactly once per manager.
	servicesMu  sync.Mutex
	lspManagers map[string]*lspEntry
	detectCtx   context.Context
	procMgr     *process.Manager

	sessions         map[string]*session
	currentSessionID string

	warningsMu      sync.Mutex
	warningGroups   map[string][]PromptWarning
	warningSnapshot []PromptWarning
}

type agentSignalSink interface {
	AddSignal(loop.PendingSignal)
}

type loopSignalSink struct {
	agent *Agent
}

func (s loopSignalSink) AddSignal(signal loop.PendingSignal) {
	if s.agent == nil {
		return
	}
	rt := s.agent.ensureRuntime()
	rt.mu.Lock()
	// LSP signals have no session attribution yet; keep them on the transitional current unit.
	unit := rt.sessionLocked()
	if unit == nil || unit.lp == nil {
		rt.mu.Unlock()
		return
	}
	unit.lp.AddPendingSignal(signal)
	rt.mu.Unlock()
	if signal.Wake {
		rt.nudgeSignalScheduler()
	}
}

type agentMemoryHooks interface {
	Reconcile() error
	IndexSummary(sessionID, projectID, projectName, summary, createdAt, compactionPath string) error
	DeleteSessionSummaries(sessionID string) error
}

type sessionLoopHooks struct {
	agent *Agent
	unit  *session
}

func (h sessionLoopHooks) BeforeModelRequest(ctx context.Context, checkpoint loop.ContextTransformCheckpoint) (loop.ContextTransformResult, error) {
	if h.agent == nil || h.unit == nil {
		return loop.ContextTransformResult{}, nil
	}
	return h.agent.beforeModelRequestForSession(ctx, h.unit, checkpoint)
}

func (h sessionLoopHooks) RecordUsage(ev loop.Event) {
	if h.agent != nil && h.unit != nil {
		h.agent.recordUsageForSession(h.unit, ev)
	}
}

var newMemoryEmbedder = memory.NewEmbedder

type compactUnitSummarizer struct {
	unit         *session
	systemPrompt string
}

func (s compactUnitSummarizer) Chat(ctx context.Context, req modelclient.ChatRequest) (modelclient.ChatResponse, error) {
	if s.unit == nil || s.unit.lp == nil || s.unit.store == nil {
		return modelclient.ChatResponse{}, fmt.Errorf("compact session is not configured")
	}
	systemPrompt, userContent := compactRequestMessages(req.Messages)
	if strings.TrimSpace(systemPrompt) == "" {
		systemPrompt = s.systemPrompt
	}
	s.unit.lp.ResetHistory()
	s.unit.lp.UpdateSystemPrompt(systemPrompt)
	turn := s.unit.store.BeginTurn()
	if turn == 0 {
		return modelclient.ChatResponse{}, fmt.Errorf("compact session is not active")
	}
	result, err := s.unit.lp.Run(ctx, userContent)
	if err != nil {
		return modelclient.ChatResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return modelclient.ChatResponse{}, err
	}
	return modelclient.ChatResponse{Content: result, HasChoice: true}, nil
}

func (s compactUnitSummarizer) Model() string {
	if s.unit == nil {
		return ""
	}
	return s.unit.currentRef.Model
}

func (s compactUnitSummarizer) ModelRef() coremodel.ModelRef {
	if s.unit == nil {
		return coremodel.ModelRef{}
	}
	return s.unit.currentRef
}

func compactRequestMessages(messages []message.Message) (string, string) {
	var systemPrompt string
	var userParts []string
	for _, msg := range messages {
		switch msg.Role {
		case message.RoleSystem:
			if systemPrompt == "" {
				systemPrompt = msg.TextContent()
			}
		case message.RoleUser:
			userParts = append(userParts, msg.TextContent())
		}
	}
	return systemPrompt, strings.Join(userParts, "\n\n")
}

func isAgentWriteTool(name string) bool {
	return name == "write_file" || name == "edit_file" || name == "apply_patch"
}

type runningUnitLoopConfig struct {
	Client             *provider.Client
	Registry           *tool.Registry
	SystemPrompt       string
	Store              loop.Store
	Events             chan<- loop.Event
	ContextTransformer loop.ContextTransformer
	UsageRecorder      loop.UsageRecorder
	PendingExecutor    tool.PendingExecutor
	ActiveAdaptation   *adaptation.Adaptation
}

type runningUnitConfig struct {
	Runtime           *runtime
	ActiveAgentType   string
	Store             *snapshot.Store
	Loop              runningUnitLoopConfig
	CurrentRef        coremodel.ModelRef
	ProjectID         string
	ProjectName       string
	ProjectRoot       string
	ContextWindowSize int
	SessionStart      time.Time
	InstalledPrompt   string
	TaskTool          *taskTool
	PendingExecutor   *tool.StagedExecutor
	FileTracker       *tool.FileTracker
	LSPDiagnostics    *tool.LSPDiagnostics
}

func newRunningUnit(cfg runningUnitConfig) *session {
	unit := &session{
		rt:                cfg.Runtime,
		store:             cfg.Store,
		registry:          cfg.Loop.Registry,
		transcript:        newTranscript(),
		activeAgentType:   cfg.ActiveAgentType,
		projectID:         cfg.ProjectID,
		projectName:       cfg.ProjectName,
		projectRoot:       cfg.ProjectRoot,
		currentRef:        cfg.CurrentRef,
		contextWindowSize: cfg.ContextWindowSize,
		activeAdapt:       cfg.Loop.ActiveAdaptation,
		sessionStart:      cfg.SessionStart,
		installedPrompt:   cfg.InstalledPrompt,
		taskToolInst:      cfg.TaskTool,
		pendingExecutor:   cfg.PendingExecutor,
		fileTracker:       cfg.FileTracker,
		lspDiagnostics:    cfg.LSPDiagnostics,
	}
	unit.lp = newRunningUnitLoop(cfg.Loop)
	return unit
}

func newRunningUnitLoop(cfg runningUnitLoopConfig) *loop.Loop {
	lp := loop.New(nil, cfg.Registry, cfg.SystemPrompt)
	if cfg.Client != nil {
		lp.SetClient(provider.NewAdapter(cfg.Client))
	}
	if cfg.Events != nil {
		lp.SetEvents(cfg.Events)
	}
	if cfg.Store != nil {
		lp.SetStore(cfg.Store)
	}
	if cfg.ContextTransformer != nil {
		lp.SetContextTransformer(cfg.ContextTransformer)
	}
	if cfg.UsageRecorder != nil {
		lp.SetUsageRecorder(cfg.UsageRecorder)
	}
	if cfg.PendingExecutor != nil {
		lp.SetPendingExecutor(cfg.PendingExecutor)
	}
	if cfg.ActiveAdaptation != nil {
		lp.SetActiveAdaptation(cfg.ActiveAdaptation)
	}
	return lp
}

func (a *Agent) ensureSessionMapLocked() {
	if a.sessions == nil {
		a.sessions = make(map[string]*session)
	}
}

func sessionIDOf(unit *session) string {
	if unit == nil || unit.store == nil || !unit.store.Active() {
		return ""
	}
	return unit.store.SessionID()
}

func (a *Agent) registerLiveSessionLocked(unit *session) {
	id := sessionIDOf(unit)
	if id == "" {
		return
	}
	unit.syncEventOwner()
	a.ensureSessionMapLocked()
	a.sessions[id] = unit
}

func (a *Agent) setCurrentSessionLocked(unit *session) {
	if unit == nil {
		a.currentSessionID = ""
		a.session = &session{rt: a.rt}
		return
	}
	if unit.rt == nil {
		unit.rt = a.rt
	}
	a.session = unit
	a.currentSessionID = sessionIDOf(unit)
	a.registerLiveSessionLocked(unit)
}

func (a *Agent) liveSessionLocked(id string) (*session, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, fmt.Errorf("session id is required")
	}
	a.ensureSessionMapLocked()
	if unit := a.sessions[id]; unit != nil && unit.store != nil && unit.store.Active() && unit.store.SessionID() == id {
		return unit, nil
	}
	if current := sessionIDOf(a.session); current == id {
		a.sessions[id] = a.session
		return a.session, nil
	}
	return nil, fmt.Errorf("unknown session %q", id)
}

func (a *Agent) resolveLiveSession(id string) (*session, error) {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit, err := a.liveSessionLocked(id)
	rt.mu.Unlock()
	return unit, err
}

// resolveRootDriveSession resolves a live session for a root-only drive
// operation — snapshot list, code/history revert, and user fork. Task-child
// and compact-child sessions are never registered as drivable live units, so
// resolveLiveSession already rejects them; the explicit compact-type guard
// keeps the root-only contract at this boundary even if that changes. A
// descendant's edits are recorded through its root's store, not its own.
func (a *Agent) resolveRootDriveSession(id string) (*session, error) {
	unit, err := a.resolveLiveSession(id)
	if err != nil {
		return nil, err
	}
	if isCompactSessionType(unit.activeAgentType) {
		return nil, internalTranscriptSessionError(id)
	}
	return unit, nil
}

func setSessionProject(unit *session, proj *project.Project) {
	if unit == nil || proj == nil {
		return
	}
	unit.projectID = proj.ID
	unit.projectName = proj.Name
	unit.projectRoot = proj.Path
	unit.syncEventOwner()
}

func (a *Agent) setSessionProject(unit *session, proj *project.Project) {
	setSessionProject(unit, proj)
	if unit == nil || unit.taskToolInst == nil || proj == nil || a == nil || a.projects == nil {
		return
	}
	unit.taskToolInst.setProject(proj.ID, filepath.Join(a.projects.Root(), proj.ID, "memories"))
}

func (unit *session) syncEventOwner() {
	if unit == nil || unit.lp == nil || unit.store == nil {
		return
	}
	unit.lp.SetEventOwner(unit.store.SessionID(), unit.projectID)
}

func (a *Agent) permissionCheckForProject(projectID, projectRoot string) tool.CheckFunc {
	cfg := a.cfg
	return tool.CheckFunc(func(toolName, arg string) permission.Decision {
		var local permission.Rules
		if projectID != "" && a.projects != nil {
			local, _ = permission.LoadLocal(a.projects.Root(), projectID)
		} else if a.projects != nil {
			if proj, err := a.projects.Current(); err == nil && proj != nil {
				local, _ = permission.LoadLocal(a.projects.Root(), proj.ID)
			}
		}
		root := projectRoot
		if root == "" {
			root = a.projectRoot
		}
		var global permission.Rules
		if cfg != nil {
			global = cfg.Permissions
		}
		return permission.Check(local, global, toolName, arg, root, a.home, root)
	})
}

func (a *Agent) permissionAskForSession(unitRef func() *session, projectID string, useTurnContext bool) tool.AskFunc {
	return tool.AskFunc(func(ctx context.Context, req permission.Request) permission.ResponseAction {
		return a.askPermissionForSession(ctx, unitRef(), req, projectID, useTurnContext)
	})
}

func (a *Agent) permissionAskActionForSession(unitRef func() *session, projectID string, useTurnContext bool) tool.AskActionFunc {
	return tool.AskActionFunc(func(ctx context.Context, req permission.Request) permission.ResponseAction {
		return a.askPermissionForSession(ctx, unitRef(), req, projectID, useTurnContext)
	})
}

func (a *Agent) askPermissionForSession(ctx context.Context, unit *session, req permission.Request, projectID string, useTurnContext bool) permission.ResponseAction {
	if a.gate == nil {
		return permission.ResponseDeny
	}
	req = permissionRequestForSession(unit, req, projectID)
	waitCtx := ctx
	if useTurnContext {
		waitCtx = nil
		rt := a.ensureRuntime()
		rt.mu.Lock()
		if unit != nil {
			waitCtx = unit.turnCtx
		}
		rt.mu.Unlock()
	}
	if waitCtx == nil {
		return permission.ResponseDeny
	}
	return a.gate.AskRequest(waitCtx, req)
}

func permissionRequestForSession(unit *session, req permission.Request, projectID string) permission.Request {
	if unit != nil {
		if unit.store != nil {
			req.SessionID = unit.store.SessionID()
		}
		if unit.projectID != "" {
			req.ProjectID = unit.projectID
		}
	}
	if req.ProjectID == "" {
		req.ProjectID = projectID
	}
	return req
}

func (a *Agent) refreshCurrentSessionProjectLocked() {
	if a == nil || a.session == nil || a.session.projectID != "" || a.projects == nil {
		return
	}
	if proj, err := a.projects.Current(); err == nil && proj != nil {
		a.setSessionProject(a.session, proj)
	}
}

type lspEntry struct {
	mgr       *lsp.Manager
	detecting bool
}

// lspManagerFor returns the LSP manager for a project root, creating it on first
// use. Managers are keyed by canonical root — equivalent to the project id,
// which derives from it — so a not-yet-created project shares one manager with
// its later id. Detection starts once, when the owner is running.
func (a *Agent) lspManagerFor(projectRoot string) *lsp.Manager {
	key := projectRoot
	if abs, err := filepath.Abs(projectRoot); err == nil {
		key = filepath.Clean(abs)
	}
	a.servicesMu.Lock()
	defer a.servicesMu.Unlock()
	if a.lspManagers == nil {
		a.lspManagers = map[string]*lspEntry{}
	}
	if e, ok := a.lspManagers[key]; ok {
		return e.mgr
	}
	m := lsp.NewManager(projectRoot, a.home)
	m.SetWarningHandler(func(kind, message string) {
		a.addWarning("lsp", prompt.Warning{Kind: kind, Message: message})
	})
	m.SetSignalHandler(func(content string) {
		a.ensureRuntime().signalSink.AddSignal(loop.PendingSignal{Payload: content, Persist: true})
	})
	e := &lspEntry{mgr: m}
	a.lspManagers[key] = e
	a.startDetectLocked(e)
	return m
}

// startDetectLocked starts detection for e exactly once, and only once the owner
// is running. Caller holds servicesMu.
func (a *Agent) startDetectLocked(e *lspEntry) {
	if a.detectCtx != nil && !e.detecting {
		e.detecting = true
		go e.mgr.Detect(a.detectCtx)
	}
}

func (a *Agent) rootRunningUnitLocked(store *snapshot.Store, activeAgentType string, projectID string, projectName string, projectRoot string) (*session, []prompt.Warning, error) {
	if activeAgentType == "" {
		activeAgentType = "primary"
	}
	if projectRoot == "" {
		projectRoot = a.projectRoot
	}
	resolved, err := a.resolvedAgentTypeForProjectLocked(activeAgentType, projectID)
	if err != nil {
		return nil, nil, err
	}
	rt := a.ensureRuntime()
	lspMgr := a.lspManagerFor(projectRoot)
	var unit *session
	unitRef := func() *session { return unit }
	checkPolicy := a.permissionCheckForProject(projectID, projectRoot)
	askPolicy := a.permissionAskForSession(unitRef, projectID, true)
	askActionPolicy := a.permissionAskActionForSession(unitRef, projectID, false)
	fileTracker := tool.NewFileTracker()
	writeDir := strings.TrimSpace(resolved.WriteDir)
	options := tool.CapabilityOptions{WriteDir: writeDir}
	registry := tool.NewRegistry()
	for _, tl := range tool.CoreToolListWithOptions(store, fileTracker, a.cfg.Tools, projectRoot, checkPolicy, askPolicy, options) {
		if resolved.Readonly && writeDir == "" && isAgentWriteTool(tl.Name()) {
			continue
		}
		registry.Register(tl)
	}
	registry.Register(tool.WrapWithPermission(tool.ExecutePending{}, checkPolicy, askPolicy))

	pendingExecutor := tool.NewStagedExecutorAtRootWithOptions(store, fileTracker, a.cfg.Tools, projectRoot, checkPolicy, askActionPolicy, options)

	if rt.taggedEvents == nil {
		rt.taggedEvents = make(chan TaggedLoopEvent, 512)
	}
	memoriesDir := ""
	if projectID != "" && a.projects != nil {
		memoriesDir = filepath.Join(a.projects.Root(), projectID, "memories")
	}
	tt := newTaskTool(taskToolConfig{
		AgentTypes:    a.agents,
		ParentStore:   store,
		ParentTracker: fileTracker,
		MaxConcurrent: a.cfg.Subagents.MaxConcurrent,
		TaggedEvents:  rt.taggedEvents,
		ModelCatalog:  a.catalog,
		ToolsConfig:   a.cfg.Tools,
		HomeDir:       a.home,
		WorkspaceRoot: projectRoot,
		ProcMgr:       a.procMgr,
		MemoryStore:   a.memoryStore,
		ProjectID:     projectID,
		MemoriesDir:   memoriesDir,
		LSPManager:    lspMgr,
		Check:         checkPolicy,
		Ask:           askPolicy,
		AskAction:     askActionPolicy,
		ResolveAdapt:  a.resolveAdapt,
	})
	registry.Register(tt)

	processes := a.procMgr.ForSession(func() string { return sessionIDOf(unitRef()) })
	rc := tool.NewRunCommandAtRoot(a.cfg.Tools, a.home, projectRoot, processes)
	if resolved.Readonly {
		registry.Register(tool.WrapWithPermission(tool.NewReadOnlyRunCommand(rc), checkPolicy, askPolicy))
	} else {
		registry.Register(tool.WrapWithPermission(rc, checkPolicy, askPolicy))
	}
	registry.Register(tool.WrapWithPermission(tool.NewProcessTool(processes), checkPolicy, askPolicy))
	registry.Register(tool.WrapWithPermission(tool.Sleep{}, checkPolicy, askPolicy))
	registry.Register(tool.WrapWithPermission(tool.NewSaveMemory(a.memoryStore, memoriesDir), checkPolicy, askPolicy))
	registry.Register(tool.WrapWithPermission(tool.NewSearchMemory(a.memoryStore, projectID), checkPolicy, askPolicy))
	registry.Register(tool.WrapWithPermission(tool.NewSearchHistory(a.memoryStore, projectID), checkPolicy, askPolicy))

	lspClient := lsp.NewClient(lspMgr)
	lspDiag := tool.NewLSPDiagnostics(lspClient, &snapshotDiagAdapter{store: store})
	registry.Register(lspDiag)
	registry.Register(tool.NewWorkspaceSymbol(lspClient))

	sessionStart := time.Now()
	promptUnit := &session{activeAgentType: activeAgentType, projectID: projectID, projectName: projectName, projectRoot: projectRoot, sessionStart: sessionStart}
	res := a.assembleSystemPromptForSessionLocked(promptUnit)
	unit = newRunningUnit(runningUnitConfig{
		Runtime:         rt,
		ActiveAgentType: activeAgentType,
		ProjectID:       projectID,
		ProjectName:     projectName,
		ProjectRoot:     projectRoot,
		SessionStart:    sessionStart,
		InstalledPrompt: res.Prompt,
		Store:           store,
		TaskTool:        tt,
		PendingExecutor: pendingExecutor,
		FileTracker:     fileTracker,
		LSPDiagnostics:  lspDiag,
		Loop: runningUnitLoopConfig{
			Registry:        registry,
			SystemPrompt:    res.Prompt,
			Store:           store,
			Events:          rt.loopEvents,
			PendingExecutor: pendingExecutor,
		},
	})
	unit.lp.SetContextTransformer(sessionLoopHooks{agent: a, unit: unit})
	unit.lp.SetUsageRecorder(sessionLoopHooks{agent: a, unit: unit})
	tt.usageRecorder = sessionLoopHooks{agent: a, unit: unit}
	registry.RegisterPendingCoordinator(tool.NewPendingCoordinator(pendingExecutor))
	return unit, res.Warnings, nil
}

func (a *Agent) compactRunningUnitForSession(parent *session) (*session, int, error) {
	if parent == nil || parent.store == nil {
		return nil, 0, fmt.Errorf("no session open")
	}
	parentSessionID := parent.store.SessionID()
	if parentSessionID == "" {
		return nil, 0, fmt.Errorf("no session open")
	}
	projectRoot := parent.projectRoot
	if projectRoot == "" {
		projectRoot = parent.store.ProjectPath()
	}
	if projectRoot == "" {
		projectRoot = a.projectRoot
	}

	rt := a.ensureRuntime()
	rt.mu.Lock()
	ref := parent.currentRef
	if compactRef, _, ok := a.resolvedAgentModelLocked("compact"); ok {
		ref = compactRef
	}
	compactPrompt := compact.DefaultSummarizerPrompt
	if resolved, err := a.resolvedAgentTypeForProjectLocked("compact", parent.projectID); err == nil && strings.TrimSpace(resolved.Prompt) != "" {
		compactPrompt = strings.TrimSpace(resolved.Prompt)
	}
	client, model, err := newProviderClient(a.catalog, ref)
	if err != nil && ref != parent.currentRef {
		ref = parent.currentRef
		client, model, err = newProviderClient(a.catalog, ref)
	}
	window := 0
	if err == nil && model != nil {
		window = model.ContextWindow
	}
	rt.mu.Unlock()
	if err != nil {
		return nil, 0, err
	}
	if window <= 0 {
		window = parent.contextWindowSize
	}

	projectsRoot := ""
	if a.projects != nil {
		projectsRoot = a.projects.Root()
	}
	store, err := snapshot.NewForSessionsRoot(parent.store.Root(), projectsRoot, parent.projectID)
	if err != nil {
		return nil, 0, err
	}
	if err := store.BeginChildSession(projectRoot, parentSessionID); err != nil {
		return nil, 0, err
	}
	if err := store.SetActiveAgentType("compact"); err != nil {
		_, _ = store.Close()
		return nil, 0, err
	}
	if ref.Provider != "" || ref.Model != "" {
		if err := store.SetModel(ref.Provider, ref.Model); err != nil {
			_, _ = store.Close()
			return nil, 0, err
		}
	}

	unit := newRunningUnit(runningUnitConfig{
		ActiveAgentType:   "compact",
		ProjectID:         parent.projectID,
		ProjectName:       parent.projectName,
		ProjectRoot:       projectRoot,
		Store:             store,
		CurrentRef:        ref,
		ContextWindowSize: window,
		Loop: runningUnitLoopConfig{
			Client:       client,
			Registry:     tool.NewRegistry(),
			SystemPrompt: compactPrompt,
			Store:        store,
		},
	})
	return unit, window, nil
}

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
		session:       &session{store: store},
		cfg:           c.Cfg,
		agents:        agentTypes,
		catalog:       modelCatalog,
		projects:      resolver,
		projectRoot:   c.ProjectRoot,
		home:          c.Home,
		configPath:    configPath,
		agentsPath:    agentsPath,
		env:           c.Env,
		warningGroups: make(map[string][]PromptWarning),
		resolveAdapt:  adaptation.Match,
		promptSvc:     prompt.NewService(c.Home),
	}
	a.rt = newRuntime(a, runtimeOptions{WorkspaceRoot: c.ProjectRoot})
	rt := a.ensureRuntime()
	events := rt.loopEvents

	gate := permission.NewGate(func(ctx context.Context, req permission.Request) {
		ev := loop.Event{
			Kind:      loop.PermissionRequest,
			SessionID: req.SessionID,
			ProjectID: req.ProjectID,
			ToolName:  req.ToolName,
			PermID:    req.ID,
			PermArg:   req.Arg,
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

	proj := &project.Project{}
	if p, err := resolver.Current(); err == nil && p != nil {
		proj = p
	}

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
		rt := a.ensureRuntime()
		rt.mu.Lock()
		var unit *session
		if event.SessionID != "" {
			a.ensureSessionMapLocked()
			unit = a.sessions[event.SessionID]
			if unit == nil && sessionIDOf(a.session) == event.SessionID {
				unit = a.session
			}
		} else {
			unit = rt.sessionLocked()
		}
		if unit == nil || unit.lp == nil || unit.store == nil || !unit.store.Active() || (event.SessionID != "" && sessionIDOf(unit) != event.SessionID) {
			rt.mu.Unlock()
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
		unit.lp.AddPendingSignal(loop.PendingSignal{
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
		rt.mu.Unlock()
		rt.nudgeSignalScheduler()
	})

	embedder, err := newMemoryEmbedder(c.Home)
	if err != nil {
		// Embedder failure is non-fatal: semantic memory search will be disabled.
		a.embedderDegraded = true
		embedder = nil
	}
	a.embedder = embedder
	memStore := memory.NewStore(embedder, resolver.Root(), c.Home)
	a.memoryStore = memStore
	a.memoryHooks = memStore

	rt.mu.Lock()
	unit, promptWarnings, err := a.rootRunningUnitLocked(store, "primary", proj.ID, proj.Name, c.ProjectRoot)
	rt.mu.Unlock()
	if err != nil {
		return nil, err
	}
	a.session = unit
	a.pendingPromptWarnings = promptWarnings
	a.pendingCatalogWarnings = catalogWarningsToPromptWarnings(catalogWarnings)
	a.pendingAgentWarnings = agentWarningsToPromptWarnings(agentTypes.Warnings())

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

// SubscribeEvents registers an additional event consumer and returns an unsubscribe function.
func (a *Agent) SubscribeEvents(fn func(Event)) func() {
	return a.ensureRuntime().subscribeEvents(fn)
}

func (rt *runtime) subscribeEvents(fn func(Event)) func() {
	if fn == nil {
		return func() {}
	}
	rt.eventMu.Lock()
	if rt.eventSubscribers == nil {
		rt.eventSubscribers = make(map[int]func(Event))
	}
	rt.nextEventSubscriber++
	id := rt.nextEventSubscriber
	rt.eventSubscribers[id] = fn
	rt.eventMu.Unlock()
	return func() {
		rt.eventMu.Lock()
		delete(rt.eventSubscribers, id)
		rt.eventMu.Unlock()
	}
}

// Init starts background goroutines, runs the session sweep, and
// resumes the most recent session if one exists. ctx controls the
// agent's lifetime.
func (a *Agent) Init(ctx context.Context) {
	a.ensureRuntime().init(ctx)
}

func (rt *runtime) init(ctx context.Context) {
	rt.initOnce.Do(func() {
		rt.initOnceLocked(ctx)
	})
}

func (rt *runtime) initOnceLocked(ctx context.Context) {
	a := rt.agent
	// The background goroutines run on an owned context. An explicit shutdown
	// (ShutdownOwner) cancels it after the in-flight turn join so the drainer
	// stays alive to deliver terminal events; host-context cancellation stops
	// them directly, and the bounded join then abandons any blocked delivery.
	rt.ownerCtx, rt.ownerCancel = context.WithCancel(ctx)
	rt.bgWG.Add(4)
	go func() { defer rt.bgWG.Done(); rt.drainLoopEvents(rt.ownerCtx) }()
	go func() { defer rt.bgWG.Done(); rt.runSignalScheduler(rt.ownerCtx) }()
	go func() { defer rt.bgWG.Done(); rt.runQueueDrainer(rt.ownerCtx) }()
	if a.memoryHooks != nil {
		_ = a.memoryHooks.Reconcile()
	}
	a.runSweep()
	if err := a.resumeMostRecent(); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: resume session: %v\n", err)
	}
	go func() { defer rt.bgWG.Done(); a.periodicSweep(rt.ownerCtx) }()

	// Host context cancellation drives the joined owner shutdown.
	go func() {
		<-ctx.Done()
		a.ShutdownOwner()
	}()

	// The owner is now running: record the detection context and start detection
	// once for every project manager created before Init (handlers are installed
	// at creation). Managers created later start detection when built.
	a.servicesMu.Lock()
	a.detectCtx = rt.ownerCtx
	for _, e := range a.lspManagers {
		a.startDetectLocked(e)
	}
	a.servicesMu.Unlock()
	go func() {
		<-ctx.Done()
		a.servicesMu.Lock()
		mgrs := make([]*lsp.Manager, 0, len(a.lspManagers))
		for _, e := range a.lspManagers {
			mgrs = append(mgrs, e.mgr)
		}
		a.servicesMu.Unlock()
		for _, m := range mgrs {
			m.ShutdownAll()
		}
	}()

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
	subscribers := make([]func(Event), 0, len(rt.eventSubscribers))
	for _, sub := range rt.eventSubscribers {
		subscribers = append(subscribers, sub)
	}
	rt.eventMu.RUnlock()
	if fn != nil {
		fn(ev)
	}
	for _, sub := range subscribers {
		sub(ev)
	}
}

// transcriptForSessionID resolves the live coordinator owning a session id, or
// nil for an unknown or sessionless event.
func (a *Agent) transcriptForSessionID(id string) *transcript {
	if id == "" {
		return nil
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if unit := a.sessions[id]; unit != nil {
		return unit.transcript
	}
	if sessionIDOf(a.session) == id {
		return a.session.transcript
	}
	return nil
}

// feedTranscript folds one delivered event into a session coordinator under its
// own seqMu. A nil coordinator (sessionless or unknown session) is a no-op.
func feedTranscript(tr *transcript, ev Event) {
	if tr == nil {
		return
	}
	tr.seqMu.Lock()
	tr.feedLocked(ev)
	tr.seqMu.Unlock()
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
	for {
		if ctx.Err() != nil {
			return
		}
		rt.mu.Lock()
		unit := rt.nextWakeableSessionLocked()
		if unit == nil {
			rt.mu.Unlock()
			return
		}
		turnCtx, cancel, err := rt.claimTurnLocked(ctx, unit)
		rt.mu.Unlock()
		if err != nil {
			if !errors.Is(err, errOwnerClosed) && !strings.Contains(err.Error(), "turn is already in progress") {
				a.emitEvent(Event{Kind: EventError, Error: err.Error()})
			}
			return
		}
		rt.launchTurn(ctx, unit, turnCtx, cancel, nil)
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
	sessionID := ev.SessionID
	projectID := ev.ProjectID
	// The coordinator sequences every root display row from the same delivered
	// event, so it stays consistent with what adapters receive.
	tr := a.transcriptForSessionID(sessionID)
	emit := func(out Event) {
		a.emitEvent(out)
		feedTranscript(tr, out)
	}
	switch ev.Kind {
	case loop.TextDelta:
		emit(Event{Kind: EventTextDelta, SessionID: sessionID, ProjectID: projectID, Result: ev.Result})
	case loop.ToolCallStart:
		emit(Event{
			SessionID:  sessionID,
			ProjectID:  projectID,
			Kind:       EventToolCallStart,
			ToolCallID: ev.ToolCallID,
			ToolName:   ev.ToolName,
			Args:       ev.Args,
		})
	case loop.ToolCallEnd:
		emit(Event{
			SessionID:  sessionID,
			ProjectID:  projectID,
			Kind:       EventToolCallEnd,
			ToolCallID: ev.ToolCallID,
			ToolName:   ev.ToolName,
			Args:       ev.Args,
			IsError:    ev.IsError,
			Result:     ev.Result,
			Metadata:   ev.Metadata,
		})
	case loop.BackgroundProcessComplete:
		emit(Event{
			SessionID:         sessionID,
			ProjectID:         projectID,
			Kind:              EventBackgroundProcessComplete,
			Result:            ev.Result,
			IsError:           ev.IsError,
			Turn:              ev.Turn,
			BackgroundProcess: agentBackgroundProcessDisplay(ev.BackgroundProcess),
		})
	case loop.UserMessageDisplay:
		emit(Event{
			SessionID: sessionID,
			ProjectID: projectID,
			Kind:      EventUserMessageDisplay,
			Turn:      ev.Turn,
			Result:    ev.Result,
		})
	case loop.GenericSystemSignalDisplay:
		emit(Event{
			SessionID: sessionID,
			ProjectID: projectID,
			Kind:      EventGenericSystemSignal,
			Turn:      ev.Turn,
			Result:    ev.Result,
		})
	case loop.PermissionRequest:
		a.emitEvent(Event{
			SessionID: sessionID,
			ProjectID: projectID,
			Kind:      EventPermissionRequest,
			PermReq:   permissionRequestFromLoopEvent(ev, sessionID, projectID),
		})
	case loop.Warning:
		kind, _ := ev.Metadata["kind"].(string)
		if kind == "" {
			kind = "protocol_warning"
		}
		a.addWarning("protocol", prompt.Warning{Kind: kind, Message: ev.Result})
	}
}

func permissionRequestFromLoopEvent(ev loop.Event, sessionID, projectID string) *PermissionRequest {
	canAllowAll, _ := ev.Metadata["can_allow_all"].(bool)
	disableProjectSave, _ := ev.Metadata["disable_project_save"].(bool)
	batchIndex, _ := ev.Metadata["batch_index"].(int)
	batchTotal, _ := ev.Metadata["batch_total"].(int)
	batchFiles, _ := ev.Metadata["batch_files"].([]string)
	batchResolvedFiles, _ := ev.Metadata["batch_resolved_files"].([]string)
	resolvedArg, _ := ev.Metadata["resolved_arg"].(string)
	return &PermissionRequest{
		ID:                 ev.PermID,
		SessionID:          sessionID,
		ProjectID:          projectID,
		ToolName:           ev.ToolName,
		Arg:                ev.PermArg,
		ResolvedArg:        resolvedArg,
		CanAllowAll:        canAllowAll,
		DisableProjectSave: disableProjectSave,
		BatchIndex:         batchIndex,
		BatchTotal:         batchTotal,
		BatchFiles:         batchFiles,
		BatchResolvedFiles: batchResolvedFiles,
	}
}

func (a *Agent) dispatchTaggedEvent(tev TaggedLoopEvent) {
	a.ensureRuntime().dispatchTaggedEvent(tev)
}

func (rt *runtime) dispatchTaggedEvent(tev TaggedLoopEvent) {
	a := rt.agent
	rt.mu.Lock()
	var unit *session
	if tev.ParentSessionID != "" {
		unit = a.sessions[tev.ParentSessionID]
	}
	isNew := false
	if unit != nil {
		if unit.seenSessions == nil {
			unit.seenSessions = make(map[string]bool)
		}
		isNew = tev.SessionID != "" && !unit.seenSessions[tev.SessionID]
		if isNew {
			unit.seenSessions[tev.SessionID] = true
		}
	}
	rt.mu.Unlock()
	if isNew {
		a.emitEvent(Event{
			SessionID:         tev.SessionID,
			ParentSessionID:   tev.ParentSessionID,
			ProjectID:         tev.ProjectID,
			Kind:              EventSubagentStart,
			SubagentSessionID: tev.SessionID,
			TaskIndex:         tev.TaskIndex,
			ToolCallID:        tev.ToolCallID,
		})
	}

	ev := tev.Event
	projectID := tev.ProjectID
	if projectID == "" {
		projectID = ev.ProjectID
	}
	base := Event{
		SessionID:         tev.SessionID,
		ParentSessionID:   tev.ParentSessionID,
		ProjectID:         projectID,
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
	case loop.PermissionRequest:
		base.Kind = EventPermissionRequest
		base.PermReq = permissionRequestFromLoopEvent(ev, tev.SessionID, projectID)
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
	a.recordUsageForSession(a.session, ev)
}

func (a *Agent) recordUsageForSession(unit *session, ev loop.Event) {
	if unit == nil {
		return
	}
	ref := unit.currentRef
	if !ev.ModelRef.IsZero() {
		ref = ev.ModelRef
	}
	prov := ref.Provider
	model := ev.Model
	if model == "" {
		model = ref.Model
	}
	key := prov + "/" + model

	unit.tokensMu.Lock()
	if unit.tokens == nil {
		unit.tokens = map[string]*TokenEntry{}
	}
	entry, ok := unit.tokens[key]
	if !ok {
		entry = &TokenEntry{Provider: prov, Model: model, Known: true}
		unit.tokens[key] = entry
	}
	entry.Cache += ev.Cache
	entry.Input += ev.Input
	entry.Output += ev.Output
	if ev.UsageKnown {
		unit.lastContextUsed = ev.Cache + ev.Input
	}
	a.persistTokensForSessionLocked(unit)
	report := a.buildReportForSessionLocked(unit)
	unit.tokensMu.Unlock()

	a.emitEvent(Event{
		SessionID:        sessionIDOf(unit),
		ProjectID:        unit.projectID,
		Kind:             EventUsage,
		Model:            model,
		Cache:            ev.Cache,
		Input:            ev.Input,
		Output:           ev.Output,
		UsageKnown:       ev.UsageKnown,
		CumulativeTokens: &report,
	})
}

func (a *Agent) buildReportLocked() TokenReport {
	return a.buildReportForSessionLocked(a.session)
}

func (a *Agent) buildReportForSessionLocked(unit *session) TokenReport {
	if unit == nil {
		return TokenReport{}
	}
	total := TokenEntry{Known: true}
	per := make([]TokenEntry, 0, len(unit.tokens))
	for _, e := range unit.tokens {
		per = append(per, *e)
		total.Cache += e.Cache
		total.Input += e.Input
		total.Output += e.Output
	}
	return TokenReport{
		Total:         total,
		PerModel:      per,
		ContextUsed:   unit.lastContextUsed,
		ContextWindow: unit.contextWindowSize,
	}
}

func (a *Agent) persistTokensLocked() {
	a.persistTokensForSessionLocked(a.session)
}

func (a *Agent) persistTokensForSessionLocked(unit *session) {
	if unit == nil || unit.store == nil || !unit.store.Active() {
		return
	}
	entries := make([]TokenEntry, 0, len(unit.tokens))
	for _, e := range unit.tokens {
		entries = append(entries, *e)
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return
	}
	data = append(data, '\n')
	_ = os.WriteFile(filepath.Join(unit.store.Dir(), tokensFileName), data, 0o600)
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
	if _, _, err := snapshot.SweepAllProjects(a.projects.Root(), cfg, onDelete, a.lockLifecycle); err != nil {
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
	return a.shouldAutoCompactForSession(a.session)
}

func (a *Agent) shouldAutoCompactForSession(unit *session) bool {
	if unit == nil {
		return false
	}
	if !a.cfg.Compaction.Enabled {
		return false
	}
	unit.tokensMu.Lock()
	used := unit.lastContextUsed
	window := unit.contextWindowSize
	unit.tokensMu.Unlock()
	if window <= 0 || used <= 0 {
		return false
	}
	return float64(used)/float64(window) >= a.cfg.Compaction.ThresholdPct
}

// BeforeModelRequest implements the loop context-transform checkpoint.
func (a *Agent) BeforeModelRequest(ctx context.Context, checkpoint loop.ContextTransformCheckpoint) (loop.ContextTransformResult, error) {
	return a.beforeModelRequestForSession(ctx, a.session, checkpoint)
}

func (a *Agent) beforeModelRequestForSession(ctx context.Context, unit *session, checkpoint loop.ContextTransformCheckpoint) (loop.ContextTransformResult, error) {
	if unit == nil {
		return loop.ContextTransformResult{}, nil
	}
	if !checkpoint.Force && !a.shouldAutoCompactForSession(unit) {
		return loop.ContextTransformResult{}, nil
	}
	activeStart, err := a.compactAtCheckpointForSession(ctx, unit, checkpoint)
	if err != nil {
		if checkpoint.Force {
			return loop.ContextTransformResult{}, err
		}
		a.emitEvent(Event{Kind: EventError, SessionID: sessionIDOf(unit), ProjectID: unit.projectID, Error: fmt.Sprintf("compaction: %v", err), Turn: checkpoint.Turn})
		return loop.ContextTransformResult{}, nil
	}
	return loop.ContextTransformResult{Transformed: true, ActiveTurnStart: activeStart}, nil
}

// runCompaction summarizes the current conversation through the same context
// transform hook used by model-request checkpoints. It is kept for existing
// focused tests and manual compaction plumbing.
func (a *Agent) runCompaction(ctx context.Context, turnInProgress bool) error {
	return a.runCompactionForSession(ctx, a.session, turnInProgress)
}

func (a *Agent) runCompactionForSession(ctx context.Context, unit *session, turnInProgress bool) error {
	if unit == nil || unit.lp == nil || unit.store == nil {
		return fmt.Errorf("no session open")
	}
	activeStart := len(unit.lp.Messages())
	checkpoint := loop.ContextTransformCheckpoint{
		Turn:            unit.store.CurrentTurn(),
		ActiveTurnStart: activeStart,
		Force:           true,
	}
	if turnInProgress && activeStart > 0 {
		checkpoint.ActiveTurnStart = activeStart
	}
	_, err := a.beforeModelRequestForSession(ctx, unit, checkpoint)
	return err
}

func (a *Agent) compactAtCheckpoint(ctx context.Context, checkpoint loop.ContextTransformCheckpoint) (int, error) {
	return a.compactAtCheckpointForSession(ctx, a.session, checkpoint)
}

func (a *Agent) compactAtCheckpointForSession(ctx context.Context, unit *session, checkpoint loop.ContextTransformCheckpoint) (int, error) {
	if unit == nil || unit.lp == nil || unit.store == nil {
		return 0, fmt.Errorf("no session open")
	}
	sessionID := sessionIDOf(unit)
	projectID := unit.projectID
	a.emitEvent(Event{Kind: EventCompactionStart, SessionID: sessionID, ProjectID: projectID})
	refreshSessionNow := false
	defer func() {
		a.emitEvent(Event{Kind: EventCompactionEnd, SessionID: sessionID, ProjectID: projectID, RefreshSession: refreshSessionNow})
	}()

	messages := unit.lp.Messages()
	activeStart := checkpoint.ActiveTurnStart
	if activeStart <= 0 || activeStart > len(messages) {
		activeStart = len(messages)
	}
	if activeStart <= 1 {
		return activeStart, fmt.Errorf("nothing to compact")
	}
	activeTail := activeStart < len(messages)
	toSummarize := append([]message.Message(nil), messages[1:activeStart]...)

	compactUnit, summarizerWindow, err := a.compactRunningUnitForSession(unit)
	if err != nil {
		return activeStart, err
	}
	defer func() {
		_, _ = compactUnit.store.Close()
	}()
	if summarizerWindow <= 0 {
		summarizerWindow = unit.contextWindowSize
	}

	prompt := ""
	if compactUnit.lp != nil {
		messages := compactUnit.lp.Messages()
		if len(messages) > 0 {
			prompt = messages[0].TextContent()
		}
	}
	if prompt == "" {
		prompt = compact.DefaultSummarizerPrompt
	}

	result, err := compact.Run(ctx, toSummarize, compact.Config{
		SummarizerClient: compactUnitSummarizer{unit: compactUnit, systemPrompt: prompt},
		ContextWindow:    summarizerWindow,
		SummarizerPrompt: prompt,
	})
	if err != nil {
		return activeStart, err
	}

	boundaryTurn := unit.store.CurrentTurn()
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
	if err := unit.store.SaveCompaction(rec); err != nil {
		return activeStart, fmt.Errorf("save compaction: %w", err)
	}

	var activeReads []tool.ReadRecord
	if unit.fileTracker != nil && activeStart < len(messages) {
		activeReads = activeTailReadRecords(messages[activeStart:], unit.fileTracker.Snapshot(), a.cfg.Tools.ReadMaxLines, unit.projectRoot)
	}

	if a.memoryHooks != nil {
		sessionID := unit.store.SessionID()
		projID := unit.projectID
		projName := unit.projectName
		compactionPath := filepath.Join(unit.store.Dir(), "compaction.json")
		if err := a.memoryHooks.IndexSummary(sessionID, projID, projName, result.Summary, rec.CompactedAt, compactionPath); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: memory index summary: %v\n", err)
		}
	}

	newActiveStart := unit.lp.LoadHistoryWithSummaryAndActiveTail(result.Summary, result.SummarizerRef, activeStart)
	if unit.fileTracker != nil {
		if len(activeReads) > 0 {
			unit.fileTracker.Restore(activeReads)
		} else {
			unit.fileTracker.Reset()
		}
	}
	if activeTail {
		a.ensureRuntime().deferSessionRefreshAfterTurnForSession(unit)
	} else {
		refreshSessionNow = true
	}

	unit.tokensMu.Lock()
	unit.lastContextUsed = 0
	unit.tokensMu.Unlock()

	return newActiveStart, nil
}

func (rt *runtime) deferSessionRefreshAfterTurnForSession(unit *session) {
	rt.mu.Lock()
	if unit != nil {
		unit.sessionRefreshAfterTurn = true
	}
	rt.mu.Unlock()
}

func (rt *runtime) takeDeferredSessionRefreshAfterTurnForSession(unit *session) bool {
	rt.mu.Lock()
	refresh := false
	if unit != nil {
		refresh = unit.sessionRefreshAfterTurn
		unit.sessionRefreshAfterTurn = false
	}
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

func (a *Agent) CompactNowForSession(ctx context.Context, sessionID string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit, err := a.liveSessionLocked(sessionID)
	if err != nil {
		rt.mu.Unlock()
		return err
	}
	if err := unitMutableLocked(unit); err != nil {
		rt.mu.Unlock()
		return err
	}
	if rt.closed {
		rt.mu.Unlock()
		return errOwnerClosed
	}
	unit.busy = true
	rt.turnWG.Add(1)
	compactCtx, cancel := context.WithCancel(ctx)
	unit.turnCancel = cancel
	unit.turnCtx = compactCtx
	rt.mu.Unlock()

	defer func() {
		rt.mu.Lock()
		unit.busy = false
		unit.turnCancel = nil
		unit.turnCtx = nil
		rt.mu.Unlock()
		cancel()
		rt.nudgeQueueDrainer()
		rt.nudgeSignalScheduler()
		rt.turnWG.Done()
	}()

	return a.runCompactionForSession(compactCtx, unit, false)
}

func (a *Agent) resumeMostRecent() error {
	defer a.lockLifecycle()()
	proj, err := a.projects.Current()
	if err != nil || proj == nil {
		return err
	}
	sessionsRoot := a.projects.SessionsRoot(proj.ID)
	if err := a.store.AttachSessionsRoot(sessionsRoot, a.projects.Root(), proj.ID); err != nil {
		return err
	}
	candidates, err := snapshot.List(sessionsRoot, "", snapshot.StateActive)
	if err != nil {
		return err
	}
	rt := a.ensureRuntime()
	resumed := false
	for _, info := range candidates {
		if info.ParentSessionID != "" {
			continue // only root sessions resume
		}
		// Try active root sessions newest-first. A contended, corrupt, or
		// unreadable candidate, or one whose history fails to load, releases its
		// provisional claim and does not stop the scan.
		if err := a.store.LoadSession(info.ID); err != nil {
			continue
		}
		if err := a.loadHistoryIntoLoop(); err != nil {
			a.store.Detach()
			continue
		}
		rt.mu.Lock()
		a.setSessionProject(a.session, proj)
		a.setCurrentSessionLocked(a.session)
		rt.mu.Unlock()
		resumed = true
		break
	}
	if !resumed {
		return nil // no candidate opened; the adapter creates a new session
	}
	a.resetFileTracker()
	a.loadTokensFromDisk()
	// Restore the model under rt.mu so the currentRef / contextWindowSize / client
	// writes publish atomically with respect to the signal scheduler and queue
	// drainer started at construction (which read currentRef under the lock),
	// mirroring SessionSwitch. restoreModelFromSession never re-acquires rt.mu.
	rt.mu.Lock()
	if err := a.reloadLocked(); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: reload config on resume: %v\n", err)
		a.restoreModelFromSession()
		rt.mu.Unlock()
		return nil
	}
	a.restoreModelFromSession()
	rt.mu.Unlock()
	return nil
}

func (a *Agent) resetFileTracker() {
	a.resetFileTrackerForSession(a.session)
}

func (a *Agent) resetFileTrackerForSession(unit *session) {
	if unit == nil || unit.fileTracker == nil {
		return
	}
	unit.fileTracker.Reset()
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
	a.setActiveModelForSessionLocked(a.session, ref, client, model)
}

func (a *Agent) setActiveModelForSessionLocked(unit *session, ref coremodel.ModelRef, client *provider.Client, model *catalog.Model) {
	if unit == nil {
		return
	}
	unit.currentRef = ref
	unit.contextWindowSize = model.ContextWindow
	unit.activeAdapt = a.resolveAdaptation(ref.Model)
	if unit.lp != nil {
		unit.lp.SetClient(provider.NewAdapter(client))
		unit.lp.SetActiveAdaptation(unit.activeAdapt)
	}
	a.applyActiveAdaptationPromptForSessionLocked(unit)
}

// clearActiveModelLocked clears the active model and its adaptation, reverting all
// three levers (tools, leak pattern, prompt) to baseline. The other sole writer;
// must be called with rt.mu held.
func (a *Agent) clearActiveModelLocked() {
	a.clearActiveModelForSessionLocked(a.session)
}

func (a *Agent) clearActiveModelForSessionLocked(unit *session) {
	if unit == nil {
		return
	}
	unit.currentRef = coremodel.ModelRef{}
	unit.contextWindowSize = 0
	unit.activeAdapt = nil
	if unit.lp != nil {
		unit.lp.SetClient(nil)
		unit.lp.SetActiveAdaptation(nil)
	}
	a.applyActiveAdaptationPromptForSessionLocked(unit)
}

// applyActiveAdaptationPromptLocked reassembles the system prompt for the current
// activeAdapt and installs it when changed. Called by the two model chokepoints with
// rt.mu held; the assembler never re-acquires rt.mu. If the active adaptation is nil,
// the baseline prompt cache path avoids UpdateSystemPrompt churn.
func (a *Agent) applyActiveAdaptationPromptLocked() {
	a.applyActiveAdaptationPromptForSessionLocked(a.session)
}

func (a *Agent) applyActiveAdaptationPromptForSessionLocked(unit *session) {
	if unit == nil || a.promptSvc == nil || unit.lp == nil {
		return
	}
	res := a.assembleSystemPromptForSessionLocked(unit)
	if res.Prompt != unit.installedPrompt {
		unit.lp.UpdateSystemPrompt(res.Prompt)
		unit.installedPrompt = res.Prompt
	}
}

// refreshSystemPrompt rebuilds the system prompt for the active model's adaptation
// and the current rules files, installing it when changed. It is the per-turn
// preamble — it runs without rt.mu, which is safe because the model-set paths are
// idle-gated, so activeAdapt is stable while a turn is in flight.
func (a *Agent) refreshSystemPrompt() {
	a.refreshSystemPromptForSession(a.session)
}

func (a *Agent) refreshSystemPromptForSession(unit *session) {
	if unit == nil || unit.lp == nil || a.promptSvc == nil {
		return
	}
	res := a.assembleSystemPromptForSessionLocked(unit)
	if res.Prompt != unit.installedPrompt {
		unit.lp.UpdateSystemPrompt(res.Prompt)
		unit.installedPrompt = res.Prompt
	}
	a.setWarningGroup("prompt", res.Warnings)
}

func (a *Agent) assembleSystemPromptLocked() prompt.Result {
	return a.assembleSystemPromptForSessionLocked(a.session)
}

func (a *Agent) assembleSystemPromptForSessionLocked(unit *session) prompt.Result {
	if unit == nil || a.promptSvc == nil {
		return prompt.Result{}
	}
	agentType := "primary"
	if strings.TrimSpace(unit.activeAgentType) != "" {
		agentType = unit.activeAgentType
	}
	spec := prompt.Spec{Size: prompt.SizeFull, Memory: true, Adapt: unit.activeAdapt}
	if resolved, err := a.resolvedAgentTypeLocked(agentType); err == nil {
		spec.Size = resolved.SystemPrompt
		spec.Body = resolved.Prompt
		spec.Memory = resolved.Memory
	}
	return a.promptSvc.Assemble(unit.projectRoot, unit.sessionStart, spec)
}

func (a *Agent) restoreModelFromSession() {
	a.restoreModelFromSessionForSession(a.session)
}

func (a *Agent) restoreModelFromSessionForSession(unit *session) {
	if unit == nil || unit.store == nil {
		return
	}
	meta, err := unit.store.Meta()
	if err != nil || meta.Provider == "" || meta.Model == "" {
		return
	}
	ref := coremodel.ModelRef{Provider: meta.Provider, Model: meta.Model}
	client, model, err := newProviderClient(a.catalog, ref)
	if err != nil {
		return
	}
	a.setActiveModelForSessionLocked(unit, ref, client, model)
}

// inheritActiveModelLocked gives the fork the source unit's live active model,
// preferring the in-memory selection over persisted metadata so an unpersisted
// model switch is not lost, and persists it into the candidate so the fork
// reopens on the same model. A source with no live selection keeps the persisted
// model. Reconstruction or persistence of a live selection fails the fork before
// publication rather than silently substituting a different model.
func (a *Agent) inheritActiveModelLocked(candidate, source *session) error {
	ref := source.currentRef
	if ref.Provider == "" || ref.Model == "" {
		a.restoreModelFromSessionForSession(candidate)
		return nil
	}
	client, model, err := newProviderClient(a.catalog, ref)
	if err != nil {
		return fmt.Errorf("fork model %s/%s: %w", ref.Provider, ref.Model, err)
	}
	a.setActiveModelForSessionLocked(candidate, ref, client, model)
	if err := candidate.store.SetModel(ref.Provider, ref.Model); err != nil {
		return fmt.Errorf("fork persist model: %w", err)
	}
	return nil
}

func (a *Agent) loadHistoryIntoLoop() error {
	return a.loadHistoryIntoLoopForSession(a.session)
}

func (a *Agent) loadHistoryIntoLoopForSession(unit *session) error {
	if unit == nil || unit.store == nil || unit.lp == nil {
		return snapshot.ErrNoSession
	}
	rec, err := unit.store.LoadCompaction()
	if err != nil {
		return err
	}

	var raw []snapshot.TurnMessages
	if rec != nil {
		raw, err = unit.store.LoadCompleteTurnsAfter(rec.BoundaryTurn)
	} else {
		raw, err = unit.store.LoadCompleteTurns()
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
		unit.lp.LoadHistoryWithSummary(rec.Summary, coremodel.ModelRef{Provider: rec.SummarizerProvider, Model: rec.SummarizerModel}, decoded)
	} else {
		unit.lp.LoadHistory(decoded)
	}
	return nil
}

func (a *Agent) loadTokensFromDisk() {
	a.loadTokensFromDiskForSession(a.session)
}

func (a *Agent) loadTokensFromDiskForSession(unit *session) {
	if unit == nil {
		return
	}
	unit.tokensMu.Lock()
	defer unit.tokensMu.Unlock()
	unit.tokens = map[string]*TokenEntry{}
	if unit.store == nil || !unit.store.Active() {
		return
	}
	data, err := os.ReadFile(filepath.Join(unit.store.Dir(), tokensFileName))
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
		unit.tokens[e.Provider+"/"+e.Model] = &e
	}
}

func (a *Agent) ensureSession() error {
	if a.store.Active() {
		a.refreshCurrentSessionProjectLocked()
		a.registerLiveSessionLocked(a.session)
		if a.currentSessionID == "" {
			a.currentSessionID = a.store.SessionID()
		}
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
	if err := a.store.BeginNewSessionStaged(a.projectRoot); err != nil {
		return err
	}
	a.setSessionProject(a.session, proj)
	a.setCurrentSessionLocked(a.session)
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

func (a *Agent) SubmitToSession(ctx context.Context, sessionID string, content string) (SubmitResult, error) {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit, err := a.liveSessionLocked(sessionID)
	rt.mu.Unlock()
	if err != nil {
		return SubmitResult{}, err
	}
	return rt.submit(ctx, unit, content)
}

func (rt *runtime) submit(ctx context.Context, unit *session, content string) (SubmitResult, error) {
	a := rt.agent
	rt.mu.Lock()
	if unit == nil {
		rt.mu.Unlock()
		return SubmitResult{}, snapshot.ErrNoSession
	}
	if unit.transitioning {
		rt.mu.Unlock()
		return SubmitResult{}, fmt.Errorf("session is changing; retry")
	}
	if rt.closed {
		rt.mu.Unlock()
		return SubmitResult{}, errOwnerClosed
	}
	if !unit.busy && len(unit.queue) == 0 {
		turnCtx, cancel, err := rt.claimTurnLocked(ctx, unit)
		if err != nil {
			rt.mu.Unlock()
			return SubmitResult{}, err
		}
		version := unit.queueVersion
		rt.mu.Unlock()
		turn := rt.launchTurn(ctx, unit, turnCtx, cancel, []string{content})
		return SubmitResult{Started: true, Turn: turn, Queue: emptyQueue(), Version: version}, nil
	}
	// Busy or queue non-empty: enqueue and let the drainer pick it up.
	unit.queueSeq++
	unit.queue = append(unit.queue, QueuedItem{ID: fmt.Sprintf("q-%d", unit.queueSeq), Content: content})
	unit.queueVersion++
	items := copyQueue(unit.queue)
	version := unit.queueVersion
	sessionID := sessionIDOf(unit)
	projectID := unit.projectID
	rt.mu.Unlock()
	rt.nudgeQueueDrainer()
	a.emitEvent(Event{Kind: EventQueueChanged, SessionID: sessionID, ProjectID: projectID, Queue: items, QueueVersion: version})
	return SubmitResult{Started: false, Queue: items, Version: version}, nil
}

func (a *Agent) QueueSnapshotForSession(sessionID string) (QueueState, error) {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	unit, err := a.liveSessionLocked(sessionID)
	if err != nil {
		return QueueState{}, err
	}
	return rt.queueSnapshotLocked(unit), nil
}

func (rt *runtime) queueSnapshot(unit *session) QueueState {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.queueSnapshotLocked(unit)
}

func (rt *runtime) queueSnapshotLocked(unit *session) QueueState {
	if unit == nil {
		return QueueState{Items: emptyQueue()}
	}
	return QueueState{Items: copyQueue(unit.queue), Version: unit.queueVersion}
}

// AppendUserMessage persists a user message as its own complete turn WITHOUT
// running the model. It is a history-seeding primitive (used to script/seed
// conversation state), not a user-input path — live input goes through Submit.
// It still routes through the loop's emit chokepoint, so it is display-ordered.
// Not exposed in the Wails layer.
func (a *Agent) AppendUserMessage(content string) (int, error) {
	return a.ensureRuntime().appendUserMessage(a.session, content)
}

func (a *Agent) AppendUserMessageToSession(sessionID string, content string) (int, error) {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	unit, err := a.liveSessionLocked(sessionID)
	if err != nil {
		return 0, err
	}
	return rt.appendUserMessageLocked(unit, content)
}

func (rt *runtime) appendUserMessage(unit *session, content string) (int, error) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.appendUserMessageLocked(unit, content)
}

func (rt *runtime) appendUserMessageLocked(unit *session, content string) (int, error) {
	a := rt.agent
	if unit == nil {
		return 0, snapshot.ErrNoSession
	}
	if unit.busy {
		return 0, fmt.Errorf("a turn is already in progress")
	}
	if unit == a.session {
		if err := a.ensureSession(); err != nil {
			return 0, err
		}
	} else if unit.store == nil || !unit.store.Active() {
		return 0, snapshot.ErrNoSession
	}
	turn := unit.store.BeginTurn()
	unit.lp.AppendUserMessage(turn, content)
	_ = unit.store.MarkTurnComplete(turn)
	return turn, nil
}

// errOwnerClosed is returned when the owner is shutting down and no longer
// admits new turns or mutations.
var errOwnerClosed = errors.New("agent: owner is shutting down")

// claimTurnLocked checks the busy gate and claims a turn (sets busy, builds the
// per-turn context). Caller must hold the runtime mutex. Returns a non-nil error if a turn
// is already in progress or ensureSession fails; on error it leaves busy
// unchanged (never half-claims). launchTurn must be called AFTER unlocking.
func (rt *runtime) claimTurnLocked(ctx context.Context, unit *session) (context.Context, context.CancelFunc, error) {
	a := rt.agent
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if rt.closed {
		return nil, nil, errOwnerClosed
	}
	if unit == nil {
		return nil, nil, snapshot.ErrNoSession
	}
	if unit.busy {
		return nil, nil, fmt.Errorf("a turn is already in progress")
	}
	if unit == a.session {
		if err := a.ensureSession(); err != nil {
			return nil, nil, err
		}
	} else if unit.store == nil || !unit.store.Active() {
		return nil, nil, snapshot.ErrNoSession
	}
	a.ensureActiveModelForSessionLocked(unit)
	a.setWarningGroup("setup", a.setupWarningsLocked())
	unit.busy = true
	unit.seenSessions = nil
	// Track the turn from the moment it is claimed, under mu, so owner shutdown's
	// join can never miss a turn between claim and launch. launchTurn's goroutine
	// calls Done.
	rt.turnWG.Add(1)
	turnCtx, cancel := context.WithCancel(ctx)
	unit.turnCancel = cancel
	unit.turnCtx = turnCtx
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
	return rt.clearQueueLockedForSession(rt.sessionLocked())
}

func (rt *runtime) clearQueueLockedForSession(unit *session) ([]QueuedItem, int, bool) {
	if unit == nil {
		return emptyQueue(), 0, false
	}
	unit.queueSeq = 0
	if len(unit.queue) == 0 {
		return emptyQueue(), unit.queueVersion, false
	}
	unit.queue = nil
	unit.queueVersion++
	return emptyQueue(), unit.queueVersion, true
}

// beginTransition marks a cancel-and-wait session change in flight. While set,
// Submit rejects and the drainer/signal-scheduler will not start a turn, so no
// queued input can launch against the session being swapped. Must be paired
// with a deferred endTransition registered BEFORE cancelAndWaitIdle so it fires
// even on the pre-lock error return.
func (rt *runtime) beginTransition() {
	rt.mu.Lock()
	rt.sessionLocked().transitioning = true
	rt.mu.Unlock()
}

// endLiveTransition clears a unit's transitioning flag and, only when the
// (failed or no-op) transition leaves the unit live, re-nudges both the queue
// drainer and the signal scheduler — a wake token may have been consumed while
// transitioning blocked them, which would otherwise strand intact queue or
// signal work. It is the single primitive for both current and non-current
// units. A committed removal leaves the unit inactive, so this is a no-op
// there and the removal boundary carries the emptied state.
// lockLifecycle acquires the owner lifecycle lock and returns its release, for
// the idiom `defer a.lockLifecycle()()`. It is the outermost owner lock and is
// taken at the entry of every identity-changing lifecycle operation, before
// any runtime.mu, so overlapping operations serialize with exactly one
// committed outcome.
func (a *Agent) lockLifecycle() func() {
	rt := a.ensureRuntime()
	rt.lifecycleMu.Lock()
	return rt.lifecycleMu.Unlock
}

func (a *Agent) endLiveTransition(unit *session) {
	if unit == nil {
		return
	}
	rt := a.ensureRuntime()
	var items []QueuedItem
	var version int
	var sessionID, projectID string
	var active bool
	rt.mu.Lock()
	if unit.store != nil && unit.store.Active() {
		unit.transitioning = false
		items = copyQueue(unit.queue)
		version = unit.queueVersion
		sessionID = sessionIDOf(unit)
		projectID = unit.projectID
		active = true
	}
	rt.mu.Unlock()
	if !active {
		return
	}
	a.emitEvent(Event{Kind: EventQueueChanged, SessionID: sessionID, ProjectID: projectID, Queue: items, QueueVersion: version})
	if len(items) > 0 {
		rt.nudgeQueueDrainer()
	}
	rt.nudgeSignalScheduler()
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
	for {
		if ctx.Err() != nil {
			return
		}
		rt.mu.Lock()
		unit := rt.nextDrainableSessionLocked()
		if ctx.Err() != nil || unit == nil || rt.closed {
			rt.mu.Unlock()
			return
		}
		contents := make([]string, len(unit.queue))
		for i, it := range unit.queue {
			contents[i] = it.Content
		}
		unit.queue = nil
		unit.queueVersion++
		version := unit.queueVersion
		for _, content := range contents[:len(contents)-1] {
			if ctx.Err() != nil {
				rt.mu.Unlock()
				return
			}
			turn := unit.store.BeginTurn()
			unit.lp.AppendUserMessage(turn, content)
			_ = unit.store.MarkTurnComplete(turn)
		}
		if ctx.Err() != nil {
			rt.mu.Unlock()
			return
		}
		unit.busy = true
		unit.seenSessions = nil
		rt.turnWG.Add(1)
		turnCtx, cancel := context.WithCancel(ctx)
		unit.turnCancel = cancel
		unit.turnCtx = turnCtx
		sessionID := sessionIDOf(unit)
		projectID := unit.projectID
		rt.mu.Unlock()

		a.emitEvent(Event{Kind: EventQueueChanged, SessionID: sessionID, ProjectID: projectID, Queue: emptyQueue(), QueueVersion: version})
		rt.launchTurn(ctx, unit, turnCtx, cancel, []string{contents[len(contents)-1]})
	}
}

func (rt *runtime) nextDrainableSessionLocked() *session {
	if rt == nil || rt.agent == nil {
		return nil
	}
	if unit := rt.sessionLocked(); drainableSession(unit) {
		return unit
	}
	for _, unit := range rt.agent.sessions {
		if drainableSession(unit) {
			return unit
		}
	}
	return nil
}

func drainableSession(unit *session) bool {
	return unit != nil &&
		!unit.busy &&
		!unit.transitioning &&
		unit.store != nil &&
		unit.store.Active() &&
		len(unit.queue) > 0
}

func (rt *runtime) nextWakeableSessionLocked() *session {
	if rt == nil || rt.agent == nil {
		return nil
	}
	if unit := rt.sessionLocked(); wakeableSession(unit) {
		return unit
	}
	for _, unit := range rt.agent.sessions {
		if wakeableSession(unit) {
			return unit
		}
	}
	return nil
}

func wakeableSession(unit *session) bool {
	return unit != nil &&
		!unit.busy &&
		!unit.transitioning &&
		unit.store != nil &&
		unit.store.Active() &&
		unit.lp != nil &&
		len(unit.queue) == 0 &&
		unit.lp.HasPendingWakeSignal()
}

func (rt *runtime) launchTurn(ctx context.Context, unit *session, turnCtx context.Context, cancel context.CancelFunc, contents []string) int {
	a := rt.agent
	if unit == nil || unit.store == nil || unit.lp == nil {
		// The turn was claimed (and counted) but cannot launch; release the count.
		rt.turnWG.Done()
		return 0
	}
	unit.syncEventOwner()
	sessionID := sessionIDOf(unit)
	projectID := unit.projectID
	turn := unit.store.BeginTurn()

	startEv := Event{Kind: EventTurnStart, SessionID: sessionID, ProjectID: projectID, Turn: turn}
	a.emitEvent(startEv)
	feedTranscript(unit.transcript, startEv)

	rt.mu.Lock()
	a.ensureActiveModelForSessionLocked(unit)
	a.applyUnitConfigLocked(unit)
	a.setWarningGroup("setup", a.setupWarningsLocked())
	rt.mu.Unlock()

	if unit.taskToolInst != nil {
		unit.taskToolInst.updateParentState(cancel)
	}

	go func() {
		defer func() {
			rt.mu.Lock()
			unit.busy = false
			unit.turnCancel = nil
			unit.turnCtx = nil
			rt.mu.Unlock()
			cancel()
			// Unconditionally nudge the queue drainer after every turn end: it
			// no-ops on an empty queue, and the unconditional nudge is the
			// reliable retry that defeats cap-1 channel coalescing for items
			// queued mid-turn. The signal scheduler no-ops when nothing is
			// pending, matching the queue drainer's unconditional nudge.
			rt.nudgeQueueDrainer()
			rt.nudgeSignalScheduler()
			rt.turnWG.Done()
		}()

		if ctx.Err() != nil {
			return
		}
		a.refreshSystemPromptForSession(unit)

		if ctx.Err() != nil {
			return
		}
		_, err := unit.lp.Run(turnCtx, contents...)

		done := make(chan struct{})
		flushed := false
		select {
		case rt.loopFlush <- done:
			select {
			case <-done:
				flushed = true
			case <-ctx.Done():
			}
		case <-ctx.Done():
		}

		if err != nil {
			errEv := Event{Kind: EventError, SessionID: sessionID, ProjectID: projectID, Error: a.turnErrorMessage(err), Turn: turn}
			a.emitEvent(errEv)
			feedTranscript(unit.transcript, errEv)
		}
		endEv := Event{Kind: EventTurnEnd, SessionID: sessionID, ProjectID: projectID, Turn: turn, Cancelled: turnCtx.Err() != nil, RefreshSession: rt.takeDeferredSessionRefreshAfterTurnForSession(unit)}
		a.emitEvent(endEv)
		// Commit the coordinator only once the drainer flush is acknowledged. If
		// owner cancellation bypassed the flush a streamed row may still be
		// buffered, and it must not be fed after the turn commits.
		if flushed {
			feedTranscript(unit.transcript, endEv)
		}
	}()

	return turn
}

func (a *Agent) turnErrorMessage(err error) string {
	if errors.Is(err, loop.ErrNoModelConfigured) {
		return "No model is configured. Select a model to get started."
	}
	return err.Error()
}

func (a *Agent) CancelSession(sessionID string) error {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit, err := a.liveSessionLocked(sessionID)
	if err == nil {
		cancel := rt.turnCancelSnapshotLocked(unit)
		rt.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if a.gate != nil {
			a.gate.CancelSession(sessionID)
		}
		return nil
	}
	rt.mu.Unlock()
	return err
}

// Busy reports whether a turn is in progress.
func (a *Agent) Busy() bool {
	return a.ensureRuntime().busySnapshot(a.session)
}

func (a *Agent) BusyForSession(sessionID string) (bool, error) {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	unit, err := a.liveSessionLocked(sessionID)
	if err != nil {
		return false, err
	}
	return rt.busySnapshotLocked(unit), nil
}

func (rt *runtime) turnCancelSnapshot(unit *session) context.CancelFunc {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.turnCancelSnapshotLocked(unit)
}

func (rt *runtime) turnCancelSnapshotLocked(unit *session) context.CancelFunc {
	if unit == nil {
		return nil
	}
	return unit.turnCancel
}

func (rt *runtime) busySnapshot(unit *session) bool {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.busySnapshotLocked(unit)
}

func (rt *runtime) busySnapshotLocked(unit *session) bool {
	return unit != nil && unit.busy
}

// RespondPermission answers a pending permission prompt.
func (a *Agent) RespondPermission(id string, allow bool) error {
	return a.RespondPermissionForSession(a.currentPermissionSessionID(), id, allow)
}

// RespondPermissionAction answers a pending permission prompt with an action.
func (a *Agent) RespondPermissionAction(id string, action string) error {
	return a.RespondPermissionActionForSession(a.currentPermissionSessionID(), id, action)
}

func (a *Agent) RespondPermissionForSession(sessionID string, id string, allow bool) error {
	if a.gate == nil {
		return permission.ErrUnknownRequest
	}
	return a.gate.RespondForSession(sessionID, id, allow)
}

func (a *Agent) RespondPermissionActionForSession(sessionID string, id string, action string) error {
	if a.gate == nil {
		return permission.ErrUnknownRequest
	}
	return a.gate.RespondActionForSession(sessionID, id, action)
}

// PermissionSuggest returns pattern suggestions for the "Allow for project" UI.
func (a *Agent) PermissionSuggest(toolName, arg string) []PermissionSuggestion {
	suggestions, err := a.PermissionSuggestForSession(a.currentPermissionSessionID(), toolName, arg)
	if err != nil {
		return nil
	}
	return suggestions
}

func (a *Agent) PermissionSuggestForSession(sessionID, toolName, arg string) ([]PermissionSuggestion, error) {
	unit, err := a.resolveLiveSession(sessionID)
	if err != nil {
		return nil, err
	}
	return permission.Suggest(toolName, arg, unit.projectRoot), nil
}

func (a *Agent) PermissionSuggestForProject(projectID, toolName, arg string) ([]PermissionSuggestion, error) {
	projectID = strings.TrimSpace(projectID)
	if projectID == "" {
		return nil, fmt.Errorf("project id is required")
	}
	projects, err := project.List(a.projects.Root())
	if err != nil {
		return nil, err
	}
	for i := range projects {
		if projects[i].ID == projectID {
			return permission.Suggest(toolName, arg, projects[i].Path), nil
		}
	}
	return nil, fmt.Errorf("unknown project %q", projectID)
}

// SaveProjectPermission appends patterns to the project's local
// permissions.json, then allows the pending request.
func (a *Agent) SaveProjectPermission(id string, patterns []string) error {
	return a.SaveProjectPermissionForSession(a.currentPermissionSessionID(), id, patterns)
}

func (a *Agent) SaveProjectPermissionForSession(sessionID string, id string, patterns []string) error {
	if a.gate == nil {
		return permission.ErrUnknownRequest
	}
	req, err := a.gate.ProjectSaveRequest(sessionID, id, true)
	if err != nil {
		return err
	}
	projectID := req.ProjectID
	if projectID == "" {
		proj, err := a.projects.Ensure()
		if err != nil {
			return err
		}
		projectID = proj.ID
	}
	add := permission.Rules{Allow: patterns}
	if err := permission.SaveLocal(a.projects.Root(), projectID, add); err != nil {
		return err
	}
	if err := a.gate.RespondForSession(sessionID, id, true); err != nil {
		if errors.Is(err, permission.ErrUnknownRequest) {
			return nil
		}
		return err
	}
	return nil
}

func (a *Agent) currentPermissionSessionID() string {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if a.currentSessionID != "" {
		return a.currentSessionID
	}
	return sessionIDOf(a.session)
}

// SwitchModel changes the active model by provider-prefixed catalog ref.
func (a *Agent) SwitchModel(refStr string) error {
	return a.switchModelForSession(a.session, refStr)
}

func (a *Agent) SwitchModelForSession(sessionID string, refStr string) error {
	unit, err := a.resolveLiveSession(sessionID)
	if err != nil {
		return err
	}
	return a.switchModelForSession(unit, refStr)
}

func (a *Agent) switchModelForSession(unit *session, refStr string) error {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if unit == nil {
		return snapshot.ErrNoSession
	}
	if err := unitMutableLocked(unit); err != nil {
		return err
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
	a.setActiveModelForSessionLocked(unit, ref, client, model)
	if unit.lp != nil {
		unit.lp.AddPendingSignal(loop.PendingSignal{Payload: fmt.Sprintf("Model switched to %s", ref.String()), Persist: true})
	}
	if unit.store != nil && unit.store.Active() {
		if err := unit.store.SetModel(ref.Provider, ref.Model); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: store.SetModel: %v\n", err)
		}
	}
	return nil
}

// Reload reloads config and catalog state for future turns.
func (a *Agent) Reload() error {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if a.ensureRuntime().sessionLocked().busy {
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
	if a.procMgr != nil {
		a.procMgr.SetLimits(cfg.Tools.MaxBackgroundProcesses, cmdoutput.Options{
			HomeDir:      a.home,
			SpillPrefix:  "proc_output_",
			MaxBytes:     cfg.Tools.MaxOutputBytes,
			MaxLineChars: cfg.Tools.ReadLineMaxChars,
		})
	}
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
	seen := map[*taskTool]bool{}
	for _, unit := range a.sessions {
		if unit != nil && unit.taskToolInst != nil && !seen[unit.taskToolInst] {
			unit.taskToolInst.setAgentTypes(agentTypes)
			seen[unit.taskToolInst] = true
		}
	}
	if a.taskToolInst != nil && !seen[a.taskToolInst] {
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
	if a.ensureRuntime().sessionLocked().busy {
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
	if a.ensureRuntime().sessionLocked().busy {
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

func (a *Agent) CurrentModelForSession(sessionID string) (ModelInfo, error) {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	unit, err := a.liveSessionLocked(sessionID)
	if err != nil {
		return ModelInfo{}, err
	}
	a.ensureActiveModelForSessionLocked(unit)
	a.setWarningGroup("setup", a.setupWarningsLocked())
	return a.modelInfo(unit.currentRef), nil
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
	if a.ensureRuntime().sessionLocked().busy {
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
	if a.ensureRuntime().sessionLocked().busy {
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

func (a *Agent) applyUnitConfigLocked(unit *session) {
	if unit.registry != nil {
		for _, name := range []string{"read_file", "write_file", "edit_file", "run_command"} {
			if t, ok := unit.registry.Get(name); ok {
				setRegisteredToolConfig(t, a.cfg.Tools)
			}
		}
	}
	if unit.pendingExecutor != nil {
		unit.pendingExecutor.SetToolsConfig(a.cfg.Tools)
	}
	if unit.taskToolInst != nil {
		unit.taskToolInst.setCatalog(a.catalog)
		unit.taskToolInst.setMaxConcurrent(a.cfg.Subagents.MaxConcurrent)
		unit.taskToolInst.setToolsConfig(a.cfg.Tools)
	}
}

// shutdownJoinTimeout bounds each owner-shutdown join. A delivery callback that
// blocks (a full adapter sink whose consumer has stopped) is abandoned rather
// than hanging the host; the abandoned goroutine holds no owner lock and ends at
// process exit.
const shutdownJoinTimeout = 5 * time.Second

// waitGroupOrTimeout waits for wg, returning true if it drained within d and
// false if the wait was abandoned.
func waitGroupOrTimeout(wg *sync.WaitGroup, d time.Duration) bool {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(d):
		return false
	}
}

// ShutdownOwner closes turn admission, cancels live turns and permission timers,
// joins in-flight turns so their terminal events flush through the still-running
// drainer, then cancels and joins the background goroutines and closes the
// subordinate services. It is one shared join: every caller waits for the same
// cleanup, and it runs exactly once. Session stores are not closed; state
// persists per turn and the process-exit boundary releases claims.
func (a *Agent) ShutdownOwner() {
	rt := a.ensureRuntime()
	rt.shutdownOnce.Do(func() {
		rt.mu.Lock()
		rt.closed = true
		var cancels []context.CancelFunc
		var sessionIDs []string
		for id, unit := range a.sessions {
			if unit == nil || unit.store == nil || !unit.store.Active() {
				continue
			}
			if cancel := rt.turnCancelSnapshotLocked(unit); cancel != nil {
				cancels = append(cancels, cancel)
			}
			sessionIDs = append(sessionIDs, id)
		}
		rt.mu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		if a.gate != nil {
			for _, id := range sessionIDs {
				a.gate.CancelSession(id)
			}
		}
		// Join in-flight turns and mutations first, while the drainer is still
		// alive, so their terminal events are delivered before delivery stops.
		turnsDrained := waitGroupOrTimeout(&rt.turnWG, shutdownJoinTimeout)
		if !turnsDrained {
			fmt.Fprintf(os.Stderr, "lightcode: owner shutdown abandoned in-flight turns after %s\n", shutdownJoinTimeout)
		}
		if rt.ownerCancel != nil {
			rt.ownerCancel()
		}
		if !waitGroupOrTimeout(&rt.bgWG, shutdownJoinTimeout) {
			fmt.Fprintf(os.Stderr, "lightcode: owner shutdown abandoned background workers after %s\n", shutdownJoinTimeout)
		}
		if a.procMgr != nil {
			a.procMgr.Close()
		}
		// Close the shared embedder only once every turn has actually finished, so
		// an abandoned turn never hits a closed embedder; a leaked embedder is
		// released at process exit.
		if turnsDrained && a.embedder != nil {
			a.embedder.Close()
		}
		close(rt.shutdownDone)
	})
	<-rt.shutdownDone
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
	if a.ensureRuntime().sessionLocked().busy {
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
	if a.ensureRuntime().sessionLocked().busy {
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
	unit := a.session
	if unit == nil {
		return TokenReport{}
	}
	unit.tokensMu.Lock()
	defer unit.tokensMu.Unlock()
	return a.buildReportForSessionLocked(unit)
}

func (a *Agent) TokenUsageForSession(sessionID string) (TokenReport, error) {
	unit, err := a.resolveLiveSession(sessionID)
	if err != nil {
		return TokenReport{}, err
	}
	unit.tokensMu.Lock()
	defer unit.tokensMu.Unlock()
	return a.buildReportForSessionLocked(unit), nil
}

// --- Session operations ---

// SessionCurrent returns the active session, or zero-value if none is open.
func (a *Agent) SessionCurrent() SessionSummary {
	return sessionSummary(a.session)
}

func (a *Agent) SessionSummaryForSession(sessionID string) (SessionSummary, error) {
	unit, err := a.resolveLiveSession(sessionID)
	if err != nil {
		return SessionSummary{}, err
	}
	return sessionSummary(unit), nil
}

func (a *Agent) SessionPayloadForSession(sessionID string) (SessionPayload, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return SessionPayload{}, fmt.Errorf("session id is required")
	}
	summary, err := a.SessionSummaryForSession(sessionID)
	if err != nil {
		return SessionPayload{}, err
	}
	messages, err := a.SessionMessagesFor(sessionID)
	if err != nil {
		return SessionPayload{}, err
	}
	tokens, err := a.TokenUsageForSession(sessionID)
	if err != nil {
		return SessionPayload{}, err
	}
	return SessionPayload{Session: summary, Messages: messages, Tokens: tokens}, nil
}

func sessionSummary(unit *session) SessionSummary {
	if unit == nil || unit.store == nil || !unit.store.Active() {
		return SessionSummary{}
	}
	meta, err := unit.store.Meta()
	if err != nil {
		return SessionSummary{ID: unit.store.SessionID()}
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
	out := make([]SessionSummary, 0, len(infos))
	for _, info := range infos {
		if isCompactSessionType(info.ActiveAgentType) || info.ParentSessionID != "" {
			continue
		}
		out = append(out, sessionSummaryFromInfo(info))
	}
	return out, nil
}

func (a *Agent) SessionListForProjectPath(projectPath string, state string) ([]SessionSummary, error) {
	if state != snapshot.StateActive && state != snapshot.StateArchived {
		return nil, fmt.Errorf("invalid state %q", state)
	}
	proj, err := a.ensureProjectForPath(projectPath)
	if err != nil {
		return nil, err
	}
	infos, err := snapshot.List(a.projects.SessionsRoot(proj.ID), proj.Path, state)
	if err != nil {
		return nil, err
	}
	out := make([]SessionSummary, 0, len(infos))
	for _, info := range infos {
		if isCompactSessionType(info.ActiveAgentType) || info.ParentSessionID != "" {
			continue
		}
		out = append(out, sessionSummaryFromInfo(info))
	}
	return out, nil
}

func sessionSummaryFromInfo(info snapshot.SessionInfo) SessionSummary {
	return SessionSummary{
		ID:              info.ID,
		CreatedAt:       info.CreatedAt,
		LastActivity:    info.LastActivity,
		State:           info.State,
		ArchivedAt:      info.ArchivedAt,
		ProjectPath:     info.ProjectPath,
		ParentSessionID: info.ParentSessionID,
	}
}

func (a *Agent) OpenSession(id string) (SessionSummary, error) {
	defer a.lockLifecycle()()
	id = strings.TrimSpace(id)
	if id == "" {
		return SessionSummary{}, fmt.Errorf("session id is required")
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	if unit, err := a.liveSessionLocked(id); err == nil {
		if isCompactSessionType(unit.activeAgentType) {
			rt.mu.Unlock()
			return SessionSummary{}, internalTranscriptSessionError(id)
		}
		summary := sessionSummary(unit)
		rt.mu.Unlock()
		return summary, nil
	}
	rt.mu.Unlock()

	proj, err := a.projectForExistingSession(id)
	if err != nil {
		return SessionSummary{}, err
	}
	if err := a.rejectInternalSession(a.projects.SessionsRoot(proj.ID), id); err != nil {
		return SessionSummary{}, err
	}

	store, err := snapshot.NewForSessionsRoot(a.projects.SessionsRoot(proj.ID), a.projects.Root(), proj.ID)
	if err != nil {
		return SessionSummary{}, err
	}

	rt.mu.Lock()
	defer rt.mu.Unlock()
	if unit, err := a.liveSessionLocked(id); err == nil {
		return sessionSummary(unit), nil
	}
	if err := a.reloadLocked(); err != nil {
		return SessionSummary{}, err
	}
	unit, _, err := a.rootRunningUnitLocked(store, "primary", proj.ID, proj.Name, proj.Path)
	if err != nil {
		return SessionSummary{}, err
	}
	if err := unit.store.LoadSession(id); err != nil {
		return SessionSummary{}, err
	}
	// Detach releases the claim acquired by LoadSession without discarding the
	// persisted session; the unit is never registered on any failure below, so
	// nothing else detaches it.
	meta, err := unit.store.Meta()
	if err != nil {
		unit.store.Detach()
		return SessionSummary{}, fmt.Errorf("open session %s: %w", id, err)
	}
	// Validate history before any reactivation, so a corrupt session is never
	// flipped from archived to active.
	if err := a.loadHistoryIntoLoopForSession(unit); err != nil {
		unit.store.Detach()
		return SessionSummary{}, err
	}
	if metaState(meta.State) == snapshot.StateArchived {
		if err := unit.store.SetState(snapshot.StateActive); err != nil {
			// Reactivation must reach disk; otherwise the owner would drive a
			// session that stays archived and absent from active listings.
			unit.store.Detach()
			return SessionSummary{}, fmt.Errorf("reactivate session %s: %w", id, err)
		}
		_ = unit.store.TouchActivity()
	}
	a.resetFileTrackerForSession(unit)
	a.loadTokensFromDiskForSession(unit)
	a.restoreModelFromSessionForSession(unit)
	a.registerLiveSessionLocked(unit)
	return sessionSummary(unit), nil
}

func (a *Agent) projectForExistingSession(id string) (*project.Project, error) {
	id = strings.TrimSpace(id)
	if err := snapshot.ValidateSessionID(id); err != nil {
		return nil, err
	}
	if a.projects == nil {
		return nil, fmt.Errorf("unknown session %q", id)
	}
	projects, err := project.List(a.projects.Root())
	if err != nil {
		return nil, err
	}
	for i := range projects {
		proj := projects[i]
		if _, err := os.Stat(filepath.Join(a.projects.SessionsRoot(proj.ID), id, "meta.json")); err == nil {
			return &proj, nil
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("unknown session %q", id)
}

func (a *Agent) cancelAndWaitIdle() error {
	a.ensureRuntime().mu.Lock()
	cancel := a.ensureRuntime().sessionLocked().turnCancel
	a.ensureRuntime().mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for i := 0; i < 200; i++ {
		a.ensureRuntime().mu.Lock()
		busy := a.ensureRuntime().sessionLocked().busy
		a.ensureRuntime().mu.Unlock()
		if !busy {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for current turn to end")
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

func (a *Agent) NewSession(projectID string, agentType string) (string, error) {
	defer a.lockLifecycle()()
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	resolvedType, err := a.explicitRootSessionTypeLocked(agentType)
	if err != nil {
		return "", err
	}
	proj, err := a.projectForSessionCreateLocked(projectID)
	if err != nil {
		return "", err
	}
	store, err := snapshot.NewForSessionsRoot(a.projects.SessionsRoot(proj.ID), a.projects.Root(), proj.ID)
	if err != nil {
		return "", err
	}
	unit, _, err := a.rootRunningUnitLocked(store, resolvedType, proj.ID, proj.Name, proj.Path)
	if err != nil {
		return "", err
	}
	if err := unit.store.BeginNewSessionStaged(proj.Path); err != nil {
		return "", err
	}
	a.ensureActiveModelForSessionLocked(unit)
	if unit.currentRef.Provider != "" && unit.currentRef.Model != "" {
		if err := unit.store.SetModel(unit.currentRef.Provider, unit.currentRef.Model); err != nil {
			fmt.Fprintf(os.Stderr, "lightcode: store.SetModel: %v\n", err)
		}
	}
	unit.lp.ResetHistory()
	if unit.fileTracker != nil {
		unit.fileTracker.Reset()
	}
	a.loadTokensFromDiskForSession(unit)
	a.setCurrentSessionLocked(unit)
	return unit.store.SessionID(), nil
}

func (a *Agent) NewSessionForProjectPath(projectPath string, agentType string) (string, error) {
	proj, err := a.ensureProjectForPath(projectPath)
	if err != nil {
		return "", err
	}
	return a.NewSession(proj.ID, agentType)
}

func (a *Agent) explicitRootSessionTypeLocked(agentType string) (string, error) {
	if strings.TrimSpace(agentType) == "" {
		agentType = "primary"
	}
	resolved, err := a.resolvedAgentTypeLocked(agentType)
	if err != nil {
		return "", err
	}
	if resolved.Name == "compact" {
		return "", fmt.Errorf("agent type %q cannot be started as a session", resolved.Name)
	}
	return resolved.Name, nil
}

func (a *Agent) projectForSessionCreateLocked(projectID string) (*project.Project, error) {
	if strings.TrimSpace(projectID) == "" {
		return a.projects.Ensure()
	}
	projects, err := project.List(a.projects.Root())
	if err != nil {
		return nil, err
	}
	for i := range projects {
		if projects[i].ID == projectID {
			p := projects[i]
			return &p, nil
		}
	}
	return nil, fmt.Errorf("unknown project %q", projectID)
}

func (a *Agent) ensureProjectForPath(projectPath string) (*project.Project, error) {
	if strings.TrimSpace(projectPath) == "" {
		return nil, fmt.Errorf("project path is required")
	}
	abs, err := filepath.Abs(projectPath)
	if err != nil {
		return nil, err
	}
	if a.projects == nil {
		return nil, fmt.Errorf("project resolver unavailable")
	}
	return project.EnsureForPath(a.projects.Root(), abs)
}

// SessionArchive archives a session.
// claimPersistedOnlySession acquires a temporary claim for a session that is
// not live in this owner, so a persisted-only archive/delete cannot mutate a
// session another process is driving. The caller releases via the returned
// func.
func (a *Agent) claimPersistedOnlySession(sessionsRoot, id string) (func(), error) {
	projectID := filepath.Base(filepath.Dir(sessionsRoot))
	projectsRoot := filepath.Dir(filepath.Dir(sessionsRoot))
	claim, ok, err := snapshot.AcquireSessionClaim(projectsRoot, projectID, id)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("session %q is being driven by another process", id)
	}
	return func() { _ = claim.Release() }, nil
}

func (a *Agent) SessionArchive(id string) error {
	defer a.lockLifecycle()()
	id = strings.TrimSpace(id)
	sessionsRoot, err := a.sessionsRootForUserManagedSession(id)
	if err != nil {
		return err
	}
	closedCurrent, err := a.closeIfCurrent(id)
	if err != nil {
		return err
	}
	needTempClaim := closedCurrent
	var releaseClose func()
	if !closedCurrent {
		releaseClose, err = a.beginLiveSessionClose(id)
		if err != nil {
			return err
		}
		if releaseClose != nil {
			defer func() {
				if releaseClose != nil {
					releaseClose()
				}
			}()
		} else {
			needTempClaim = true
		}
	}
	if needTempClaim {
		// A current or persisted-only target holds no live store claim; take a
		// temporary claim so the durable mutation still owns the session.
		claimRelease, cerr := a.claimPersistedOnlySession(sessionsRoot, id)
		if cerr != nil {
			return cerr
		}
		defer claimRelease()
	}
	if err := snapshot.ArchiveSession(sessionsRoot, id); err != nil {
		return err
	}
	if a.gate != nil {
		a.gate.CancelSession(id)
	}
	if _, err := a.closeLiveSession(id); err != nil {
		return err
	}
	releaseClose = nil
	if closedCurrent {
		a.resetCurrentSessionState()
	}
	return nil
}

// SessionDelete removes a session from disk.
func (a *Agent) SessionDelete(id string) error {
	defer a.lockLifecycle()()
	id = strings.TrimSpace(id)
	sessionsRoot, err := a.sessionsRootForUserManagedSession(id)
	if err != nil {
		return err
	}
	closedCurrent, err := a.closeIfCurrent(id)
	if err != nil {
		return err
	}
	needTempClaim := closedCurrent
	var releaseClose func()
	if !closedCurrent {
		releaseClose, err = a.beginLiveSessionClose(id)
		if err != nil {
			return err
		}
		if releaseClose != nil {
			defer func() {
				if releaseClose != nil {
					releaseClose()
				}
			}()
		} else {
			needTempClaim = true
		}
	}
	if needTempClaim {
		// A current or persisted-only target holds no live store claim; take a
		// temporary claim so the durable mutation still owns the session.
		claimRelease, cerr := a.claimPersistedOnlySession(sessionsRoot, id)
		if cerr != nil {
			return cerr
		}
		defer claimRelease()
	}
	if err := snapshot.DeleteSession(sessionsRoot, id); err != nil {
		return err
	}
	if a.gate != nil {
		a.gate.CancelSession(id)
	}
	if a.memoryHooks != nil {
		_ = a.memoryHooks.DeleteSessionSummaries(id)
	}
	if _, err := a.closeLiveSession(id); err != nil {
		return err
	}
	releaseClose = nil
	if closedCurrent {
		a.resetCurrentSessionState()
	}
	return nil
}

// SessionMessages returns the persisted messages for the current session.
// SessionMessagesFor returns persisted messages for a session without
// switching the active session.
func (a *Agent) SessionMessagesFor(id string) ([]DisplayMessage, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		rt := a.ensureRuntime()
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if a.store == nil || !a.store.Active() {
			return nil, nil
		}
		msgs, _ := a.messagesForFrontendForSession("")
		return msgs, nil
	}
	a.ensureRuntime().mu.Lock()
	unit, err := a.liveSessionLocked(id)
	a.ensureRuntime().mu.Unlock()
	if err == nil {
		return a.messagesForFrontendForStore(unit.store, id)
	}
	// Not live: resolve the session's owning project and read it read-only,
	// without a mutating load or a claim, rather than falling back to the
	// current session (which would return a different session's messages).
	root, rerr := a.sessionsRootForSession(id)
	if rerr != nil {
		return nil, rerr
	}
	store, serr := snapshot.NewForSessionsRoot(root, "", "")
	if serr != nil {
		return nil, serr
	}
	return a.messagesForFrontendForStore(store, id)
}

func (a *Agent) SessionMessagesByID(id string) ([]DisplayMessage, error) {
	a.ensureRuntime().mu.Lock()
	unit, err := a.liveSessionLocked(id)
	a.ensureRuntime().mu.Unlock()
	if err != nil {
		return nil, err
	}
	return a.messagesForFrontendForStore(unit.store, id)
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

func (a *Agent) sessionsRootForSession(id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return a.currentSessionsRoot()
	}
	if err := snapshot.ValidateSessionID(id); err != nil {
		return "", err
	}
	if a.projects != nil {
		if root, ok := a.liveSessionsRoot(id); ok {
			return root, nil
		}
		projects, err := project.List(a.projects.Root())
		if err != nil {
			return "", err
		}
		for _, proj := range projects {
			root := a.projects.SessionsRoot(proj.ID)
			_, err := os.Stat(filepath.Join(root, id, "meta.json"))
			if err == nil {
				return root, nil
			}
			if err != nil && !os.IsNotExist(err) {
				return "", err
			}
		}
	}
	return a.currentSessionsRoot()
}

func (a *Agent) sessionsRootForUserManagedSession(id string) (string, error) {
	root, err := a.sessionsRootForSession(id)
	if err != nil {
		return "", err
	}
	// For live sessions, check the in-memory unit directly — child sessions
	// are never registered live, and compact is identifiable by agent type.
	// This avoids a disk read that may fail if the session directory was
	// created by a different store instance.
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit := a.sessions[id]
	rt.mu.Unlock()
	if unit != nil {
		if isCompactSessionType(unit.activeAgentType) {
			return "", internalTranscriptSessionError(id)
		}
		return root, nil
	}
	if err := a.rejectInternalSession(root, id); err != nil {
		return "", err
	}
	return root, nil
}

func (a *Agent) rejectInternalSession(root, id string) error {
	meta, err := snapshot.LoadSessionMeta(root, id)
	if err != nil {
		return err
	}
	if isCompactSessionType(meta.ActiveAgentType) || meta.ParentSessionID != "" {
		return internalTranscriptSessionError(id)
	}
	return nil
}

func isCompactSessionType(agentType string) bool {
	return strings.TrimSpace(agentType) == "compact"
}

func internalTranscriptSessionError(id string) error {
	return fmt.Errorf("session %q is an internal transcript session", id)
}

func (a *Agent) liveSessionsRoot(id string) (string, bool) {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	a.ensureSessionMapLocked()
	unit := a.sessions[id]
	if unit == nil || unit.projectID == "" || a.projects == nil {
		return "", false
	}
	return a.projects.SessionsRoot(unit.projectID), true
}

func unitMutableLocked(unit *session) error {
	if unit == nil {
		return snapshot.ErrNoSession
	}
	if unit.transitioning {
		return fmt.Errorf("session is closing or switching")
	}
	if unit.busy {
		return fmt.Errorf("a turn is running")
	}
	return nil
}

func (a *Agent) beginLiveSessionClose(id string) (func(), error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	a.ensureSessionMapLocked()
	unit := a.sessions[id]
	if unit == nil || unit == a.session {
		rt.mu.Unlock()
		return nil, nil
	}
	if unit.store == nil || !unit.store.Active() || unit.store.SessionID() != id {
		rt.mu.Unlock()
		return nil, nil
	}
	if err := unitMutableLocked(unit); err != nil {
		rt.mu.Unlock()
		return nil, err
	}
	unit.transitioning = true
	rt.mu.Unlock()
	return func() { a.endLiveTransition(unit) }, nil
}

func (a *Agent) closeIfCurrent(id string) (bool, error) {
	if !a.store.Active() || a.store.SessionID() != id {
		return false, nil // not the current session: not a transition, queue untouched
	}
	// Transition begins only once we've decided to actually close the current
	// session; clear registered before cancelAndWaitIdle covers its error path.
	a.ensureRuntime().beginTransition()
	defer a.endLiveTransition(a.session)
	if err := a.cancelAndWaitIdle(); err != nil {
		return false, err
	}
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	// Detach (not Close) so an empty session is preserved for archive; delete
	// removes it via the atomic rename, not an empty-session discard.
	a.store.Detach()
	if a.currentSessionID == id {
		a.currentSessionID = ""
	}
	delete(a.sessions, id)
	// Close (no LoadSession follows for archive/delete) is the irreversible
	// change: clear the queue now.
	a.ensureRuntime().clearQueueLocked()
	return true, nil
}

func (a *Agent) closeLiveSession(id string) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, nil
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	a.ensureSessionMapLocked()
	unit := a.sessions[id]
	if unit == nil {
		return false, nil
	}
	if unit.store == nil || !unit.store.Active() || unit.store.SessionID() != id {
		delete(a.sessions, id)
		return false, nil
	}
	if unit == a.session {
		return false, nil
	}
	if unit.busy {
		return false, fmt.Errorf("cannot close session while a turn is running")
	}
	// Detach (not Close) so an archived empty session is preserved; delete
	// removes the directory via the atomic rename.
	unit.store.Detach()
	rt.clearQueueLockedForSession(unit)
	delete(a.sessions, id)
	return true, nil
}

func (a *Agent) messagesForFrontend() []DisplayMessage {
	msgs, _ := a.messagesForFrontendForSession("")
	return msgs
}

func (a *Agent) messagesForFrontendForSession(sessionID string) ([]DisplayMessage, error) {
	return a.messagesForFrontendForStore(a.store, sessionID)
}

// completeState is a session's complete live state: its transcript plus the
// captured live classes read as one consistent set. The compacting flag is
// captured with its reader in a later change.
type completeState struct {
	transcript  completeTranscript
	tokens      TokenReport
	model       coremodel.ModelRef
	busy        bool
	queue       QueueState
	warnings    []PromptWarning
	permissions []permission.Request
}

// captureState reads a session's durable committed history outside the
// captured-state locks (it does I/O), then acquires the live-class locks in the
// total order and reads activity/queue/model, tokens, warnings, and the
// transcript tail/errors/revision while holding them, so the snapshot is one
// consistent set with no class read outside the lock that guards it.
func (a *Agent) captureState(unit *session) (completeState, error) {
	if unit == nil || unit.store == nil || unit.transcript == nil {
		return completeState{}, snapshot.ErrNoSession
	}
	sessionID := sessionIDOf(unit)
	var committed []DisplayMessage
	if unit.store.Active() {
		var err error
		committed, err = a.messagesForFrontendForStore(unit.store, sessionID)
		if err != nil {
			return completeState{}, err
		}
	}
	rt := a.ensureRuntime()
	tr := unit.transcript

	rt.mu.Lock()
	defer rt.mu.Unlock()
	busy := unit.busy
	model := unit.currentRef
	queue := rt.queueSnapshotLocked(unit)
	var permissions []permission.Request
	if a.gate != nil {
		permissions = a.gate.PendingForSession(sessionID)
	}

	unit.tokensMu.Lock()
	defer unit.tokensMu.Unlock()
	tokens := a.buildReportForSessionLocked(unit)

	a.warningsMu.Lock()
	defer a.warningsMu.Unlock()
	warnings := append([]PromptWarning(nil), a.warningSnapshot...)

	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()
	return completeState{
		transcript: completeTranscript{
			committed: committed,
			tail:      tr.tailSnapshotLocked(),
			errors:    tr.errorSnapshotLocked(),
			revision:  tr.revisionLocked(),
		},
		tokens:      tokens,
		model:       model,
		busy:        busy,
		queue:       queue,
		warnings:    warnings,
		permissions: permissions,
	}, nil
}

// captureTranscript reads a session's durable committed history outside the
// sequence lock (it does I/O), then snapshots the coordinator's retained tail,
// retained errors, and revision under seqMu so the live pieces form one
// consistent set. This is the single-read shape; a live-selection capture layers
// revision revalidation on top.
func (a *Agent) captureTranscript(unit *session) (completeTranscript, error) {
	if unit == nil || unit.store == nil || unit.transcript == nil {
		return completeTranscript{}, snapshot.ErrNoSession
	}
	var committed []DisplayMessage
	if unit.store.Active() {
		var err error
		committed, err = a.messagesForFrontendForStore(unit.store, sessionIDOf(unit))
		if err != nil {
			return completeTranscript{}, err
		}
	}
	tr := unit.transcript
	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()
	return completeTranscript{
		committed: committed,
		tail:      tr.tailSnapshotLocked(),
		errors:    tr.errorSnapshotLocked(),
		revision:  tr.revisionLocked(),
	}, nil
}

func (a *Agent) messagesForFrontendForStore(store *snapshot.Store, sessionID string) ([]DisplayMessage, error) {
	if store == nil {
		return nil, snapshot.ErrNoSession
	}
	var rec *snapshot.CompactionRecord
	var err error
	if sessionID == "" {
		rec, err = store.LoadCompaction()
	} else {
		rec, err = store.LoadCompactionForSession(sessionID)
	}
	if err != nil {
		return nil, err
	}
	var raw []snapshot.TurnMessages
	if sessionID == "" {
		if rec != nil {
			raw, err = store.LoadCompleteTurnsAfterReadOnly(rec.BoundaryTurn)
		} else {
			raw, err = store.LoadCompleteTurnsReadOnly()
		}
	} else if rec != nil {
		raw, err = store.LoadCompleteTurnsAfterForSessionReadOnly(sessionID, rec.BoundaryTurn)
	} else {
		raw, err = store.LoadCompleteTurnsForSessionReadOnly(sessionID)
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
	return a.applyTurnActionForSession(a.session, turn, action, alsoRevertCode)
}

func (a *Agent) ApplyTurnActionForSession(sessionID string, turn int, action string, alsoRevertCode bool) (TurnActionResult, error) {
	defer a.lockLifecycle()()
	unit, err := a.resolveRootDriveSession(sessionID)
	if err != nil {
		return TurnActionResult{}, err
	}
	return a.applyTurnActionForSession(unit, turn, action, alsoRevertCode)
}

func (a *Agent) applyTurnActionForSession(unit *session, turn int, action string, alsoRevertCode bool) (TurnActionResult, error) {
	// fork / revert_history change the session; clear the queue at the
	// irreversible store mutation and emit after unlock (defer LIFO).
	var clearedVersion int
	var queueCleared bool
	var eventSessionID string
	var eventProjectID string
	defer func() {
		if queueCleared {
			a.emitEvent(Event{Kind: EventQueueChanged, SessionID: eventSessionID, ProjectID: eventProjectID, Queue: emptyQueue(), QueueVersion: clearedVersion})
		}
	}()
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if unit == nil || unit.store == nil || !unit.store.Active() {
		return TurnActionResult{}, snapshot.ErrNoSession
	}
	if err := unitMutableLocked(unit); err != nil {
		return TurnActionResult{}, err
	}
	if turn < 1 {
		return TurnActionResult{}, fmt.Errorf("turn must be >= 1")
	}

	eventSessionID = sessionIDOf(unit)
	eventProjectID = unit.projectID
	prefill := a.userMessageContentForTurnForSession(unit, turn)
	result := TurnActionResult{Action: action, Turn: turn}

	switch action {
	case TurnActionRevertCode:
		target := turn - 1
		result.TargetTurn = target
		revertResult, err := unit.store.RevertCode(target)
		if err != nil {
			return TurnActionResult{}, err
		}
		result.RestoredFiles = revertResult.Restored
		result.SkippedFiles = revertResult.Skipped
		a.resetFileTrackerForSession(unit)
		return result, nil

	case TurnActionRevertHistory:
		target := turn - 1
		result.TargetTurn = target
		result.Prefill = prefill
		result.SessionChanged = true
		if alsoRevertCode {
			revertResult, err := unit.store.RevertCode(target)
			if err != nil {
				return TurnActionResult{}, err
			}
			result.RestoredFiles = revertResult.Restored
			result.SkippedFiles = revertResult.Skipped
		}
		if err := unit.store.RevertHistory(target); err != nil {
			return TurnActionResult{}, err
		}
		// History irreversibly truncated: the queued input no longer applies.
		_, clearedVersion, queueCleared = a.ensureRuntime().clearQueueLockedForSession(unit)
		if err := a.loadHistoryIntoLoopForSession(unit); err != nil {
			return TurnActionResult{}, err
		}
		a.resetFileTrackerForSession(unit)
		return a.populateTurnActionResultForSession(unit, result), nil

	case TurnActionFork:
		target := turn
		result.TargetTurn = target
		result.SessionChanged = true
		candidate, err := a.forkUnitStagedLocked(unit, target)
		if err != nil {
			return TurnActionResult{}, err
		}
		// Code revert runs only after the fork is published, so a fork failure
		// never mutates the working tree. It is best-effort against the source
		// store, which retains the post-target snapshots needed to undo later
		// changes: a revert error keeps the partial result rather than failing
		// the already-committed fork.
		if alsoRevertCode {
			revertResult, revertErr := unit.store.RevertCode(target)
			result.RestoredFiles = revertResult.Restored
			result.SkippedFiles = revertResult.Skipped
			if revertErr != nil {
				fmt.Fprintf(os.Stderr, "lightcode: fork code revert: %v\n", revertErr)
			}
		}
		return a.populateTurnActionResultForSession(candidate, result), nil

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
	return a.populateTurnActionResultForSession(a.session, result)
}

func (a *Agent) populateTurnActionResultForSession(unit *session, result TurnActionResult) TurnActionResult {
	result.Session = sessionSummary(unit)
	if unit != nil && unit.store != nil && unit.store.Active() {
		result.Messages, _ = a.messagesForFrontendForStore(unit.store, unit.store.SessionID())
		unit.tokensMu.Lock()
		result.Tokens = a.buildReportForSessionLocked(unit)
		unit.tokensMu.Unlock()
	}
	return result
}

func (a *Agent) userMessageContentForTurn(turn int) string {
	return a.userMessageContentForTurnForSession(a.session, turn)
}

func (a *Agent) userMessageContentForTurnForSession(unit *session, turn int) string {
	var msgs []DisplayMessage
	if unit != nil && unit.store != nil && unit.store.Active() {
		msgs, _ = a.messagesForFrontendForStore(unit.store, unit.store.SessionID())
	}
	for _, msg := range msgs {
		if msg.Type == "user" && msg.Turn == turn {
			return msg.Content
		}
	}
	return ""
}

// RevertCode restores files to their state at the given turn.
func (a *Agent) RevertCode(turn int) (snapshot.RevertResult, error) {
	return a.revertCodeForSession(a.session, turn)
}

func (a *Agent) RevertCodeForSession(sessionID string, turn int) (snapshot.RevertResult, error) {
	unit, err := a.resolveRootDriveSession(sessionID)
	if err != nil {
		return snapshot.RevertResult{}, err
	}
	return a.revertCodeForSession(unit, turn)
}

func (a *Agent) revertCodeForSession(unit *session, turn int) (snapshot.RevertResult, error) {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if unit == nil || unit.store == nil || !unit.store.Active() {
		return snapshot.RevertResult{}, snapshot.ErrNoSession
	}
	if err := unitMutableLocked(unit); err != nil {
		return snapshot.RevertResult{}, err
	}
	result, err := unit.store.RevertCode(turn)
	if err != nil {
		return result, err
	}
	a.resetFileTrackerForSession(unit)
	return result, nil
}

// RevertHistory truncates conversation after the given turn.
func (a *Agent) RevertHistory(turn int) error {
	return a.revertHistoryForSession(a.session, turn)
}

func (a *Agent) RevertHistoryForSession(sessionID string, turn int) error {
	defer a.lockLifecycle()()
	unit, err := a.resolveRootDriveSession(sessionID)
	if err != nil {
		return err
	}
	return a.revertHistoryForSession(unit, turn)
}

func (a *Agent) revertHistoryForSession(unit *session, turn int) error {
	var clearedVersion int
	var queueCleared bool
	var eventSessionID string
	var eventProjectID string
	defer func() {
		if queueCleared {
			a.emitEvent(Event{Kind: EventQueueChanged, SessionID: eventSessionID, ProjectID: eventProjectID, Queue: emptyQueue(), QueueVersion: clearedVersion})
		}
	}()
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if unit == nil || unit.store == nil || !unit.store.Active() {
		return snapshot.ErrNoSession
	}
	if err := unitMutableLocked(unit); err != nil {
		return err
	}
	eventSessionID = sessionIDOf(unit)
	eventProjectID = unit.projectID
	if err := unit.store.RevertHistory(turn); err != nil {
		return err
	}
	_, clearedVersion, queueCleared = a.ensureRuntime().clearQueueLockedForSession(unit)
	if err := a.loadHistoryIntoLoopForSession(unit); err != nil {
		return err
	}
	a.resetFileTrackerForSession(unit)
	return nil
}

// cleanupStaging removes a fork's staging directory after a preparation
// failure, joining any cleanup error with the original cause so neither is lost.
func cleanupStaging(stagingRoot string, cause error) error {
	if err := os.RemoveAll(stagingRoot); err != nil {
		return errors.Join(cause, fmt.Errorf("snapshot: fork staging cleanup: %w", err))
	}
	return cause
}

// forkUnitStagedLocked forks unit at the given turn through staged publication.
// The source session is copied into an unlisted staging directory and loaded
// through a separate candidate store while the source stays live and claimed.
// The single durable commit is the atomic rename of the validated candidate
// into the session namespace; only then is the new fork registered as its own
// live unit and, when the source was current, selected. Any preparation or
// rename failure leaves the source unchanged and removes only its staging
// directory. Caller holds rt.mu.
func (a *Agent) forkUnitStagedLocked(unit *session, turn int) (*session, error) {
	if unit == nil || unit.store == nil || !unit.store.Active() {
		return nil, snapshot.ErrNoSession
	}
	if err := unitMutableLocked(unit); err != nil {
		return nil, err
	}
	oldID := unit.store.SessionID()
	sessionsRoot := unit.store.Root()
	stagingRoot, err := snapshot.NewStagingSessionsRoot(sessionsRoot)
	if err != nil {
		return nil, err
	}
	// Copy the source into staging; the source store stays authoritative.
	newID, _, err := unit.store.ForkInto(turn, stagingRoot)
	if err != nil {
		return nil, cleanupStaging(stagingRoot, err)
	}
	// Prepare a separate candidate store and unit against the staged copy. The
	// source (oldID) and candidate (newID) claims are both held here.
	candidateStore, err := unit.store.NewStagingStore(stagingRoot)
	if err != nil {
		return nil, cleanupStaging(stagingRoot, err)
	}
	if err := candidateStore.LoadSession(newID); err != nil {
		return nil, cleanupStaging(stagingRoot, err)
	}
	candidate, _, err := a.rootRunningUnitLocked(candidateStore, unit.activeAgentType, unit.projectID, unit.projectName, unit.projectRoot)
	if err != nil {
		candidateStore.Detach()
		return nil, cleanupStaging(stagingRoot, err)
	}
	if err := a.loadHistoryIntoLoopForSession(candidate); err != nil {
		candidateStore.Detach()
		return nil, cleanupStaging(stagingRoot, err)
	}
	// Prepare all remaining candidate state from the staged copy before the
	// durable commit, so no fallible read runs after the rename.
	a.resetFileTrackerForSession(candidate)
	a.loadTokensFromDiskForSession(candidate)
	if err := a.inheritActiveModelLocked(candidate, unit); err != nil {
		candidateStore.Detach()
		return nil, cleanupStaging(stagingRoot, err)
	}
	// Durable commit: the atomic rename publishes the candidate. The source is
	// still authoritative until it succeeds.
	if err := snapshot.PublishStagedSession(stagingRoot, sessionsRoot, newID); err != nil {
		candidateStore.Detach()
		return nil, cleanupStaging(stagingRoot, err)
	}
	if err := candidateStore.RelocateActiveSessionPaths(sessionsRoot); err != nil {
		candidateStore.Detach()
		return nil, err
	}
	// Post-commit publication is infallible: drop the empty staging parent, then
	// register the fork as its own live unit, selecting it only when the source
	// was current.
	_ = os.RemoveAll(stagingRoot)
	if a.currentSessionID == oldID {
		a.setCurrentSessionLocked(candidate)
	} else {
		a.registerLiveSessionLocked(candidate)
	}
	return candidate, nil
}

// ForkSession creates a new session branched from the given turn.
func (a *Agent) ForkSession(turn int) error {
	return a.forkSessionForSession(a.session, turn)
}

func (a *Agent) ForkSessionForSession(sessionID string, turn int) error {
	defer a.lockLifecycle()()
	unit, err := a.resolveRootDriveSession(sessionID)
	if err != nil {
		return err
	}
	return a.forkSessionForSession(unit, turn)
}

func (a *Agent) forkSessionForSession(unit *session, turn int) error {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	_, err := a.forkUnitStagedLocked(unit, turn)
	return err
}

// SnapshotList returns the timeline of all snapshots in the session.
func (a *Agent) SnapshotList() ([]Snapshot, error) {
	if !a.store.Active() {
		return nil, nil
	}
	return snapshotListForStore(a.store)
}

func (a *Agent) SnapshotListForSession(sessionID string) ([]Snapshot, error) {
	unit, err := a.resolveRootDriveSession(sessionID)
	if err != nil {
		return nil, err
	}
	return snapshotListForStore(unit.store)
}

func snapshotListForStore(store *snapshot.Store) ([]Snapshot, error) {
	if store == nil || !store.Active() {
		return nil, nil
	}
	turns, err := store.ListTurns()
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
	return viewerReadAllowedAtRoot(a.projectRoot, path)
}

func viewerReadAllowedAtRoot(projectRoot string, path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(projectRoot, path)
	}
	canonicalRoot, _, err := pathutil.ResolveAbsPath(projectRoot)
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
	return readFileContentAtCanonical(canonicalPath)
}

func (a *Agent) ReadFileContentForProjectPath(projectPath string, path string) (string, error) {
	proj, err := a.ensureProjectForPath(projectPath)
	if err != nil {
		return "", err
	}
	canonicalPath, err := viewerReadAllowedAtRoot(proj.Path, path)
	if err != nil {
		return "", err
	}
	return readFileContentAtCanonical(canonicalPath)
}

func readFileContentAtCanonical(canonicalPath string) (string, error) {
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

func (a *Agent) ProjectCurrentForPath(projectPath string) (ProjectSummary, error) {
	proj, err := a.ensureProjectForPath(projectPath)
	if err != nil {
		return ProjectSummary{}, err
	}
	return ProjectSummary{
		ID:           proj.ID,
		Name:         proj.Name,
		Path:         proj.Path,
		CreatedAt:    proj.CreatedAt,
		LastActivity: proj.LastActivity,
	}, nil
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
	return a.resolvedAgentTypeForProjectLocked(name, "")
}

func (a *Agent) resolvedAgentTypeForProjectLocked(name string, projectID string) (agentcfg.Resolved, error) {
	if a.agents == nil {
		return agentcfg.Resolved{}, fmt.Errorf("agents config is not loaded")
	}
	ctx := a.agentResolveContextLocked()
	if strings.TrimSpace(projectID) != "" {
		ctx.ProjectID = projectID
	}
	return a.agents.Resolve(name, ctx)
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
	return a.ensureActiveModelForSessionLocked(a.session)
}

func (a *Agent) ensureActiveModelForSessionLocked(unit *session) bool {
	if unit == nil {
		return false
	}
	if a.modelRefConnected(unit.currentRef) {
		return true
	}
	a.clearActiveModelForSessionLocked(unit)
	agentType := unit.activeAgentType
	if agentType == "" {
		agentType = "primary"
	}
	ref, _, ok := a.resolvedAgentModelLocked(agentType)
	if !ok || !a.modelRefConnected(ref) {
		return false
	}
	client, model, err := newProviderClient(a.catalog, ref)
	if err != nil {
		return false
	}
	a.setActiveModelForSessionLocked(unit, ref, client, model)
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
