package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/MMinasyan/lightcode/internal/loop"
	"github.com/MMinasyan/lightcode/internal/provider"
	"github.com/MMinasyan/lightcode/internal/subagent"
	"github.com/MMinasyan/lightcode/internal/tool"
)

type taskDef struct {
	Prompt       string `json:"prompt"`
	SubagentType string `json:"subagent_type"`
}

type taskResult struct {
	index  int
	result string
	denied bool
	err    error
}

type TaggedLoopEvent struct {
	SessionID  string
	TaskIndex  int
	ToolCallID string
	Event      loop.Event
}

type taskTool struct {
	loader        *subagent.Loader
	parentStore   tool.SnapshotStore
	baseRegistry  *tool.Registry
	maxConcurrent int
	taggedEvents  chan<- TaggedLoopEvent

	mu           sync.Mutex
	providerName string
	model        string
	baseURL      string
	apiKey       string
	cancelParent func()

	subProviderName string
	subModel        string
	subBaseURL      string
	subAPIKey       string
}

type taskToolConfig struct {
	Loader        *subagent.Loader
	ParentStore   tool.SnapshotStore
	BaseRegistry  *tool.Registry
	MaxConcurrent int
	TaggedEvents  chan<- TaggedLoopEvent

	ProviderName string
	Model        string
	BaseURL      string
	APIKey       string

	SubProviderName string
	SubModel        string
	SubBaseURL      string
	SubAPIKey       string
}

func newTaskTool(cfg taskToolConfig) *taskTool {
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = 4
	}
	return &taskTool{
		loader:          cfg.Loader,
		parentStore:     cfg.ParentStore,
		baseRegistry:    cfg.BaseRegistry,
		maxConcurrent:   cfg.MaxConcurrent,
		taggedEvents:    cfg.TaggedEvents,
		providerName:    cfg.ProviderName,
		model:           cfg.Model,
		baseURL:         cfg.BaseURL,
		apiKey:          cfg.APIKey,
		subProviderName: cfg.SubProviderName,
		subModel:        cfg.SubModel,
		subBaseURL:      cfg.SubBaseURL,
		subAPIKey:       cfg.SubAPIKey,
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

	sem := make(chan struct{}, t.maxConcurrent)
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
	allDenied := true
	for i, r := range results {
		if i > 0 {
			b.WriteString("\n\n")
		}
		fmt.Fprintf(&b, "## Task %d (%s)\n\n", i+1, tasks[i].SubagentType)
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

func (t *taskTool) runSubagent(ctx context.Context, index int, td taskDef, parentToolCallID string) taskResult {
	at, err := t.loader.Load(td.SubagentType)
	if err != nil {
		return taskResult{index: index, err: fmt.Errorf("unknown subagent type %q: %w", td.SubagentType, err)}
	}

	registry := t.buildRegistry(at)
	client := t.resolveClient()
	sessionID := genSessionID()

	var events chan loop.Event
	if t.taggedEvents != nil {
		events = make(chan loop.Event, 128)
		defer close(events)
		go t.forwardEvents(events, index, sessionID, parentToolCallID)
	}

	lp := loop.New(client, registry, at.Prompt)
	if events != nil {
		lp.SetEvents(events)
	}

	result, err := subagent.Run(ctx, lp, td.Prompt)
	if err != nil {
		if result == "Tool denied by user." {
			return taskResult{index: index, result: result, denied: true}
		}
		return taskResult{index: index, err: err}
	}
	return taskResult{index: index, result: result}
}

func genSessionID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (t *taskTool) buildRegistry(at subagent.AgentType) *tool.Registry {
	reg := tool.NewRegistry()
	for _, name := range at.Tools {
		if name == "task" {
			continue
		}
		if name == "run_command" && isReadOnlyType(at) {
			reg.Register(tool.NewReadOnlyRunCommand())
			continue
		}
		if tt, ok := t.baseRegistry.Get(name); ok {
			reg.Register(tt)
		}
	}
	return reg
}

func isReadOnlyType(at subagent.AgentType) bool {
	for _, name := range at.Tools {
		if name == "write_file" || name == "edit_file" {
			return false
		}
	}
	return true
}

func (t *taskTool) resolveClient() *provider.Client {
	t.mu.Lock()
	baseURL := t.baseURL
	apiKey := t.apiKey
	model := t.model
	t.mu.Unlock()

	if t.subBaseURL != "" {
		baseURL = t.subBaseURL
	}
	if t.subAPIKey != "" {
		apiKey = t.subAPIKey
	}
	if t.subModel != "" {
		model = t.subModel
	}

	return provider.New(baseURL, apiKey, model)
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

func (t *taskTool) updateParentState(providerName, model, baseURL, apiKey string, cancelParent func()) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.providerName = providerName
	t.model = model
	t.baseURL = baseURL
	t.apiKey = apiKey
	t.cancelParent = cancelParent
}
