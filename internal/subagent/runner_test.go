package subagent

import (
	"context"
	"testing"

	"github.com/MMinasyan/lightcode/internal/loop"
	"github.com/MMinasyan/lightcode/internal/tool"
)

func TestRunDelegatesToLoop(t *testing.T) {
	lp := loop.New(nil, tool.NewRegistry(), "system")

	_, err := Run(context.Background(), lp, "inspect this")
	if err == nil {
		t.Fatal("Run error = nil, want loop.Run error from nil client")
	}
}

func TestRunPropagatesCancelledContext(t *testing.T) {
	lp := loop.New(nil, tool.NewRegistry(), "system")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Run(ctx, lp, "inspect this")
	if err == nil {
		t.Fatal("Run error = nil, want cancellation/nil-client error")
	}
}
