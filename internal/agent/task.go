package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/MMinasyan/lightcode/internal/adaptation"
	"github.com/MMinasyan/lightcode/internal/catalog"
	"github.com/MMinasyan/lightcode/internal/cmdoutput"
	"github.com/MMinasyan/lightcode/internal/config"
	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/engine/coremodel"
	"github.com/MMinasyan/lightcode/internal/lsp"
	"github.com/MMinasyan/lightcode/internal/memory"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/process"
	"github.com/MMinasyan/lightcode/internal/provider"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/subagent"
	"github.com/MMinasyan/lightcode/internal/tool"
)

type taskDef struct {
	Prompt       string `json:"prompt"`
	SubagentType string `json:"subagent_type"`
}

type taskResult struct {
	index     int
	result    string
	sessionID string
	denied    bool
	err       error
}

type TaggedLoopEvent struct {
	SessionID  string
	TaskIndex  int
	ToolCallID string
	Event      loop.Event
}

type taskTool struct {
	loader        *subagent.Loader
	parentStore   *snapshot.Store
	parentTracker *tool.FileTracker
	maxConcurrent int
	taggedEvents  chan<- TaggedLoopEvent

	mu           sync.Mutex
	modelCatalog *catalog.Catalog
	providerName string
	model        string
	cancelParent func()

	subModel string

	resolveAdapt adaptation.Resolver

	toolsConfig   config.ToolsConfig
	homeDir       string
	workspaceRoot string
	procMgr       *process.Manager
	memoryStore   *memory.Store
	projectID     string
	memoriesDir   string
	lspManager    *lsp.Manager
	check         tool.CheckFunc
	ask           tool.AskFunc
	askAction     tool.AskActionFunc
	usageRecorder loop.UsageRecorder

	resultMetadata map[string]map[string]any
}

type taskToolConfig struct {
	Loader        *subagent.Loader
	ParentStore   *snapshot.Store
	ParentTracker *tool.FileTracker
	MaxConcurrent int
	TaggedEvents  chan<- TaggedLoopEvent

	ModelCatalog *catalog.Catalog
	ProviderName string
	Model        string

	SubModel string

	ResolveAdapt adaptation.Resolver

	ToolsConfig   config.ToolsConfig
	HomeDir       string
	WorkspaceRoot string
	ProcMgr       *process.Manager
	MemoryStore   *memory.Store
	ProjectID     string
	MemoriesDir   string
	LSPManager    *lsp.Manager
	Check         tool.CheckFunc
	Ask           tool.AskFunc
	AskAction     tool.AskActionFunc
	UsageRecorder loop.UsageRecorder
}

func newTaskTool(cfg taskToolConfig) *taskTool {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	return &taskTool{
		loader:        cfg.Loader,
		parentStore:   cfg.ParentStore,
		parentTracker: cfg.ParentTracker,
		maxConcurrent: cfg.MaxConcurrent,
		taggedEvents:  cfg.TaggedEvents,
		modelCatalog:  cfg.ModelCatalog,
		providerName:  cfg.ProviderName,
		model:         cfg.Model,
		subModel:      cfg.SubModel,
		resolveAdapt:  cfg.ResolveAdapt,
		toolsConfig:   cfg.ToolsConfig,
		homeDir:       cfg.HomeDir,
		workspaceRoot: cfg.WorkspaceRoot,
		procMgr:       cfg.ProcMgr,
		memoryStore:   cfg.MemoryStore,
		projectID:     cfg.ProjectID,
		memoriesDir:   cfg.MemoriesDir,
		lspManager:    cfg.LSPManager,
		check:         cfg.Check,
		ask:           cfg.Ask,
		askAction:     cfg.AskAction,
		usageRecorder: cfg.UsageRecorder,
	}
}

func (*taskTool) Name() string { return "task" }

func (t *taskTool) Description() string {
	var b strings.Builder
	b.WriteString("Spawn one or more subagents to work on tasks concurrently. Each task runs in its own context with a restricted toolset defined by its subagent_type. Results are returned when all tasks complete.\n\nAvailable subagent types:\n")
	if t.loader != nil {
		for _, at := range t.loader.All() {
			fmt.Fprintf(&b, "- %s: %s\n", at.Name, at.Description)
		}
	}
	return b.String()
}

func (*taskTool) ParametersSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tasks": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"prompt": map[string]any{
							"type":        "string",
							"description": "The task prompt for this subagent.",
						},
						"subagent_type": map[string]any{
							"type":        "string",
							"description": "The type of subagent to use.",
						},
					},
					"required": []string{"prompt", "subagent_type"},
				},
				"description": "Array of tasks to run concurrently.",
			},
		},
		"required": []string{"tasks"},
	}
}

func (t *taskTool) setMaxConcurrent(maxConcurrent int) {
	if maxConcurrent <= 0 {
		maxConcurrent = 4
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.maxConcurrent = maxConcurrent
}

func (t *taskTool) setToolsConfig(cfg config.ToolsConfig) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.toolsConfig = cfg
}

func (t *taskTool) Execute(ctx context.Context, params map[string]any) (string, error) {
	tasksRaw, ok := params["tasks"]
	if !ok {
		return "", fmt.Errorf("missing 'tasks' parameter")
	}
	data, err := json.Marshal(tasksRaw)
	if err != nil {
		return "", fmt.Errorf("invalid tasks: %v", err)
	}
	var tasks []taskDef
	if err := json.Unmarshal(data, &tasks); err != nil {
		return "", fmt.Errorf("invalid tasks: %v", err)
	}
	if len(tasks) == 0 {
		return "", fmt.Errorf("tasks array is empty")
	}

	toolCallID, _ := params["_tool_call_id"].(string)

	t.mu.Lock()
	maxConcurrent := t.maxConcurrent
	t.mu.Unlock()
	sem := make(chan struct{}, maxConcurrent)
	results := make([]taskResult, len(tasks))
	var wg sync.WaitGroup

	for i, task := range tasks {
		wg.Add(1)
		go func(idx int, td taskDef) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			results[idx] = t.runSubagent(ctx, idx, td, toolCallID)
		}(i, task)
	}
	wg.Wait()

	var b strings.Builder
	links := make([]SubagentSessionLink, 0, len(results))
	allDenied := true
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## Task %d (%s)\n\n", i+1, tasks[i].SubagentType)
		if r.sessionID != "" {
			links = append(links, SubagentSessionLink{Index: i, SessionID: r.sessionID})
		}
		if r.err != nil {
			fmt.Fprintf(&b, "Error: %v", r.err)
			allDenied = false
		} else if r.denied {
			b.WriteString("Tool denied by user.")
		} else {
			b.WriteString(r.result)
			allDenied = false
		}
	}

	output := b.String()
	if toolCallID != "" && len(links) > 0 {
		t.mu.Lock()
		if t.resultMetadata == nil {
			t.resultMetadata = map[string]map[string]any{}
		}
		t.resultMetadata[toolCallID] = map[string]any{"subagent_session_ids": links}
		t.mu.Unlock()
	}
	if allDenied {
		t.mu.Lock()
		cancel := t.cancelParent
		t.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	return output, nil
}

func (t *taskTool) DisplayMetadata(_ context.Context, args json.RawMessage, _ string) map[string]any {
	var params map[string]any
	if err := json.Unmarshal(args, &params); err != nil {
		return nil
	}
	toolCallID, _ := params["_tool_call_id"].(string)
	if toolCallID == "" {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	metadata := t.resultMetadata[toolCallID]
	delete(t.resultMetadata, toolCallID)
	return metadata
}

// resolveAdaptation maps a child model id to its adaptation, defaulting to the
// production matcher when no resolver is injected.
func (t *taskTool) resolveAdaptation(modelID string) *adaptation.Adaptation {
	if t.resolveAdapt != nil {
		return t.resolveAdapt(modelID)
	}
	return adaptation.Match(modelID)
}

// appendAdaptationBlocks appends an adaptation's coaching blocks to a subagent prompt
// (subagents bypass the section assembler), trimmed and blank-line separated. A nil or
// empty adaptation returns the prompt unchanged.
func appendAdaptationBlocks(prompt string, adapt *adaptation.Adaptation) string {
	if adapt == nil || len(adapt.Blocks) == 0 {
		return prompt
	}
	var b strings.Builder
	b.WriteString(strings.TrimSpace(prompt))
	for _, block := range adapt.Blocks {
		b.WriteString("\n\n")
		b.WriteString(strings.TrimSpace(block))
	}
	return b.String()
}

// buildChildLoop constructs the child loop for a subagent and applies the adaptation
// resolved from the child's OWN model (which may differ from the parent's via
// cfg.Subagents.Model): it appends the adaptation's coaching blocks to the prompt and
// installs the adaptation on the loop (tool advertisement, dispatch gate, leak pattern).
func (t *taskTool) buildChildLoop(at subagent.AgentType, client *provider.Client, registry *tool.Registry, ref coremodel.ModelRef) *loop.Loop {
	adapt := t.resolveAdaptation(ref.Model)
	lp := loop.New(provider.NewAdapter(client), registry, appendAdaptationBlocks(at.Prompt, adapt))
	lp.SetActiveAdaptation(adapt)
	return lp
}

func (t *taskTool) runSubagent(ctx context.Context, index int, td taskDef, parentToolCallID string) taskResult {
	at, err := t.loader.Load(td.SubagentType)
	if err != nil {
		return taskResult{index: index, err: fmt.Errorf("unknown subagent type %q: %w", td.SubagentType, err)}
	}

	client, ref, err := t.resolveClient()
	if err != nil {
		return taskResult{index: index, err: err}
	}
	if t.parentStore == nil {
		return taskResult{index: index, err: fmt.Errorf("subagent: parent session store is not configured")}
	}
	parentSessionID := t.parentStore.SessionID()
	parentTurn := t.parentStore.CurrentTurn()
	if parentSessionID == "" || parentTurn == 0 {
		return taskResult{index: index, err: fmt.Errorf("subagent: parent turn is not active")}
	}

	childStore, err := snapshot.NewForSessionsRoot(t.parentStore.Root(), "", "")
	if err != nil {
		return taskResult{index: index, err: err}
	}
	if err := childStore.BeginChildSession(t.workspaceRoot, parentSessionID); err != nil {
		return taskResult{index: index, err: err}
	}
	defer func() {
		_, _ = childStore.Close()
	}()
	if err := childStore.SetModel(ref.Provider, ref.Model); err != nil {
		return taskResult{index: index, err: err}
	}
	sessionID := childStore.SessionID()

	scope := parentMutationScope{
		store:         t.parentStore,
		turn:          parentTurn,
		tracker:       t.parentTracker,
		workspaceRoot: t.workspaceRoot,
	}

	var lp *loop.Loop
	childProcMgr := t.newChildProcessManager(sessionID, func(signal loop.PendingSignal) {
		if lp != nil {
			lp.AddPendingSignal(signal)
		}
	})
	registry := t.buildRegistry(at, scope, childProcMgr)

	var events chan loop.Event
	var forwardDone chan struct{}
	if t.taggedEvents != nil {
		events = make(chan loop.Event, 128)
		forwardDone = make(chan struct{})
		go func() {
			defer close(forwardDone)
			t.forwardEvents(events, index, sessionID, parentToolCallID)
		}()
	}
	finish := func(result taskResult) taskResult {
		result.sessionID = sessionID
		if childProcMgr != nil {
			childProcMgr.KillAll()
		}
		if events != nil {
			close(events)
			<-forwardDone
		}
		return result
	}

	lp = t.buildChildLoop(at, client, registry, ref)
	if events != nil {
		lp.SetEvents(events)
	}
	lp.SetStore(childStore)
	lp.SetUsageRecorder(t.usageRecorder)
	pendingExecutor := tool.NewStagedExecutorAtRoot(scope.snapshotStore(), scope.tracker, t.toolsConfig, scope.workspaceRoot, t.permissionCheck(), t.permissionAskAction())
	lp.SetPendingExecutor(pendingExecutor)
	registry.RegisterPendingCoordinator(tool.NewPendingCoordinator(pendingExecutor))

	if turn := childStore.BeginTurn(); turn == 0 {
		return finish(taskResult{index: index, err: fmt.Errorf("subagent: child session is not active")})
	}
	result, err := lp.Run(ctx, td.Prompt)
	if result == "Tool denied by user." {
		return finish(taskResult{index: index, result: result, denied: true})
	}
	if err != nil {
		return finish(taskResult{index: index, err: err})
	}
	return finish(taskResult{index: index, result: result})
}

func (t *taskTool) buildRegistry(at subagent.AgentType, scope parentMutationScope, procMgr *process.Manager) *tool.Registry {
	reg := tool.NewRegistry()
	for _, name := range at.Tools {
		if name == "task" {
			continue
		}
		if tt := t.newChildTool(name, at, scope, procMgr); tt != nil {
			reg.Register(tt)
		}
	}
	return reg
}

type parentMutationScope struct {
	store         *snapshot.Store
	turn          int
	tracker       *tool.FileTracker
	workspaceRoot string
}

func (s parentMutationScope) snapshotStore() tool.SnapshotStore {
	return parentTurnSnapshotStore{store: s.store, turn: s.turn}
}

type parentTurnSnapshotStore struct {
	store *snapshot.Store
	turn  int
}

func (s parentTurnSnapshotStore) Snapshot(_ int, absPath string) error {
	return s.store.Snapshot(s.turn, absPath)
}

func (s parentTurnSnapshotStore) CurrentTurn() int {
	return s.turn
}

func (s parentTurnSnapshotStore) SnapshotResolved(_ int, originalPath, canonicalPath string) error {
	return s.store.SnapshotResolved(s.turn, originalPath, canonicalPath)
}

func (s parentTurnSnapshotStore) SnapshotResolvedEntry(_ int, originalPath, canonicalPath string) (string, bool, error) {
	return s.store.SnapshotResolvedEntry(s.turn, originalPath, canonicalPath)
}

func (s parentTurnSnapshotStore) DiscardSnapshotEntry(_ int, entryID string) error {
	return s.store.DiscardSnapshotEntry(s.turn, entryID)
}

func (s parentTurnSnapshotStore) RetainSnapshotEntry(_ int, entryID string) {
	s.store.RetainSnapshotEntry(s.turn, entryID)
}

func (t *taskTool) newChildTool(name string, at subagent.AgentType, scope parentMutationScope, procMgr *process.Manager) tool.Tool {
	check := t.permissionCheck()
	ask := t.permissionAsk()
	root := scope.workspaceRoot
	switch name {
	case "read_file":
		return tool.WrapWithPermissionAtRoot(tool.NewReadFileAtRoot(t.toolsConfig, scope.tracker, root), root, check, ask)
	case "write_file":
		return tool.WrapWithPermissionAtRoot(tool.NewWriteFileWithSnapshotAtRoot(scope.snapshotStore(), scope.tracker, t.toolsConfig, root), root, check, ask)
	case "edit_file":
		return tool.WrapWithPermissionAtRoot(tool.NewEditFileWithSnapshotAtRoot(scope.snapshotStore(), scope.tracker, t.toolsConfig, root), root, check, ask)
	case "execute_pending":
		return tool.WrapWithPermission(tool.ExecutePending{}, check, ask)
	case "run_command":
		rc := tool.NewRunCommandAtRoot(t.toolsConfig, t.homeDir, root, procMgr)
		if isReadOnlyType(at) {
			return tool.WrapWithPermission(tool.NewReadOnlyRunCommand(rc), check, ask)
		}
		return tool.WrapWithPermission(rc, check, ask)
	case "process":
		if procMgr == nil {
			return nil
		}
		return tool.WrapWithPermission(tool.NewProcessTool(procMgr), check, ask)
	case "sleep":
		return tool.WrapWithPermission(tool.Sleep{}, check, ask)
	case "save_memory":
		if t.memoryStore == nil {
			return nil
		}
		return tool.WrapWithPermission(tool.NewSaveMemory(t.memoryStore, t.memoriesDir), check, ask)
	case "search_memory":
		if t.memoryStore == nil {
			return nil
		}
		return tool.WrapWithPermission(tool.NewSearchMemory(t.memoryStore, t.projectID), check, ask)
	case "search_history":
		if t.memoryStore == nil {
			return nil
		}
		return tool.WrapWithPermission(tool.NewSearchHistory(t.memoryStore, t.projectID), check, ask)
	case "diagnostics":
		if t.lspManager == nil {
			return nil
		}
		return tool.NewLSPDiagnostics(lsp.NewClient(t.lspManager), &snapshotDiagAdapter{store: t.parentStore})
	case "workspace_symbol":
		if t.lspManager == nil {
			return nil
		}
		return tool.NewWorkspaceSymbol(lsp.NewClient(t.lspManager))
	default:
		return nil
	}
}

func (t *taskTool) newChildProcessManager(sessionID string, addSignal func(loop.PendingSignal)) *process.Manager {
	if t.procMgr == nil {
		return nil
	}
	mgr := process.NewManagerAtRoot(t.toolsConfig.MaxBackgroundProcesses, cmdoutput.Options{
		HomeDir:      t.homeDir,
		SpillPrefix:  "proc_output_",
		MaxBytes:     t.toolsConfig.MaxOutputBytes,
		MaxLineChars: t.toolsConfig.ReadLineMaxChars,
	}, t.workspaceRoot)
	mgr.SetSessionProvider(func() string { return sessionID })
	mgr.SetExitHandler(func(event process.ExitEvent) {
		if addSignal == nil {
			return
		}
		if event.SessionID != "" && event.SessionID != sessionID {
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
		addSignal(loop.PendingSignal{
			Payload: backgroundTerminalPayload(event, output),
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
	})
	return mgr
}

func (t *taskTool) permissionCheck() tool.CheckFunc {
	if t.check != nil {
		return t.check
	}
	return func(string, string) permission.Decision {
		return permission.DecisionDeny
	}
}

func (t *taskTool) permissionAsk() tool.AskFunc {
	if t.ask != nil {
		return t.ask
	}
	return func(context.Context, permission.Request) permission.ResponseAction {
		return permission.ResponseDeny
	}
}

func (t *taskTool) permissionAskAction() tool.AskActionFunc {
	if t.askAction != nil {
		return t.askAction
	}
	return func(context.Context, permission.Request) permission.ResponseAction {
		return permission.ResponseDeny
	}
}

func isReadOnlyType(at subagent.AgentType) bool {
	for _, name := range at.Tools {
		if name == "write_file" || name == "edit_file" {
			return false
		}
	}
	return true
}

func (t *taskTool) resolveClient() (*provider.Client, coremodel.ModelRef, error) {
	t.mu.Lock()
	providerName := t.providerName
	modelID := t.model
	subModel := t.subModel
	modelCatalog := t.modelCatalog
	t.mu.Unlock()

	ref := coremodel.ModelRef{Provider: providerName, Model: modelID}
	if subModel != "" {
		parsed, err := coremodel.Parse(subModel)
		if err != nil {
			return nil, coremodel.ModelRef{}, err
		}
		ref = parsed
	}

	client, _, err := newProviderClient(modelCatalog, ref)
	if err != nil {
		return nil, coremodel.ModelRef{}, err
	}
	return client, ref, nil
}

func (t *taskTool) forwardEvents(ch <-chan loop.Event, taskIndex int, sessionID, toolCallID string) {
	for ev := range ch {
		if t.taggedEvents != nil {
			t.taggedEvents <- TaggedLoopEvent{
				SessionID:  sessionID,
				TaskIndex:  taskIndex,
				ToolCallID: toolCallID,
				Event:      ev,
			}
		}
	}
}

func (t *taskTool) updateParentState(providerName, model string, cancelParent func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.providerName = providerName
	t.model = model
	t.cancelParent = cancelParent
}

func (t *taskTool) setCatalog(catalog *catalog.Catalog) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.modelCatalog = catalog
}

func (t *taskTool) setSubModel(subModel string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.subModel = subModel
}
