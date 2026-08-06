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
	"github.com/MMinasyan/lightcode/internal/atomicfs"
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

	store    *snapshot.Store
	lp       *loop.Loop
	registry *tool.Registry

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
	compacting bool
	turnCancel context.CancelFunc
	turnCtx    context.Context

	queue         []QueuedItem
	queueVersion  int
	queueSeq      int
	transitioning bool
	seenSessions  map[string]bool

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

	// captureProbe is a test seam invoked after each durable read and before
	// the locked revalidation, to deterministically inject a revision change or
	// a read error in the window the retry exists for; nil in production.
	captureProbe func(attempt int) error

	// durableReadHook is a test seam invoked immediately before each observed
	// durable I/O at the four owner-lock sites: the fork staging tree copy
	// (which must run outside runtime.mu), resume's listing/history loads
	// (which run under lifecycleMu), the tokens file read (outside the unit's
	// tokensMu), and the tokens file write (inside the unit's tokensMu). The
	// seam is nil in production; tests swap it to assert each I/O's position
	// relative to its site's owner lock.
	durableReadHook func()

	// shutdownBarrierHook is a test seam invoked immediately before
	// ShutdownOwner acquires the lifecycle lock to publish closed — the one
	// statement before shutdown blocks on the admission barrier. The seam is
	// nil in production; tests swap it to observe that moment.
	shutdownBarrierHook func()
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

// projectRootOf resolves the running unit's project root for its per-session
// process view, with the same liveness gate as sessionIDOf so an inactive unit
// falls back to the owner-level root.
func projectRootOf(unit *session) string {
	if unit == nil || unit.store == nil || !unit.store.Active() {
		return ""
	}
	return unit.projectRoot
}

// registerLiveSessionLocked records unit in the live-session map. An
// existing entry for the same id is only acceptable when it is the same unit
// (idempotent re-registration); a different unit is refused with an error —
// a silently overwritten registration is indistinguishable from success, and
// a bare id must resolve to exactly one live session.
func (a *Agent) registerLiveSessionLocked(unit *session) error {
	id := sessionIDOf(unit)
	if id == "" {
		return nil
	}
	unit.syncEventOwner()
	a.ensureSessionMapLocked()
	if existing := a.sessions[id]; existing != nil && existing != unit {
		return fmt.Errorf("session %q is already registered to a different live session", id)
	}
	a.sessions[id] = unit
	// A session becomes live here: publish its transcript registry entry so
	// every by-id resolution finds its coordinator. Idempotent re-registration
	// keeps the existing coordinator (and its sequenced rows).
	a.ensureRuntime().registerTranscript(id, unit.store)
	return nil
}

func (a *Agent) setCurrentSessionLocked(unit *session) error {
	if unit == nil {
		a.currentSessionID = ""
		a.session = &session{rt: a.rt}
		return nil
	}
	if unit.rt == nil {
		unit.rt = a.rt
	}
	a.session = unit
	a.currentSessionID = sessionIDOf(unit)
	return a.registerLiveSessionLocked(unit)
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
		a.ensureRuntime().registerTranscript(id, a.session.store)
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
// is running. The Detect goroutine is tracked on the runtime's background group
// like every other owner background goroutine, so shutdown joins it. Caller
// holds servicesMu.
func (a *Agent) startDetectLocked(e *lspEntry) {
	if a.detectCtx != nil && !e.detecting {
		e.detecting = true
		rt := a.ensureRuntime()
		rt.bgWG.Add(1)
		go func() {
			defer rt.bgWG.Done()
			e.mgr.Detect(a.detectCtx)
		}()
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
	projectsRoot := ""
	if a.projects != nil {
		projectsRoot = a.projects.Root()
	}
	tt := newTaskTool(taskToolConfig{
		AgentTypes:    a.agents,
		ParentStore:   store,
		ParentTracker: fileTracker,
		MaxConcurrent: a.cfg.Subagents.MaxConcurrent,
		TaggedEvents:  rt.taggedEvents,
		Runtime:       rt,
		ModelCatalog:  a.catalog,
		ToolsConfig:   a.cfg.Tools,
		HomeDir:       a.home,
		WorkspaceRoot: projectRoot,
		ProcMgr:       a.procMgr,
		MemoryStore:   a.memoryStore,
		ProjectID:     projectID,
		ProjectsRoot:  projectsRoot,
		MemoriesDir:   memoriesDir,
		LSPManager:    lspMgr,
		Check:         checkPolicy,
		Ask:           askPolicy,
		AskAction:     askActionPolicy,
		ResolveAdapt:  a.resolveAdapt,
	})
	registry.Register(tt)

	processes := a.procMgr.ForSession(
		func() string { return sessionIDOf(unitRef()) },
		func() string { return projectRootOf(unitRef()) },
	)
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

	// The gate calls this under its mutex; emit the request event directly so the
	// insert and its delivery are one atomic section a navigation capture cannot
	// split. The adapter callback only appends to its leaf queue, so this takes no
	// lock above the gate mutex.
	gate := permission.NewGate(func(_ context.Context, req permission.Request) {
		a.emitEvent(Event{
			Kind:      EventPermissionRequest,
			SessionID: req.SessionID,
			ProjectID: req.ProjectID,
			PermReq:   permissionRequestFromGateRequest(req),
		})
	})
	// The removal side mirrors the registration side: publish the resolution in
	// the same gate-mutex section that removed the pending request, so an adapter
	// clears its prompt mirror before the turn-end event arrives.
	gate.OnResolved = func(req permission.Request) {
		a.emitEvent(Event{
			Kind:      EventPermissionResolved,
			SessionID: req.SessionID,
			PermReq:   &PermissionRequest{ID: req.ID, SessionID: req.SessionID},
		})
	}
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

// Init starts background goroutines, runs the session sweep, and
// resumes the most recent session if one exists. ctx controls the
// agent's lifetime. It returns the id of the session it resumed, or ""
// when none was resumed, so the adapter can adopt that session as its
// startup selection instead of re-deriving one from a listing.
func (a *Agent) Init(ctx context.Context) string {
	return a.ensureRuntime().init(ctx)
}

func (rt *runtime) init(ctx context.Context) string {
	rt.initOnce.Do(func() {
		rt.resumedSessionID = rt.initOnceLocked(ctx)
	})
	return rt.resumedSessionID
}

func (rt *runtime) initOnceLocked(ctx context.Context) string {
	a := rt.agent
	// The background goroutines run on an owned context that is independent of
	// the host context. Once the owner accepts work, that work's lifetime is
	// the owner's: a host context may trigger shutdown but never severs
	// accepted work. Cancelling the host context still triggers the joined
	// shutdown through the watcher below, which cancels the owner context
	// after the in-flight turn join so the drainer stays alive to deliver
	// terminal events.
	rt.ownerCtx, rt.ownerCancel = context.WithCancel(context.Background())
	// The project LSP teardown watches the owner context and is tracked on the
	// same bgWG: ShutdownOwner cancels the owner context immediately before
	// joining bgWG, so the teardown fires and completes inside that join. It
	// cannot watch the host context — two of the three hosts cancel that only
	// after shutdown returns, so the trigger would never fire while the join
	// waits.
	rt.bgWG.Add(5)
	go func() { defer rt.bgWG.Done(); rt.drainLoopEvents(rt.ownerCtx) }()
	go func() { defer rt.bgWG.Done(); rt.runSignalScheduler(rt.ownerCtx) }()
	go func() { defer rt.bgWG.Done(); rt.runQueueDrainer(rt.ownerCtx) }()
	go func() {
		defer rt.bgWG.Done()
		<-rt.ownerCtx.Done()
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
	if a.memoryHooks != nil {
		_ = a.memoryHooks.Reconcile()
	}
	a.runSweep()
	resumed, err := a.resumeMostRecent()
	if err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: resume session: %v\n", err)
	}
	go func() { defer rt.bgWG.Done(); a.periodicSweep(rt.ownerCtx) }()

	// The owner is now running: record the detection context and start detection
	// once for every project manager created before Init (handlers are installed
	// at creation). Managers created later start detection when built.
	a.servicesMu.Lock()
	a.detectCtx = rt.ownerCtx
	for _, e := range a.lspManagers {
		a.startDetectLocked(e)
	}
	a.servicesMu.Unlock()

	// Host context cancellation drives the joined owner shutdown. It is started
	// after detection above so an already-cancelled host context cannot let the
	// watcher run ShutdownOwner — and join the background group — before
	// detection has registered on it.
	go func() {
		<-ctx.Done()
		a.ShutdownOwner()
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
	return resumed
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

// transcriptForSessionID resolves the live coordinator owning a session id, or
// nil for an unknown or sessionless event. Resolution takes the registry's map
// lock briefly and releases it before the caller takes the coordinator's own
// seqMu — two sessions' feeds never contend on one lock.
func (a *Agent) transcriptForSessionID(id string) *transcript {
	return a.ensureRuntime().transcriptForSessionID(id)
}

// feedTranscript folds one event into a session coordinator under its own seqMu
// and returns the sequence assigned to its display row (zero for a rowless event
// or a nil coordinator). A nil coordinator (sessionless or unknown session) is a
// no-op.
func feedTranscript(tr *transcript, ev Event) int {
	if tr == nil {
		return 0
	}
	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()
	return tr.feedLocked(ev)
}

// feedAndEmit sequences a transcript row (or session error) and enqueues its
// event in one seqMu section, so a navigation capture that reads the retained
// tail and errors under seqMu and appends its boundary cannot interleave: the row
// is in the snapshot and delivered before the boundary, or after both.
func (a *Agent) feedAndEmit(tr *transcript, ev Event) {
	if tr == nil {
		a.emitEvent(ev)
		return
	}
	tr.seqMu.Lock()
	ev.Seq = tr.feedLocked(ev)
	a.emitEvent(ev)
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
		rt.launchTurn(unit, turnCtx, cancel, nil)
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
	// Enqueue the warning event inside the warningsMu section that mutated the
	// snapshot, so a navigation capture (which reads warnings under warningsMu and
	// appends its boundary) cannot interleave.
	a.emitEvent(Event{Kind: EventWarning, Warnings: next})
	a.warningsMu.Unlock()
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
	// The coordinator sequences every root display row before the same event is
	// delivered, so the delivered event carries the row's sequence and an adapter
	// can gate it against a later capture's high-water.
	tr := a.transcriptForSessionID(sessionID)
	emit := func(out Event) {
		a.feedAndEmit(tr, out)
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
	case loop.Warning:
		kind, _ := ev.Metadata["kind"].(string)
		if kind == "" {
			kind = "protocol_warning"
		}
		a.addWarning("protocol", prompt.Warning{Kind: kind, Message: ev.Result})
	}
}

// permissionRequestFromGateRequest maps a gate request to the adapter-facing
// permission request the gate callback emits directly under the gate mutex.
func permissionRequestFromGateRequest(req permission.Request) *PermissionRequest {
	return &PermissionRequest{
		ID:                 req.ID,
		SessionID:          req.SessionID,
		ProjectID:          req.ProjectID,
		ToolName:           req.ToolName,
		Arg:                req.Arg,
		ResolvedArg:        req.ResolvedArg,
		CanAllowAll:        req.CanAllowAll,
		DisableProjectSave: req.DisableProjectSave,
		BatchIndex:         req.BatchIndex,
		BatchTotal:         req.BatchTotal,
		BatchFiles:         req.BatchFiles,
		BatchResolvedFiles: req.BatchResolvedFiles,
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
	// The coordinator sequences every child display row before the same event
	// is delivered, exactly as the root path does in dispatchLoopEvent, so the
	// delivered event carries the row's sequence and an adapter can gate it
	// against a later capture's high-water. Control events — subagent start,
	// permission requests, warnings — produce no transcript row and stay bare.
	tr := a.transcriptForSessionID(tev.SessionID)
	emit := func(out Event) { a.feedAndEmit(tr, out) }
	switch ev.Kind {
	case loop.TextDelta:
		base.Kind = EventTextDelta
		base.Result = ev.Result
		emit(base)
	case loop.ToolCallStart:
		base.Kind = EventToolCallStart
		base.ToolCallID = ev.ToolCallID
		base.ToolName = ev.ToolName
		base.Args = ev.Args
		emit(base)
	case loop.ToolCallEnd:
		base.Kind = EventToolCallEnd
		base.ToolCallID = ev.ToolCallID
		base.ToolName = ev.ToolName
		base.Args = ev.Args
		base.IsError = ev.IsError
		base.Result = ev.Result
		base.Metadata = ev.Metadata
		emit(base)
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
		emit(base)
	case loop.UserMessageDisplay:
		base.Kind = EventUserMessageDisplay
		base.Turn = ev.Turn
		base.Result = ev.Result
		emit(base)
	case loop.GenericSystemSignalDisplay:
		base.Kind = EventGenericSystemSignal
		base.Turn = ev.Turn
		base.Result = ev.Result
		emit(base)
	case loop.PermissionRequest:
		base.Kind = EventPermissionRequest
		base.PermReq = permissionRequestFromLoopEvent(ev, tev.SessionID, projectID)
		a.emitEvent(base)
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
	// Persist the snapshot inside the same tokensMu section that mutated the
	// entries: the marshal cannot be hoisted ahead of the lock, and the file
	// write is small and local, so writing it inside the section keeps two
	// concurrent writers from landing their snapshots in the opposite order.
	a.persistTokensForSessionLocked(unit)
	report := a.buildReportForSessionLocked(unit)
	// Enqueue the usage event inside the tokensMu section that produced the report,
	// so a navigation capture (which reads the report under tokensMu and appends its
	// boundary) cannot interleave: the event is captured in the snapshot or delivered
	// after the boundary, never lost or delivered before a boundary that omits it.
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
	unit.tokensMu.Unlock()
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

// persistTokensForSessionLocked serializes the unit's token entries and
// durably writes them to the session's tokens file. Caller holds
// unit.tokensMu: the entries are mutated inside the caller's same section, so
// the marshal cannot be hoisted ahead of the lock, and the small local write
// stays inside the section so concurrent writers cannot land their snapshots
// out of order.
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
	a.fireDurableReadHook()
	_ = atomicfs.Write(filepath.Join(unit.store.Dir(), tokensFileName), append(data, '\n'), 0o600)
}

func (a *Agent) runSweep() {
	if a.projects == nil {
		return
	}
	// The sweep runs from the hourly background goroutine (and once at init),
	// which holds no lock; snapshot the sessions policy under runtime.mu so the
	// reads cannot race applyReloadStateLocked's write of a.cfg. The directory
	// sweep itself runs after the lock is released.
	rt := a.ensureRuntime()
	rt.mu.Lock()
	autoArchive := a.cfg.Sessions.AutoArchive
	archiveAfterDays := a.cfg.Sessions.ArchiveAfterDays
	deleteAfterArchiveDays := a.cfg.Sessions.DeleteAfterArchiveDays
	rt.mu.Unlock()
	cfg := snapshot.LifecycleConfig{
		Enabled:                autoArchive,
		ArchiveAfterDays:       archiveAfterDays,
		DeleteAfterArchiveDays: deleteAfterArchiveDays,
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
	// Runs on the turn goroutine's model-request hook, which holds no lock;
	// snapshot the compaction policy under runtime.mu so these reads cannot
	// race a concurrent session switch's config reload.
	rt := a.ensureRuntime()
	rt.mu.Lock()
	enabled := a.cfg.Compaction.Enabled
	thresholdPct := a.cfg.Compaction.ThresholdPct
	rt.mu.Unlock()
	if !enabled {
		return false
	}
	unit.tokensMu.Lock()
	used := unit.lastContextUsed
	window := unit.contextWindowSize
	unit.tokensMu.Unlock()
	if window <= 0 || used <= 0 {
		return false
	}
	return float64(used)/float64(window) >= thresholdPct
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
		errEv := Event{Kind: EventError, SessionID: sessionIDOf(unit), ProjectID: unit.projectID, Error: fmt.Sprintf("compaction: %v", err), Turn: checkpoint.Turn}
		a.feedAndEmit(a.transcriptForSessionID(sessionIDOf(unit)), errEv)
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
	rt := a.ensureRuntime()
	// Set compacting and enqueue the start event in one runtime.mu section so a
	// capture reads a compacting flag consistent with compaction_start. The end
	// event clears compacting and enqueues in the same runtime.mu section, so no
	// capture snapshots compacting=true after compaction_end is delivered. The
	// replacement transcript is published separately as the rewrite boundary below.
	rt.mu.Lock()
	unit.compacting = true
	a.emitEvent(Event{Kind: EventCompactionStart, SessionID: sessionID, ProjectID: projectID})
	rt.mu.Unlock()
	defer func() {
		rt.mu.Lock()
		unit.compacting = false
		a.emitEvent(Event{Kind: EventCompactionEnd, SessionID: sessionID, ProjectID: projectID})
		rt.mu.Unlock()
	}()

	messages := unit.lp.Messages()
	activeStart := checkpoint.ActiveTurnStart
	if activeStart <= 0 || activeStart > len(messages) {
		activeStart = len(messages)
	}
	if activeStart <= 1 {
		return activeStart, fmt.Errorf("nothing to compact")
	}
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
	// Prebuild the fallible parts of the rewrite replacement before SaveCompaction
	// so no fallible read runs after the durable commit: the compacted completed
	// messages (durable turns after boundaryTurn, unchanged by SaveCompaction) and
	// the session summary. The live tail is read with the emit inside one seqMu
	// section on success, so its read-and-append stays atomic against row delivery.
	summary := sessionSummary(unit)
	committed, err := a.prebuiltCompactionCommitted(unit, sessionID, boundaryTurn)
	if err != nil {
		return activeStart, fmt.Errorf("prepare compacted replacement: %w", err)
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
	// Publish the rewrite boundary on durable success, before the loop-history
	// rewrite and memory indexing, so a live-selection capture racing the compaction
	// sees the revision advance promptly and re-reads the rewritten durable prefix
	// rather than publishing the pre-compaction one.
	a.publishCompactionRewrite(unit, sessionID, projectID, boundaryTurn, summary, committed)

	var activeReads []tool.ReadRecord
	if unit.fileTracker != nil && activeStart < len(messages) {
		// The tools policy is read under runtime.mu: this runs on the turn
		// goroutine's model-request hook and the explicit compact-now path,
		// both outside the lock, while a session switch can reload config.
		rt.mu.Lock()
		readMaxLines := a.cfg.Tools.ReadMaxLines
		rt.mu.Unlock()
		activeReads = activeTailReadRecords(messages[activeStart:], unit.fileTracker.Snapshot(), readMaxLines, unit.projectRoot)
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

	return newActiveStart, nil
}

// prebuiltCompactionCommitted renders the compacted completed messages — the
// durable completed turns after boundaryTurn, the rows messagesForFrontendForStore
// yields once the compaction record names that boundary. Those turns are unchanged
// by SaveCompaction, so rendering them before the durable commit lets the rewrite
// boundary carry a prebuilt committed prefix with no fallible post-commit read. The
// live tail is not read here: it is read inside the boundary's own seqMu section
// (with the emit), so the tail read-and-append stays one atomic section against
// concurrent row delivery.
func (a *Agent) prebuiltCompactionCommitted(unit *session, sessionID string, boundaryTurn int) ([]DisplayMessage, error) {
	var raw []snapshot.TurnMessages
	var err error
	if sessionID == "" {
		raw, err = unit.store.LoadCompleteTurnsAfterReadOnly(boundaryTurn)
	} else {
		raw, err = unit.store.LoadCompleteTurnsAfterForSessionReadOnly(sessionID, boundaryTurn)
	}
	if err != nil {
		return nil, err
	}
	return a.renderCompleteTurns(raw), nil
}

// publishCompactionRewrite advances the transcript rewrite epoch, prunes
// retained errors tagged to the compacted range, and appends the replacement
// transcript as one rewrite boundary after a durable compaction. The summary and
// committed prefix are prebuilt before SaveCompaction (the only fallible reads),
// so this performs no fallible post-commit read. The reset context total and its
// cumulative report share one tokensMu section, taken before the transcript
// section so tokensMu is never held inside seqMu. Reading the live tail and
// appending the boundary happen in one seqMu section — mutually exclusive with
// feedAndEmit's row publication — so a row delivered concurrently is either in
// the tail and enqueued before the boundary or absent and enqueued after it,
// never split. Advancing the epoch makes a live-selection capture that raced the
// compaction revalidate and re-read the rewritten durable prefix.
func (a *Agent) publishCompactionRewrite(unit *session, sessionID, projectID string, boundaryTurn int, summary SessionSummary, committed []DisplayMessage) {
	tr := a.transcriptForSessionID(sessionID)
	if tr == nil {
		return
	}
	unit.tokensMu.Lock()
	unit.lastContextUsed = 0
	tokens := a.buildReportForSessionLocked(unit)
	unit.tokensMu.Unlock()

	tr.seqMu.Lock()
	tr.compactionRewriteLocked()
	tr.dropErrorsThroughTurnLocked(boundaryTurn)
	messages := append(append([]DisplayMessage(nil), committed...), tr.tailMessagesLocked()...)
	payload := SessionPayload{Session: summary, Messages: messages, Tokens: tokens}
	a.emitEvent(Event{Kind: EventSessionRewrite, SessionID: sessionID, ProjectID: projectID, RewritePayload: &payload})
	tr.seqMu.Unlock()
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

// CompactNowForSession runs a compaction for a live session. The caller's ctx
// gates admission: an already-cancelled context refuses the compaction before
// the session is marked busy. Once admitted, the compaction's context derives
// from the owner context, so its lifetime is the owner's and a caller's
// cancellation never severs it.
func (a *Agent) CompactNowForSession(ctx context.Context, sessionID string) error {
	// A nil context is not a cancelled one: normalise it to a live context
	// ahead of the admission check, so the compaction is admitted as it
	// always was instead of being refused or panicking.
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
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
	compactCtx, cancel := context.WithCancel(rt.workCtx())
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

// resumeMostRecent scans active root sessions newest-first and resumes the
// first whose claim is acquirable, returning its id. A contended, corrupt, or
// unreadable candidate, or one whose history fails to load, releases its
// provisional claim and does not stop the scan. It returns ("", nil) when no
// candidate was resumed, and ("", err) on failure.
func (a *Agent) resumeMostRecent() (string, error) {
	defer a.lockLifecycle()()
	proj, err := a.projects.Current()
	if err != nil || proj == nil {
		return "", err
	}
	sessionsRoot := a.projects.SessionsRoot(proj.ID)
	if err := a.store.AttachSessionsRoot(sessionsRoot, a.projects.Root(), proj.ID); err != nil {
		return "", err
	}
	// Candidate enumeration and loading run under the lifecycle lock: the
	// listing and history reads are durable and fallible, but re-validating
	// every candidate after a hoisted read would require machinery this path
	// does not have, so the reads stay where the lifecycle lock already
	// excludes concurrent lifecycle operations.
	rt := a.ensureRuntime()
	rt.mu.Lock()
	closed := rt.closed
	rt.mu.Unlock()
	// Admission check before the candidate loop: a resume starting after
	// shutdown published closed must not load a candidate and take its claim.
	if closed {
		return "", errOwnerClosed
	}
	a.fireDurableReadHook()
	candidates, err := snapshot.List(sessionsRoot, "", snapshot.StateActive)
	if err != nil {
		return "", err
	}
	resumed := false
	for _, info := range candidates {
		if info.ParentSessionID != "" {
			continue // only root sessions resume
		}
		// Try active root sessions newest-first. A contended, corrupt, or
		// unreadable candidate, or one whose history fails to load, releases its
		// provisional claim and does not stop the scan.
		a.fireDurableReadHook()
		if err := a.store.LoadSession(info.ID); err != nil {
			continue
		}
		a.fireDurableReadHook()
		if err := a.loadHistoryIntoLoop(); err != nil {
			a.store.Detach()
			continue
		}
		rt.mu.Lock()
		a.setSessionProject(a.session, proj)
		if err := a.setCurrentSessionLocked(a.session); err != nil {
			rt.mu.Unlock()
			return "", err
		}
		rt.mu.Unlock()
		resumed = true
		break
	}
	if !resumed {
		return "", nil // no candidate opened; the adapter creates a new session
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
		return sessionIDOf(a.session), nil
	}
	a.restoreModelFromSession()
	rt.mu.Unlock()
	return sessionIDOf(a.session), nil
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
	agentType := "primary"
	if strings.TrimSpace(unit.activeAgentType) != "" {
		agentType = unit.activeAgentType
	}
	// The agents config is written under runtime.mu by a concurrent session
	// switch (applyReloadStateLocked -> setAgentTypesLocked), and this runs on
	// the turn goroutine without that lock. Resolve the agent type under
	// runtime.mu and copy the resolved values out: resolution is a pure
	// in-memory read, so only it runs under the lock. The resolve context is
	// built first because the current project id is a durable read, and the
	// prompt assembly (rules-file I/O) runs after release.
	ctx := agentcfg.ResolveContext{Home: a.home}
	if a.projects != nil {
		if proj, err := a.projects.Current(); err == nil && proj != nil {
			ctx.ProjectID = proj.ID
		}
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	resolved, resolveErr := a.resolvedAgentTypeWithContextLocked(agentType, ctx)
	rt.mu.Unlock()
	res := a.assembleSystemPromptForSessionResolved(unit, resolved, resolveErr)
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
	resolved, err := a.resolvedAgentTypeLocked(agentType)
	return a.assembleSystemPromptForSessionResolved(unit, resolved, err)
}

// assembleSystemPromptForSessionResolved renders the system prompt from an
// already-resolved agent type. The caller performs the resolution, so an
// unlocked caller (refreshSystemPromptForSession) can snapshot the resolved
// values under runtime.mu and release before the rules-file I/O below, while
// locked callers resolve under their existing hold. The resolved values are a
// copy, not pointers into the agents config.
func (a *Agent) assembleSystemPromptForSessionResolved(unit *session, resolved agentcfg.Resolved, resolveErr error) prompt.Result {
	if unit == nil || a.promptSvc == nil {
		return prompt.Result{}
	}
	spec := prompt.Spec{Size: prompt.SizeFull, Memory: true, Adapt: unit.activeAdapt}
	if resolveErr == nil {
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

// inheritActiveModelForForkedSession gives the fork candidate the source
// unit's live active model, preferring the in-memory selection over persisted
// metadata so an unpersisted model switch is not lost, and persists it into
// the candidate so the fork reopens on the same model. srcRef is the source's
// currentRef snapshotted under rt.mu by the caller in the fork's first lock
// half (currentRef is rt.mu-guarded) and consumed in the re-acquired commit
// half. A source with no live selection keeps the persisted model.
// Reconstruction or persistence of a live selection fails the fork before
// publication rather than silently substituting a different model.
func (a *Agent) inheritActiveModelForForkedSession(candidate *session, srcRef coremodel.ModelRef) error {
	if srcRef.Provider == "" || srcRef.Model == "" {
		a.restoreModelFromSessionForSession(candidate)
		return nil
	}
	client, model, err := newProviderClient(a.catalog, srcRef)
	if err != nil {
		return fmt.Errorf("fork model %s/%s: %w", srcRef.Provider, srcRef.Model, err)
	}
	a.setActiveModelForSessionLocked(candidate, srcRef, client, model)
	if err := candidate.store.SetModel(srcRef.Provider, srcRef.Model); err != nil {
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

// fireDurableReadHook invokes the durableReadHook test seam when installed.
// The seam is nil in production, so every observation point guards through
// this helper.
func (a *Agent) fireDurableReadHook() {
	if a.durableReadHook != nil {
		a.durableReadHook()
	}
}

// fireShutdownBarrierHook invokes the shutdownBarrierHook test seam when
// installed. The seam is nil in production, so the call site guards through
// this helper.
func (a *Agent) fireShutdownBarrierHook() {
	if a.shutdownBarrierHook != nil {
		a.shutdownBarrierHook()
	}
}

func (a *Agent) loadTokensFromDiskForSession(unit *session) {
	if unit == nil {
		return
	}
	// Read the tokens file before taking the tokens mutex: the read is durable
	// and fallible and must not run while the mutex is held. The parsed entries
	// replace unit.tokens atomically under the mutex.
	var entries []TokenEntry
	valid := false
	if unit.store != nil && unit.store.Active() {
		a.fireDurableReadHook()
		data, err := os.ReadFile(filepath.Join(unit.store.Dir(), tokensFileName))
		if err == nil {
			valid = json.Unmarshal(data, &entries) == nil
		}
	}
	unit.tokensMu.Lock()
	defer unit.tokensMu.Unlock()
	unit.tokens = map[string]*TokenEntry{}
	if !valid {
		return
	}
	for i := range entries {
		e := entries[i]
		e.Known = true
		unit.tokens[e.Provider+"/"+e.Model] = &e
	}
}

func (a *Agent) ensureSession() error {
	a.refreshCurrentSessionProjectLocked()
	if err := a.registerLiveSessionLocked(a.session); err != nil {
		return err
	}
	if a.currentSessionID == "" {
		a.currentSessionID = a.store.SessionID()
	}
	return nil
}

// --- Public methods (the service API) ---

func (a *Agent) SubmitToSession(ctx context.Context, sessionID string, content string) (SubmitResult, error) {
	return a.SubmitToSessionWithBoundary(ctx, sessionID, content, nil)
}

// SubmitToSessionWithBoundary is SubmitToSession that fires admitted under the
// runtime mutex at the point admission becomes certain — after the turn claim
// for an immediate start, after the queue-append decision for a queued submit,
// and before any event for that submit is emitted — so an adapter can commit
// routing state ordered ahead of the submit's own frames. A submit that fails
// never fires it. SubmitToSession passes nil.
func (a *Agent) SubmitToSessionWithBoundary(ctx context.Context, sessionID string, content string, admitted func()) (SubmitResult, error) {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	unit, err := a.liveSessionLocked(sessionID)
	rt.mu.Unlock()
	if err != nil {
		return SubmitResult{}, err
	}
	return rt.submit(ctx, unit, content, admitted)
}

func (rt *runtime) submit(ctx context.Context, unit *session, content string, admitted func()) (SubmitResult, error) {
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
		if admitted != nil {
			admitted()
		}
		version := unit.queueVersion
		rt.mu.Unlock()
		turn := rt.launchTurn(unit, turnCtx, cancel, []string{content})
		return SubmitResult{Started: true, Turn: turn, Queue: emptyQueue(), Version: version}, nil
	}
	// Busy or queue non-empty: enqueue and let the drainer pick it up. The
	// caller's context gates admission to the queue exactly as it gates the
	// immediate claim in claimTurnLocked: an already-cancelled context refuses
	// before the item is admitted, and once admitted the item's lifetime is
	// the owner's. A nil context is not a cancelled one: normalise it to a
	// live context ahead of the check, so it is admitted as it always was
	// instead of panicking or being refused.
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		rt.mu.Unlock()
		return SubmitResult{}, err
	}
	unit.queueSeq++
	unit.queue = append(unit.queue, QueuedItem{ID: fmt.Sprintf("q-%d", unit.queueSeq), Content: content})
	unit.queueVersion++
	items := copyQueue(unit.queue)
	version := unit.queueVersion
	sessionID := sessionIDOf(unit)
	projectID := unit.projectID
	if admitted != nil {
		admitted()
	}
	// Enqueue the queue-changed event inside the runtime.mu section that bumped the
	// version, so a navigation capture reads a queue snapshot consistent with the
	// events it delivers.
	a.emitEvent(Event{Kind: EventQueueChanged, SessionID: sessionID, ProjectID: projectID, Queue: items, QueueVersion: version})
	rt.mu.Unlock()
	rt.nudgeQueueDrainer()
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
// The caller's ctx gates admission only: an already-cancelled context refuses
// the claim. The accepted turn's context derives from the owner context, so a
// caller's cancellation after admission never severs the accepted turn — only
// owner shutdown (which cancels it through the per-session cancel) or an
// explicit per-session cancel can end it.
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
	turnCtx, cancel := context.WithCancel(rt.workCtx())
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
		a.emitEvent(Event{Kind: EventQueueChanged, SessionID: sessionID, ProjectID: projectID, Queue: items, QueueVersion: version})
	}
	rt.mu.Unlock()
	if !active {
		return
	}
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
// being swapped. Each claimed unit is preseeding and launching outside the
// runtime mutex: the preseed phase counts on turnWG so owner shutdown joins it,
// and the coordinator commits each preseeded turn before the next item or the
// launch, so a hydration never returns a preseed from both the durable half
// and the retained tail. The section that reacquires the runtime mutex before
// the final launch re-validates what changed while it was released: admission
// closed or the turn cancelled during the preseed aborts the drain without
// launching, requeueing the untouched remainder.
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
		// Claim the unit inside the still-locked section: busy marks it
		// "claimed but not yet launched" so no concurrent claim (Submit, the
		// signal scheduler) can re-select it mid-preseed. The turn's context
		// and cancel are installed here too — not just before the launch — so
		// the whole claimed-but-not-yet-launched window is cancellable: a
		// cancel or shutdown arriving during the preseed finds a live
		// turnCancel and actually cancels something, and the preseed loop
		// honours it instead of launching afterwards. The turnWG count covers
		// the preseed phase, which now runs unlocked and does durable store
		// writes shutdown cannot see. This registration is independent of the
		// pre-launch Add below: the closure releases it; launchTurn's
		// goroutine (or its abort path) releases the launch's.
		unit.busy = true
		unit.seenSessions = nil
		rt.turnWG.Add(1)
		turnCtx, cancel := context.WithCancel(ctx)
		unit.turnCancel = cancel
		unit.turnCtx = turnCtx
		items := unit.queue
		contents := make([]string, len(items))
		for i, it := range items {
			contents[i] = it.Content
		}
		unit.queue = nil
		unit.queueVersion++
		version := unit.queueVersion
		sessionID := sessionIDOf(unit)
		projectID := unit.projectID
		a.emitEvent(Event{Kind: EventQueueChanged, SessionID: sessionID, ProjectID: projectID, Queue: emptyQueue(), QueueVersion: version})
		rt.mu.Unlock()

		// The claimed-unit work runs in its own closure with its own defer so
		// each pass's Add is matched and this unit's busy is evaluated: one
		// call can drain session A then session B, and a function-level defer
		// would fire once over the last unit only. The closure reports whether
		// the whole drain must stop (owner cancellation or a failed preseed);
		// a bare return inside it would end only the current iteration.
		stop := func() bool {
			tr := a.transcriptForSessionID(sessionID)
			launched := 0
			// requeue puts the untouched remainder back on the queue,
			// prepended ahead of anything submitted meanwhile, and publishes
			// the real queue. It is the shared abort shape of the
			// marker-failure, shutdown, and cancellation paths: the remainder
			// keeps its original ids, and the queue-changed event carries the
			// actual queue rather than an empty snapshot.
			requeue := func(remainder []QueuedItem) {
				rt.mu.Lock()
				unit.queue = append(remainder, unit.queue...)
				unit.queueVersion++
				a.emitEvent(Event{Kind: EventQueueChanged, SessionID: sessionID, ProjectID: projectID, Queue: copyQueue(unit.queue), QueueVersion: unit.queueVersion})
				rt.mu.Unlock()
			}
			// The preseed-phase registration releases here, unconditionally:
			// every exit of this closure — cancellation, a failed marker
			// write, a launch abort, or a successful launch — leaves it
			// counted exactly once. When no turn was launched, the claim is
			// unwound completely: busy and the per-turn context are cleared,
			// matching the residue launchTurn's receiving-end rejection
			// leaves, so no later reader can tell the abort paths apart. A
			// launched turn clears its own state in its turn-end section; the
			// claim-time context is released here so a cancelled turn that
			// never launched cannot outlive the drain.
			defer func() {
				rt.mu.Lock()
				if launched == 0 {
					unit.busy = false
					unit.turnCancel = nil
					unit.turnCtx = nil
				}
				rt.mu.Unlock()
				if launched == 0 {
					cancel()
				}
				rt.turnWG.Done()
			}()
			// Preseed every item but the last as a user-only turn, unlocked:
			// BeginTurn, feed the turn start (so curTurn names the turn being
			// persisted), append the user message, complete it durably, flush
			// the loop event drainer, then commit via the turn end feed. The
			// commit runs only when the marker write succeeded and the flush
			// completed; feeding the end before the flush would commit an
			// empty tail and the queued display event would then duplicate
			// durable history. The feeds use feedTranscript, not
			// feedAndEmit: a preseed produces no running state, so no spurious
			// turn boundaries may flash through the adapters.
			for i := 0; i < len(contents)-1; i++ {
				// A cancellation arriving during the preseed stops the drain
				// without launching. The check precedes BeginTurn, so nothing
				// at or after i was reached and the remainder starts at i; a
				// cancel landing mid-item lets that item finish (it is already
				// durable) and aborts on the next iteration instead.
				if turnCtx.Err() != nil {
					requeue(items[i:])
					return true
				}
				turn := unit.store.BeginTurn()
				feedTranscript(tr, Event{Kind: EventTurnStart, SessionID: sessionID, ProjectID: projectID, Turn: turn})
				unit.lp.AppendUserMessage(turn, contents[i])
				if err := unit.store.MarkTurnComplete(turn); err != nil {
					// The marker write failed: the message is in loop history
					// and on screen but cannot be made durable, and no commit
					// for this turn will run. Surface the failure through the
					// transcript (sequenced, so a hydration or reconnect sees
					// it), stop the drain outright — no remaining preseed, no
					// final launch — and requeue the untouched remainder, and
					// only that, prepended ahead of anything submitted
					// meanwhile. The failed item is not requeued: re-appending
					// it would render it twice.
					a.feedAndEmit(tr, Event{Kind: EventError, SessionID: sessionID, ProjectID: projectID, Error: fmt.Sprintf("failed to persist queued message (turn %d): %v", turn, err), Turn: turn})
					requeue(items[i+1:])
					return true
				}
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
				if flushed {
					feedTranscript(tr, Event{Kind: EventTurnEnd, SessionID: sessionID, ProjectID: projectID, Turn: turn})
				}
			}
			// Reacquire the runtime mutex and re-validate what changed while it
			// was released: admission may have closed and the turn may have
			// been cancelled during the preseed. Either aborts the way the
			// marker-failure path aborts — requeue the untouched remainder
			// (the final item was never reached), do not launch — and the
			// per-iteration cleanup clears busy and releases the count.
			rt.mu.Lock()
			if rt.closed || turnCtx.Err() != nil {
				rt.mu.Unlock()
				requeue(items[len(items)-1:])
				return true
			}
			// The pre-launch registration stays here, immediately before
			// launchTurn, exactly as before: it belongs to the launched turn,
			// and launchTurn's goroutine — or its abort path, which returns 0
			// after releasing this count — is its only releaser.
			rt.turnWG.Add(1)
			rt.mu.Unlock()
			launched = rt.launchTurn(unit, turnCtx, cancel, []string{contents[len(contents)-1]})
			if launched == 0 {
				// The handoff was refused (a cancel or shutdown landed after
				// the revalidation above, or the unit cannot launch): the
				// final item was already removed from the queue but never
				// launched, and it is in no turn and no durable history.
				// Requeue it the same way the other abort paths requeue the
				// untouched remainder — prepended ahead of anything submitted
				// meanwhile, with the version bumped and the queue-changed
				// event carrying the real queue — and stop the drain.
				requeue(items[len(items)-1:])
				return true
			}
			return false
		}()
		if stop {
			return
		}
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

func (rt *runtime) launchTurn(unit *session, turnCtx context.Context, cancel context.CancelFunc, contents []string) int {
	a := rt.agent
	if unit == nil || unit.store == nil || unit.lp == nil {
		// The turn was claimed (and counted) but cannot launch; release the count.
		rt.turnWG.Done()
		return 0
	}
	// Reject a handoff whose turn context is already cancelled or whose
	// runtime has closed. A cancel or shutdown can land after the caller's
	// revalidation passed and before this call begins; accepting it would
	// create and emit a turn for a dead handoff. Reject before anything is
	// created or emitted, and unwind the claim the way the abort path above
	// does — release the wait-group count and clear the busy flag (and the
	// per-turn context) — so the unit is not left wedged.
	rt.mu.Lock()
	if rt.closed || turnCtx.Err() != nil {
		unit.busy = false
		unit.turnCancel = nil
		unit.turnCtx = nil
		rt.mu.Unlock()
		rt.turnWG.Done()
		return 0
	}
	rt.mu.Unlock()
	unit.syncEventOwner()
	sessionID := sessionIDOf(unit)
	projectID := unit.projectID
	turn := unit.store.BeginTurn()

	startEv := Event{Kind: EventTurnStart, SessionID: sessionID, ProjectID: projectID, Turn: turn}
	rt.mu.Lock()
	// Enqueue turn_start in a runtime.mu section — busy is already true under
	// runtime.mu from the turn claim — so a capture observes busy consistent with the
	// turn events it delivers.
	a.emitEvent(startEv)
	a.ensureActiveModelForSessionLocked(unit)
	a.applyUnitConfigLocked(unit)
	a.setWarningGroup("setup", a.setupWarningsLocked())
	rt.mu.Unlock()
	feedTranscript(a.transcriptForSessionID(sessionID), startEv)

	if unit.taskToolInst != nil {
		unit.taskToolInst.updateParentState(cancel)
	}

	go func() {
		defer func() {
			rt.mu.Lock()
			// Clear only this turn's claim. A later turn may have claimed the
			// unit between this turn's busy clear and this deferred running;
			// clearing busy then would drop the later turn's gate while its
			// loop is still running, letting a concurrent submit launch a
			// second Run on the same loop. The turn-end section above already
			// cleared busy and nilled the per-turn values in one runtime.mu
			// section, so this defer acts only when that section never ran
			// (owner-cancelled early returns) and the unit still holds this
			// turn's context.
			if unit.turnCtx == turnCtx {
				unit.busy = false
				unit.turnCancel = nil
				unit.turnCtx = nil
			}
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

		// The post-admission checks key off the owner's lifetime, never the
		// caller's: once a turn is admitted, only owner shutdown (or an
		// explicit per-session cancel, which cancels the turn's own context)
		// may end it. Pre-admission checks stay on the caller's context in
		// claimTurnLocked, which refuses to accept work on an already-cancelled
		// context.
		if rt.ownerShuttingDown() {
			return
		}
		a.refreshSystemPromptForSession(unit)

		if rt.ownerShuttingDown() {
			return
		}
		_, err := unit.lp.Run(turnCtx, contents...)

		// The flush and the commit funnel through the shared helper, which
		// takes no caller context: its wait gives up on the owner context, so
		// no call site can reintroduce a turn-cancellable or caller-cancellable
		// context that would race the flush and commit an understated cursor.
		// The commit therefore runs while the unit is still busy — before
		// turn_end is emitted and before a submit can observe busy cleared —
		// so a submit that claims the unit next feeds the next turn's rows
		// into a tail this commit already wiped. The contract requires the
		// flush and commit to complete after the turn's loop returns and
		// before EventTurnEnd.
		rt.flushAndCommitTranscript(sessionID, turn)

		if err != nil {
			errEv := Event{Kind: EventError, SessionID: sessionID, ProjectID: projectID, Error: a.turnErrorMessage(err), Turn: turn}
			a.feedAndEmit(a.transcriptForSessionID(sessionID), errEv)
		}
		endEv := Event{Kind: EventTurnEnd, SessionID: sessionID, ProjectID: projectID, Turn: turn, Cancelled: turnCtx.Err() != nil}
		// Clear busy and enqueue turn_end in one runtime.mu section: busy is already
		// false when turn_end is delivered, and no idle gap lets a concurrent submit
		// claim the unit and enqueue turn_start(N+1) before turn_end(N). The deferred
		// clear below is an idempotent fallback for owner-cancelled early-return paths
		// that never reach here.
		rt.mu.Lock()
		unit.busy = false
		unit.turnCancel = nil
		unit.turnCtx = nil
		a.emitEvent(endEv)
		rt.mu.Unlock()
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
	_, err := a.switchModelForSession(a.session, refStr)
	return err
}

func (a *Agent) SwitchModelForSession(sessionID string, refStr string) error {
	_, err := a.SwitchModelForSessionInfo(sessionID, refStr)
	return err
}

// SwitchModelForSessionInfo switches the model and returns the committed model
// info from the same mutation, so a caller appends it as a presentation item
// without a follow-up owner read.
func (a *Agent) SwitchModelForSessionInfo(sessionID string, refStr string) (ModelInfo, error) {
	unit, err := a.resolveLiveSession(sessionID)
	if err != nil {
		return ModelInfo{}, err
	}
	return a.switchModelForSession(unit, refStr)
}

func (a *Agent) switchModelForSession(unit *session, refStr string) (ModelInfo, error) {
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if unit == nil {
		return ModelInfo{}, snapshot.ErrNoSession
	}
	if err := unitMutableLocked(unit); err != nil {
		return ModelInfo{}, err
	}
	ref, err := coremodel.Parse(refStr)
	if err != nil {
		return ModelInfo{}, err
	}
	client, model, err := newProviderClient(a.catalog, ref)
	if err != nil {
		return ModelInfo{}, err
	}
	if err := a.writePrimaryModelLocked(ref); err != nil {
		return ModelInfo{}, err
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
	return a.modelInfo(ref), nil
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
	cfg, agentTypes, modelCatalog, catalogWarnings, err := a.loadReloadStateLocked(allowBackgroundDiscovery)
	if err != nil {
		return err
	}
	return a.applyReloadStateLocked(cfg, agentTypes, modelCatalog, catalogWarnings)
}

// loadReloadStateLocked loads the config, agents config, and model catalog
// without touching shared agent state. When allowBackgroundDiscovery is true,
// connected providers that are due for a refresh have their live discovery
// fetched (network) before the catalog is assembled; callers that must not run
// network I/O under the lock pass false. Caller holds runtime.mu.
func (a *Agent) loadReloadStateLocked(allowBackgroundDiscovery bool) (*config.Config, *agentcfg.Config, *catalog.Catalog, []catalog.Warning, error) {
	cfg, err := config.Load(a.configPath)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	agentTypes, err := agentcfg.Load(a.agentsPath)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("load agents config: %w", err)
	}
	modelCatalog, catalogWarnings, err := a.loadCatalogLocked(allowBackgroundDiscovery)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return cfg, agentTypes, modelCatalog, catalogWarnings, nil
}

// loadCatalogLocked loads and assembles the model catalog from the bundled
// catalog, the user config, and the discovery cache. Caller holds runtime.mu.
func (a *Agent) loadCatalogLocked(allowBackgroundDiscovery bool) (*catalog.Catalog, []catalog.Warning, error) {
	modelLoader := catalog.NewLoaderWithConfigPath(a.home, nil, a.configPath)
	modelLoader.AllowRefresh = func(_ string, prov *catalog.Provider) bool {
		if !allowBackgroundDiscovery {
			return false
		}
		return providerConnected(prov)
	}
	modelCatalog, catalogWarnings, err := modelLoader.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("load model catalog: %w", err)
	}
	return modelCatalog, catalogWarnings, nil
}

// applyReloadStateLocked publishes freshly loaded config, agents, and catalog
// state and refreshes the derived session state (active model, process limits,
// warning groups). Caller holds runtime.mu.
func (a *Agent) applyReloadStateLocked(cfg *config.Config, agentTypes *agentcfg.Config, modelCatalog *catalog.Catalog, catalogWarnings []catalog.Warning) error {
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
	return atomicfs.Write(path, data, 0o600)
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
// cleanup, and it runs exactly once. When every turn has actually finished, the
// live session stores are detached — releasing their claims — and the shared
// embedder is closed; an abandoned turn keeps both, so a still-running turn
// never loses its store or hits a closed embedder, and the process-exit
// boundary releases what shutdown did not.
//
// It also closes session-identity admission: closed is published under the
// lifecycle lock, so every open/resume/new/fork either completes before the
// flag is visible or refuses at its entry check, and no session can register
// after the live stores are snapshotted with a claim nothing will detach.
//
// It reports whether shutdown completed every join — both the turn join and
// the background join drained. The result is stored on the runtime rather than
// returned from inside the Once body, because only the caller that wins the
// Once executes that body; every other caller (e.g. the Init watcher that
// triggers the same shutdown) waits on the channel and must read the field
// after it, which the channel close makes visible without a lock.
func (a *Agent) ShutdownOwner() bool {
	rt := a.ensureRuntime()
	rt.shutdownOnce.Do(func() {
		// Shutdown is a barrier against session-identity operations: closed is
		// published under lifecycleMu, so an open/resume/new/fork already running
		// finishes completely before the flag is visible, and one starting
		// afterwards sees the flag at its entry check — before it acquires any
		// claim or registers anything. The lifecycle lock is released before the
		// turn join, so in-flight turn teardown never waits on it.
		a.fireShutdownBarrierHook()
		releaseLifecycle := a.lockLifecycle()
		rt.mu.Lock()
		rt.closed = true
		var cancels []context.CancelFunc
		var sessionIDs []string
		var stores []*snapshot.Store
		for id, unit := range a.sessions {
			if unit == nil || unit.store == nil || !unit.store.Active() {
				continue
			}
			if cancel := rt.turnCancelSnapshotLocked(unit); cancel != nil {
				cancels = append(cancels, cancel)
			}
			sessionIDs = append(sessionIDs, id)
			stores = append(stores, unit.store)
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
		releaseLifecycle()
		// Join in-flight turns and mutations first, while the drainer is still
		// alive, so their terminal events are delivered before delivery stops.
		turnsDrained := waitGroupOrTimeout(&rt.turnWG, shutdownJoinTimeout)
		if !turnsDrained {
			fmt.Fprintf(os.Stderr, "lightcode: owner shutdown abandoned in-flight turns after %s\n", shutdownJoinTimeout)
		}
		// Detach every live session store only once every turn has actually
		// finished. The join is bounded and may return while a turn still runs;
		// detaching then would release that session's claim under the live turn,
		// letting another process drive the same saved session — the condition
		// the active-process marker exists to prevent. The embedder close below
		// carries the same gate for the same reason. The gate is all-or-nothing
		// across every live session; there is deliberately no per-session
		// tracking.
		if turnsDrained {
			for _, store := range stores {
				store.Detach()
			}
		}
		if rt.ownerCancel != nil {
			rt.ownerCancel()
		}
		bgDrained := waitGroupOrTimeout(&rt.bgWG, shutdownJoinTimeout)
		if !bgDrained {
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
		// Store the outcome before the close: only the caller that wins the Once
		// executes this body, so a value returned from inside the Do would reach
		// every other caller as the zero value. The close supplies the
		// happens-before edge, so the field needs no lock.
		rt.shutdownClean = turnsDrained && bgDrained
		close(rt.shutdownDone)
	})
	<-rt.shutdownDone
	return rt.shutdownClean
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

// SessionSummaryForSessionOrPersisted resolves a session's summary whether the
// session is live in this owner or only persisted on disk: a live id takes the
// SessionSummaryForSession path, and any other id is resolved against the
// persisted sessions of every project. The resolved summary reports the id
// that resolved and the project the session actually lives in, never the
// metadata's unvalidated project path.
func (a *Agent) SessionSummaryForSessionOrPersisted(sessionID string) (SessionSummary, error) {
	sessionID = strings.TrimSpace(sessionID)
	unit, err := a.resolveLiveSession(sessionID)
	if err == nil {
		return sessionSummary(unit), nil
	}
	proj, err := a.projectForExistingSession(sessionID)
	if err != nil {
		return SessionSummary{}, err
	}
	meta, err := snapshot.LoadSessionMeta(a.projects.SessionsRoot(proj.ID), sessionID)
	if err != nil {
		return SessionSummary{}, err
	}
	return persistedSessionSummary(sessionID, meta, proj), nil
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
		// The metadata read failure stays suppressed to the bare id here, but the
		// unit is already resolved against its project; carry that project so a
		// selection boundary and an open-session result keep routing the adapter
		// to the destination instead of reporting no project at all.
		return SessionSummary{ID: unit.store.SessionID(), ProjectPath: unit.projectRoot}
	}
	return sessionSummaryFromUnit(unit, meta)
}

// sessionSummaryFromUnit builds a session summary from an already-read meta
// record, taking the identity and project from the resolved unit
// unconditionally: the unit is resolved from the session's actual directory,
// while the persisted metadata's id and project path are unvalidated and can
// be nonempty and stale. Every boundary builder and the open-session result
// build through it, so a selection always routes to the id the session
// actually lives under and the project it lives in, for empty, stale and
// correct metadata alike.
func sessionSummaryFromUnit(unit *session, meta snapshot.SessionMeta) SessionSummary {
	out := sessionSummaryFromMeta(meta)
	out.ProjectPath = unit.projectRoot
	out.ID = unit.store.SessionID()
	return out
}

// sessionSummaryFromMeta builds a session summary from an already-read meta record, so
// a caller that must not re-read meta after a durable commit can reuse the meta it
// prepared before the commit.
func sessionSummaryFromMeta(meta snapshot.SessionMeta) SessionSummary {
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

// persistedSessionSummary builds a session summary from a session's persisted
// metadata and the project record it was resolved from, reporting the id that
// resolved and the project the session actually lives in. The metadata's own
// id and project path are unvalidated and can be stale, so neither is used —
// exactly the two fields the live path deliberately overrides.
func persistedSessionSummary(id string, meta snapshot.SessionMeta, proj *project.Project) SessionSummary {
	return SessionSummary{
		ID:              id,
		CreatedAt:       meta.CreatedAt,
		LastActivity:    meta.LastActivity,
		State:           metaState(meta.State),
		ArchivedAt:      meta.ArchivedAt,
		ProjectPath:     proj.Path,
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
	return a.openSession(id, nil)
}

// OpenSessionWithBoundary is OpenSession that publishes the destination's complete
// state through emit in the same section that commits its selection, so no fallible
// capture runs after the durable commit. Reactivation prebuilds the compacted
// committed history before the durable reactivation and emits the replacement
// in-commit; live selection emits under the three-attempt revision revalidation
// before any routing change. emit is not called when the session cannot be resolved,
// leaving the adapter's current presentation unchanged.
func (a *Agent) OpenSessionWithBoundary(id string, emit func(HydrationState)) (SessionSummary, error) {
	return a.openSession(id, emit)
}

func (a *Agent) openSession(id string, emit func(HydrationState)) (SessionSummary, error) {
	defer a.lockLifecycle()()
	id = strings.TrimSpace(id)
	if id == "" {
		return SessionSummary{}, fmt.Errorf("session id is required")
	}
	rt := a.ensureRuntime()
	rt.mu.Lock()
	// Admission check at entry, before any disk work or claim acquisition: an
	// open starting after shutdown published closed must not resolve a project,
	// build a store, or take the session's claim.
	if rt.closed {
		rt.mu.Unlock()
		return SessionSummary{}, errOwnerClosed
	}
	if unit, err := a.liveSessionLocked(id); err == nil {
		if isCompactSessionType(unit.activeAgentType) {
			rt.mu.Unlock()
			return SessionSummary{}, internalTranscriptSessionError(id)
		}
		summary := sessionSummary(unit)
		rt.mu.Unlock()
		// Live selection performs no durable mutation: capture with the three-attempt
		// revision revalidation before returning, so a concurrent commit forces a retry
		// rather than a stale prefix and the boundary is atomic with the live state.
		if emit != nil {
			if _, err := a.captureStateForSelection(unit, func(cs completeState) {
				emit(hydrationStateFrom(summary, cs))
			}); err != nil {
				return SessionSummary{}, err
			}
		}
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
		if emit != nil {
			a.emitBoundaryForLiveUnitLocked(unit, id, emit)
		}
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
	// Prebuild the compacted committed history before the durable reactivation, so the
	// in-commit boundary carries a prebuilt replacement with no postcommit read. The
	// durable turns are unchanged by the state flip, so this equals the post-commit
	// projection. A read failure aborts before the reactivation, leaving the source
	// archived and unregistered.
	reactivate := metaState(meta.State) == snapshot.StateArchived
	var prebuilt []DisplayMessage
	var prebuiltMaxTurn int
	var prebuiltSummary SessionSummary
	if emit != nil {
		prebuilt, prebuiltMaxTurn, err = a.captureDurableHistory(unit, id)
		if err != nil {
			unit.store.Detach()
			return SessionSummary{}, err
		}
		// Prebuild the summary from the meta already read; the reactivation below flips
		// an archived session active, so reflect that here rather than re-reading meta
		// after the durable commit. The unit's resolved project is authoritative, so
		// the boundary agrees with the live-selection path by construction.
		prebuiltSummary = sessionSummaryFromUnit(unit, meta)
		if reactivate {
			prebuiltSummary.State = snapshot.StateActive
			prebuiltSummary.ArchivedAt = 0
		}
	}
	if reactivate {
		if err := unit.store.SetState(snapshot.StateActive); err != nil {
			// Reactivation must reach disk; otherwise the owner would drive a
			// session that stays archived and absent from active listings.
			unit.store.Detach()
			return SessionSummary{}, fmt.Errorf("reactivate session %s: %w", id, err)
		}
		// Stamp the session's LastActivity with one sampled timestamp and report it in
		// the boundary only when the session-meta write itself landed; the project-level
		// touch is best-effort and does not affect the reported value, so a boundary
		// summary never disagrees with the session's committed LastActivity.
		ts := time.Now().Unix()
		if unit.store.SetLastActivity(ts) == nil {
			prebuiltSummary.LastActivity = ts
		}
		_ = unit.store.TouchProjectActivity()
	}
	a.resetFileTrackerForSession(unit)
	a.loadTokensFromDiskForSession(unit)
	a.restoreModelFromSessionForSession(unit)
	if err := a.registerLiveSessionLocked(unit); err != nil {
		unit.store.Detach()
		return SessionSummary{}, err
	}
	// Infallible in-commit publication: the committed prefix and summary are prebuilt,
	// so this only captures the live classes and appends the boundary under their locks.
	if emit != nil {
		a.captureUnderLocksRTHeld(unit, prebuilt, id, prebuiltMaxTurn, nil, func(cs completeState) {
			emit(hydrationStateFrom(prebuiltSummary, cs))
		})
	}
	return sessionSummary(unit), nil
}

// emitBoundaryForLiveUnitLocked publishes an already-live unit's complete-state
// boundary while runtime.mu is held (the open-session re-check race where the session
// became live between the two lookups). The committed prefix is read here — this path
// performed no durable commit, so the read is not a postcommit capture.
func (a *Agent) emitBoundaryForLiveUnitLocked(unit *session, id string, emit func(HydrationState)) {
	committed, maxDurableTurn, err := a.captureDurableHistory(unit, id)
	if err != nil {
		return
	}
	summary := sessionSummary(unit)
	a.captureUnderLocksRTHeld(unit, committed, id, maxDurableTurn, nil, func(cs completeState) {
		emit(hydrationStateFrom(summary, cs))
	})
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
	var found *project.Project
	for i := range projects {
		proj := projects[i]
		if _, err := os.Stat(filepath.Join(a.projects.SessionsRoot(proj.ID), id, "meta.json")); err == nil {
			if found != nil {
				return nil, fmt.Errorf("session %q is ambiguous: it exists in more than one project", id)
			}
			p := proj
			found = &p
		} else if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	if found == nil {
		return nil, fmt.Errorf("unknown session %q", id)
	}
	return found, nil
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
	return a.newSession(projectID, agentType, nil)
}

// NewSessionWithBoundary is NewSession that publishes the fresh session's complete
// state through emit in the same section that commits it. A new session has empty
// durable history and a deterministic summary, so the capture appends the boundary
// under the live-state locks with no fallible read.
func (a *Agent) NewSessionWithBoundary(projectID string, agentType string, emit func(HydrationState)) (string, error) {
	return a.newSession(projectID, agentType, emit)
}

func (a *Agent) newSession(projectID string, agentType string, emit func(HydrationState)) (string, error) {
	defer a.lockLifecycle()()
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	// Admission check at entry, before any staged preparation or publication: a
	// new session starting after shutdown published closed must publish nothing
	// and acquire no claim.
	if rt.closed {
		return "", errOwnerClosed
	}
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
	// Prepare the candidate under an unlisted staging directory and run every
	// fallible step against the not-yet-published session before the durable
	// commit, mirroring the fork's staged preparation. From here until publish the
	// receiver (store) is mid-transaction: active, bound to the final location,
	// and holding the session's claim, while the returned prepared store carries
	// no claim of its own and addresses the candidate under staging. The single
	// durable commit is the atomic rename in PublishPreparedSession; a failure
	// at any earlier step aborts the creation with nothing published.
	prepared, err := store.PrepareStagedNewSession(proj.Path)
	if err != nil {
		return "", err
	}
	a.ensureActiveModelForSessionLocked(unit)
	if unit.currentRef.Provider != "" && unit.currentRef.Model != "" {
		if err := prepared.SetModel(unit.currentRef.Provider, unit.currentRef.Model); err != nil {
			stagingRoot := prepared.Root()
			prepared.Detach()
			store.Detach()
			return "", cleanupStaging(stagingRoot, fmt.Errorf("persist model: %w", err))
		}
	}
	unit.lp.ResetHistory()
	if unit.fileTracker != nil {
		unit.fileTracker.Reset()
	}
	a.loadTokensFromDiskForSession(unit)
	// Prepare the complete summary from the staged meta before the durable
	// commit, so the boundary carries the durable CreatedAt/LastActivity and no
	// fallible read runs after that commit.
	var prebuiltSummary SessionSummary
	if emit != nil {
		meta, merr := prepared.Meta()
		if merr != nil {
			stagingRoot := prepared.Root()
			prepared.Detach()
			store.Detach()
			return "", cleanupStaging(stagingRoot, fmt.Errorf("read session meta: %w", merr))
		}
		prebuiltSummary = sessionSummaryFromUnit(unit, meta)
	}
	// Durable commit: the atomic rename publishes the candidate into the
	// sessions root. The receiver keeps the claim on the published session, and
	// the unit keeps the receiver as its store, so the claim is released only
	// when the session closes. Post-commit publication is infallible.
	if err := store.PublishPreparedSession(prepared); err != nil {
		return "", err
	}
	if err := a.setCurrentSessionLocked(unit); err != nil {
		unit.store.Detach()
		return "", err
	}
	sid := unit.store.SessionID()
	if emit != nil {
		// New sessions have empty durable history, so the in-commit capture appends the
		// boundary from the prebuilt summary with no further read.
		a.captureUnderLocksRTHeld(unit, nil, sid, 0, nil, func(cs completeState) {
			emit(hydrationStateFrom(prebuiltSummary, cs))
		})
	}
	return sid, nil
}

func (a *Agent) NewSessionForProjectPath(projectPath string, agentType string) (string, error) {
	return a.newSessionForProjectPath(projectPath, agentType, nil)
}

// NewSessionForProjectPathWithBoundary is NewSessionForProjectPath that publishes the
// fresh session's boundary in-commit.
func (a *Agent) NewSessionForProjectPathWithBoundary(projectPath string, agentType string, emit func(HydrationState)) (string, error) {
	return a.newSessionForProjectPath(projectPath, agentType, emit)
}

func (a *Agent) newSessionForProjectPath(projectPath string, agentType string, emit func(HydrationState)) (string, error) {
	proj, err := a.ensureProjectForPath(projectPath)
	if err != nil {
		return "", err
	}
	return a.newSession(proj.ID, agentType, emit)
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

// SessionContendedError builds the user-facing message for a session another
// process is driving, with no package prefix. The snapshot sentinel stays the
// store's internal cause, wrapped by acquireClaimLocked and recognised by
// callers with errors.Is.
func SessionContendedError(id string) error {
	return fmt.Errorf("session %q is being driven by another process", id)
}

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
		return nil, SessionContendedError(id)
	}
	return func() { _ = claim.Release() }, nil
}

// SessionArchive archives a session.
func (a *Agent) SessionArchive(id string) error {
	return a.removeSession(id, func(sessionsRoot, id string) error {
		return snapshot.ArchiveSession(sessionsRoot, id)
	})
}

// SessionDelete removes a session from disk.
func (a *Agent) SessionDelete(id string) error {
	return a.removeSession(id, func(sessionsRoot, id string) error {
		if err := snapshot.DeleteSession(sessionsRoot, id); err != nil {
			return err
		}
		if a.memoryHooks != nil {
			_ = a.memoryHooks.DeleteSessionSummaries(id)
		}
		return nil
	})
}

// removeSession is the single removal transaction behind SessionArchive and
// SessionDelete, parameterised by the durable mutation. Ordering is reserve,
// commit, release: the target is quiesced and reserved (transitioning) while
// its claim is held, the durable mutation runs under that claim, and only a
// successful commit publishes the teardown (detach, evict from the live map,
// clear the selection and the queue, reset current-session state). The durable
// commit is the point of no return — every step before it is reversible, so a
// failure there leaves the unit live, claimed, selected and with its queue
// intact, and the reservation release on the way out clears transitioning and
// re-nudges the drainers.
func (a *Agent) removeSession(id string, durable func(sessionsRoot string, id string) error) error {
	defer a.lockLifecycle()()
	id = strings.TrimSpace(id)
	sessionsRoot, err := a.sessionsRootForUserManagedSession(id)
	if err != nil {
		return err
	}

	// Reserve before the durable mutation. The current session quiesces its
	// running turn while staying attached, so its claim is held across the
	// commit; a live non-current unit gets the same transitioning reservation
	// while its claim stays held. Neither releases the claim nor touches the
	// queue here, so a failed commit leaves everything intact.
	isCurrent := a.store.Active() && a.store.SessionID() == id
	var releaseReservation func()
	if isCurrent {
		a.ensureRuntime().beginTransition()
		if err := a.cancelAndWaitIdle(); err != nil {
			a.endLiveTransition(a.session)
			return err
		}
		releaseReservation = func() { a.endLiveTransition(a.session) }
	} else {
		releaseReservation, err = a.beginLiveSessionClose(id)
		if err != nil {
			return err
		}
	}
	if releaseReservation != nil {
		defer func() {
			if releaseReservation != nil {
				releaseReservation()
			}
		}()
	}
	// A target that is not live in this owner holds no claim of its own; take
	// a temporary claim so the durable mutation still owns the session.
	if releaseReservation == nil {
		claimRelease, cerr := a.claimPersistedOnlySession(sessionsRoot, id)
		if cerr != nil {
			return cerr
		}
		defer claimRelease()
	}

	// Point of no return: commit durably with the claim held.
	if err := durable(sessionsRoot, id); err != nil {
		return err
	}
	if a.gate != nil {
		a.gate.CancelSession(id)
	}
	if isCurrent {
		// Publish the removal: detach (not Close, so an empty session is
		// preserved for archive), evict from the live map, and clear the
		// selection and the queue.
		rt := a.ensureRuntime()
		rt.mu.Lock()
		a.store.Detach()
		if a.currentSessionID == id {
			a.currentSessionID = ""
		}
		delete(a.sessions, id)
		rt.unregisterTranscript(id)
		rt.clearQueueLocked()
		rt.mu.Unlock()
		a.resetCurrentSessionState()
	} else if _, err := a.closeLiveSession(id); err != nil {
		return err
	}
	releaseReservation = nil
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
		var found string
		for _, proj := range projects {
			root := a.projects.SessionsRoot(proj.ID)
			_, err := os.Stat(filepath.Join(root, id, "meta.json"))
			if err == nil {
				if found != "" {
					return "", fmt.Errorf("session %q is ambiguous: it exists in more than one project", id)
				}
				found = root
			}
			if err != nil && !os.IsNotExist(err) {
				return "", err
			}
		}
		if found != "" {
			return found, nil
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
		rt.unregisterTranscript(id)
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
	rt.unregisterTranscript(id)
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
// captured live classes read as one consistent set.
type completeState struct {
	transcript  completeTranscript
	tokens      TokenReport
	model       ModelInfo
	busy        bool
	compacting  bool
	queue       QueueState
	warnings    []PromptWarning
	permissions []permission.Request
}

// errCaptureRevisionChanged is returned by the live-selection capture when the
// coordinator revision changes across all attempts, so the caller retries the
// whole selection without any partial publication.
var errCaptureRevisionChanged = errors.New("capture revision changed")

// captureStateForSelection is the live-selection/hydration capture shape. It reads the
// durable history outside the locks, then revalidates the coordinator revision
// under the locks: a turn committed during the read leaves the read-again
// prefix, so it retries on the first two mismatches and returns
// errCaptureRevisionChanged on the third, never publishing a partial result. A
// durable read/decode error returns immediately. The caller holds lifecycleMu.
func (a *Agent) captureStateForSelection(unit *session, boundary func(completeState)) (completeState, error) {
	if unit == nil || unit.store == nil {
		return completeState{}, snapshot.ErrNoSession
	}
	sessionID := sessionIDOf(unit)
	tr := a.transcriptForSessionID(sessionID)
	if tr == nil {
		return completeState{}, snapshot.ErrNoSession
	}
	for attempt := 1; attempt <= 3; attempt++ {
		tr.seqMu.Lock()
		rev0 := tr.revisionLocked()
		tr.seqMu.Unlock()

		committed, maxDurableTurn, err := a.captureDurableHistory(unit, sessionID)
		if err != nil {
			return completeState{}, err
		}
		// The probe fires in the window the retry exists for: after the durable
		// read, before the locked revalidation. A test injects the revision
		// change or read error here, where a real producer would land it.
		if a.captureProbe != nil {
			if err := a.captureProbe(attempt); err != nil {
				return completeState{}, err
			}
		}
		if state, ok := a.captureUnderLocks(unit, committed, sessionID, maxDurableTurn, &rev0, boundary); ok {
			return state, nil
		}
	}
	return completeState{}, errCaptureRevisionChanged
}

// captureDurableHistory reads a session's committed display history and the
// highest durable turn in it. It does I/O and must run outside the captured-state
// locks. The turn maximum is what a later locked capture compares the live
// coordinator's current turn against, so it is reported alongside the read,
// never recomputed from the rendered rows.
func (a *Agent) captureDurableHistory(unit *session, sessionID string) ([]DisplayMessage, int, error) {
	if !unit.store.Active() {
		return nil, 0, nil
	}
	a.fireDurableReadHook()
	msgs, maxTurn, err := a.messagesForFrontendForStoreAndMaxTurn(unit.store, sessionID)
	return msgs, maxTurn, err
}

// captureUnderLocks reads every live class while holding the captured-state locks
// in the total order and builds the immutable state. When wantRev is non-nil it
// revalidates the transcript revision under seqMu, returning ok=false if it
// changed so the caller can retry with a fresh durable read. When the state is
// built and boundary is non-nil, boundary is invoked while runtime.mu, tokensMu,
// gateMu, warningsMu, and seqMu are still held, so an adapter's boundary append
// orders with those classes' events. The pending-permission snapshot is read and
// held under the gate lock through the boundary, so a request registering during
// the capture is either in the snapshot or delivered after the boundary.
// maxDurableTurn is the highest turn the committed prefix was read from, so the
// locked section can drop a retained tail the durable half already covers.
func (a *Agent) captureUnderLocks(unit *session, committed []DisplayMessage, sessionID string, maxDurableTurn int, wantRev *captureRevision, boundary func(completeState)) (completeState, bool) {
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return a.captureUnderLocksRTHeld(unit, committed, sessionID, maxDurableTurn, wantRev, boundary)
}

// captureUnderLocksRTHeld is captureUnderLocks for a caller that already holds
// runtime.mu (a reactivation or current removal publishing its boundary in-commit): it
// captures the remaining live classes and appends the boundary under their locks,
// so the boundary stays atomic with the operation's committed state. The committed
// prefix is prebuilt by the caller before its durable commit, so this performs no
// fallible read.
func (a *Agent) captureUnderLocksRTHeld(unit *session, committed []DisplayMessage, sessionID string, maxDurableTurn int, wantRev *captureRevision, boundary func(completeState)) (completeState, bool) {
	rt := a.ensureRuntime()
	tr := a.transcriptForSessionID(sessionID)
	if tr == nil {
		return completeState{}, false
	}

	busy := unit.busy
	compacting := unit.compacting
	// The resolved model shape: identifier plus catalog display name, built with
	// the same helper CurrentModel uses so the captured state and the live model
	// event speak one shape. It degrades to the bare ref when the catalog has no
	// entry (or the session has no model), so this needs no error path.
	model := a.modelInfo(unit.currentRef)
	queue := rt.queueSnapshotLocked(unit)
	var permissions []permission.Request
	if a.gate != nil {
		a.gate.Lock()
		defer a.gate.Unlock()
		permissions = a.gate.PendingForSessionLocked(sessionID)
	}

	unit.tokensMu.Lock()
	defer unit.tokensMu.Unlock()
	tokens := a.buildReportForSessionLocked(unit)

	a.warningsMu.Lock()
	defer a.warningsMu.Unlock()
	warnings := append([]PromptWarning(nil), a.warningSnapshot...)

	tr.seqMu.Lock()
	defer tr.seqMu.Unlock()
	ct, ok := captureTranscriptLocked(tr, committed, maxDurableTurn, wantRev)
	if !ok {
		return completeState{}, false
	}
	state := completeState{
		transcript:  ct,
		tokens:      tokens,
		model:       model,
		busy:        busy,
		compacting:  compacting,
		queue:       queue,
		warnings:    warnings,
		permissions: permissions,
	}
	if boundary != nil {
		boundary(state)
	}
	return state, true
}

// captureTranscriptLocked reads the transcript portion of a complete capture
// under seqMu: the committed prefix, the retained tail, retained errors, and
// the revision, applying the disjoint-halves guard. When wantRev is non-nil the
// revision is revalidated and ok=false reports a change so the caller can retry
// with a fresh durable read. It is the single implementation of the locked
// transcript read, shared by the unit capture and the child capture, so the two
// cannot diverge. The caller holds seqMu.
func captureTranscriptLocked(tr *transcript, committed []DisplayMessage, maxDurableTurn int, wantRev *captureRevision) (completeTranscript, bool) {
	if wantRev != nil && tr.revisionLocked() != *wantRev {
		return completeTranscript{}, false
	}
	rev := tr.revisionLocked()
	tail := tr.tailSnapshotLocked()
	// Disjoint-halves guard: when the current turn is already durable while its
	// commit has not run (the completion marker lands on disk before the flush
	// and commit), the retained tail duplicates the durable half, so drop the
	// tail whole — the tail never mixes turns, so one comparison over it
	// suffices — and raise the captured cursor over the dropped sequences so a
	// frame buffered for post-snapshot replay gates as already present. The
	// lower bound keeps a row appended after the commit (a background
	// completion between turns, which carries the same curTurn but is not
	// durable) from being dropped with it. Only the captured cursor is raised;
	// the coordinator's own commit stays with commitLocked.
	if rev.committedTurn < tr.curTurn && tr.curTurn <= maxDurableTurn && len(tail) > 0 {
		highest := 0
		for _, r := range tail {
			if r.seq > highest {
				highest = r.seq
			}
		}
		if highest > rev.committedSeq {
			rev.committedSeq = highest
		}
		tail = nil
	}
	return completeTranscript{
		committed: committed,
		tail:      tail,
		errors:    tr.errorSnapshotLocked(),
		revision:  rev,
	}, true
}

// messagesForFrontendForStore renders a session's durable display history. It
// is the wrapper callers use when they only need the messages.
func (a *Agent) messagesForFrontendForStore(store *snapshot.Store, sessionID string) ([]DisplayMessage, error) {
	msgs, _, err := a.messagesForFrontendForStoreAndMaxTurn(store, sessionID)
	return msgs, err
}

// messagesForFrontendForStoreAndMaxTurn renders a session's durable display
// history and the highest durable turn number in the raw turn records. The
// maximum is taken from the raw records, where every element carries its turn,
// not from the rendered rows: tool, system, and background rows carry no turn,
// and a staged-flush wrapper produces no row at all, so a turn made only of
// such rows would render nothing carrying a turn.
func (a *Agent) messagesForFrontendForStoreAndMaxTurn(store *snapshot.Store, sessionID string) ([]DisplayMessage, int, error) {
	if store == nil {
		return nil, 0, snapshot.ErrNoSession
	}
	var rec *snapshot.CompactionRecord
	var err error
	if sessionID == "" {
		rec, err = store.LoadCompaction()
	} else {
		rec, err = store.LoadCompactionForSession(sessionID)
	}
	if err != nil {
		return nil, 0, err
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
		return nil, 0, err
	}
	maxTurn := 0
	for _, t := range raw {
		if t.Turn > maxTurn {
			maxTurn = t.Turn
		}
	}
	return a.renderCompleteTurns(raw), maxTurn, nil
}

// renderCompleteTurns renders durable turn messages into display rows. It is the
// shared body of full-history hydration and the compaction rewrite's prebuilt
// committed prefix, so both render identically.
func (a *Agent) renderCompleteTurns(raw []snapshot.TurnMessages) []DisplayMessage {
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
	return out
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

func (a *Agent) ApplyTurnActionForSession(sessionID string, turn int, action string, alsoRevertCode bool) (TurnActionResult, error) {
	return a.applyTurnActionResolved(sessionID, turn, action, alsoRevertCode, nil)
}

// ApplyTurnActionForSessionWithBoundary is ApplyTurnActionForSession that publishes a
// session-changing revert/fork result as an in-commit boundary through emit — from the
// operation's own prebuilt result under the mutating lock — so the adapter performs no
// separate postcommit capture of the mutated session. Code-only revert changes no
// session and never calls emit.
func (a *Agent) ApplyTurnActionForSessionWithBoundary(sessionID string, turn int, action string, alsoRevertCode bool, emit func(HydrationState, []snapshot.SkippedRevert)) (TurnActionResult, error) {
	return a.applyTurnActionResolved(sessionID, turn, action, alsoRevertCode, emit)
}

func (a *Agent) applyTurnActionResolved(sessionID string, turn int, action string, alsoRevertCode bool, emit func(HydrationState, []snapshot.SkippedRevert)) (TurnActionResult, error) {
	defer a.lockLifecycle()()
	unit, err := a.resolveRootDriveSession(sessionID)
	if err != nil {
		return TurnActionResult{}, err
	}
	return a.applyTurnActionForSession(unit, turn, action, alsoRevertCode, emit)
}

// emitTurnActionBoundaryLocked publishes a revert/fork result as an in-commit boundary
// while runtime.mu is held: the committed prefix is the result's prebuilt Messages, so
// this captures only the live classes and appends the boundary under their locks. The
// code-revert skips ride the boundary so the adapter reassembles its combined frame.
// maxDurableTurn is the highest durable turn the result's Messages were read from, so
// the locked capture can drop a retained tail the durable half already covers.
func (a *Agent) emitTurnActionBoundaryLocked(unit *session, result TurnActionResult, maxDurableTurn int, emit func(HydrationState, []snapshot.SkippedRevert)) {
	if emit == nil || unit == nil {
		return
	}
	a.captureUnderLocksRTHeld(unit, result.Messages, sessionIDOf(unit), maxDurableTurn, nil, func(cs completeState) {
		emit(hydrationStateFrom(result.Session, cs), result.SkippedFiles)
	})
}

// reserveTurnActionUnit reserves a live unit across a durable revert mutation
// with the same transitioning reservation pair the session-removal path uses:
// the current unit gets the flag directly, a live non-current unit gets it
// through beginLiveSessionClose, and endLiveTransition is the release for both.
// Unlike the removal path it refuses a busy unit instead of cancelling its
// turn. The release must run after the caller's runtime.mu section ends.
func (a *Agent) reserveTurnActionUnit(unit *session) (func(), error) {
	if unit == nil || unit.store == nil || !unit.store.Active() {
		return nil, snapshot.ErrNoSession
	}
	if unit == a.session {
		rt := a.ensureRuntime()
		rt.mu.Lock()
		defer rt.mu.Unlock()
		if err := unitMutableLocked(unit); err != nil {
			return nil, err
		}
		unit.transitioning = true
		return func() { a.endLiveTransition(unit) }, nil
	}
	release, err := a.beginLiveSessionClose(sessionIDOf(unit))
	if err != nil {
		return nil, err
	}
	if release == nil {
		return nil, fmt.Errorf("session %q is not a live non-current session", sessionIDOf(unit))
	}
	return release, nil
}

func (a *Agent) applyTurnActionForSession(unit *session, turn int, action string, alsoRevertCode bool, emit func(HydrationState, []snapshot.SkippedRevert)) (TurnActionResult, error) {
	// The revert cases reserve the unit across their durable mutation with the
	// same reservation pair the removal path uses: the unit must not be
	// driveable while its loop state and its durable history disagree (history
	// truncation) or while its files are mid-restore. The release
	// (endLiveTransition) is registered before the runtime.mu defer, so it runs
	// after the lock is dropped and re-arms the drainers. The fork case is not
	// reserved: it publishes a new session and changes no durable state of the
	// source unit.
	var release func()
	switch action {
	case TurnActionRevertCode, TurnActionRevertHistory:
		var err error
		release, err = a.reserveTurnActionUnit(unit)
		if err != nil {
			return TurnActionResult{}, err
		}
	}
	// The release must run on success as well as failure because the unit
	// stays live after a successful revert; the release is what clears its
	// transitioning reservation. That is why a successful revert emits one
	// extra queue-changed event, which is idempotent for consumers.
	defer func() {
		if release != nil {
			release()
		}
	}()

	// The fork case runs outside the locked body below: its durable tree copy
	// must run with rt.mu released, so the fork manages its own lock split and
	// validations (see forkUnitAtTurn).
	if action == TurnActionFork {
		return a.applyForkTurnAction(unit, turn, alsoRevertCode, emit)
	}

	// The remaining actions (the reverts) change the session; revert_history
	// clears the queue at the irreversible store mutation and enqueues the
	// event in the same runtime.mu section. Both were validated by the
	// reservation.
	var eventSessionID string
	var eventProjectID string
	a.ensureRuntime().mu.Lock()
	defer a.ensureRuntime().mu.Unlock()
	if unit == nil || unit.store == nil || !unit.store.Active() {
		return TurnActionResult{}, snapshot.ErrNoSession
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
		result.RestoredFiles = revertResult.Restored
		result.SkippedFiles = revertResult.Skipped
		if err != nil {
			// Some files may already be restored and some skipped; report the
			// partial outcome alongside the error.
			result, _ = a.populateTurnActionResultForSession(unit, result)
			return result, err
		}
		a.resetFileTrackerForSession(unit)
		return result, nil

	case TurnActionRevertHistory:
		target := turn - 1
		result.TargetTurn = target
		result.Prefill = prefill
		result.SessionChanged = true
		if alsoRevertCode {
			revertResult, err := unit.store.RevertCode(target)
			result.RestoredFiles = revertResult.Restored
			result.SkippedFiles = revertResult.Skipped
			if err != nil {
				result, _ = a.populateTurnActionResultForSession(unit, result)
				return result, err
			}
		}
		// One rule, keyed on the reload: the loop must match what the revert
		// achieved, whatever that was. The walk stops at the first failed
		// removal, so even a failed walk removed every turn above the one it
		// stopped at; the loop is therefore re-derived from disk after the
		// walk, and the pre-walk truncation point tells the queue rule whether
		// any turn was removed.
		before := unit.store.CurrentTurn()
		removedRecord, revertErr := unit.store.RevertHistory(target)
		// A revert below a compaction boundary invalidates the session's
		// indexed summary along with the record: search_history would keep
		// serving a "Full summary" path that no longer resolves. Delete the
		// session's summaries before the reload, so the eviction path is
		// covered by the same call. The error is discarded exactly as the
		// sweep and delete call sites discard it.
		if removedRecord && a.memoryHooks != nil {
			_ = a.memoryHooks.DeleteSessionSummaries(eventSessionID)
		}
		// The store maintains the post-walk turn for all three outcomes: the
		// target on a completed walk, the turn the walk stopped at on a
		// partial failure, unchanged when the first removal failed. It is the
		// prune point for retained errors and the queue rule alike, so it is
		// read once.
		after := unit.store.CurrentTurn()
		if err := a.loadHistoryIntoLoopForSession(unit); err != nil {
			// The loop can no longer match disk, so the unit must not stay
			// live: releasing the reservation would re-arm the drainers over
			// the mismatched loop, and leaving it set fails unitMutableLocked
			// forever. Evict instead — the files are untouched and reopening
			// loads them again.
			id := sessionIDOf(unit)
			if unit == a.session {
				a.store.Detach()
				if a.currentSessionID == id {
					a.currentSessionID = ""
				}
				delete(a.sessions, id)
				a.ensureRuntime().unregisterTranscript(id)
				a.ensureRuntime().clearQueueLocked()
				a.resetCurrentSessionStateLocked()
			} else {
				unit.store.Detach()
				a.ensureRuntime().clearQueueLockedForSession(unit)
				delete(a.sessions, id)
				a.ensureRuntime().unregisterTranscript(id)
			}
			release = nil
			if emit != nil {
				emit(HydrationState{}, nil)
			}
			result, _ = a.populateTurnActionResultForSession(unit, result)
			if revertErr != nil {
				return result, revertErr
			}
			return result, err
		}
		// The loop now matches disk. History irreversibly truncated: the
		// queued input no longer applies whenever the walk removed at least
		// one turn, and is kept when the walk removed nothing.
		if after < before {
			_, clearedVersion, queueCleared := a.ensureRuntime().clearQueueLockedForSession(unit)
			if queueCleared {
				a.emitEvent(Event{Kind: EventQueueChanged, SessionID: eventSessionID, ProjectID: eventProjectID, Queue: emptyQueue(), QueueVersion: clearedVersion})
			}
		}
		a.resetFileTrackerForSession(unit)
		// Retained errors tagged to turns the revert removed point at history
		// that is gone; prune them before the boundary capture, so the
		// published state carries the survivor set once. This is the only
		// thing a revert does to the coordinator: no cursor moves.
		if tr := a.transcriptForSessionID(eventSessionID); tr != nil {
			tr.seqMu.Lock()
			tr.dropErrorsAboveTurnLocked(after)
			tr.seqMu.Unlock()
		}
		result, maxDurableTurn := a.populateTurnActionResultForSession(unit, result)
		// A reconciled walk failure still publishes the boundary: the loop is
		// reconciled, so the capture is over state that matches disk, and the
		// adapters learn the turns are gone from both.
		a.emitTurnActionBoundaryLocked(unit, result, maxDurableTurn, emit)
		if revertErr != nil {
			return result, revertErr
		}
		return result, nil

	default:
		return TurnActionResult{}, fmt.Errorf("unknown turn action %q", action)
	}
}

// applyForkTurnAction applies the fork turn action. It runs outside
// applyTurnActionForSession's locked body because the fork's durable tree copy
// must run with rt.mu released; the fork manages its own lock split and
// validations (see forkUnitAtTurn). The caller holds lifecycleMu. The code
// revert, when requested, runs only after the fork is published, so a fork
// failure never mutates the working tree; it is best-effort against the source
// store, which retains the post-target snapshots needed to undo later changes:
// a revert error keeps the partial result rather than failing the
// already-committed fork.
func (a *Agent) applyForkTurnAction(unit *session, turn int, alsoRevertCode bool, emit func(HydrationState, []snapshot.SkippedRevert)) (TurnActionResult, error) {
	result := TurnActionResult{Action: TurnActionFork, Turn: turn}
	result.TargetTurn = turn
	result.SessionChanged = true
	if turn < 1 {
		return TurnActionResult{}, fmt.Errorf("turn must be >= 1")
	}
	candidate, err := a.forkUnitAtTurn(unit, turn)
	if err != nil {
		// The fork never published; the source unit is unchanged, so its
		// state is still the accurate result payload.
		result, _ = a.populateTurnActionResultForSession(unit, result)
		return result, err
	}
	result, maxDurableTurn := a.populateTurnActionResultForSession(candidate, result)
	// The revert and the in-commit boundary both run under rt.mu; the result
	// payload above was built before the lock so no durable read runs inside
	// it.
	rt := a.ensureRuntime()
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if alsoRevertCode {
		revertResult, revertErr := unit.store.RevertCode(turn)
		result.RestoredFiles = revertResult.Restored
		result.SkippedFiles = revertResult.Skipped
		if revertErr != nil {
			fmt.Fprintf(os.Stderr, "lightcode: fork code revert: %v\n", revertErr)
		}
	}
	a.emitTurnActionBoundaryLocked(candidate, result, maxDurableTurn, emit)
	return result, nil
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
	r, _ := a.populateTurnActionResultForSession(a.session, result)
	return r
}

// populateTurnActionResultForSession fills a turn-action result with the
// session's complete post-mutation view: the summary, the durable display
// history (prebuilt before the boundary emission), and the token report. It
// also reports the highest durable turn in that history, which the boundary
// capture needs to drop a retained tail the durable half already covers.
func (a *Agent) populateTurnActionResultForSession(unit *session, result TurnActionResult) (TurnActionResult, int) {
	result.Session = sessionSummary(unit)
	maxDurableTurn := 0
	if unit != nil && unit.store != nil && unit.store.Active() {
		result.Messages, maxDurableTurn, _ = a.messagesForFrontendForStoreAndMaxTurn(unit.store, unit.store.SessionID())
		unit.tokensMu.Lock()
		result.Tokens = a.buildReportForSessionLocked(unit)
		unit.tokensMu.Unlock()
	}
	return result, maxDurableTurn
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

// cleanupStaging removes a candidate's staging directory after a preparation
// failure, joining any cleanup error with the original cause so neither is lost.
func cleanupStaging(stagingRoot string, cause error) error {
	if err := os.RemoveAll(stagingRoot); err != nil {
		return errors.Join(cause, fmt.Errorf("snapshot: fork staging cleanup: %w", err))
	}
	return cause
}

// forkUnitAtTurn forks unit at turn through the split shape: the source is
// validated and the copy inputs snapshotted under rt.mu, the durable tree copy
// into staging runs with rt.mu released, then rt.mu is re-acquired and — only
// after the source is re-validated — the candidate is built and published.
// Each lock half's unlock is deferred within its own scope, so a panic cannot
// leave the bracket unbalanced. The caller holds lifecycleMu; any failure
// removes only the staging directory and leaves the source unchanged.
func (a *Agent) forkUnitAtTurn(unit *session, turn int) (*session, error) {
	rt := a.ensureRuntime()

	// First half: validate the source and snapshot the copy inputs under rt.mu.
	sessionID, sessionsRoot, srcRef, err := func() (string, string, coremodel.ModelRef, error) {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return a.forkSnapshotSourceLocked(unit)
	}()
	if err != nil {
		return nil, err
	}

	// The released section: only the durable tree copy runs without rt.mu.
	stagingRoot, newID, err := a.forkCopyTree(unit, sessionsRoot, turn)
	if err != nil {
		return nil, err
	}

	// Second half: re-acquire rt.mu, re-validate the source, then build and
	// publish the candidate.
	return func() (*session, error) {
		rt.mu.Lock()
		defer rt.mu.Unlock()
		return a.forkCommitStagedLocked(unit, sessionID, sessionsRoot, srcRef, stagingRoot, newID)
	}()
}

// forkSnapshotSourceLocked validates the source and snapshots the fork inputs
// under rt.mu: the source session id and sessions root (the copy destination)
// and the source's live model ref (currentRef is rt.mu-guarded). Caller holds
// rt.mu.
func (a *Agent) forkSnapshotSourceLocked(unit *session) (sessionID, sessionsRoot string, srcRef coremodel.ModelRef, err error) {
	// Admission check at the fork's entry, before the staged copy and the
	// publication: a fork starting after shutdown published closed must not
	// copy the tree or publish a new session.
	if a.ensureRuntime().closed {
		return "", "", coremodel.ModelRef{}, errOwnerClosed
	}
	if unit == nil || unit.store == nil || !unit.store.Active() {
		return "", "", coremodel.ModelRef{}, snapshot.ErrNoSession
	}
	if err := unitMutableLocked(unit); err != nil {
		return "", "", coremodel.ModelRef{}, err
	}
	return unit.store.SessionID(), unit.store.Root(), unit.currentRef, nil
}

// forkCopyTree copies the source session's tree into a fresh unlisted staging
// root. It is the fork's only hoisted durable I/O and runs with rt.mu
// released; its inputs were snapshotted under rt.mu by the caller. Returns the
// new session id and the staging root; the source store stays authoritative.
func (a *Agent) forkCopyTree(unit *session, sessionsRoot string, turn int) (stagingRoot, newID string, err error) {
	stagingRoot, err = snapshot.NewStagingSessionsRoot(sessionsRoot)
	if err != nil {
		return "", "", err
	}
	a.fireDurableReadHook()
	newID, _, err = unit.store.ForkInto(turn, stagingRoot)
	if err != nil {
		return "", "", cleanupStaging(stagingRoot, err)
	}
	return stagingRoot, newID, nil
}

// forkCommitStagedLocked re-validates the source under rt.mu — the copy ran
// without it, so a concurrent close, replacement, or turn start may have made
// the source unforkable — and only then builds the candidate from the staged
// copy and publishes it with the single atomic rename into the session
// namespace, registering the fork as its own live unit and selecting it when
// the source was current. The source is still authoritative until the rename
// succeeds; any failure leaves the source unchanged and removes only its
// staging directory. Caller holds rt.mu.
func (a *Agent) forkCommitStagedLocked(unit *session, sessionID, sessionsRoot string, srcRef coremodel.ModelRef, stagingRoot, newID string) (*session, error) {
	// Re-validate the source before committing on the staged copy: the source
	// was validated and snapshotted under rt.mu before the copy released it,
	// so never commit on state read before the release.
	if unit == nil || unit.store == nil || !unit.store.Active() {
		return nil, snapshot.ErrNoSession
	}
	if unit.store.SessionID() != sessionID {
		return nil, snapshot.ErrNoSession
	}
	if err := unitMutableLocked(unit); err != nil {
		return nil, err
	}
	// Prepare a separate candidate store and unit against the staged copy. The
	// source (sessionID) and candidate (newID) claims are both held here.
	candidateStore, err := unit.store.NewStagingStore(stagingRoot)
	if err != nil {
		return nil, cleanupStaging(stagingRoot, err)
	}
	a.fireDurableReadHook()
	if err := candidateStore.LoadSession(newID); err != nil {
		return nil, cleanupStaging(stagingRoot, err)
	}
	candidate, _, err := a.rootRunningUnitLocked(candidateStore, unit.activeAgentType, unit.projectID, unit.projectName, unit.projectRoot)
	if err != nil {
		candidateStore.Detach()
		return nil, cleanupStaging(stagingRoot, err)
	}
	a.fireDurableReadHook()
	if err := a.loadHistoryIntoLoopForSession(candidate); err != nil {
		candidateStore.Detach()
		return nil, cleanupStaging(stagingRoot, err)
	}
	// Prepare all remaining candidate state from the staged copy before the
	// durable commit, so no fallible read runs after the rename.
	a.resetFileTrackerForSession(candidate)
	a.loadTokensFromDiskForSession(candidate)
	if err := a.inheritActiveModelForForkedSession(candidate, srcRef); err != nil {
		candidateStore.Detach()
		return nil, cleanupStaging(stagingRoot, err)
	}
	// Durable commit: the atomic rename publishes the candidate.
	if err := snapshot.PublishStagedSession(stagingRoot, sessionsRoot, newID); err != nil {
		candidate.store.Detach()
		return nil, cleanupStaging(stagingRoot, err)
	}
	if err := candidate.store.RelocateActiveSessionPaths(sessionsRoot); err != nil {
		candidate.store.Detach()
		return nil, err
	}
	// Post-commit publication is infallible: drop the empty staging parent, then
	// register the fork as its own live unit, selecting it only when the source
	// was current.
	_ = os.RemoveAll(stagingRoot)
	if a.currentSessionID == sessionID {
		if err := a.setCurrentSessionLocked(candidate); err != nil {
			return nil, err
		}
	} else {
		if err := a.registerLiveSessionLocked(candidate); err != nil {
			return nil, err
		}
	}
	return candidate, nil
}

func (a *Agent) ForkSessionForSession(sessionID string, turn int) error {
	defer a.lockLifecycle()()
	unit, err := a.resolveRootDriveSession(sessionID)
	if err != nil {
		return err
	}
	return a.forkSessionForSession(unit, turn)
}

// forkSessionForSession forks unit through forkUnitAtTurn's snapshot/copy/
// revalidate-commit split, which owns the rt.mu bracket.
func (a *Agent) forkSessionForSession(unit *session, turn int) error {
	_, err := a.forkUnitAtTurn(unit, turn)
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
	ctx := a.agentResolveContextLocked()
	if strings.TrimSpace(projectID) != "" {
		ctx.ProjectID = projectID
	}
	return a.resolvedAgentTypeWithContextLocked(name, ctx)
}

// resolvedAgentTypeWithContextLocked resolves one agent type against the agents
// config. The config is written under runtime.mu (applyReloadStateLocked ->
// setAgentTypesLocked), so the caller must hold runtime.mu across this call;
// resolution is a pure in-memory read returning a value copy, never pointers
// into the config. The resolve context is prebuilt by the caller so no durable
// read (the current project id) runs under the lock.
func (a *Agent) resolvedAgentTypeWithContextLocked(name string, ctx agentcfg.ResolveContext) (agentcfg.Resolved, error) {
	if a.agents == nil {
		return agentcfg.Resolved{}, fmt.Errorf("agents config is not loaded")
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
