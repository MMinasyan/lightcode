package agent

import (
	"context"
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
