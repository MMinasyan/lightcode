package agent

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestWailsModelSelectorUsesPrefixRefContract(t *testing.T) {
	appPath := filepath.Join("..", "..", "frontend", "src", "App.svelte")
	appBytes, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatalf("read App.svelte: %v", err)
	}
	app := string(appBytes)
	if !strings.Contains(app, "<ModelSelector currentRef={modelRef}") {
		t.Fatalf("App.svelte must pass the provider-prefixed current model ref to ModelSelector")
	}
	if strings.Contains(app, "currentProvider={provider}") || strings.Contains(app, "currentModel={model}") {
		t.Fatalf("App.svelte still passes old provider/model props to ModelSelector")
	}

	bindingPath := filepath.Join("..", "..", "frontend", "wailsjs", "go", "main", "App.d.ts")
	bindingBytes, err := os.ReadFile(bindingPath)
	if err != nil {
		t.Fatalf("read App.d.ts: %v", err)
	}
	binding := string(bindingBytes)
	if !strings.Contains(binding, "export function SwitchModel(arg1:string):Promise<void>;") {
		t.Fatalf("Wails binding must expose SwitchModel(ref string)")
	}
	if strings.Contains(binding, "SwitchModel(arg1:string,arg2:string)") {
		t.Fatalf("Wails binding still exposes old SwitchModel(provider, model) signature")
	}
	if !strings.Contains(binding, "ModelList():Promise<Array<agent.ModelListEntry>>") {
		t.Fatalf("Wails binding must expose enriched []ModelListEntry")
	}
	if !strings.Contains(binding, "CurrentWarnings():Promise<Array<agent.PromptWarning>>") {
		t.Fatalf("Wails binding must expose CurrentWarnings for warning state hydration")
	}
	if !strings.Contains(binding, "ApplyTurnAction(arg1:number,arg2:string,arg3:boolean):Promise<agent.TurnActionResult>;") {
		t.Fatalf("Wails binding must expose ApplyTurnAction for adapter-neutral turn actions")
	}
	if !strings.Contains(binding, "Submit(arg1:string):Promise<agent.SubmitResult>;") {
		t.Fatalf("Wails binding must expose Submit as the single backend input entry point")
	}
	if !strings.Contains(binding, "QueueSnapshot():Promise<agent.QueueState>;") {
		t.Fatalf("Wails binding must expose QueueSnapshot for queue hydration")
	}

	modelsPath := filepath.Join("..", "..", "frontend", "wailsjs", "go", "models.ts")
	modelsBytes, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatalf("read models.ts: %v", err)
	}
	models := string(modelsBytes)
	if !strings.Contains(models, "export class ModelListEntry") {
		t.Fatalf("models.ts must include generated ModelListEntry type")
	}
	if !strings.Contains(models, "ref: string") || !strings.Contains(models, "displayName: string") || !strings.Contains(models, "contextWindow: number") {
		t.Fatalf("generated model types must include enriched ref/displayName/contextWindow fields")
	}
	if !strings.Contains(models, "export class PromptWarning") || !strings.Contains(models, "kind: string") || !strings.Contains(models, "message: string") {
		t.Fatalf("generated models must include PromptWarning type")
	}
	if strings.Contains(models, "export class ProviderModels") {
		t.Fatalf("models.ts still contains old ProviderModels type")
	}
}

func TestAdaptersUseSharedTurnActionContracts(t *testing.T) {
	appPath := filepath.Join("..", "..", "frontend", "src", "App.svelte")
	appBytes, err := os.ReadFile(appPath)
	if err != nil {
		t.Fatalf("read App.svelte: %v", err)
	}
	app := string(appBytes)
	if !strings.Contains(app, "ApplyTurnAction(turn, 'revert_code', false)") ||
		!strings.Contains(app, "ApplyTurnAction(turn, 'revert_history', !!alsoRevertCode)") ||
		!strings.Contains(app, "ApplyTurnAction(turn, 'fork', !!alsoRevertCode)") {
		t.Fatalf("App.svelte must route revert/fork actions through ApplyTurnAction")
	}
	if strings.Contains(app, "await RevertCode(") || strings.Contains(app, "await RevertHistory(") || strings.Contains(app, "await ForkSession(") {
		t.Fatalf("App.svelte still calls low-level turn actions directly")
	}
	if !strings.Contains(app, "await Submit(content)") {
		t.Fatalf("App.svelte must submit input through the backend Submit entry point")
	}
	if !strings.Contains(app, "EventsOn('queue_changed'") {
		t.Fatalf("App.svelte must render the queue from the backend queue_changed event")
	}
	if strings.Contains(app, "SendQueuedMessages(") || strings.Contains(app, "SendPrompt(") {
		t.Fatalf("App.svelte must not use the removed SendPrompt/SendQueuedMessages APIs")
	}
	if strings.Contains(app, "messageQueue = [...messageQueue") {
		t.Fatalf("App.svelte must not push to a local queue; the backend owns queue state")
	}
	if strings.Contains(app, "nextSessionId !== sessionId) messageQueue = [];") {
		t.Fatalf("App.svelte must not clear the queue locally; the backend clears it and emits queue_changed")
	}
	if strings.Contains(app, "AppendUserMessage(") {
		t.Fatalf("App.svelte must not use the removed AppendUserMessage API")
	}
	handleCompact, ok := extractSvelteFunctionBody(app, "async function handleCompact(")
	if !ok {
		t.Fatal("handleCompact not found in App.svelte")
	}
	if strings.Contains(handleCompact, "busy = false") {
		t.Fatalf("handleCompact must not locally clear busy; a queued post-compaction turn_start can arrive before CompactNow returns")
	}
	if !strings.Contains(app, "busy={busy || compacting}") {
		t.Fatalf("InputArea must treat compaction as busy without mutating the backend-driven busy flag")
	}

	messagePath := filepath.Join("..", "..", "frontend", "src", "components", "Message.svelte")
	messageBytes, err := os.ReadFile(messagePath)
	if err != nil {
		t.Fatalf("read Message.svelte: %v", err)
	}
	if strings.Contains(string(messageBytes), "turn - 1") || strings.Contains(string(messageBytes), "turn + 1") {
		t.Fatalf("Message.svelte must pass the clicked turn without local arithmetic")
	}

	menuPath := filepath.Join("..", "cli", "menu.go")
	menuBytes, err := os.ReadFile(menuPath)
	if err != nil {
		t.Fatalf("read menu.go: %v", err)
	}
	menu := string(menuBytes)
	if !strings.Contains(menu, "ApplyTurnAction(turn, agent.TurnActionRevertCode") ||
		!strings.Contains(menu, "ApplyTurnAction(turn, agent.TurnActionRevertHistory") ||
		!strings.Contains(menu, "ApplyTurnAction(turn, agent.TurnActionFork") {
		t.Fatalf("CLI revert menu must route through ApplyTurnAction")
	}
	if strings.Contains(menu, "RevertCode(turn - 1)") || strings.Contains(menu, "ForkSession(turn") {
		t.Fatalf("CLI revert menu still performs adapter-local turn action logic")
	}
}

func TestProjectSwitchHandlesStoreCloseErrors(t *testing.T) {
	app := mustReadContractFile(t, filepath.Join("..", "..", "app.go"))
	if strings.Contains(app, "_, _ = a.svc.Store().Close()") {
		t.Fatalf("ProjectSwitch must not ignore Store().Close errors")
	}
	if strings.Contains(app, "a.svc.Store().Close()") {
		t.Fatalf("ProjectSwitch must route session close through Agent.CloseForProjectSwitch")
	}
	if !strings.Contains(app, "a.svc.CloseForProjectSwitch()") {
		t.Fatalf("ProjectSwitch must use Agent.CloseForProjectSwitch")
	}
	if !strings.Contains(app, "close current session") {
		t.Fatalf("ProjectSwitch must return close-session errors with context")
	}

	cli := mustReadContractFile(t, filepath.Join("..", "cli", "cli.go"))
	if strings.Contains(cli, "_, _ = c.agent.Store().Close()") {
		t.Fatalf("CLI projectSwitch must not ignore Store().Close errors")
	}
	if strings.Contains(cli, "c.agent.Store().Close()") {
		t.Fatalf("CLI projectSwitch must route session close through Agent.CloseForProjectSwitch")
	}
	if !strings.Contains(cli, "c.agent.CloseForProjectSwitch()") {
		t.Fatalf("CLI projectSwitch must use Agent.CloseForProjectSwitch")
	}
	if !strings.Contains(cli, "close current session") {
		t.Fatalf("CLI projectSwitch must report close-session errors with context")
	}
}

func TestWailsBindingsCoverExportedAppMethods(t *testing.T) {
	app := mustReadContractFile(t, filepath.Join("..", "..", "app.go"))
	binding := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "wailsjs", "go", "main", "App.d.ts"))

	methods := extractUniqueMatches(app, `func \(a \*App\) ([A-Z]\w*)\(`)
	exports := stringSet(extractUniqueMatches(binding, `export function (\w+)\(`))
	if len(methods) == 0 {
		t.Fatal("no exported App methods found in app.go")
	}
	var missing []string
	for _, name := range methods {
		if !exports[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("App.d.ts missing Wails bindings for exported App methods: %s", strings.Join(missing, ", "))
	}
}

func TestFrontendRuntimeConfigTabExcludesMasterTogglesAndPermissions(t *testing.T) {
	settings := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "components", "Settings.svelte"))
	for _, want := range []string{"GetRuntimeConfig", "SetRuntimeConfig", "active === 'agent'", "archive_after_days", "delete_after_archive_days", "threshold_pct", "max_concurrent", "max_output_bytes", "max_background_processes"} {
		if !strings.Contains(settings, want) {
			t.Fatalf("Settings.svelte runtime config tab missing %q", want)
		}
	}
	for _, excluded := range []string{"auto_archive", "auto-archive", "compaction.enabled", "master enable", "permissions"} {
		if strings.Contains(strings.ToLower(settings), excluded) {
			t.Fatalf("Settings.svelte runtime config tab must not expose %q", excluded)
		}
	}
}

func TestFrontendNoActiveModelSendGate(t *testing.T) {
	app := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "App.svelte"))
	input := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "components", "InputArea.svelte"))
	if !strings.Contains(app, "hasActiveModel={!!modelRef}") {
		t.Fatalf("App.svelte must pass active-model state into InputArea")
	}
	if !strings.Contains(app, "Connect a provider or pick a model before sending.") {
		t.Fatalf("handleSubmit must gate sends when no active model is available")
	}
	if !strings.Contains(input, "Connect a provider or pick a model") || !strings.Contains(input, "!hasActiveModel") {
		t.Fatalf("InputArea must surface and disable the no-active-model send state")
	}
}

func TestWailsLifecycleSurfaceContract(t *testing.T) {
	binding := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "wailsjs", "go", "main", "App.d.ts"))
	required := []string{
		"export function Submit(arg1:string):Promise<agent.SubmitResult>;",
		"export function QueueSnapshot():Promise<agent.QueueState>;",
		"export function CompactNow():Promise<void>;",
		"export function ProjectSwitch(arg1:string):Promise<void>;",
		"export function SessionNew():Promise<void>;",
		"export function SessionSwitch(arg1:string):Promise<void>;",
		"export function SessionMessagesFor(arg1:string):Promise<Array<agent.DisplayMessage>>;",
		"export function ApplyTurnAction(arg1:number,arg2:string,arg3:boolean):Promise<agent.TurnActionResult>;",
		"export function TokenUsage():Promise<agent.TokenReport>;",
		"export function CurrentWarnings():Promise<Array<agent.PromptWarning>>;",
		"export function SetDefaultModel(arg1:string):Promise<void>;",
		"export function GetRuntimeConfig():Promise<agent.RuntimeConfigSettings>;",
		"export function SetRuntimeConfig(arg1:agent.RuntimeConfigSettings):Promise<void>;",
	}
	for _, want := range required {
		if !strings.Contains(binding, want) {
			t.Fatalf("Wails binding missing lifecycle surface: %s", want)
		}
	}

	app := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "App.svelte"))
	for _, eventName := range []string{
		"queue_changed",
		"turn_start",
		"turn_end",
		"user_message",
		"system_signal",
		"compaction_start",
		"compaction_end",
		"usage",
		"warnings",
		"subagent_background_process_complete",
	} {
		if !strings.Contains(app, "EventsOn('"+eventName+"'") {
			t.Fatalf("App.svelte must listen for backend lifecycle event %q", eventName)
		}
	}
}

func TestWailsDefersActiveCompactionSessionRefreshUntilTurnEnd(t *testing.T) {
	app := mustReadContractFile(t, filepath.Join("..", "..", "app.go"))
	compactionEnd := extractSwitchCase(t, app, "case agent.EventCompactionEnd:")
	if !strings.Contains(compactionEnd, `EventsEmit(a.ctx, "compaction_end"`) {
		t.Fatalf("EventCompactionEnd must still emit compaction_end; case:\n%s", compactionEnd)
	}
	if !strings.Contains(compactionEnd, "if ev.RefreshSession") || !strings.Contains(compactionEnd, "a.emitSessionChanged()") {
		t.Fatalf("EventCompactionEnd must refresh history only when backend marks it safe; case:\n%s", compactionEnd)
	}
	turnEnd := extractSwitchCase(t, app, "case agent.EventTurnEnd:")
	if !strings.Contains(turnEnd, `EventsEmit(a.ctx, "turn_end"`) {
		t.Fatalf("EventTurnEnd must still emit turn_end; case:\n%s", turnEnd)
	}
	if !strings.Contains(turnEnd, "if ev.RefreshSession") || !strings.Contains(turnEnd, "a.emitSessionChanged()") {
		t.Fatalf("EventTurnEnd must perform deferred compaction history refresh; case:\n%s", turnEnd)
	}
}

func TestFrontendUserAndSystemTranscriptEntriesArriveViaBackendEvents(t *testing.T) {
	app := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "App.svelte"))

	if !strings.Contains(app, "EventsOn('user_message',") {
		t.Fatalf("App.svelte must subscribe to the user_message event so transcript order matches backend ordering")
	}
	if !strings.Contains(app, "EventsOn('system_signal',") {
		t.Fatalf("App.svelte must subscribe to the system_signal event so transcript order matches backend ordering")
	}

	handleSubmit, ok := extractSvelteFunctionBody(app, "async function handleSubmit(")
	if !ok {
		t.Fatal("handleSubmit not found in App.svelte")
	}
	if strings.Contains(handleSubmit, "type:'user'") || strings.Contains(handleSubmit, "type: 'user'") {
		t.Fatalf("handleSubmit must not push a user message into messages locally; ordering is owned by the backend")
	}

	if strings.Contains(app, "function flushQueue(") {
		t.Fatalf("App.svelte must not keep a local flushQueue; queue draining is backend-owned")
	}

	if strings.Contains(app, "type:'system', content:'interrupted'") || strings.Contains(app, "type: 'system', content: 'interrupted'") {
		t.Fatalf("App.svelte must not synthesize an 'interrupted' transcript entry from turn_end; system_signal carries that text now")
	}
}

func TestWailsSubagentTaskLinksAreOrderIndependent(t *testing.T) {
	app := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "App.svelte"))
	for _, want := range []string{
		"pendingSubagentSessionLinks",
		"rememberSubagentLink(pendingSubagentSessionLinks, data.taskToolCallId",
		"takePendingSubagentLinks(pendingSubagentSessionLinks, data.id",
		"subagentLinksFromMetadata(metadata)",
	} {
		if !strings.Contains(app, want) {
			t.Fatalf("App.svelte missing subagent task-link ordering guard %q", want)
		}
	}

	helpers := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "lib", "subagentLinks.js"))
	for _, want := range []string{"metadata.subagent_session_ids", "metadata.subagentSessionIds"} {
		if !strings.Contains(helpers, want) {
			t.Fatalf("subagentLinks.js missing metadata recovery path %q", want)
		}
	}
}

func TestFrontendTaskSubagentLinksRemainClickableAfterCompletion(t *testing.T) {
	toolCall := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "components", "ToolCall.svelte"))
	taskStart := strings.Index(toolCall, "{:else if name === 'task'}")
	if taskStart < 0 {
		t.Fatal("ToolCall.svelte task branch not found")
	}
	taskEnd := strings.Index(toolCall[taskStart:], "{:else if name === 'save_memory'}")
	if taskEnd < 0 {
		t.Fatal("ToolCall.svelte task branch end not found")
	}
	taskBlock := toolCall[taskStart : taskStart+taskEnd]

	for _, want := range []string{"subtask-row", "subagentSessionIds", "openSubagentTranscript"} {
		if !strings.Contains(taskBlock, want) {
			t.Fatalf("task branch missing persisted child-session affordance path %q", want)
		}
	}
	for _, want := range []string{"SessionMessagesFor", "hydrateSubagentViewer"} {
		if !strings.Contains(toolCall, want) {
			t.Fatalf("ToolCall.svelte missing persisted child-session hydration path %q", want)
		}
	}
	if strings.Contains(taskBlock, "openSubagentViewer(t.subagent_type") {
		t.Fatalf("task subagent rows must hydrate persisted child history, not open an empty live-only viewer")
	}
	if strings.Contains(taskBlock, "{#if !done}") {
		t.Fatalf("task subagent rows must not be gated by !done; completed and reloaded rows still need backend-provided child links")
	}
	if !strings.Contains(taskBlock, "{#if done}") || !strings.Contains(taskBlock, "toggleOrOpenOutput('task results')") {
		t.Fatalf("task branch must still gate task output on done while keeping subtask rows visible")
	}
}

// extractSvelteFunctionBody returns the brace-balanced body of the first JS
// function in source whose definition begins with prefix.
func extractSvelteFunctionBody(source, prefix string) (string, bool) {
	idx := strings.Index(source, prefix)
	if idx < 0 {
		return "", false
	}
	rest := source[idx:]
	open := strings.Index(rest, "{")
	if open < 0 {
		return "", false
	}
	depth := 1
	for i := open + 1; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[open+1 : i], true
			}
		}
	}
	return "", false
}

func TestBackendEmittedEventsAreListenedToByFrontend(t *testing.T) {
	emitted := collectContractMatches(t, filepath.Join("..", ".."), ".go", `EventsEmit\([^,]+,\s*["']([^"']+)["']`)
	listened := stringSet(collectContractMatches(t, filepath.Join("..", "..", "frontend", "src"), ".svelte", `EventsOn\(["']([^"']+)["']`))
	if len(emitted) == 0 {
		t.Fatal("no runtime.EventsEmit calls found")
	}
	var missing []string
	for _, name := range emitted {
		if !listened[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("backend events emitted without frontend EventsOn listener: %s", strings.Join(missing, ", "))
	}
}

func TestFrontendEventListenersHaveBackendEmitters(t *testing.T) {
	listened := collectContractMatches(t, filepath.Join("..", "..", "frontend", "src"), ".svelte", `EventsOn\(["']([^"']+)["']`)
	emitted := stringSet(collectContractMatches(t, filepath.Join("..", ".."), ".go", `EventsEmit\([^,]+,\s*["']([^"']+)["']`))
	if len(listened) == 0 {
		t.Fatal("no frontend EventsOn calls found")
	}
	var missing []string
	for _, name := range listened {
		if !emitted[name] {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("frontend EventsOn listeners without backend EventsEmit source: %s", strings.Join(missing, ", "))
	}
}

func mustReadContractFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func collectContractMatches(t *testing.T, root, ext, pattern string) []string {
	t.Helper()
	seen := map[string]bool{}
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "build" || d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ext {
			return nil
		}
		for _, match := range extractUniqueMatches(mustReadContractFile(t, path), pattern) {
			seen[match] = true
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return sortedKeys(seen)
}

func extractUniqueMatches(content, pattern string) []string {
	re := regexp.MustCompile(pattern)
	seen := map[string]bool{}
	for _, match := range re.FindAllStringSubmatch(content, -1) {
		if len(match) > 1 {
			seen[match[1]] = true
		}
	}
	return sortedKeys(seen)
}

func extractSwitchCase(t *testing.T, content, marker string) string {
	t.Helper()
	start := strings.Index(content, marker)
	if start < 0 {
		t.Fatalf("switch case %q not found", marker)
	}
	rest := content[start+len(marker):]
	next := strings.Index(rest, "\n\tcase ")
	if next < 0 {
		next = strings.Index(rest, "\n\tdefault:")
	}
	if next < 0 {
		return rest
	}
	return rest[:next]
}

func stringSet(values []string) map[string]bool {
	set := make(map[string]bool, len(values))
	for _, value := range values {
		set[value] = true
	}
	return set
}

func sortedKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
