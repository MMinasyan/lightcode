package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	openai "github.com/sashabaranov/go-openai"

	"github.com/MMinasyan/lightcode/internal/compact"
	"github.com/MMinasyan/lightcode/internal/config"
	"github.com/MMinasyan/lightcode/internal/loop"
	"github.com/MMinasyan/lightcode/internal/lsp"
	"github.com/MMinasyan/lightcode/internal/memory"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/project"
	"github.com/MMinasyan/lightcode/internal/prompt"
	"github.com/MMinasyan/lightcode/internal/provider"
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

	currentProvider string
	currentModel    string

	tokensMu          sync.Mutex
	tokens            map[string]*TokenEntry
	lastContextUsed   int
	contextWindowSize int

	assembler       *prompt.Assembler
	pendingWarnings []prompt.Warning

	memoryStore *memory.Store

	lspManager     *lsp.Manager
	lspDiagnostics *tool.LSPDiagnostics

	subagentLoader *subagent.Loader
	taggedEvents   chan TaggedLoopEvent
	taskToolInst   *taskTool
	seenSessions   map[string]bool

	loopFlush chan chan struct{}
}

// New constructs an Agent from the given config. It creates the
// provider client, tool registry, permission gate, snapshot store,
// and loop. Call Init after setting up the event handler.
func New(c Config) (*Agent, error) {
	prov, modelID, apiKey, err := c.Cfg.ResolveDefault()
	if err != nil {
		return nil, err
	}
	client := provider.New(prov.BaseURL, apiKey, modelID)

	resolver, err := project.NewResolver(c.Home, c.ProjectRoot)
	if err != nil {
		return nil, fmt.Errorf("init project resolver: %w", err)
	}
	store, err := snapshot.NewForSessionsRoot("", resolver.Root(), "")
	if err != nil {
		return nil, fmt.Errorf("init snapshot store: %w", err)
	}

	events := make(chan loop.Event, 256)

	a := &Agent{
		cfg:               c.Cfg,
		store:             store,
		projects:          resolver,
		projectRoot:       c.ProjectRoot,
		home:              c.Home,
		loopEvents:        events,
		currentProvider:   c.Cfg.DefaultModel.Provider,
		currentModel:      modelID,
		contextWindowSize: resolveContextWindow(client, c.Cfg, c.Cfg.DefaultModel.Provider, modelID, c.Home),
		loopFlush:         make(chan chan struct{}, 1),
	}

	gate := permission.NewGate(func(req permission.Request) {
		a.emitEvent(Event{
			Kind: EventPermissionRequest,
			PermReq: &PermissionRequest{
				ID:       req.ID,
				ToolName: req.ToolName,
				Arg:      req.Arg,
			},
		})
	})
	a.gate = gate

	checkFunc := tool.CheckFunc(func(toolName, arg string) permission.Decision {
		var local permission.Rules
		if proj, err := resolver.Current(); err == nil && proj != nil {
			local, _ = permission.LoadLocal(resolver.Root(), proj.ID)
		}
		return permission.Check(local, c.Cfg.Permissions, toolName, arg, c.ProjectRoot, c.Home, c.ProjectRoot)
	})

	askFunc := tool.AskFunc(func(toolName, arg string) bool {
		a.mu.Lock()
		ctx := a.turnCtx
		a.mu.Unlock()
		if ctx == nil {
			return false
		}
		return gate.Ask(ctx, toolName, arg)
	})

	registry := tool.NewRegistry()
	registry.Register(tool.WrapWithPermission(tool.ReadFile{}, checkFunc, askFunc))
	registry.Register(tool.WrapWithPermission(tool.NewWriteFileWithSnapshot(store), checkFunc, askFunc))
	registry.Register(tool.WrapWithPermission(tool.NewEditFileWithSnapshot(store), checkFunc, askFunc))
	registry.Register(tool.WrapWithPermission(&tool.RunCommand{}, checkFunc, askFunc))

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

	var subBaseURL, subAPIKey, subProvName, subModel string
	if c.Cfg.Subagents.Provider != "" {
		if sp, ok := c.Cfg.Providers[c.Cfg.Subagents.Provider]; ok {
			subBaseURL = sp.BaseURL
			subProvName = c.Cfg.Subagents.Provider
			if sp.APIKeyEnv != "" {
				subAPIKey = os.Getenv(sp.APIKeyEnv)
			}
		}
	}
	if c.Cfg.Subagents.Model != "" {
		subModel = c.Cfg.Subagents.Model
	}

	tt := newTaskTool(taskToolConfig{
		Loader:          loader,
		ParentStore:     store,
		BaseRegistry:    registry,
		MaxConcurrent:   c.Cfg.Subagents.MaxConcurrent,
		TaggedEvents:    taggedEvts,
		ProviderName:    c.Cfg.DefaultModel.Provider,
		Model:           modelID,
		BaseURL:         prov.BaseURL,
		APIKey:          apiKey,
		SubProviderName: subProvName,
		SubModel:        subModel,
		SubBaseURL:      subBaseURL,
		SubAPIKey:       subAPIKey,
	})
	registry.Register(tt)
	a.subagentLoader = loader
	a.taggedEvents = taggedEvts
	a.taskToolInst = tt

	a.registry = registry

	asm := prompt.New(c.ProjectRoot, c.Home)
	res := asm.Assemble()
	a.assembler = asm
	a.pendingWarnings = res.Warnings

	l := loop.New(client, registry, res.Prompt)
	l.SetEvents(events)
	l.SetStore(store)
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
	if a.memoryStore != nil {
		a.memoryStore.Reconcile()
	}
	a.runSweep()
	if err := a.resumeMostRecent(); err != nil {
		fmt.Fprintf(os.Stderr, "lightcode: resume session: %v\n", err)
	}
	go a.periodicSweep(ctx)

	if a.lspManager != nil {
		a.lspManager.SetWarningHandler(func(kind, message string) {
			a.emitWarnings([]prompt.Warning{{Kind: kind, Message: message}})
		})
		a.lspManager.SetSignalHandler(func(content string) {
			if a.lp != nil {
				a.lp.AppendSignal(content)
			}
		})
		go a.lspManager.Detect(ctx)
		go func() {
			<-ctx.Done()
			a.lspManager.ShutdownAll()
		}()
	}

	a.emitWarnings(a.pendingWarnings)
	a.pendingWarnings = nil
}

func (a *Agent) emitEvent(ev Event) {
	if a.onEvent != nil {
		a.onEvent(ev)
	}
}

func (a *Agent) emitWarnings(warnings []prompt.Warning) {
	if len(warnings) == 0 {
		return
	}
	pw := make([]PromptWarning, len(warnings))
	for i, w := range warnings {
		pw[i] = PromptWarning{Kind: w.Kind, Message: w.Message}
	}
	a.emitEvent(Event{Kind: EventWarning, Warnings: pw})
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
			IsError:    ev.IsError,
			Result:     ev.Result,
		})
	case loop.Usage:
		a.recordUsage(ev)
	}
}

func (a *Agent) dispatchTaggedEvent(tev TaggedLoopEvent) {
	if a.seenSessions == nil {
		a.seenSessions = make(map[string]bool)
	}
	if tev.SessionID != "" && !a.seenSessions[tev.SessionID] {
		a.seenSessions[tev.SessionID] = true
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
		base.IsError = ev.IsError
		base.Result = ev.Result
	case loop.Usage:
		a.recordUsage(ev)
		return
	default:
		return
	}
	a.emitEvent(base)
}

func (a *Agent) recordUsage(ev loop.Event) {
	a.tokensMu.Lock()
	prov := a.currentProvider
	model := ev.Model
	if model == "" {
		model = a.currentModel
	}
	key := prov + "/" + model
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
		onDelete = func(sessionID string) { a.memoryStore.DeleteSessionSummaries(sessionID) }
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
		Summary:         result.Summary,
		BoundaryTurn:    boundaryTurn,
		CompactedAt:     time.Now().UTC().Format(time.RFC3339),
		SummarizerModel: result.SummarizerModel,
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
		a.memoryStore.IndexSummary(sessionID, projID, projName, result.Summary, rec.CompactedAt, compactionPath)
	}

	a.lp.LoadHistoryWithSummary(result.Summary, nil)

	a.tokensMu.Lock()
	a.lastContextUsed = 0
	a.tokensMu.Unlock()

	return nil
}

func (a *Agent) summarizerClientAndWindow() (*provider.Client, int) {
	provName := a.cfg.Compaction.SummarizerProvider
	model := a.cfg.Compaction.SummarizerModel
	if provName == "" {
		provName = a.currentProvider
	}
	if model == "" {
		model = a.currentModel
	}
	prov, ok := a.cfg.Providers[provName]
	if !ok {
		prov = a.cfg.Providers[a.currentProvider]
		provName = a.currentProvider
	}
	var apiKey string
	if prov.APIKeyEnv != "" {
		apiKey = os.Getenv(prov.APIKeyEnv)
	}
	client := provider.New(prov.BaseURL, apiKey, model)
	window := resolveContextWindow(client, a.cfg, provName, model, a.home)
	return client, window
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
	a.loadTokensFromDisk()
	a.restoreModelFromSession()
	return nil
}

func (a *Agent) restoreModelFromSession() {
	meta, err := a.store.Meta()
	if err != nil || meta.Provider == "" || meta.Model == "" {
		return
	}
	prov, ok := a.cfg.Providers[meta.Provider]
	if !ok {
		return
	}
	var apiKey string
	if prov.APIKeyEnv != "" {
		apiKey = os.Getenv(prov.APIKeyEnv)
		if apiKey == "" {
			return
		}
	}
	client := provider.New(prov.BaseURL, apiKey, meta.Model)
	a.lp.SetClient(client)
	a.currentProvider = meta.Provider
	a.currentModel = meta.Model
	a.contextWindowSize = resolveContextWindow(client, a.cfg, meta.Provider, meta.Model, a.home)
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

	decoded := make([][]openai.ChatCompletionMessage, 0, len(raw))
	for _, t := range raw {
		var turnMsgs []openai.ChatCompletionMessage
		for _, line := range t.Messages {
			var m openai.ChatCompletionMessage
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
		a.lp.LoadHistoryWithSummary(rec.Summary, decoded)
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
	_ = a.store.SetModel(a.currentProvider, a.currentModel)
	a.lp.ResetHistory()
	a.loadTokensFromDisk()
	return nil
}

// --- Public methods (the service API) ---

// SendPrompt starts a turn with a single user message.
func (a *Agent) SendPrompt(ctx context.Context, content string) (int, error) {
	return a.sendMessages(ctx, []string{content})
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
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return 0, fmt.Errorf("a turn is already in progress")
	}
	a.busy = true
	a.seenSessions = nil
	turnCtx, cancel := context.WithCancel(ctx)
	a.turnCancel = cancel
	a.turnCtx = turnCtx
	a.mu.Unlock()

	if err := a.ensureSession(); err != nil {
		a.mu.Lock()
		a.busy = false
		a.turnCancel = nil
		a.turnCtx = nil
		a.mu.Unlock()
		cancel()
		return 0, err
	}

	turn := a.store.BeginTurn()

	a.emitEvent(Event{Kind: EventTurnStart, Turn: turn})

	if a.taskToolInst != nil {
		prov, ok := a.cfg.Providers[a.currentProvider]
		var ak string
		if ok && prov.APIKeyEnv != "" {
			ak = os.Getenv(prov.APIKeyEnv)
		}
		var bu string
		if ok {
			bu = prov.BaseURL
		}
		a.taskToolInst.updateParentState(a.currentProvider, a.currentModel, bu, ak, cancel)
	}

	go func() {
		defer func() {
			a.mu.Lock()
			a.busy = false
			a.turnCancel = nil
			a.turnCtx = nil
			a.mu.Unlock()
			cancel()
		}()

		res := a.assembler.Assemble()
		if res.Rebuilt {
			a.lp.UpdateSystemPrompt(res.Prompt)
		}
		a.emitWarnings(res.Warnings)

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

	return turn, nil
}

// Cancel aborts the current turn.
func (a *Agent) Cancel() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.turnCancel != nil {
		a.turnCancel()
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
	return a.gate.Respond(id, true)
}

// SwitchModel changes the active provider and model.
func (a *Agent) SwitchModel(providerName, model string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return fmt.Errorf("cannot switch model while a turn is running")
	}
	prov, ok := a.cfg.Providers[providerName]
	if !ok {
		return fmt.Errorf("unknown provider %q", providerName)
	}
	var apiKey string
	if prov.APIKeyEnv != "" {
		apiKey = os.Getenv(prov.APIKeyEnv)
		if apiKey == "" {
			return fmt.Errorf("env var %s is unset for provider %q", prov.APIKeyEnv, providerName)
		}
	}
	client := provider.New(prov.BaseURL, apiKey, model)
	a.lp.SetClient(client)
	a.currentProvider = providerName
	a.currentModel = model
	a.contextWindowSize = resolveContextWindow(client, a.cfg, providerName, model, a.home)
	a.lp.AppendSignal(fmt.Sprintf("<system-signal>Model switched to %s (%s)</system-signal>", model, providerName))
	if a.store.Active() {
		_ = a.store.SetModel(providerName, model)
	}
	return nil
}

// CurrentModel returns the active provider and model.
func (a *Agent) CurrentModel() ModelInfo {
	return ModelInfo{Provider: a.currentProvider, Model: a.currentModel}
}

// ModelList returns all configured providers and their models.
func (a *Agent) ModelList() []ProviderModels {
	var result []ProviderModels
	for name, prov := range a.cfg.Providers {
		result = append(result, ProviderModels{Provider: name, Models: prov.Models})
	}
	return result
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
	a.loadTokensFromDisk()
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
		a.memoryStore.DeleteSessionSummaries(id)
	}
	if err := snapshot.DeleteSession(sessionsRoot, id); err != nil {
		return false, err
	}
	if closedCurrent {
		a.lp.ResetHistory()
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
			var m openai.ChatCompletionMessage
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			switch m.Role {
			case openai.ChatMessageRoleSystem:

			case openai.ChatMessageRoleUser:
				c := m.Content
				if strings.HasPrefix(c, "<system-signal>") && strings.HasSuffix(c, "</system-signal>") {
					signal := c[len("<system-signal>") : len(c)-len("</system-signal>")]
					if strings.Contains(signal, "interrupted") {
						out = append(out, DisplayMessage{Type: "system", Content: "interrupted"})
					} else if strings.HasPrefix(signal, "Model switched") {
						out = append(out, DisplayMessage{Type: "system", Content: signal})
					}
				} else {
					out = append(out, DisplayMessage{Type: "user", Content: c, Turn: t.Turn})
				}

			case openai.ChatMessageRoleAssistant:
				if m.Content != "" {
					out = append(out, DisplayMessage{Type: "assistant", Content: m.Content, Turn: t.Turn})
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

			case openai.ChatMessageRoleTool:
				if idx, ok := toolStubs[m.ToolCallID]; ok {
					out[idx].Done = true
					out[idx].Success = m.Content != "denied by user" && !strings.HasPrefix(m.Content, "error: ")
					out[idx].Result = m.Content
				}
			}
		}
	}
	return out
}

// --- Snapshot / revert operations ---

// RevertCode restores files to their state at the given turn.
func (a *Agent) RevertCode(turn int) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy {
		return fmt.Errorf("cannot revert while a turn is running")
	}
	if !a.store.Active() {
		return fmt.Errorf("no session open")
	}
	_, err := a.store.RevertCode(turn)
	return err
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
	return a.loadHistoryIntoLoop()
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

// ReadFileContent reads a file for the inline viewer. Relative paths
// resolve against the project root.
func (a *Agent) ReadFileContent(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(a.projectRoot, path)
	}
	b, err := os.ReadFile(path)
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

func contextWindowFromConfig(cfg *config.Config, provName, model string) int {
	prov, ok := cfg.Providers[provName]
	if !ok {
		return 0
	}
	return prov.ContextWindows[model]
}

func resolveContextWindow(client *provider.Client, cfg *config.Config, provName, model, home string) int {
	cacheKey := provName + "/" + model
	fromCfg := contextWindowFromConfig(cfg, provName, model)

	// Check disk cache first.
	cached := loadCachedContextWindow(home, cacheKey)
	if cached > 0 {
		if fromCfg > 0 && fromCfg < cached {
			return fromCfg
		}
		return cached
	}

	// Try API.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	fromAPI := client.FetchContextWindow(ctx)

	if fromAPI > 0 {
		saveCachedContextWindow(home, cacheKey, fromAPI)
		if fromCfg > 0 && fromCfg < fromAPI {
			return fromCfg
		}
		return fromAPI
	}

	return fromCfg
}

func contextWindowCachePath(home string) string {
	return filepath.Join(home, ".lightcode", "context_windows.json")
}

func loadCachedContextWindow(home, key string) int {
	data, err := os.ReadFile(contextWindowCachePath(home))
	if err != nil {
		return 0
	}
	var cache map[string]int
	if json.Unmarshal(data, &cache) != nil {
		return 0
	}
	return cache[key]
}

func saveCachedContextWindow(home, key string, value int) {
	path := contextWindowCachePath(home)
	var cache map[string]int
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &cache)
	}
	if cache == nil {
		cache = map[string]int{}
	}
	cache[key] = value
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, append(data, '\n'), 0o600)
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
