package agent

import (
	"context"
	"testing"

	"github.com/MMinasyan/lightcode/internal/permission"
	"github.com/MMinasyan/lightcode/internal/prompt"
)

func TestAgentFacadeInitializesRuntimeForPartialAgent(t *testing.T) {
	a := &Agent{warningGroups: make(map[string][]PromptWarning)}
	var got []Event
	a.SetEventHandler(func(ev Event) {
		got = append(got, ev)
	})

	a.addWarning("prompt", prompt.Warning{Kind: "test", Message: "runtime ready"})

	if len(got) != 1 || got[0].Kind != EventWarning {
		t.Fatalf("events = %#v, want one warning event", got)
	}
	if snapshot := a.QueueSnapshot(); snapshot.Items == nil {
		t.Fatalf("QueueSnapshot items = nil, want empty slice")
	}
}

func TestRuntimeDefaultPermissionPolicyFailsClosed(t *testing.T) {
	rt := (&Agent{}).ensureRuntime()
	if got := rt.permissionPolicy.checkFunc()("run_command", "echo ok"); got != permission.DecisionDeny {
		t.Fatalf("default permission check = %v, want deny", got)
	}
	if got := rt.permissionPolicy.askFunc()(context.Background(), permission.Request{}); got != permission.ResponseDeny {
		t.Fatalf("default permission ask = %v, want deny", got)
	}
	if got := rt.permissionPolicy.askActionFunc()(context.Background(), permission.Request{}); got != permission.ResponseDeny {
		t.Fatalf("default permission ask action = %v, want deny", got)
	}
}
