package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/project"
)

func TestSaveProjectPermissionReturnsSuccessWhenRuleSavedButRespondStale(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	resolver, err := project.NewResolver(home, projectRoot)
	if err != nil {
		t.Fatal(err)
	}

	idCh := make(chan string, 1)
	gate := permission.NewGate(func(ctx context.Context, req permission.Request) {
		idCh <- req.ID
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan permission.ResponseAction, 1)
	go func() {
		result <- gate.AskRequest(ctx, permission.Request{ToolName: "write_file", Arg: "{}"})
	}()
	id := <-idCh
	cancel()
	select {
	case <-result:
	case <-time.After(time.Second):
		t.Fatal("permission request did not return after cancel")
	}

	a := &Agent{projects: resolver, gate: gate}
	if err := a.SaveProjectPermission(id, []string{"write_file:*"}); err != nil {
		t.Fatalf("SaveProjectPermission returned error for stale id after saving rule: %v", err)
	}

	proj, err := resolver.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	rules, err := permission.LoadLocal(resolver.Root(), proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules.Allow) != 1 || rules.Allow[0] != "write_file:*" {
		t.Fatalf("saved rules = %#v", rules)
	}
}

func TestSaveProjectPermissionRejectsDisabledPendingRequest(t *testing.T) {
	home := t.TempDir()
	projectRoot := t.TempDir()
	resolver, err := project.NewResolver(home, projectRoot)
	if err != nil {
		t.Fatal(err)
	}

	idCh := make(chan string, 1)
	gate := permission.NewGate(func(ctx context.Context, req permission.Request) {
		idCh <- req.ID
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan permission.ResponseAction, 1)
	go func() {
		result <- gate.AskRequest(ctx, permission.Request{
			ToolName:           "apply_patch",
			Arg:                "/tmp/project/a.txt",
			ResolvedArg:        "/tmp/project/a.txt",
			BatchFiles:         []string{"/tmp/project/a.txt", "/tmp/project/.env"},
			BatchResolvedFiles: []string{"/tmp/project/a.txt", "/tmp/project/.env"},
			DisableProjectSave: true,
		})
	}()
	id := <-idCh

	a := &Agent{projects: resolver, gate: gate}
	err = a.SaveProjectPermission(id, []string{"apply_patch(/tmp/project/a.txt)"})
	if err == nil || !strings.Contains(err.Error(), "project permission") {
		t.Fatalf("SaveProjectPermission err = %v, want disabled project-save error", err)
	}
	if err := a.RespondPermission(id, false); err != nil {
		t.Fatalf("RespondPermission deny: %v", err)
	}
	select {
	case got := <-result:
		if got != permission.ResponseDeny {
			t.Fatalf("permission result = %v, want deny", got)
		}
	case <-time.After(time.Second):
		t.Fatal("permission request did not resolve after deny")
	}

	proj, err := resolver.Ensure()
	if err != nil {
		t.Fatal(err)
	}
	rules, err := permission.LoadLocal(resolver.Root(), proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules.Allow) != 0 {
		t.Fatalf("saved rules = %#v, want none", rules.Allow)
	}
}
