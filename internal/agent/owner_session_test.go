package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/config"
	loop "github.com/MMinasyan/lightcode/internal/engine"
	"github.com/MMinasyan/lightcode/internal/memory"
	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/snapshot"
	"github.com/MMinasyan/lightcode/internal/tool"
)

func TestUnknownSessionIDsAreRejected(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	ctx := context.Background()

	checks := []struct {
		name string
		run  func() error
	}{
		{
			name: "submit",
			run: func() error {
				_, err := a.SubmitToSession(ctx, "missing-session", "hello")
				return err
			},
		},
		{
			name: "queue",
			run: func() error {
				_, err := a.QueueSnapshotForSession("missing-session")
				return err
			},
		},
		{
			name: "compact",
			run: func() error {
				return a.CompactNowForSession(context.Background(), "missing-session")
			},
		},
		{
			name: "snapshots",
			run: func() error {
				_, err := a.SnapshotListForSession("missing-session")
				return err
			},
		},
		{
			name: "append",
			run: func() error {
				_, err := a.AppendUserMessageToSession("missing-session", "hello")
				return err
			},
		},
		{
			name: "messages",
			run: func() error {
				_, err := a.SessionMessagesByID("missing-session")
				return err
			},
		},
		{
			name: "current model",
			run: func() error {
				_, err := a.CurrentModelForSession("missing-session")
				return err
			},
		},
		{
			name: "revert code",
			run: func() error {
				_, err := a.RevertCodeForSession("missing-session", 0)
				return err
			},
		},
		{
			name: "cancel",
			run: func() error {
				return a.CancelSession("missing-session")
			},
		},
	}

	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil || !strings.Contains(err.Error(), `unknown session "missing-session"`) {
				t.Fatalf("error = %v, want unknown session", err)
			}
		})
	}
}

func TestLiveSessionsHaveSeparateHistoryAndQueues(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	ctx := context.Background()

	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if firstID == "" || secondID == "" || firstID == secondID {
		t.Fatalf("session ids first=%q second=%q, want distinct non-empty ids", firstID, secondID)
	}

	a.ensureRuntime().mu.Lock()
	first := a.sessions[firstID]
	second := a.sessions[secondID]
	a.ensureRuntime().mu.Unlock()
	if first == nil || second == nil {
		t.Fatalf("live session map missing first=%v second=%v", first != nil, second != nil)
	}
	if first.store == nil || second.store == nil || first.store == second.store {
		t.Fatalf("stores are not distinct: first=%p second=%p", first.store, second.store)
	}
	if first.lp == nil || second.lp == nil || first.lp == second.lp {
		t.Fatalf("loops are not distinct: first=%p second=%p", first.lp, second.lp)
	}

	if _, err := a.AppendUserMessageToSession(firstID, "first only"); err != nil {
		t.Fatalf("append first: %v", err)
	}
	if _, err := a.AppendUserMessageToSession(secondID, "second only"); err != nil {
		t.Fatalf("append second: %v", err)
	}

	firstMessages, err := a.SessionMessagesByID(firstID)
	if err != nil {
		t.Fatalf("messages first: %v", err)
	}
	secondMessages, err := a.SessionMessagesByID(secondID)
	if err != nil {
		t.Fatalf("messages second: %v", err)
	}
	if got := userContents(firstMessages); !equalStrings(got, []string{"first only"}) {
		t.Fatalf("first messages = %#v, want first only", got)
	}
	if got := userContents(secondMessages); !equalStrings(got, []string{"second only"}) {
		t.Fatalf("second messages = %#v, want second only", got)
	}

	a.ensureRuntime().mu.Lock()
	first.busy = true
	second.busy = true
	a.ensureRuntime().mu.Unlock()
	t.Cleanup(func() {
		a.ensureRuntime().mu.Lock()
		first.busy = false
		second.busy = false
		a.ensureRuntime().mu.Unlock()
	})

	if _, err := a.SubmitToSession(ctx, firstID, "queued first"); err != nil {
		t.Fatalf("submit first: %v", err)
	}
	if _, err := a.SubmitToSession(ctx, secondID, "queued second"); err != nil {
		t.Fatalf("submit second: %v", err)
	}
	firstQueue, err := a.QueueSnapshotForSession(firstID)
	if err != nil {
		t.Fatalf("queue first: %v", err)
	}
	secondQueue, err := a.QueueSnapshotForSession(secondID)
	if err != nil {
		t.Fatalf("queue second: %v", err)
	}
	if len(firstQueue.Items) != 1 || firstQueue.Items[0].Content != "queued first" {
		t.Fatalf("first queue = %#v, want queued first", firstQueue.Items)
	}
	if len(secondQueue.Items) != 1 || secondQueue.Items[0].Content != "queued second" {
		t.Fatalf("second queue = %#v, want queued second", secondQueue.Items)
	}
}

func TestPermissionSessionMatch(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	events := make(chan Event, 16)
	a.SetEventHandler(func(ev Event) {
		events <- ev
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)

	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	a.ensureRuntime().mu.Lock()
	first := a.sessions[firstID]
	first.turnCtx = ctx
	readTool, ok := first.registry.Get("read_file")
	firstProjectID := first.projectID
	a.ensureRuntime().mu.Unlock()
	if !ok {
		t.Fatal("read_file not registered")
	}
	if current := a.SessionCurrent().ID; current != secondID {
		t.Fatalf("current session = %q, want second %q", current, secondID)
	}

	done := make(chan error, 1)
	go func() {
		_, err := readTool.Execute(ctx, map[string]any{"path": "needs-approval.txt"})
		done <- err
	}()
	reqEvent := waitPermissionEvent(t, events)
	req := reqEvent.PermReq
	if reqEvent.SessionID != firstID || req.SessionID != firstID {
		t.Fatalf("permission session event/request = %q/%q, want %q", reqEvent.SessionID, req.SessionID, firstID)
	}
	if reqEvent.ProjectID != firstProjectID || req.ProjectID != firstProjectID {
		t.Fatalf("permission project event/request = %q/%q, want %q", reqEvent.ProjectID, req.ProjectID, firstProjectID)
	}

	if err := a.RespondPermissionAction(req.ID, "allow"); !errors.Is(err, permission.ErrSessionMismatch) {
		t.Fatalf("current-session RespondPermissionAction err = %v, want session mismatch", err)
	}
	if err := a.RespondPermissionActionForSession("", req.ID, "allow"); !errors.Is(err, permission.ErrSessionMismatch) {
		t.Fatalf("empty-session RespondPermissionActionForSession err = %v, want session mismatch", err)
	}
	if err := a.RespondPermissionActionForSession(firstID, req.ID, "deny"); err != nil {
		t.Fatalf("matching RespondPermissionActionForSession: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, tool.ErrDenied) {
			t.Fatalf("read_file err = %v, want denied", err)
		}
	case <-time.After(time.Second):
		t.Fatal("read_file stayed blocked after denial")
	}
	if err := a.RespondPermissionActionForSession(firstID, req.ID, "allow"); !errors.Is(err, permission.ErrUnknownRequest) {
		t.Fatalf("stale RespondPermissionActionForSession err = %v, want unknown request", err)
	}
}

func TestPermissionStampOwner(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	events := make(chan Event, 16)
	a.SetEventHandler(func(ev Event) {
		events <- ev
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)

	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	a.ensureRuntime().mu.Lock()
	first := a.sessions[firstID]
	projectID := first.projectID
	a.ensureRuntime().mu.Unlock()

	done := make(chan permission.ResponseAction, 1)
	go func() {
		done <- a.askPermissionForSession(ctx, first, permission.Request{
			SessionID: secondID,
			ProjectID: "forged-project",
			ToolName:  "read_file",
			Arg:       "forged.txt",
		}, projectID, false)
	}()
	req := waitPermissionEvent(t, events).PermReq
	if req.SessionID != firstID || req.ProjectID != projectID {
		t.Fatalf("permission owner = %q/%q, want %q/%q", req.SessionID, req.ProjectID, firstID, projectID)
	}
	if err := a.RespondPermissionActionForSession(secondID, req.ID, "allow"); !errors.Is(err, permission.ErrSessionMismatch) {
		t.Fatalf("forged session response err = %v, want session mismatch", err)
	}
	if err := a.RespondPermissionActionForSession(firstID, req.ID, "deny"); err != nil {
		t.Fatalf("owner response: %v", err)
	}
	select {
	case got := <-done:
		if got != permission.ResponseDeny {
			t.Fatalf("permission response = %q, want deny", got)
		}
	case <-time.After(time.Second):
		t.Fatal("permission ask stayed blocked")
	}
}

func TestEventFanout(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	legacy := make(chan Event, 2)
	extra := make(chan Event, 2)
	a.SetEventHandler(func(ev Event) {
		legacy <- ev
	})
	unsubscribe := a.SubscribeEvents(func(ev Event) {
		extra <- ev
	})

	a.emitEvent(Event{Kind: EventWarning, Warnings: []PromptWarning{{Kind: "test", Message: "first"}}})
	if got := waitEvent(t, legacy).Warnings[0].Message; got != "first" {
		t.Fatalf("legacy handler saw %q, want first", got)
	}
	if got := waitEvent(t, extra).Warnings[0].Message; got != "first" {
		t.Fatalf("extra subscriber saw %q, want first", got)
	}

	unsubscribe()
	a.emitEvent(Event{Kind: EventWarning, Warnings: []PromptWarning{{Kind: "test", Message: "second"}}})
	if got := waitEvent(t, legacy).Warnings[0].Message; got != "second" {
		t.Fatalf("legacy handler after unsubscribe saw %q, want second", got)
	}
	select {
	case ev := <-extra:
		t.Fatalf("unsubscribed handler saw event %#v", ev)
	default:
	}
}

func TestPermissionSaveProject(t *testing.T) {
	home := t.TempDir()
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	a := newCatalogBackedTestAgentForRoot(t, home, firstRoot)
	firstProject, err := a.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure first project: %v", err)
	}
	secondAgent := newCatalogBackedTestAgentForRoot(t, home, secondRoot)
	secondProject, err := secondAgent.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure second project: %v", err)
	}

	events := make(chan Event, 16)
	a.SetEventHandler(func(ev Event) {
		events <- ev
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)

	secondID, err := a.NewSession(secondProject.ID, "primary")
	if err != nil {
		t.Fatalf("NewSession second project: %v", err)
	}
	firstID, err := a.NewSession(firstProject.ID, "primary")
	if err != nil {
		t.Fatalf("NewSession first project: %v", err)
	}
	if current := a.SessionCurrent().ID; current != firstID {
		t.Fatalf("current session = %q, want first %q", current, firstID)
	}

	a.ensureRuntime().mu.Lock()
	second := a.sessions[secondID]
	second.turnCtx = ctx
	runTool, ok := second.registry.Get("run_command")
	a.ensureRuntime().mu.Unlock()
	if !ok {
		t.Fatal("run_command not registered")
	}
	done := make(chan error, 1)
	go func() {
		_, err := runTool.Execute(ctx, map[string]any{"command": "printf save-target"})
		done <- err
	}()
	req := waitPermissionEvent(t, events).PermReq
	if req.SessionID != secondID || req.ProjectID != secondProject.ID {
		t.Fatalf("permission request owner = %q/%q, want %q/%q", req.SessionID, req.ProjectID, secondID, secondProject.ID)
	}
	if err := a.SaveProjectPermissionForSession(firstID, req.ID, []string{"run_command(printf save-target)"}); !errors.Is(err, permission.ErrSessionMismatch) {
		t.Fatalf("wrong-session save err = %v, want session mismatch", err)
	}
	if err := a.SaveProjectPermissionForSession(secondID, req.ID, []string{"run_command(printf save-target)"}); err != nil {
		t.Fatalf("matching save: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run_command after save: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("run_command stayed blocked after save")
	}

	secondRules, err := permission.LoadLocal(a.projects.Root(), secondProject.ID)
	if err != nil {
		t.Fatalf("load second project rules: %v", err)
	}
	if !equalStrings(secondRules.Allow, []string{"run_command(printf save-target)"}) {
		t.Fatalf("second project rules = %#v", secondRules.Allow)
	}
	firstRules, err := permission.LoadLocal(a.projects.Root(), firstProject.ID)
	if err != nil {
		t.Fatalf("load first project rules: %v", err)
	}
	if len(firstRules.Allow) != 0 {
		t.Fatalf("first project rules = %#v, want none", firstRules.Allow)
	}
}

func TestStagedPermissionOwner(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	events := make(chan Event, 16)
	a.SetEventHandler(func(ev Event) {
		events <- ev
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Init(ctx)

	sessionID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	a.ensureRuntime().mu.Lock()
	unit := a.sessions[sessionID]
	unit.turnCtx = ctx
	projectID := unit.projectID
	exec := unit.pendingExecutor
	a.ensureRuntime().mu.Unlock()
	if exec == nil {
		t.Fatal("pending executor is nil")
	}

	staged := []tool.StagedCall{{
		ToolName:   "write_file",
		ToolCallID: "call-write",
		Args:       `{"path":"staged.txt","content":"ok"}`,
		Params: map[string]any{
			"path":    "staged.txt",
			"content": "ok",
		},
	}}
	done := make(chan []tool.BatchResult, 1)
	go func() {
		done <- exec.ExecutePending(ctx, staged)
	}()
	req := waitPermissionEvent(t, events).PermReq
	if req.SessionID != sessionID || req.ProjectID != projectID {
		t.Fatalf("staged permission owner = %q/%q, want %q/%q", req.SessionID, req.ProjectID, sessionID, projectID)
	}
	if req.BatchIndex != 1 || req.BatchTotal != 1 {
		t.Fatalf("staged batch position = %d/%d, want 1/1", req.BatchIndex, req.BatchTotal)
	}
	if err := a.RespondPermissionActionForSession(sessionID, req.ID, "deny"); err != nil {
		t.Fatalf("deny staged permission: %v", err)
	}
	select {
	case results := <-done:
		if len(results) != 1 || results[0].Error != "denied by user" {
			t.Fatalf("staged results = %#v, want denied by user", results)
		}
	case <-time.After(time.Second):
		t.Fatal("staged executor stayed blocked after denial")
	}
}

func TestTaskProjectSync(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if err := a.ensureSession(); err != nil {
		t.Fatalf("ensureSession: %v", err)
	}
	projectID := a.session.projectID
	if projectID == "" {
		t.Fatal("session project id is empty")
	}
	tt := a.session.taskToolInst
	if tt == nil {
		t.Fatal("task tool is nil")
	}
	tt.mu.Lock()
	taskProjectID := tt.projectID
	memoriesDir := tt.memoriesDir
	tt.mu.Unlock()
	if taskProjectID != projectID {
		t.Fatalf("task project id = %q, want %q", taskProjectID, projectID)
	}
	if memoriesDir == "" {
		t.Fatal("task memories dir is empty")
	}
	parentSessionID := a.session.store.SessionID()
	if parentSessionID == "" {
		t.Fatal("parent session id is empty")
	}

	tagged := make(chan TaggedLoopEvent, 1)
	tt.taggedEvents = tagged
	source := make(chan loop.Event, 1)
	source <- loop.Event{Kind: loop.ToolCallStart, ToolName: "read_file"}
	close(source)
	tt.forwardEvents(source, 0, "child-session", parentSessionID, taskProjectID, "call-task")
	got := <-tagged
	if got.ProjectID != projectID {
		t.Fatalf("forwarded project id = %q, want %q", got.ProjectID, projectID)
	}
}

func TestQueuedInputDrainsFromInactiveSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeTextResponse(w, "drained")
	}))
	defer server.Close()
	a := newEventOrderAgent(t, server.URL+"/v1")
	ctx := startEventOrderAgent(t, a, &eventCapture{})

	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if firstID == secondID {
		t.Fatalf("session ids both %q, want distinct", firstID)
	}

	a.ensureRuntime().mu.Lock()
	first := a.sessions[firstID]
	first.busy = true
	a.ensureRuntime().mu.Unlock()
	if _, err := a.SubmitToSession(ctx, firstID, "queued first"); err != nil {
		t.Fatalf("SubmitToSession first: %v", err)
	}
	a.ensureRuntime().mu.Lock()
	first.busy = false
	a.ensureRuntime().mu.Unlock()
	a.ensureRuntime().tryDrainQueue(ctx)

	waitUntilSessionQueueEmpty(t, a, firstID)
	firstMessages, err := a.SessionMessagesByID(firstID)
	if err != nil {
		t.Fatalf("messages first: %v", err)
	}
	if got := userContents(firstMessages); !equalStrings(got, []string{"queued first"}) {
		t.Fatalf("first messages after drain = %#v, want queued first", got)
	}
	secondQueue, err := a.QueueSnapshotForSession(secondID)
	if err != nil {
		t.Fatalf("queue second: %v", err)
	}
	if len(secondQueue.Items) != 0 {
		t.Fatalf("second queue = %#v, want empty", secondQueue.Items)
	}
}

func TestTokenUsageLoadsIntoSelectedSession(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	a.ensureRuntime().mu.Lock()
	first := a.sessions[firstID]
	second := a.sessions[secondID]
	a.ensureRuntime().mu.Unlock()
	writeTokenFile(t, first, TokenEntry{Provider: "test", Model: "test-model", Input: 11, Output: 7, Known: true})
	writeTokenFile(t, second, TokenEntry{Provider: "test", Model: "alt-model", Input: 3, Output: 2, Known: true})

	a.loadTokensFromDiskForSession(first)
	report, err := a.TokenUsageForSession(firstID)
	if err != nil {
		t.Fatalf("TokenUsageForSession first: %v", err)
	}
	if report.Total.Input != 11 || report.Total.Output != 7 {
		t.Fatalf("first token report = %#v, want 11/7", report.Total)
	}
	secondReport, err := a.TokenUsageForSession(secondID)
	if err != nil {
		t.Fatalf("TokenUsageForSession second: %v", err)
	}
	if secondReport.Total.Input != 0 || secondReport.Total.Output != 0 {
		t.Fatalf("second token report after first load = %#v, want zero", secondReport.Total)
	}

	a.loadTokensFromDiskForSession(second)
	secondReport, err = a.TokenUsageForSession(secondID)
	if err != nil {
		t.Fatalf("TokenUsageForSession second after load: %v", err)
	}
	if secondReport.Total.Input != 3 || secondReport.Total.Output != 2 {
		t.Fatalf("second token report = %#v, want 3/2", secondReport.Total)
	}
	report, err = a.TokenUsageForSession(firstID)
	if err != nil {
		t.Fatalf("TokenUsageForSession first after second load: %v", err)
	}
	if report.Total.Input != 11 || report.Total.Output != 7 {
		t.Fatalf("first token report after second load = %#v, want 11/7", report.Total)
	}
}

func TestMessagesComeFromSelectedSessionProject(t *testing.T) {
	home := t.TempDir()
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	first := newCatalogBackedTestAgentForRoot(t, home, firstRoot)
	firstID, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	if _, err := first.AppendUserMessageToSession(firstID, "from first project"); err != nil {
		t.Fatalf("append first: %v", err)
	}

	second := newCatalogBackedTestAgentForRoot(t, home, secondRoot)
	secondID, err := second.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if _, err := second.AppendUserMessageToSession(secondID, "from second project"); err != nil {
		t.Fatalf("append second: %v", err)
	}

	second.ensureRuntime().mu.Lock()
	second.sessions[firstID] = first.sessions[firstID]
	second.ensureRuntime().mu.Unlock()
	firstMessages, err := second.SessionMessagesFor(firstID)
	if err != nil {
		t.Fatalf("messages first via second owner: %v", err)
	}
	if got := userContents(firstMessages); !equalStrings(got, []string{"from first project"}) {
		t.Fatalf("first messages through resolved store = %#v, want first project", got)
	}
	secondMessages, err := second.SessionMessagesFor(secondID)
	if err != nil {
		t.Fatalf("messages second: %v", err)
	}
	if got := userContents(secondMessages); !equalStrings(got, []string{"from second project"}) {
		t.Fatalf("second messages = %#v, want second project", got)
	}
}

func TestSessionsUseTheirProjectRoot(t *testing.T) {
	home := t.TempDir()
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(firstRoot, "target.txt"), []byte("first-root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondRoot, "target.txt"), []byte("second-root"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(firstRoot, "AGENTS.md"), []byte("first project rules"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secondRoot, "AGENTS.md"), []byte("second project rules"), 0o600); err != nil {
		t.Fatal(err)
	}

	first := newCatalogBackedTestAgentForRoot(t, home, firstRoot)
	secondTarget := filepath.Join(secondRoot, "target.txt")
	first.cfg.Permissions.Allow = []string{
		"read_file(//" + strings.TrimPrefix(secondTarget, "/") + ")",
		"run_command(pwd)",
	}
	if _, err := first.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondProjectAgent := newCatalogBackedTestAgentForRoot(t, home, secondRoot)
	secondProjectID, err := secondProjectAgent.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure second project: %v", err)
	}
	secondPlanTarget := filepath.Join(first.projects.Root(), secondProjectID.ID, "plans", "plan.md")
	first.cfg.Permissions.Allow = append(first.cfg.Permissions.Allow, "write_file(//"+strings.TrimPrefix(secondPlanTarget, "/")+")")

	secondID, err := first.NewSession(secondProjectID.ID, "primary")
	if err != nil {
		t.Fatalf("NewSession second project: %v", err)
	}
	first.ensureRuntime().mu.Lock()
	second := first.sessions[secondID]
	readTool, ok := second.registry.Get("read_file")
	runTool, runOK := second.registry.Get("run_command")
	systemPrompt := second.lp.Messages()[0].TextContent()
	first.ensureRuntime().mu.Unlock()
	if !ok {
		t.Fatal("read_file not registered on second project session")
	}
	if !runOK {
		t.Fatal("run_command not registered on second project session")
	}
	if !strings.Contains(systemPrompt, "second project rules") || strings.Contains(systemPrompt, "first project rules") {
		t.Fatalf("system prompt uses wrong project rules: %q", systemPrompt)
	}
	out, err := readTool.Execute(context.Background(), map[string]any{"path": "target.txt"})
	if err != nil {
		t.Fatalf("read_file second project: %v", err)
	}
	if !strings.Contains(out, "second-root") || strings.Contains(out, "first-root") {
		t.Fatalf("read_file output = %q, want second project content only", out)
	}
	out, err = runTool.Execute(context.Background(), map[string]any{"command": "pwd"})
	if err != nil {
		t.Fatalf("run_command pwd second project: %v", err)
	}
	if !strings.Contains(out, secondRoot) || strings.Contains(out, firstRoot) {
		t.Fatalf("run_command output = %q, want second project root only", out)
	}

	planID, err := first.NewSession(secondProjectID.ID, "plan")
	if err != nil {
		t.Fatalf("NewSession plan second project: %v", err)
	}
	first.ensureRuntime().mu.Lock()
	plan := first.sessions[planID]
	writeTool, ok := plan.registry.Get("write_file")
	first.ensureRuntime().mu.Unlock()
	if !ok {
		t.Fatal("write_file not registered on second project plan session")
	}
	turn := plan.store.BeginTurn()
	if _, err := writeTool.Execute(context.Background(), map[string]any{"path": secondPlanTarget, "content": "second project plan"}); err != nil {
		t.Fatalf("write_file second project plan: %v", err)
	}
	if err := plan.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("complete plan write turn: %v", err)
	}
	data, err := os.ReadFile(secondPlanTarget)
	if err != nil {
		t.Fatalf("read second project plan: %v", err)
	}
	if got := string(data); got != "second project plan" {
		t.Fatalf("second project plan content = %q, want written content", got)
	}
}

func TestTurnActionsUseSelectedSessionHistory(t *testing.T) {
	home := t.TempDir()
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	first := newCatalogBackedTestAgentForRoot(t, home, firstRoot)
	firstID, err := first.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	if _, err := first.AppendUserMessageToSession(firstID, "first project only"); err != nil {
		t.Fatalf("append first: %v", err)
	}

	second := newCatalogBackedTestAgentForRoot(t, home, secondRoot)
	secondID, err := second.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if _, err := second.AppendUserMessageToSession(secondID, "keep"); err != nil {
		t.Fatalf("append second keep: %v", err)
	}
	if _, err := second.AppendUserMessageToSession(secondID, "selected"); err != nil {
		t.Fatalf("append second selected: %v", err)
	}

	first.ensureRuntime().mu.Lock()
	first.sessions[secondID] = second.sessions[secondID]
	first.ensureRuntime().mu.Unlock()
	result, err := first.ApplyTurnActionForSession(secondID, 2, TurnActionRevertHistory, false)
	if err != nil {
		t.Fatalf("ApplyTurnActionForSession second: %v", err)
	}
	if result.Prefill != "selected" {
		t.Fatalf("Prefill = %q, want selected", result.Prefill)
	}
	if got := userContents(result.Messages); !equalStrings(got, []string{"keep"}) {
		t.Fatalf("result messages = %#v, want keep", got)
	}
	firstMessages, err := first.SessionMessagesByID(firstID)
	if err != nil {
		t.Fatalf("messages first: %v", err)
	}
	if got := userContents(firstMessages); !equalStrings(got, []string{"first project only"}) {
		t.Fatalf("first messages = %#v, want unchanged first project", got)
	}
}

func TestCompactionIndexesSelectedSessionProject(t *testing.T) {
	const summary = "## Goal\nremember second project detail"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"chat-1","model":"test-model","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`, summary)
	}))
	defer server.Close()

	home := t.TempDir()
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	first := newCatalogBackedTestAgentForRoot(t, home, firstRoot)
	first.catalog.Providers["test"].Transport.BaseURL = server.URL + "/v1"
	firstProject, err := first.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure first project: %v", err)
	}
	firstID, err := first.NewSession(firstProject.ID, "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}

	secondProjectAgent := newCatalogBackedTestAgentForRoot(t, home, secondRoot)
	secondProject, err := secondProjectAgent.projects.Ensure()
	if err != nil {
		t.Fatalf("ensure second project: %v", err)
	}
	secondID, err := first.NewSession(secondProject.ID, "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if _, err := first.AppendUserMessageToSession(secondID, "second project context"); err != nil {
		t.Fatalf("append second: %v", err)
	}

	memStore := memory.NewStoreWithEmbedder(deterministicMemoryEmbedder{}, first.projects.Root(), first.home)
	hooks := &recordingMemoryHooks{store: memStore}
	first.memoryHooks = hooks

	first.ensureRuntime().mu.Lock()
	second := first.sessions[secondID]
	first.setCurrentSessionLocked(first.sessions[firstID])
	first.ensureRuntime().mu.Unlock()
	if err := first.runCompactionForSession(context.Background(), second, false); err != nil {
		t.Fatalf("runCompactionForSession second: %v", err)
	}

	if hooks.sessionID != secondID {
		t.Fatalf("indexed session id = %q, want %q", hooks.sessionID, secondID)
	}
	if hooks.projectID != secondProject.ID || hooks.projectName != secondProject.Name {
		t.Fatalf("indexed project = %q/%q, want %q/%q", hooks.projectID, hooks.projectName, secondProject.ID, secondProject.Name)
	}

	secondSearch := tool.NewSearchHistory(memStore, secondProject.ID)
	result, err := secondSearch.Execute(context.Background(), map[string]any{"query": "second project detail"})
	if err != nil {
		t.Fatalf("search_history second: %v", err)
	}
	if !strings.Contains(result, secondID) || !strings.Contains(result, "remember second project detail") {
		t.Fatalf("second project search result = %q, want indexed second session summary", result)
	}
	firstSearch := tool.NewSearchHistory(memStore, firstProject.ID)
	result, err = firstSearch.Execute(context.Background(), map[string]any{"query": "second project detail"})
	if err != nil {
		t.Fatalf("search_history first: %v", err)
	}
	if strings.Contains(result, secondID) {
		t.Fatalf("first project search result = %q, should not include second project session %s", result, secondID)
	}
}

func TestCompactTypeCannotBeStartedByUser(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	if _, err := a.NewSession("", "primary"); err != nil {
		t.Fatalf("NewSession primary: %v", err)
	}
	_, err := a.NewSession("", "compact")
	if err == nil || !strings.Contains(err.Error(), `agent type "compact" cannot be started as a session`) {
		t.Fatalf("NewSession compact error = %v, want code-only rejection", err)
	}
}

func TestCloseRemovedLiveSession(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(*Agent, string) (bool, error)
	}{
		{name: "archive", run: func(a *Agent, id string) (bool, error) { return a.SessionArchive(id) }},
		{name: "delete", run: func(a *Agent, id string) (bool, error) { return a.SessionDelete(id) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newCatalogBackedTestAgent(t)
			firstID, err := a.NewSession("", "primary")
			if err != nil {
				t.Fatalf("NewSession first: %v", err)
			}
			if _, err := a.AppendUserMessageToSession(firstID, "first"); err != nil {
				t.Fatalf("append first: %v", err)
			}
			secondID, err := a.NewSession("", "primary")
			if err != nil {
				t.Fatalf("NewSession second: %v", err)
			}
			if _, err := a.AppendUserMessageToSession(secondID, "second"); err != nil {
				t.Fatalf("append second: %v", err)
			}
			if current := a.SessionCurrent().ID; current != secondID {
				t.Fatalf("current session = %q, want %q", current, secondID)
			}

			closedCurrent, err := tc.run(a, firstID)
			if err != nil {
				t.Fatalf("%s first: %v", tc.name, err)
			}
			if closedCurrent {
				t.Fatalf("%s closed backend current session", tc.name)
			}
			if _, err := a.SessionSummaryForSession(firstID); err == nil {
				t.Fatalf("%s left first session live", tc.name)
			}
			if current := a.SessionCurrent().ID; current != secondID {
				t.Fatalf("current session after %s = %q, want %q", tc.name, current, secondID)
			}
		})
	}
}

func TestCloseClaimBlocksSubmit(t *testing.T) {
	a := newCatalogBackedTestAgent(t)
	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	if _, err := a.AppendUserMessageToSession(firstID, "first"); err != nil {
		t.Fatalf("append first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}
	if _, err := a.AppendUserMessageToSession(secondID, "second"); err != nil {
		t.Fatalf("append second: %v", err)
	}
	if current := a.SessionCurrent().ID; current != secondID {
		t.Fatalf("current session = %q, want %q", current, secondID)
	}

	release, err := a.beginLiveSessionClose(firstID)
	if err != nil {
		t.Fatalf("begin close: %v", err)
	}
	if release == nil {
		t.Fatal("begin close returned no release")
	}
	defer release()

	_, err = a.SubmitToSession(context.Background(), firstID, "blocked")
	if err == nil || !strings.Contains(err.Error(), "session is changing") {
		t.Fatalf("submit while closing = %v, want session changing", err)
	}
	if current := a.SessionCurrent().ID; current != secondID {
		t.Fatalf("current session after blocked submit = %q, want %q", current, secondID)
	}
}

func TestRemoveCrossProjectLiveSession(t *testing.T) {
	for _, tc := range []struct {
		name  string
		run   func(*Agent, string) (bool, error)
		check func(*testing.T, string, string)
	}{
		{
			name: "archive",
			run:  func(a *Agent, id string) (bool, error) { return a.SessionArchive(id) },
			check: func(t *testing.T, sessionsRoot, id string) {
				t.Helper()
				infos, err := snapshot.List(sessionsRoot, "", snapshot.StateArchived)
				if err != nil {
					t.Fatalf("list archived: %v", err)
				}
				for _, info := range infos {
					if info.ID == id {
						return
					}
				}
				t.Fatalf("archived session %s not found in %s", id, sessionsRoot)
			},
		},
		{
			name: "delete",
			run:  func(a *Agent, id string) (bool, error) { return a.SessionDelete(id) },
			check: func(t *testing.T, sessionsRoot, id string) {
				t.Helper()
				if _, err := os.Stat(filepath.Join(sessionsRoot, id)); !os.IsNotExist(err) {
					t.Fatalf("deleted session dir stat err = %v, want not exist", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			firstRoot := t.TempDir()
			secondRoot := t.TempDir()
			a := newCatalogBackedTestAgentForRoot(t, home, firstRoot)
			firstProject, err := a.projects.Ensure()
			if err != nil {
				t.Fatalf("ensure first project: %v", err)
			}
			secondAgent := newCatalogBackedTestAgentForRoot(t, home, secondRoot)
			secondProject, err := secondAgent.projects.Ensure()
			if err != nil {
				t.Fatalf("ensure second project: %v", err)
			}

			secondID, err := a.NewSession(secondProject.ID, "primary")
			if err != nil {
				t.Fatalf("NewSession second project: %v", err)
			}
			if _, err := a.AppendUserMessageToSession(secondID, "second"); err != nil {
				t.Fatalf("append second: %v", err)
			}
			firstID, err := a.NewSession(firstProject.ID, "primary")
			if err != nil {
				t.Fatalf("NewSession first project: %v", err)
			}
			if _, err := a.AppendUserMessageToSession(firstID, "first"); err != nil {
				t.Fatalf("append first: %v", err)
			}
			if current := a.SessionCurrent().ID; current != firstID {
				t.Fatalf("current session = %q, want %q", current, firstID)
			}

			closedCurrent, err := tc.run(a, secondID)
			if err != nil {
				t.Fatalf("%s second project: %v", tc.name, err)
			}
			if closedCurrent {
				t.Fatalf("%s closed backend current session", tc.name)
			}
			if _, err := a.SessionSummaryForSession(secondID); err == nil {
				t.Fatalf("%s left second session live", tc.name)
			}
			tc.check(t, a.projects.SessionsRoot(secondProject.ID), secondID)
			if current := a.SessionCurrent().ID; current != firstID {
				t.Fatalf("current session after %s = %q, want %q", tc.name, current, firstID)
			}
		})
	}
}

func TestTaskTokensStayWithLaunchingSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, `data: {"choices":[{"delta":{"role":"assistant","content":%q},"finish_reason":"stop"}]}`+"\n\n", "child done")
		fmt.Fprint(w, `data: {"choices":[],"usage":{"prompt_tokens":13,"completion_tokens":5}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	a := newEventOrderAgent(t, server.URL+"/v1")
	ctx := startEventOrderAgent(t, a, &eventCapture{})
	firstID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession first: %v", err)
	}
	secondID, err := a.NewSession("", "primary")
	if err != nil {
		t.Fatalf("NewSession second: %v", err)
	}

	a.ensureRuntime().mu.Lock()
	first := a.sessions[firstID]
	second := a.sessions[secondID]
	a.ensureRuntime().mu.Unlock()
	if first == nil || second == nil {
		t.Fatalf("test setup missing sessions first=%v second=%v", first != nil, second != nil)
	}
	if current := a.SessionCurrent().ID; current != secondID {
		t.Fatalf("current session = %q, want second %q", current, secondID)
	}

	turn := first.store.BeginTurn()
	if turn == 0 {
		t.Fatal("first session turn = 0")
	}
	_, err = first.taskToolInst.Execute(ctx, map[string]any{
		"tasks": []any{
			map[string]any{"prompt": "child usage", "subagent_type": "secondary"},
		},
	})
	if err != nil {
		t.Fatalf("task Execute: %v", err)
	}
	if err := first.store.MarkTurnComplete(turn); err != nil {
		t.Fatalf("complete first turn: %v", err)
	}

	firstReport, err := a.TokenUsageForSession(firstID)
	if err != nil {
		t.Fatalf("TokenUsageForSession first: %v", err)
	}
	if firstReport.Total.Input != 13 || firstReport.Total.Output != 5 {
		t.Fatalf("first token report = %#v, want input 13 output 5", firstReport.Total)
	}
	secondReport, err := a.TokenUsageForSession(secondID)
	if err != nil {
		t.Fatalf("TokenUsageForSession second: %v", err)
	}
	if secondReport.Total.Input != 0 || secondReport.Total.Output != 0 {
		t.Fatalf("second token report = %#v, want zero", secondReport.Total)
	}
}

func waitUntilSessionQueueEmpty(t *testing.T, a *Agent, sessionID string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		queue, err := a.QueueSnapshotForSession(sessionID)
		if err == nil && len(queue.Items) == 0 {
			busy, err := a.BusyForSession(sessionID)
			if err == nil && !busy {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("session %s did not drain", sessionID)
}

func waitPermissionEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	for {
		select {
		case ev := <-events:
			if ev.Kind == EventPermissionRequest && ev.PermReq != nil {
				return ev
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for permission request")
		}
	}
}

func waitEvent(t *testing.T, events <-chan Event) Event {
	t.Helper()
	select {
	case ev := <-events:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

func writeTokenFile(t *testing.T, unit *session, entries ...TokenEntry) {
	t.Helper()
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(unit.store.Dir(), tokensFileName), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newCatalogBackedTestAgentForRoot(t *testing.T, home, projectRoot string) *Agent {
	t.Helper()
	lightcodeDir := filepath.Join(home, ".lightcode")
	if err := os.MkdirAll(lightcodeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LIGHTCODE_TEST_KEY", "test-key")
	configPath := filepath.Join(lightcodeDir, "config.json")
	if _, err := os.Stat(configPath); err != nil {
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, []byte(`{
  "providers": {
    "test": {
      "name": "Test Provider",
      "transport": { "base_url": "http://127.0.0.1:9/v1", "api_key_env": "LIGHTCODE_TEST_KEY" },
      "discovery": false,
      "models": {
        "test-model": { "name": "Test Model", "context_window": 8192, "max_output_tokens": 1024 },
        "alt-model": { "name": "Alt Model", "context_window": 4096, "max_output_tokens": 512 }
      }
    }
  }
}`), 0o600); err != nil {
			t.Fatal(err)
		}
		writeAgentsTestConfig(t, configPath, `{"primary": {"model": "test/test-model"}}`)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	a, err := New(Config{Cfg: cfg, ConfigPath: configPath, ProjectRoot: projectRoot, Home: home})
	if err != nil {
		t.Fatalf("new agent: %v", err)
	}
	return a
}
