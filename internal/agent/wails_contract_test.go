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
	if !strings.Contains(binding, "RevertCode(arg1:number):Promise<snapshot.RevertResult>;") {
		t.Fatalf("Wails binding must expose RevertCode result with restored/skipped files")
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
	if !strings.Contains(models, "export class RevertResult") || !strings.Contains(models, "restored?: string[]") || !strings.Contains(models, "skipped?: SkippedRevert[]") {
		t.Fatalf("generated models must include snapshot.RevertResult with restored/skipped fields")
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
	if !strings.Contains(menu, "ApplyTurnActionForSession(sessionID, turn, agent.TurnActionRevertCode") ||
		!strings.Contains(menu, "ApplyTurnActionForSession(sessionID, turn, agent.TurnActionRevertHistory") ||
		!strings.Contains(menu, "ApplyTurnActionForSession(sessionID, turn, agent.TurnActionFork") {
		t.Fatalf("CLI revert menu must route through ApplyTurnActionForSession")
	}
	if strings.Contains(menu, "RevertCode(turn - 1)") || strings.Contains(menu, "ForkSession(turn") {
		t.Fatalf("CLI revert menu still performs adapter-local turn action logic")
	}
}

// TestTurnActionAppliesDestinationStateThroughOrderedBoundary proves the Wails
// turn-action path (fork / history revert) applies the destination's complete
// state and the code-revert skip notice through one ordered delivery frame, not an
// out-of-band read that could race live frames or lose the notice. The backend
// commits routing and appends a turn_action boundary (never a legacy
// session_changed or navigation frame), and the frontend's turn_action handler
// applies the snapshot before the skip notice so the two land atomically; the
// handlers apply nothing out of band. A reconciled postcommit partial error
// resolves the direct method: the wrapper records synchronously whether the
// boundary callback emitted and treats the error as frame-owned when it did.
func TestTurnActionAppliesDestinationStateThroughOrderedBoundary(t *testing.T) {
	app := mustReadContractFile(t, filepath.Join("..", "..", "app.go"))
	body, ok := extractSvelteFunctionBody(app, "func (a *App) applyTurnActionWithOwnedBoundary(")
	if !ok {
		t.Fatal("applyTurnActionWithOwnedBoundary not found in app.go")
	}
	if !strings.Contains(body, "ApplyTurnActionForSessionWithBoundary") {
		t.Fatal("the turn-action wrapper must publish through the shared ApplyTurnActionForSessionWithBoundary route")
	}
	if !strings.Contains(body, "emitted = true") {
		t.Fatal("the turn-action wrapper must record synchronously whether the owner boundary callback emitted")
	}
	if !strings.Contains(body, "err != nil && emitted") {
		t.Fatal("the turn-action wrapper must resolve a postcommit error as frame-owned (the boundary owns the error through its warning)")
	}
	if strings.Contains(body, "emitSessionChanged") || strings.Contains(body, "emitNavigationBoundary") {
		t.Fatal("ApplyTurnAction must not emit a legacy session_changed or navigation frame")
	}
	applyBody, ok := extractSvelteFunctionBody(app, "func (a *App) ApplyTurnAction(")
	if !ok {
		t.Fatal("ApplyTurnAction not found in app.go")
	}
	if !strings.Contains(applyBody, "applyTurnActionWithOwnedBoundary(") {
		t.Fatal("ApplyTurnAction must route through the frame-owning wrapper")
	}

	svelte := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "App.svelte"))
	handler, ok := extractSvelteFunctionBody(svelte, "EventsOn('turn_action'")
	if !ok {
		t.Fatal("turn_action handler not found in App.svelte")
	}
	snap := strings.Index(handler, "applySnapshot(")
	notice := strings.Index(handler, "appendRevertSkipNotice(")
	if snap < 0 || notice < 0 {
		t.Fatal("turn_action handler must apply the snapshot and append the skip notice")
	}
	if snap > notice {
		t.Fatal("turn_action handler must apply the snapshot before the skip notice")
	}
	// A fork's failed-code-revert warning rides the same frame: it is appended
	// after the snapshot and the skip notice, or the snapshot replace clobbers
	// it. The warning arrives only on the frame; the handler reads nothing off
	// the returned value.
	warn := strings.Index(handler, "showError(data.warning)")
	if warn < 0 {
		t.Fatal("turn_action handler must append a fork's failed-code-revert warning")
	}
	if warn < notice {
		t.Fatal("turn_action handler must append the warning after the skip notice")
	}
	for _, fn := range []string{"async function handleFork(", "async function handleRevertHistory(", "async function handleRevertCode("} {
		fnBody, ok := extractSvelteFunctionBody(svelte, fn)
		if !ok {
			t.Fatalf("%s not found in App.svelte", fn)
		}
		if strings.Contains(fnBody, "applySnapshot(") || strings.Contains(fnBody, "appendRevertSkipNotice(") {
			t.Fatalf("%s must not apply state or notice out of band; the ordered turn_action frame is authoritative", fn)
		}
		if strings.Contains(fnBody, "result.warning") || strings.Contains(fnBody, "result?.warning") {
			t.Fatalf("%s must not apply the warning out of band; the ordered turn_action frame is authoritative", fn)
		}
	}
}

// TestAdapterRevertOutcomeContract pins what each host renders when a revert
// fails midway. Only the terminal host renders the skipped set, on every error
// branch that follows the action call: ApplyTurnActionForSession returns the
// populated result alongside the error, and the CLI prints the skipped set
// before returning. A branch that returns before the action is invoked — the
// confirmation-read abort for the exiting-terminal case — has no result to
// render and is not in this scan's scope. A precommit desktop failure carries
// the enriched error text naming where the revert stopped: the binding layer
// attaches the return value only on success, and the frontend renders the
// enriched text from the rejection. A reconciled postcommit partial error no
// longer rejects on the desktop at all — the wrapper resolves it because the
// ordered turn_action frame owns the error through its warning — so no second
// renderer exists and the frame's warning is the single copy.
func TestAdapterRevertOutcomeContract(t *testing.T) {
	menu := mustReadContractFile(t, filepath.Join("..", "cli", "menu.go"))
	for _, action := range []string{`"code"`, `"history"`, `"fork"`} {
		body := extractSwitchCase(t, menu, "case "+action+":")
		// Anchor at the action call: an error branch that precedes it returns
		// before any action was attempted, so there is nothing to render.
		actionIdx := strings.Index(body, "ApplyTurnActionForSession(")
		if actionIdx < 0 {
			t.Fatalf("CLI revert menu %s case has no action call:\n%s", action, body)
		}
		errIdx := strings.Index(body[actionIdx:], "if err != nil {")
		if errIdx < 0 {
			t.Fatalf("CLI revert menu %s case has no post-action error branch:\n%s", action, body)
		}
		errIdx += actionIdx
		retIdx := strings.Index(body[errIdx:], "return")
		if retIdx < 0 {
			t.Fatalf("CLI revert menu %s post-action error branch has no return:\n%s", action, body)
		}
		errBranch := body[errIdx : errIdx+retIdx]
		if !strings.Contains(errBranch, "c.printRevertSkipped(result)") {
			t.Fatalf("CLI revert menu %s post-action error branch must render the skipped set before returning:\n%s", action, errBranch)
		}
	}

	app := mustReadContractFile(t, filepath.Join("..", "..", "app.go"))
	body, ok := extractSvelteFunctionBody(app, "func (a *App) ApplyTurnAction(")
	if !ok {
		t.Fatal("ApplyTurnAction not found in app.go")
	}
	if !strings.Contains(body, "return result, err") {
		t.Fatal("ApplyTurnAction must return the populated result alongside the error; the desktop binding attaches the return value only on success, so a failing call surfaces only the enriched error text naming where the revert stopped")
	}

	svelte := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "App.svelte"))
	for _, fn := range []string{"async function handleRevertCode(", "async function handleRevertHistory(", "async function handleFork("} {
		fnBody, ok := extractSvelteFunctionBody(svelte, fn)
		if !ok {
			t.Fatalf("%s not found in App.svelte", fn)
		}
		if !strings.Contains(fnBody, "showError(err)") {
			t.Fatalf("%s must render the enriched error text (naming where the revert stopped) via showError", fn)
		}
	}

	acp := mustReadContractFile(t, filepath.Join("..", "acp", "acp.go"))
	acpBody, ok := extractSvelteFunctionBody(acp, "func (r *Runner) handleTurnAction(")
	if !ok {
		t.Fatal("handleTurnAction not found in acp.go")
	}
	if !strings.Contains(acpBody, "r.respondError(req.ID, -32000, err.Error())") {
		t.Fatal("handleTurnAction must carry the enriched error string (naming where the revert stopped) in the protocol error response")
	}
}

// TestWailsModelSwitchAppendsOrderedPresentationItem proves a root-model switch
// updates the selector through an ordered, presentation-scoped item, not an
// out-of-band immediate update: SwitchModel appends a model item to the FIFO, the
// frontend applies it only when its root is the presented session, and the switch
// handler never sets the model itself — it only invokes the switch against the
// captured session/generation and closes the picker (or shows the error) when
// both still match.
func TestWailsModelSwitchAppendsOrderedPresentationItem(t *testing.T) {
	app := mustReadContractFile(t, filepath.Join("..", "..", "app.go"))
	body, ok := extractSvelteFunctionBody(app, "func (a *App) SwitchModel(")
	if !ok {
		t.Fatal("SwitchModel not found in app.go")
	}
	if !strings.Contains(body, `emitFrame("model"`) {
		t.Fatal("SwitchModel must append a model item to the ordered delivery FIFO")
	}

	svelte := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "App.svelte"))
	handler, ok := extractSvelteFunctionBody(svelte, "EventsOn('model'")
	if !ok {
		t.Fatal("model handler not found in App.svelte")
	}
	if !strings.Contains(handler, "data.rootId !== sessionId") {
		t.Fatal("model handler must apply only for the presentation-current root")
	}
	if !strings.Contains(handler, "modelRef =") {
		t.Fatal("model handler must update the selector from the ordered item")
	}

	switched, ok := extractSvelteFunctionBody(svelte, "async function handleModelSelect(")
	if !ok {
		t.Fatal("handleModelSelect not found in App.svelte")
	}
	if strings.Contains(switched, "modelRef =") {
		t.Fatal("handleModelSelect must not set the model out of band; the ordered item does")
	}
	if !strings.Contains(switched, "await SwitchModel(") {
		t.Fatal("handleModelSelect must invoke the switch through the backend entry point")
	}
	if !strings.Contains(switched, "presentationGeneration !== opGen") {
		t.Fatal("handleModelSelect must gate closing the picker and showing errors on the captured session and generation")
	}
}

// TestSnapshotCarriesModelAndProjectSwitchFetchesNone proves the hydration
// snapshot applies the destination's resolved model alongside the rest of the
// session classes, and that a project switch performs no out-of-band fetch of
// the destination's project name or model: the ordered navigation boundary
// carries both, which the snapshot applies. The project-name fetch is called
// exactly once, at mount, for the startup case where no session exists and no
// snapshot can answer; any second call site, under any name, is an out-of-band
// fetch.
func TestSnapshotCarriesModelAndProjectSwitchFetchesNone(t *testing.T) {
	svelte := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "App.svelte"))
	body, ok := extractSvelteFunctionBody(svelte, "function applySnapshot(")
	if !ok {
		t.Fatal("applySnapshot not found in App.svelte")
	}
	if !strings.Contains(body, "modelRef =") || !strings.Contains(body, "modelName =") {
		t.Fatal("applySnapshot must apply the destination session's resolved model to the selector")
	}

	if got := strings.Count(svelteCodeWithoutCommentLines(svelte), "ProjectName("); got != 1 {
		t.Fatalf("App.svelte calls ProjectName() %d times, want exactly 1 (the mount-time startup fetch; a second call site anywhere would fetch the destination project out of band)", got)
	}
}

func TestProjectSwitchDoesNotCloseOwnerSession(t *testing.T) {
	app := mustReadContractFile(t, filepath.Join("..", "..", "app.go"))
	if strings.Contains(app, "CloseForProjectSwitch") || strings.Contains(app, "close current session") {
		t.Fatalf("ProjectSwitch must not close the owner session")
	}
	if strings.Contains(app, "relaunchIn") || strings.Contains(app, "wailsRuntime.Quit") {
		t.Fatalf("ProjectSwitch must not relaunch the process or quit the adapter")
	}
	if !strings.Contains(app, "openOrCreateSession") {
		t.Fatalf("ProjectSwitch must navigate in-place via openOrCreateSession")
	}

	cli := mustReadContractFile(t, filepath.Join("..", "cli", "cli.go"))
	if strings.Contains(cli, "CloseForProjectSwitch") || strings.Contains(cli, "close current session") {
		t.Fatalf("CLI projectSwitch must not close the owner session")
	}
	if strings.Contains(cli, "relaunchIn") || strings.Contains(cli, "syscall.Exec") {
		t.Fatalf("CLI projectSwitch must not relaunch or exec")
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
	if !strings.Contains(app, "hasActiveModel={!!modelRef") {
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
		"export function RevertCode(arg1:number):Promise<snapshot.RevertResult>;",
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

func TestWailsCompactionResyncPublishesThroughRewriteBoundary(t *testing.T) {
	app := mustReadContractFile(t, filepath.Join("..", "..", "app.go"))
	compactionEnd := extractSwitchCase(t, app, "case agent.EventCompactionEnd:")
	if !strings.Contains(compactionEnd, `emit("compaction_end"`) {
		t.Fatalf("EventCompactionEnd must still emit compaction_end; case:\n%s", compactionEnd)
	}
	if strings.Contains(compactionEnd, "emitResyncBoundary") {
		t.Fatalf("EventCompactionEnd must not resync; the rewrite boundary does; case:\n%s", compactionEnd)
	}
	turnEnd := extractSwitchCase(t, app, "case agent.EventTurnEnd:")
	if !strings.Contains(turnEnd, `emit("turn_end"`) {
		t.Fatalf("EventTurnEnd must still emit turn_end; case:\n%s", turnEnd)
	}
	if strings.Contains(turnEnd, "emitResyncBoundary") {
		t.Fatalf("EventTurnEnd must not resync; the rewrite boundary does; case:\n%s", turnEnd)
	}
	rewrite := extractSwitchCase(t, app, "case agent.EventSessionRewrite:")
	if !strings.Contains(rewrite, "a.emitResyncBoundary(ev.SessionID, ev.RewritePayload)") {
		t.Fatalf("EventSessionRewrite must apply the producer-built replacement; case:\n%s", rewrite)
	}
}

// TestCompactionResyncRefreshesTranscriptWithoutRestickingActivity proves the
// compaction resync applies only the transcript and tokens — never activity or
// queue. Compaction changes none of those, and the resync runs at turn end before
// the deferred busy clear, so applying busy from it would re-stick a stale busy the
// turn_end frame already cleared.
func TestCompactionResyncRefreshesTranscriptWithoutRestickingActivity(t *testing.T) {
	svelte := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "App.svelte"))
	body, ok := extractSvelteFunctionBody(svelte, "function applyResync(")
	if !ok {
		t.Fatal("applyResync not found in App.svelte")
	}
	for _, forbidden := range []string{"busy =", "compacting =", "messageQueue =", "lastQueueVersion =", "warnings =", "permissions ="} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("applyResync must not apply %q: compaction does not change it and a turn-end refresh's busy is stale", strings.TrimSpace(forbidden))
		}
	}
	if !strings.Contains(body, "messages =") || !strings.Contains(body, "tokens =") {
		t.Fatal("applyResync must refresh the transcript and tokens")
	}
	// A resync never switches sessions: it must reject a payload for a session the
	// frontend has navigated away from, or a stale resync would restore it over the
	// destination of a concurrent switch.
	if !strings.Contains(body, "!== sessionId") {
		t.Fatal("applyResync must reject a resync whose session differs from the current session")
	}
	if strings.Contains(body, "sessionId =") {
		t.Fatal("applyResync must not assign sessionId; a resync never changes the session")
	}
}

// TestTurnEndDoesNotReresolveSession proves the turn_end handler never reassigns
// sessionId. Session identity is owned by the ordered navigation/turn_action/
// hydration boundaries; an out-of-band SessionCurrent() at turn end could restore a
// session a concurrent switch already navigated away from, defeating the resync
// guard that compares against the current session.
func TestTurnEndDoesNotReresolveSession(t *testing.T) {
	svelte := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "App.svelte"))
	body, ok := extractSvelteFunctionBody(svelte, "EventsOn('turn_end'")
	if !ok {
		t.Fatal("turn_end handler not found in App.svelte")
	}
	if strings.Contains(body, "sessionId =") || strings.Contains(body, "SessionCurrent(") {
		t.Fatal("turn_end must not re-resolve sessionId; only navigation/turn_action/hydration boundaries own session identity")
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
	// The backend folds the child association into the parent's tool row before
	// emitting the start frame, so the frontend keeps no pending-link storage or
	// module-level link authority: the row exists by the time the id-keyed
	// update arrives.
	for _, forbidden := range []string{
		"pendingSubagentSessionLinks",
		"rememberSubagentLink",
		"takePendingSubagentLinks",
	} {
		if strings.Contains(app, forbidden) {
			t.Fatalf("App.svelte must not keep pending subagent-link storage %q; the backend folds the association into the tool row before the start frame", forbidden)
		}
	}
	for _, want := range []string{
		"subagentLinksFromMetadata(metadata)",
		"mergeSubagentLinks(m.subagentSessionIds, [link])",
	} {
		if !strings.Contains(app, want) {
			t.Fatalf("App.svelte missing subagent task-link path %q", want)
		}
	}

	helpers := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "lib", "subagentLinks.js"))
	for _, want := range []string{"metadata.subagent_session_ids", "metadata.subagentSessionIds"} {
		if !strings.Contains(helpers, want) {
			t.Fatalf("subagentLinks.js missing metadata recovery path %q", want)
		}
	}
	if strings.Contains(helpers, "rememberSubagentLink") || strings.Contains(helpers, "takePendingSubagentLinks") {
		t.Fatal("subagentLinks.js must not keep pending-link helpers; the backend owns the association")
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
	for _, want := range []string{"HydrateSession", "hydrateSubagentViewer"} {
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

// TestHydrateSurfacesCurrentSessionLookupFailure proves hydrate() surfaces a
// failed current-session lookup through showError instead of swallowing it: the
// silent empty catch left id empty, skipped hydration entirely, and still ran
// hydrated = true — a silently empty transcript indistinguishable from a
// genuinely empty one. Only the lookup catch is silent; the HydrateSession
// catch already surfaces through showError.
func TestHydrateSurfacesCurrentSessionLookupFailure(t *testing.T) {
	svelte := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "App.svelte"))
	body, ok := extractSvelteFunctionBody(svelte, "async function hydrate(")
	if !ok {
		t.Fatal("hydrate not found in App.svelte")
	}
	if strings.Contains(body, "catch (e) {}") {
		t.Fatal("hydrate must not swallow the current-session lookup failure: the silent empty session is indistinguishable from a genuinely empty one")
	}
	if !strings.Contains(body, "showError(e, 'Load session failed')") {
		t.Fatal("hydrate must surface the current-session lookup failure through showError")
	}
}

// TestFrontendPresentationOwnershipGates proves the Wails presentation owns
// each stale writer: the settings-refresh model path captures the session,
// generation, and both model fields, and the snapshot-gated listeners
// (warnings, usage, queue_changed, compaction start/end, error, turn_action)
// refuse to mutate an unseeded view. The busy clear is reserved for sequenced
// turn errors, and the turn_action handler applies a stateful frame first but
// skips notices over an unseeded view.
func TestFrontendPresentationOwnershipGates(t *testing.T) {
	svelte := mustReadContractFile(t, filepath.Join("..", "..", "frontend", "src", "App.svelte"))

	// refreshCurrentModel must capture all four ownership terms and compare all
	// four — session, generation, model ref, and model name — in both the
	// resolution branch and the rejection branch. Each comparison term therefore
	// appears once per branch (twice total); capture declarations alone cannot
	// gate the continuation.
	refresh, ok := extractSvelteFunctionBody(svelte, "async function refreshCurrentModel(")
	if !ok {
		t.Fatal("refreshCurrentModel not found in App.svelte")
	}
	for _, want := range []string{
		"const opSession = sessionId",
		"const opGen = presentationGeneration",
		"const opRef = modelRef",
		"const opName = modelName",
	} {
		if !strings.Contains(refresh, want) {
			t.Fatalf("refreshCurrentModel must capture %q for the settings-refresh ownership guard", want)
		}
	}
	for _, term := range []string{
		"sessionId !== opSession",
		"presentationGeneration !== opGen",
		"modelRef !== opRef",
		"modelName !== opName",
	} {
		if n := strings.Count(refresh, term); n < 2 {
			t.Fatalf("refreshCurrentModel must compare %q in both the resolution and rejection branches (found %d occurrences)", term, n)
		}
	}
	// The refresh expects the captured session from the backend, ignores a
	// superseded result, and applies the returned model (not a bare model).
	if !strings.Contains(refresh, "CurrentModel(opSession)") {
		t.Fatal("refreshCurrentModel must expect the captured session from CurrentModel")
	}
	if !strings.Contains(refresh, "superseded") {
		t.Fatal("refreshCurrentModel must ignore a superseded CurrentModel result")
	}
	if !strings.Contains(refresh, "r?.model") {
		t.Fatal("refreshCurrentModel must apply the resolved CurrentModelResult.model")
	}
	// Mount-time loading expects no session: CurrentModel('') keeps startup
	// routing behavior before hydration.
	if !strings.Contains(svelte, "CurrentModel('')") {
		t.Fatal("mount-time model loading must call CurrentModel('') to preserve pre-hydration routing")
	}

	// Every snapshot-gated listener must refuse to mutate an unseeded view.
	for _, ev := range []string{"warnings", "usage", "queue_changed", "compaction_start", "compaction_end", "error", "turn_action"} {
		body, ok := extractSvelteFunctionBody(svelte, "EventsOn('"+ev+"'")
		if !ok {
			t.Fatalf("listener '%s' not found in App.svelte", ev)
		}
		if !strings.Contains(body, "snapshotApplied") {
			t.Fatalf("listener '%s' must gate its mutations on snapshotApplied", ev)
		}
	}

	// queue_changed keeps its version guard after the snapshot gate.
	queue, ok := extractSvelteFunctionBody(svelte, "EventsOn('queue_changed'")
	if !ok {
		t.Fatal("queue_changed listener not found")
	}
	if !strings.Contains(queue, "lastQueueVersion") || !strings.Contains(queue, "version <= lastQueueVersion") {
		t.Fatal("queue_changed must keep the queue version guard after the snapshot gate")
	}

	// The error listener renders admitted sequenced and unsequenced errors but
	// clears busy only when the frame carries a sequence.
	errBody, ok := extractSvelteFunctionBody(svelte, "EventsOn('error'")
	if !ok {
		t.Fatal("error listener not found in App.svelte")
	}
	if !strings.Contains(errBody, "data?.seq") || !strings.Contains(errBody, "busy = false") {
		t.Fatal("error listener must clear busy only when the frame carries a sequence")
	}
	if strings.Index(errBody, "busy = false") < strings.Index(errBody, "data?.seq") {
		t.Fatal("error listener must not clear busy unconditionally; the sequence must gate it")
	}

	// The turn_action listener applies a stateful frame first, then skips
	// notices when the view is still unseeded.
	turnAction, ok := extractSvelteFunctionBody(svelte, "EventsOn('turn_action'")
	if !ok {
		t.Fatal("turn_action listener not found in App.svelte")
	}
	snapIdx := strings.Index(turnAction, "applySnapshot(")
	guardIdx := strings.Index(turnAction, "!data?.state && !snapshotApplied")
	noticeIdx := strings.Index(turnAction, "appendRevertSkipNotice(")
	if snapIdx < 0 || guardIdx < 0 || noticeIdx < 0 {
		t.Fatal("turn_action listener must apply the snapshot first and return over an unseeded view before notices")
	}
	if snapIdx > guardIdx || guardIdx > noticeIdx {
		t.Fatal("turn_action listener must apply the snapshot, then the unseeded guard, then the notices")
	}

	// The existing ordered-model ownership and resync session checks are intact.
	model, ok := extractSvelteFunctionBody(svelte, "EventsOn('model'")
	if !ok {
		t.Fatal("model listener not found in App.svelte")
	}
	if !strings.Contains(model, "data.rootId !== sessionId") {
		t.Fatal("model listener must keep the root-ownership check")
	}
	resync, ok := extractSvelteFunctionBody(svelte, "function applyResync(")
	if !ok {
		t.Fatal("applyResync not found in App.svelte")
	}
	if !strings.Contains(resync, "!== sessionId") {
		t.Fatal("applyResync must keep the session check")
	}
}

// svelteCodeWithoutCommentLines returns source with // comment lines removed,
// so a symbol-count assertion counts call sites rather than prose that happens
// to name the function.
func svelteCodeWithoutCommentLines(source string) string {
	var b strings.Builder
	for _, line := range strings.Split(source, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
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
	emitted := collectContractMatches(t, filepath.Join("..", ".."), ".go", `(?:EventsEmit\([^,]+,\s*|emit(?:Frame)?\(\s*|enqueueBoundary\(\s*|emitSessionFrame\([^,]+,\s*)["']([^"']+)["']`)
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
	emitted := stringSet(collectContractMatches(t, filepath.Join("..", ".."), ".go", `(?:EventsEmit\([^,]+,\s*|emit(?:Frame)?\(\s*|enqueueBoundary\(\s*|emitSessionFrame\([^,]+,\s*)["']([^"']+)["']`))
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
		// Contracts live in production code; test files may reference the same
		// helpers with throwaway names.
		if strings.HasSuffix(d.Name(), "_test.go") {
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
