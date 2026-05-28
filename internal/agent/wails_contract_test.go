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
	if !strings.Contains(app, "close current session") {
		t.Fatalf("ProjectSwitch must return close-session errors with context")
	}

	cli := mustReadContractFile(t, filepath.Join("..", "cli", "cli.go"))
	if strings.Contains(cli, "_, _ = c.agent.Store().Close()") {
		t.Fatalf("CLI projectSwitch must not ignore Store().Close errors")
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
