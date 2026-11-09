package melta

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
	"github.com/MMinasyan/lightcode/protocol"
)

type traceLog struct {
	mu     sync.Mutex
	events []string
}

func (l *traceLog) add(event string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *traceLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

var (
	errFactory      = errors.New("test factory failure")
	errCloser       = errors.New("test closer failure")
	errWorkspaceClo = errors.New("test workspace closer failure")
)

func runtimeScopeInfo() ScopeInfo {
	return ScopeInfo{Kind: ScopeRuntime, DataDir: "/owner/data"}
}

func workspaceScopeInfo(workspace string) ScopeInfo {
	return ScopeInfo{Kind: ScopeWorkspace, DataDir: "/owner/data", Workspace: workspace}
}

func mustComposition(t *testing.T, natives ...extension.Native) *composition {
	t.Helper()
	c, err := newNativeComposition(natives)
	testassert.NoError(t, err)
	return c
}

func mustOpenScope(t *testing.T, c *composition, ctx context.Context, info ScopeInfo, ancestors []*scope) *scope {
	t.Helper()
	sc, err := c.openScope(ctx, info, ancestors)
	testassert.Require(t, err == nil, "openScope(%s %q): %v", info.Kind, info.Workspace, err)
	return sc
}

type workspaceFactoryError struct{}
type workspaceCleanupError struct{}

func (*workspaceFactoryError) Error() string { return "workspace factory failure" }
func (*workspaceCleanupError) Error() string { return "workspace cleanup failure" }

func runtimeNormalizeContext(_ context.Context, call model.ToolCall) (json.RawMessage, error) {
	return runtimeNormalize(call)
}

// testCaller is the authenticated user grant the protocol host binds after
// authentication; it is issued through the owner under test so every fixture
// Caller is bound to that owner.
func testCaller(owner *Melta) Caller {
	c, err := owner.Caller(CallerContext{Origin: "user", Scope: harnessScope})
	if err != nil {
		panic(err)
	}
	return c
}

// textContentPart builds one generated text content part.
func textContentPart(t *testing.T, text string, extra map[string]json.RawMessage) protocol.ContentPart {
	t.Helper()
	var part protocol.ContentPart
	var optional *protocol.JSONObject
	if extra != nil {
		optional = &extra
	}
	testassert.NoError(t, part.FromTextPart(protocol.TextPart{Text: text, Extra: optional}))
	return part
}

// ptrTo returns a pointer to one value.
func ptrTo[T any](value T) *T { return &value }
