package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MMinasyan/lightcode/harness"
)

// Composition fixtures: capability contracts and services defined here, not
// by any Core-known category, exactly as plugin authors outside the Runtime
// will declare them.

type greeter interface{ Greet() string }

type loudGreeter struct{ word string }

func (g loudGreeter) Greet() string { return strings.ToUpper(g.word) }

type counted interface{ Count() int }

type counterService struct{ n int }

func (*counterService) Count() int { return 0 }

type worker interface{ Work(inv Invocation) string }

type betaWorker struct{}

func (betaWorker) Work(inv Invocation) string {
	return fmt.Sprintf("b:%s bcfg:%s", inv.Revision(), settingsOrNone(inv.Config("beta")))
}

type alphaWorker struct{ dep worker }

func (a alphaWorker) Work(inv Invocation) string {
	return fmt.Sprintf("a:%s acfg:%s | %s", inv.Revision(), settingsOrNone(inv.Config("alpha")), a.dep.Work(inv))
}

func settingsOrNone(raw json.RawMessage) string {
	if raw == nil {
		return "none"
	}
	return string(raw)
}

type stubCoreStorage struct{ harness.Storage }

// hookValue implements PreparationHook with a value receiver, so both it and
// its pointer implement the contract; ptrHookValue is reachable only through
// its pointer type.
type hookValue struct{}

func (hookValue) Prepare(context.Context, Invocation, harness.ExecutionCapture) (harness.ExecutionCapture, error) {
	return harness.ExecutionCapture{}, nil
}

type ptrHookValue struct{}

func (*ptrHookValue) Prepare(context.Context, Invocation, harness.ExecutionCapture) (harness.ExecutionCapture, error) {
	return harness.ExecutionCapture{}, nil
}

// tamperScratch rewrites an equal-length substring of one raw JSON buffer in
// place: exactly the scratch mutation a synchronous validator could apply to
// the bytes it receives.
func tamperScratch(raw json.RawMessage, want, with string) {
	if len(want) != len(with) {
		panic("in-place scratch rewrite needs an equal-length replacement")
	}
	i := strings.Index(string(raw), want)
	if i < 0 {
		panic("in-place scratch rewrite target absent")
	}
	copy(raw[i:], with)
}

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
	errValidator    = errors.New("test validator failure")
)

func runtimeScopeInfo() ScopeInfo {
	return ScopeInfo{Kind: ScopeRuntime, DataDir: "/owner/data"}
}

func workspaceScopeInfo(workspace string) ScopeInfo {
	return ScopeInfo{Kind: ScopeWorkspace, DataDir: "/owner/data", Workspace: workspace}
}

func mustComposition(t *testing.T, plugins ...Plugin) *composition {
	t.Helper()
	c, err := newComposition(plugins)
	if err != nil {
		t.Fatalf("newComposition: %v", err)
	}
	return c
}

func mustOpenScope(t *testing.T, c *composition, ctx context.Context, info ScopeInfo, ancestors []*scope) *scope {
	t.Helper()
	sc, err := c.openScope(ctx, info, ancestors)
	if err != nil {
		t.Fatalf("openScope(%s %q): %v", info.Kind, info.Workspace, err)
	}
	return sc
}

func TestCompositionRejectsInvalidDeclarations(t *testing.T) {
	var opened int
	provides := func(id string, scope ScopeKind, specs ...CapabilitySpec) Plugin {
		return Plugin{ID: id, Scope: scope, Provides: specs, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			opened++
			values := make(map[string]any, len(specs))
			for _, spec := range specs {
				values[spec.id] = loudGreeter{word: id}
			}
			return Instance{Values: values}, nil
		}}
	}
	declares := func(id string, scope ScopeKind, providesIDs ...string) Plugin {
		specs := make([]CapabilitySpec, 0, len(providesIDs))
		for _, capID := range providesIDs {
			specs = append(specs, Spec[greeter](capID))
		}
		return provides(id, scope, specs...)
	}
	requires := func(id string, scope ScopeKind, reqs ...CapabilitySpec) Plugin {
		p := provides(id, scope, Spec[greeter](id+".own"))
		p.Requires = reqs
		return p
	}
	for _, row := range []struct {
		name    string
		plugins []Plugin
	}{
		{"empty plugin ID", []Plugin{declares("", ScopeRuntime, "a.g")}},
		{"duplicate plugin ID", []Plugin{declares("a", ScopeRuntime, "a.g"), declares("a", ScopeWorkspace, "b.g")}},
		{"invalid scope kind", []Plugin{declares("a", ScopeKind("session"), "a.g")}},
		{"missing Open", []Plugin{{ID: "a", Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[greeter]("a.g")}}}},
		{"missing providers", []Plugin{{ID: "a", Scope: ScopeRuntime, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			opened++
			return Instance{}, nil
		}}}},
		{"empty export ID", []Plugin{provides("a", ScopeRuntime, Spec[greeter](""))}},
		{"empty dependency ID", []Plugin{requires("a", ScopeRuntime, Spec[greeter](""))}},
		{"duplicate dependency ID", []Plugin{
			declares("a", ScopeRuntime, "a.n"),
			requires("b", ScopeRuntime, Spec[greeter]("a.n"), Spec[greeter]("a.n")),
		}},
		{"export ID collision across plugins", []Plugin{declares("a", ScopeRuntime, "dup"), declares("b", ScopeRuntime, "dup")}},
		{"export ID collision within one plugin", []Plugin{declares("a", ScopeRuntime, "dup", "dup")}},
		{"ordinary export collides with Core storage export", []Plugin{
			provides("a", ScopeRuntime, Spec[harness.Storage]("core")),
			declares("b", ScopeRuntime, "core"),
		}},
		{"missing binding", []Plugin{requires("a", ScopeRuntime, Spec[greeter]("nobody"))}},
		{"dependency on Core storage export by ID", []Plugin{
			provides("a", ScopeRuntime, Spec[harness.Storage]("core")),
			requires("b", ScopeRuntime, Spec[harness.Storage]("core")),
		}},
		{"provider type not assignable to requested type", []Plugin{
			provides("a", ScopeRuntime, Spec[greeter]("g")),
			requires("b", ScopeRuntime, Spec[loudGreeter]("g")),
		}},
		{"incompatible value export", []Plugin{
			provides("a", ScopeRuntime, Spec[int]("n")),
			requires("b", ScopeRuntime, Spec[string]("n")),
		}},
		{"Runtime scope requiring a Workspace export", []Plugin{
			declares("w", ScopeWorkspace, "w.g"),
			requires("r", ScopeRuntime, Spec[greeter]("w.g")),
		}},
		{"Workspace scope requiring an Agent export", []Plugin{
			declares("ag", ScopeAgent, "ag.g"),
			requires("w", ScopeWorkspace, Spec[greeter]("ag.g")),
		}},
		{"dependency cycle within one scope", []Plugin{
			requires("a", ScopeWorkspace, Spec[greeter]("b.own")),
			requires("b", ScopeWorkspace, Spec[greeter]("a.own")),
		}},
		{"self dependency", []Plugin{requires("a", ScopeWorkspace, Spec[greeter]("a.own"))}},
		{"Operation-scoped PreparationHook interface export", []Plugin{provides("a", ScopeOperation, Spec[PreparationHook]("hook"))}},
		{"Agent-scoped PreparationHook interface export", []Plugin{provides("a", ScopeAgent, Spec[PreparationHook]("hook"))}},
		{"Operation-scoped concrete hook export", []Plugin{provides("a", ScopeOperation, Spec[hookValue]("hook"))}},
		{"Agent-scoped pointer hook export", []Plugin{provides("a", ScopeAgent, Spec[*ptrHookValue]("hook"))}},
	} {
		t.Run(row.name, func(t *testing.T) {
			opened = 0
			_, err := newComposition(row.plugins)
			if !errors.Is(err, ErrComposition) {
				t.Fatalf("newComposition error = %v, want ErrComposition", err)
			}
			if opened != 0 {
				t.Fatalf("%d factories ran for a rejected declaration set; validation must precede every factory", opened)
			}
		})
	}
}

// TestCompositionAcceptsLongLivedHookDeclarations proves the hook-declaration
// lifetime row: PreparationHook interface and both implementing concrete
// types compose at Runtime and Workspace scope, ordinary short-scope exports
// stay valid beside them, and the nearest forbidden sibling — a hook-typed
// export at Operation scope — rejects the whole set before any factory.
func TestCompositionAcceptsLongLivedHookDeclarations(t *testing.T) {
	hookPlugin := func(id string, scope ScopeKind, specs ...CapabilitySpec) Plugin {
		return Plugin{ID: id, Scope: scope, Provides: specs, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{}}, nil
		}}
	}
	plugins := []Plugin{
		hookPlugin("rt", ScopeRuntime, Spec[PreparationHook]("hook.iface")),
		hookPlugin("ws", ScopeWorkspace, Spec[hookValue]("hook.value"), Spec[*ptrHookValue]("hook.ptr")),
		hookPlugin("op", ScopeOperation, Spec[greeter]("op.g")),
		hookPlugin("ag", ScopeAgent, Spec[ptrHookValue]("ag.byvalue"), Spec[greeter]("ag.g")),
	}
	c := mustComposition(t, plugins...)
	for _, kind := range []ScopeKind{ScopeRuntime, ScopeWorkspace, ScopeOperation, ScopeAgent} {
		if got := len(c.plan[kind]); got != 1 {
			t.Fatalf("%s plan holds %d plugins, want 1", kind, got)
		}
	}
	ids := append([]string(nil), c.capabilityIDs...)
	slices.Sort(ids)
	if want := []string{"ag.byvalue", "ag.g", "hook.iface", "hook.ptr", "hook.value", "op.g"}; !slices.Equal(ids, want) {
		t.Fatalf("capability universe = %v, want every export including the ordinary short-scope ones", ids)
	}
	forbidden := append(append([]Plugin(nil), plugins...), hookPlugin("op-hook", ScopeOperation, Spec[hookValue]("hook.op")))
	if _, err := newComposition(forbidden); !errors.Is(err, ErrComposition) {
		t.Fatalf("newComposition with an Operation-scoped hook export = %v, want ErrComposition", err)
	}
}

func TestCompositionAcceptsLongerLivedAndSameScopeDependencies(t *testing.T) {
	c := mustComposition(t,
		Plugin{ID: "rt", Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[int]("rt.n")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"rt.n": 1}}, nil
		}},
		Plugin{ID: "wa", Scope: ScopeWorkspace, Requires: []CapabilitySpec{Spec[int]("rt.n")}, Provides: []CapabilitySpec{Spec[greeter]("wa.g")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"wa.g": loudGreeter{}}}, nil
		}},
		Plugin{ID: "wb", Scope: ScopeWorkspace, Requires: []CapabilitySpec{Spec[greeter]("wa.g")}, Provides: []CapabilitySpec{Spec[greeter]("wb.g")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"wb.g": loudGreeter{}}}, nil
		}},
		Plugin{ID: "ag", Scope: ScopeAgent, Requires: []CapabilitySpec{Spec[greeter]("wa.g"), Spec[greeter]("wb.g"), Spec[int]("rt.n")}, Provides: []CapabilitySpec{Spec[greeter]("ag.g")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"ag.g": loudGreeter{}}}, nil
		}},
	)
	if got := len(c.plan[ScopeWorkspace]); got != 2 {
		t.Fatalf("workspace plan holds %d plugins, want 2", got)
	}
	if c.plan[ScopeWorkspace][0].ID != "wa" || c.plan[ScopeWorkspace][1].ID != "wb" {
		t.Fatalf("workspace plan = %s before %s, want the provider first", c.plan[ScopeWorkspace][0].ID, c.plan[ScopeWorkspace][1].ID)
	}
}

func TestCompositionConstructsDependenciesFirstInStableOrder(t *testing.T) {
	trace := &traceLog{}
	rec := func(id string, requires []CapabilitySpec, providesID string) Plugin {
		return Plugin{ID: id, Scope: ScopeRuntime, Requires: requires, Provides: []CapabilitySpec{Spec[greeter](providesID)}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			trace.add("open:" + id)
			return Instance{Values: map[string]any{providesID: loudGreeter{word: id}}, Close: func() error {
				trace.add("close:" + id)
				return nil
			}}, nil
		}}
	}
	// Registration order late, early, ties: the topological order must move
	// the provider first while ties keep registration order.
	c := mustComposition(t,
		rec("late", []CapabilitySpec{Spec[greeter]("early.shared")}, "late.out"),
		rec("early", nil, "early.shared"),
		rec("ties", []CapabilitySpec{Spec[greeter]("early.shared")}, "ties.out"),
	)
	sc := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	want := []string{"open:early", "open:late", "open:ties"}
	if got := trace.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("construction order = %q, want %q", got, want)
	}
	if err := sc.close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	want = append(want, "close:ties", "close:late", "close:early")
	if got := trace.all(); !reflect.DeepEqual(got, want) {
		t.Fatalf("close order = %q, want reverse of construction %q", got, want)
	}
}

func TestScopeBindingsCarryOnlyDeclaredDependencies(t *testing.T) {
	var captured Bindings
	c := mustComposition(t,
		Plugin{ID: "rt", Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[greeter]("rt.g"), Spec[int]("rt.n"), Spec[counted]("rt.c")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{
				"rt.g": loudGreeter{word: "hi"},
				"rt.n": 0,
				"rt.c": (*counterService)(nil),
			}}, nil
		}},
		Plugin{ID: "unneeded", Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[greeter]("unneeded.g")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"unneeded.g": loudGreeter{}}}, nil
		}},
		Plugin{ID: "cons", Scope: ScopeWorkspace, Requires: []CapabilitySpec{Spec[greeter]("rt.g"), Spec[int]("rt.n"), Spec[counted]("rt.c")}, Provides: []CapabilitySpec{Spec[greeter]("cons.out")}, Open: func(_ context.Context, _ ScopeInfo, deps Bindings) (Instance, error) {
			captured = deps
			return Instance{Values: map[string]any{"cons.out": loudGreeter{}}}, nil
		}},
	)
	rt := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	ws := mustOpenScope(t, c, context.Background(), workspaceScopeInfo("/ws/one"), []*scope{rt})

	if len(captured.entries) != 3 || len(ws.bindings.entries) != 1 {
		t.Fatalf("consumer received %d entries (want its 3 declared), scope exports %d (want its 1)", len(captured.entries), len(ws.bindings.entries))
	}
	if g, err := Bind[greeter](captured, "rt.g"); err != nil || g.Greet() != "HI" {
		t.Fatalf("Bind assignable dependency: err = %v, value = %v", err, g)
	}
	if n, err := Bind[int](captured, "rt.n"); err != nil || n != 0 {
		t.Fatalf("Bind zero concrete value: err = %v, value = %d, want the ordinary zero value accepted", err, n)
	}
	if _, err := Bind[counted](captured, "rt.c"); err != nil {
		t.Fatalf("Bind typed nil dependency: %v, want the ordinary type rule to accept it", err)
	}
	if _, err := Bind[greeter](captured, "rt.hidden"); !errors.Is(err, ErrComposition) {
		t.Errorf("Bind undeclared capability: %v, want ErrComposition (no ambient lookup)", err)
	}
	if _, err := Bind[greeter](captured, "unneeded.g"); !errors.Is(err, ErrComposition) {
		t.Errorf("Bind another plugin's export: %v, want ErrComposition (binding isolation)", err)
	}
	if _, err := Bind[counted](captured, "rt.g"); !errors.Is(err, ErrComposition) {
		t.Errorf("Bind wrong requested type: %v, want ErrComposition", err)
	}
	if _, err := Bind[greeter](Bindings{}, "rt.g"); !errors.Is(err, ErrComposition) {
		t.Errorf("Bind on zero Bindings: %v, want ErrComposition", err)
	}
}

func TestScopeConstructionFailuresRollBackWithoutPublishing(t *testing.T) {
	good := func(trace *traceLog, id string, closeErr error) Plugin {
		return Plugin{ID: id, Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[greeter](id + ".g")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			trace.add("open:" + id)
			return Instance{Values: map[string]any{id + ".g": loudGreeter{word: id}}, Close: func() error {
				trace.add("close:" + id)
				return closeErr
			}}, nil
		}}
	}
	plain := func(trace *traceLog, id string) Plugin {
		return Plugin{ID: id, Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[greeter](id + ".g")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			trace.add("open:" + id)
			return Instance{Values: map[string]any{id + ".g": loudGreeter{word: id}}}, nil
		}}
	}
	failing := func(trace *traceLog, id string) Plugin {
		return Plugin{ID: id, Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[greeter](id + ".g")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			trace.add("open:" + id)
			return Instance{}, errFactory
		}}
	}
	invalid := func(trace *traceLog, id string, values map[string]any) Plugin {
		return Plugin{ID: id, Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[greeter](id + ".g")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			trace.add("open:" + id)
			return Instance{Values: values, Close: func() error { trace.add("close:" + id); return nil }}, nil
		}}
	}
	for _, row := range []struct {
		name        string
		plugins     func(trace *traceLog, cancelOwner context.CancelFunc) []Plugin
		cancelFirst bool
		wantEvents  []string
		wantIs      []error
	}{
		{"first factory fails with nothing disposed", func(_ *traceLog, _ context.CancelFunc) []Plugin {
			return []Plugin{failing(&traceLog{}, "bad"), good(&traceLog{}, "one", nil)}
		}, false, nil, []error{errFactory}},
		{"later factory fails and disposes the constructed prefix", func(trace *traceLog, _ context.CancelFunc) []Plugin {
			return []Plugin{good(trace, "one", errCloser), plain(trace, "two"), failing(trace, "bad")}
		}, false, []string{"open:one", "open:two", "open:bad", "close:one"}, []error{errFactory, errCloser}},
		{"successful invalid instance uses the same cleanup including its own closer", func(trace *traceLog, _ context.CancelFunc) []Plugin {
			return []Plugin{good(trace, "one", nil), good(trace, "two", errCloser), invalid(trace, "bad", map[string]any{"other": loudGreeter{}})}
		}, false, []string{"open:one", "open:two", "open:bad", "close:bad", "close:two", "close:one"}, []error{ErrComposition, errCloser}},
		{"missing declared export is rejected", func(trace *traceLog, _ context.CancelFunc) []Plugin {
			return []Plugin{good(trace, "one", nil), invalid(trace, "bad", map[string]any{})}
		}, false, []string{"open:one", "open:bad", "close:bad", "close:one"}, []error{ErrComposition}},
		{"untyped nil export supplies no verifiable type", func(trace *traceLog, _ context.CancelFunc) []Plugin {
			return []Plugin{good(trace, "one", nil), invalid(trace, "bad", map[string]any{"bad.g": nil})}
		}, false, []string{"open:one", "open:bad", "close:bad", "close:one"}, []error{ErrComposition}},
		{"export with a wrong dynamic type is rejected", func(trace *traceLog, _ context.CancelFunc) []Plugin {
			return []Plugin{good(trace, "one", nil), invalid(trace, "bad", map[string]any{"bad.g": "not a greeter"})}
		}, false, []string{"open:one", "open:bad", "close:bad", "close:one"}, []error{ErrComposition}},
		{"cancellation observed between factories aborts later opens", func(trace *traceLog, cancelOwner context.CancelFunc) []Plugin {
			return []Plugin{
				Plugin{ID: "one", Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[greeter]("one.g")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
					trace.add("open:one")
					cancelOwner()
					return Instance{Values: map[string]any{"one.g": loudGreeter{}}, Close: func() error { trace.add("close:one"); return nil }}, nil
				}},
				good(trace, "two", nil),
			}
		}, false, []string{"open:one", "close:one"}, []error{context.Canceled}},
		{"canceled before any factory opens nothing", func(_ *traceLog, _ context.CancelFunc) []Plugin {
			return []Plugin{good(&traceLog{}, "one", nil)}
		}, true, nil, []error{context.Canceled}},
	} {
		t.Run(row.name, func(t *testing.T) {
			trace := &traceLog{}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c, err := newComposition(row.plugins(trace, cancel))
			if err != nil {
				t.Fatalf("newComposition: %v", err)
			}
			if row.cancelFirst {
				cancel()
			}
			sc, err := c.openScope(ctx, runtimeScopeInfo(), nil)
			if sc != nil {
				t.Fatalf("failed construction returned a scope")
			}
			for _, want := range row.wantIs {
				if !errors.Is(err, want) {
					t.Errorf("error = %v, want identity %v", err, want)
				}
			}
			if got := trace.all(); !reflect.DeepEqual(got, append([]string(nil), row.wantEvents...)) {
				t.Fatalf("disposal trace = %q, want %q", got, row.wantEvents)
			}
		})
	}
}

func TestScopeAcceptsZeroTypedAndCoreStorageValues(t *testing.T) {
	c := mustComposition(t,
		Plugin{ID: "ok", Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[int]("ok.n"), Spec[counted]("ok.c"), Spec[harness.Storage]("ok.store")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"ok.n": 0, "ok.c": (*counterService)(nil), "ok.store": stubCoreStorage{}}}, nil
		}},
	)
	if len(c.capabilityIDs) != 2 || c.capabilityIDs[0] != "ok.n" || c.capabilityIDs[1] != "ok.c" {
		t.Fatalf("capability universe = %q, want the ordinary IDs without the Core storage export", c.capabilityIDs)
	}
	if len(c.coreExports) != 1 || c.coreExports[0].id != "ok.store" || c.coreExports[0].plugin != "ok" || c.coreExports[0].scope != ScopeRuntime {
		t.Fatalf("Core storage declarations = %+v, want the one Runtime export", c.coreExports)
	}
	sc := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	if _, ok := sc.storages["ok.store"].(stubCoreStorage); !ok {
		t.Fatalf("Core storage resolution = %#v, want the supplied stub", sc.storages)
	}
	if _, leaked := sc.bindings.entries["ok.store"]; leaked {
		t.Fatalf("Core storage export leaked into ordinary bindings")
	}
	if _, err := selectCapabilities([]*scope{sc}, []string{"ok.store"}); !errors.Is(err, ErrComposition) {
		t.Errorf("selecting the Core storage export: %v, want ErrComposition", err)
	}
	if _, err := selectCapabilities([]*scope{sc}, []string{"ok.n", "ok.c"}); err != nil {
		t.Errorf("selecting ordinary exports: %v", err)
	}
}

func TestCompositionRejectsScopeInfoShapes(t *testing.T) {
	c := mustComposition(t,
		Plugin{ID: "ok", Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[int]("ok.n")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"ok.n": 1}}, nil
		}},
	)
	base := ScopeInfo{Kind: ScopeRuntime, DataDir: "/owner/data"}
	mutate := func(apply func(*ScopeInfo)) ScopeInfo {
		info := base
		apply(&info)
		return info
	}
	for _, row := range []struct {
		name string
		info ScopeInfo
	}{
		{"missing data root", mutate(func(i *ScopeInfo) { i.DataDir = "" })},
		{"runtime scope with workspace attribution", mutate(func(i *ScopeInfo) { i.Workspace = "/ws" })},
		{"workspace scope without a workspace", mutate(func(i *ScopeInfo) { i.Kind = ScopeWorkspace })},
		{"workspace scope with session attribution", mutate(func(i *ScopeInfo) { i.Kind = ScopeWorkspace; i.Workspace = "/ws"; i.SessionID = "s" })},
		{"operation scope without full attribution", mutate(func(i *ScopeInfo) { i.Kind = ScopeOperation; i.Workspace = "/ws"; i.SessionID = "s" })},
		{"agent scope without full attribution", mutate(func(i *ScopeInfo) { i.Kind = ScopeAgent; i.Workspace = "/ws"; i.OperationID = "o" })},
		{"unknown kind", mutate(func(i *ScopeInfo) { i.Kind = ScopeKind("project") })},
	} {
		t.Run(row.name, func(t *testing.T) {
			if _, err := c.openScope(context.Background(), row.info, nil); !errors.Is(err, ErrComposition) {
				t.Fatalf("openScope error = %v, want ErrComposition", err)
			}
		})
	}
	if _, err := c.openScope(context.Background(), ScopeInfo{Kind: ScopeOperation, DataDir: "/owner/data", Workspace: "/ws", SessionID: "s", OperationID: "o"}, nil); err != nil {
		t.Fatalf("fully attributed short scope: %v", err)
	}
}

type guardFixture struct {
	rt       *scope
	ws       *scope
	trace    *traceLog
	consDeps Bindings
}

func openGuardFixture(t *testing.T, owner context.Context) *guardFixture {
	t.Helper()
	f := &guardFixture{trace: &traceLog{}}
	c := mustComposition(t,
		Plugin{ID: "dep", Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[greeter]("dep.g")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"dep.g": loudGreeter{word: "hi"}}}, nil
		}},
		Plugin{ID: "cons", Scope: ScopeWorkspace, Requires: []CapabilitySpec{Spec[greeter]("dep.g")}, Provides: []CapabilitySpec{Spec[int]("cons.out")}, Open: func(_ context.Context, _ ScopeInfo, deps Bindings) (Instance, error) {
			f.consDeps = deps
			return Instance{Values: map[string]any{"cons.out": 7}, Close: func() error { f.trace.add("close:cons"); return errCloser }}, nil
		}},
	)
	f.rt = mustOpenScope(t, c, owner, runtimeScopeInfo(), nil)
	f.ws = mustOpenScope(t, c, owner, workspaceScopeInfo("/ws/guard"), []*scope{f.rt})
	return f
}

// startBlockedCall enters one guard and runs a call that keeps working until
// released, resolving its native ancestor dependency inside the call.
func startBlockedCall(t *testing.T, f *guardFixture) (gctx context.Context, released, done chan struct{}) {
	t.Helper()
	gctx, release, err := f.ws.enter(context.Background())
	if err != nil {
		t.Fatalf("guard entry: %v", err)
	}
	started := make(chan struct{})
	released = make(chan struct{})
	done = make(chan struct{})
	go func() {
		defer release()
		f.trace.add("call-start")
		close(started)
		<-released
		if v, err := Bind[greeter](f.consDeps, "dep.g"); err != nil {
			f.trace.add("bind-failed")
		} else {
			f.trace.add("greet:" + v.Greet())
		}
		f.trace.add("call-end")
		close(done)
	}()
	<-started
	return gctx, released, done
}

func TestScopeGuardOwnsAdmittedCallsThroughClose(t *testing.T) {
	t.Run("closure joins the admitted call before disposing", func(t *testing.T) {
		f := openGuardFixture(t, context.Background())
		gctx, released, done := startBlockedCall(t, f)
		closeResult := make(chan error, 1)
		go func() { closeResult <- f.ws.close() }()
		select {
		case <-gctx.Done():
		case <-time.After(2 * time.Second):
			t.Fatal("scope closure did not cancel the admitted call context")
		}
		// The dependent's supplying dependency lives in the still-open
		// Runtime scope, so the guarded call must succeed after closure
		// began; a dispose-without-join would close while the call is
		// blocked and land in this window.
		time.Sleep(50 * time.Millisecond)
		if got := f.trace.all(); !slices.Contains(got, "call-start") || slices.Contains(got, "close:cons") {
			t.Errorf("state during the blocked call = %q, want the admitted call not yet disposed", got)
		}
		close(released)
		<-done
		firstErr := <-closeResult
		if !errors.Is(firstErr, errCloser) {
			t.Fatalf("close = %v, want the retained cleanup error", firstErr)
		}
		want := []string{"call-start", "greet:HI", "call-end", "close:cons"}
		if got := f.trace.all(); !reflect.DeepEqual(got, want) {
			t.Fatalf("guard trace = %q, want %q", got, want)
		}
		if _, _, err := f.ws.enter(context.Background()); !errors.Is(err, ErrClosed) {
			t.Errorf("enter after closure: %v, want ErrClosed", err)
		}
		if _, err := Bind[int](f.ws.bindings, "cons.out"); !errors.Is(err, ErrClosed) {
			t.Errorf("binding from the closed supplying scope: %v, want ErrClosed", err)
		}
		if _, err := Bind[greeter](f.rt.bindings, "dep.g"); err != nil {
			t.Errorf("binding from the still-open supplying scope: %v", err)
		}
		if err := f.ws.close(); err != firstErr {
			t.Errorf("repeated close: %v, want the retained shared result", err)
		}
		if err := f.rt.close(); err != nil {
			t.Errorf("runtime close: %v", err)
		}
	})

	t.Run("private close callers retain the completed cleanup result", func(t *testing.T) {
		synctest.Test(t, func(tt *testing.T) {
			f := openGuardFixture(tt, context.Background())
			_, released, done := startBlockedCall(tt, f)
			letGo := func() {
				if released != nil {
					close(released)
					released = nil
				}
			}
			defer letGo() // held work can unwind even when an assertion fails
			first := make(chan error, 1)
			go func() { first <- f.ws.close() }()
			// Quiescence: the admitted call is durably blocked on the
			// fixture's release channel, so the first close can only have
			// reached its join of that call.
			synctest.Wait()
			select {
			case err := <-first:
				tt.Fatalf("first close completed without joining the admitted call: %v", err)
			default:
			}
			// A second caller may wait on Once's mutex, which synctest does
			// not treat as durably blocked. Release the call before joining
			// the results; this checks retention, not overlap inside Once.
			second := make(chan error, 1)
			go func() { second <- f.ws.close() }()
			letGo()
			<-done
			firstErr := <-first
			if !errors.Is(firstErr, errCloser) {
				tt.Fatalf("first close = %v, want the retained cleanup error", firstErr)
			}
			// The joined caller returns the same completed result, and the
			// exact trace proves one join and one dispose.
			if err := <-second; err != firstErr {
				tt.Fatalf("joined close = %v, want the same completed result", err)
			}
			want := []string{"call-start", "greet:HI", "call-end", "close:cons"}
			if got := f.trace.all(); !reflect.DeepEqual(got, want) {
				tt.Fatalf("guard trace = %q, want %q", got, want)
			}
		})
	})

	t.Run("owner cancellation before admission makes the scope unavailable", func(t *testing.T) {
		owner, cancelOwner := context.WithCancel(context.Background())
		f := openGuardFixture(t, owner)
		cancelOwner()
		if _, _, err := f.ws.enter(context.Background()); !errors.Is(err, ErrClosed) {
			t.Errorf("enter on an owner-canceled scope: %v, want ErrClosed", err)
		}
		if _, _, err := f.rt.enter(context.Background()); !errors.Is(err, ErrClosed) {
			t.Errorf("enter on an owner-canceled ancestor scope: %v, want ErrClosed", err)
		}
		if _, err := Bind[int](f.ws.bindings, "cons.out"); !errors.Is(err, ErrClosed) {
			t.Errorf("Bind from an owner-canceled supplying scope: %v, want ErrClosed", err)
		}
		if _, err := Bind[greeter](f.consDeps, "dep.g"); !errors.Is(err, ErrClosed) {
			t.Errorf("Bind from an owner-canceled ancestor supplying scope: %v, want ErrClosed", err)
		}
		// Rejected admissions registered no work, so closure joins and
		// disposes synchronously with no registered call to wait on.
		if err := f.ws.close(); !errors.Is(err, errCloser) {
			t.Errorf("close after rejected admissions: %v, want prompt completion with the shared cleanup error", err)
		}
	})
}

// workspaceFixture drives shared Workspace construction with per-key gates:
// held keys block their factories on a test-owned channel, other keys
// proceed, and a fail switch produces shared attempt failures.
type workspaceFixture struct {
	w     *workspaceScopes
	rt    *scope
	trace *traceLog

	stopOwner context.CancelFunc

	mu      sync.Mutex
	gates   map[string]chan struct{}
	entered map[string]chan struct{}
	opens   map[string]int
	failNow atomic.Bool
}

func newWorkspaceFixture(t *testing.T) *workspaceFixture {
	t.Helper()
	f := &workspaceFixture{
		trace:   &traceLog{},
		gates:   make(map[string]chan struct{}),
		entered: make(map[string]chan struct{}),
		opens:   make(map[string]int),
	}
	c := mustComposition(t,
		Plugin{ID: "rt", Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[int]("rt.n")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"rt.n": 42}}, nil
		}},
		Plugin{ID: "ws", Scope: ScopeWorkspace, Requires: []CapabilitySpec{Spec[int]("rt.n")}, Provides: []CapabilitySpec{Spec[greeter]("ws.g")}, Open: func(ctx context.Context, info ScopeInfo, deps Bindings) (Instance, error) {
			n, err := Bind[int](deps, "rt.n")
			if err != nil || n != 42 {
				return Instance{}, fmt.Errorf("ancestor dependency: %v %d", err, n)
			}
			f.mu.Lock()
			f.opens[info.Workspace]++
			gate := f.gates[info.Workspace]
			entered := f.entered[info.Workspace]
			f.mu.Unlock()
			f.trace.add("open:" + info.Workspace)
			if entered != nil {
				select {
				case entered <- struct{}{}:
				default:
				}
			}
			if gate != nil {
				select {
				case <-gate:
				case <-ctx.Done():
					f.trace.add("canceled:" + info.Workspace)
					return Instance{}, ctx.Err()
				}
			}
			if f.failNow.Load() {
				return Instance{}, errFactory
			}
			key := info.Workspace
			return Instance{Values: map[string]any{"ws.g": loudGreeter{word: key}}, Close: func() error {
				f.trace.add("close:" + key)
				return errWorkspaceClo
			}}, nil
		}},
	)
	owner, stop := context.WithCancel(context.Background())
	f.stopOwner = stop
	t.Cleanup(stop)
	f.rt = mustOpenScope(t, c, owner, runtimeScopeInfo(), nil)
	f.w = newWorkspaceScopes(owner, c, []*scope{f.rt}, newObservation())
	return f
}

// hold registers a gate for one Workspace key and returns its release
// channel plus a buffered entry signal the factory sends on every attempt.
func (f *workspaceFixture) hold(key string) (release chan struct{}, entered chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	release = make(chan struct{})
	entered = make(chan struct{}, 4)
	f.gates[key] = release
	f.entered[key] = entered
	return release, entered
}

func (f *workspaceFixture) openCount(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens[key]
}

type getOutcome struct {
	sc  *scope
	err error
}

func (f *workspaceFixture) getAsync(ctx context.Context, key string) chan getOutcome {
	res := make(chan getOutcome, 1)
	go func() {
		sc, err := f.w.get(ctx, workspaceScopeInfo(key))
		res <- getOutcome{sc, err}
	}()
	return res
}

func TestWorkspaceScopeAttemptsAreSharedPerKey(t *testing.T) {
	t.Run("same key joins one construction", func(t *testing.T) {
		f := newWorkspaceFixture(t)
		release, entered := f.hold("/ws/x")
		res1 := f.getAsync(context.Background(), "/ws/x")
		<-entered
		res2 := f.getAsync(context.Background(), "/ws/x")
		close(release)
		r1, r2 := <-res1, <-res2
		if r1.err != nil || r2.err != nil {
			t.Fatalf("shared construction errors: %v, %v", r1.err, r2.err)
		}
		if r1.sc != r2.sc || r1.sc == nil {
			t.Fatalf("same-key callers received %p and %p, want one shared scope", r1.sc, r2.sc)
		}
		if got := f.openCount("/ws/x"); got != 1 {
			t.Fatalf("workspace factory ran %d times, want 1", got)
		}
	})

	t.Run("other keys proceed while one is held", func(t *testing.T) {
		f := newWorkspaceFixture(t)
		release, entered := f.hold("/ws/a")
		resA := f.getAsync(context.Background(), "/ws/a")
		<-entered
		scB, err := f.w.get(context.Background(), workspaceScopeInfo("/ws/b"))
		if err != nil || scB == nil {
			t.Fatalf("unrelated key: %v", err)
		}
		close(release)
		rA := <-resA
		if rA.err != nil || rA.sc == scB {
			t.Fatalf("held key: %v %p", rA.err, rA.sc)
		}
	})

	t.Run("initiator cancellation preserves the attempt for surviving waiters", func(t *testing.T) {
		f := newWorkspaceFixture(t)
		release, entered := f.hold("/ws/c")
		ctx1, cancel1 := context.WithCancel(context.Background())
		res1 := f.getAsync(ctx1, "/ws/c")
		<-entered
		cancel1()
		res2 := f.getAsync(context.Background(), "/ws/c")
		if r := <-res1; !errors.Is(r.err, context.Canceled) || r.sc != nil {
			t.Fatalf("canceled initiator: %v %p", r.err, r.sc)
		}
		close(release)
		r2 := <-res2
		if r2.err != nil || r2.sc == nil {
			t.Fatalf("surviving waiter: %v", r2.err)
		}
		sc3, err := f.w.get(context.Background(), workspaceScopeInfo("/ws/c"))
		if err != nil || sc3 != r2.sc {
			t.Fatalf("later same-key request: %v %p vs %p, want the published scope", err, sc3, r2.sc)
		}
		if got := f.openCount("/ws/c"); got != 1 {
			t.Fatalf("workspace factory ran %d times after the shared attempt, want 1", got)
		}
	})

	t.Run("failed attempt shares one result and a later request retries", func(t *testing.T) {
		f := newWorkspaceFixture(t)
		release, entered := f.hold("/ws/d")
		f.failNow.Store(true)
		res1 := f.getAsync(context.Background(), "/ws/d")
		<-entered
		res2 := f.getAsync(context.Background(), "/ws/d")
		close(release)
		r1, r2 := <-res1, <-res2
		if !errors.Is(r1.err, errFactory) || !errors.Is(r2.err, errFactory) || r1.sc != nil || r2.sc != nil {
			t.Fatalf("shared failure: %v vs %v", r1.err, r2.err)
		}
		f.w.mu.Lock()
		published := len(f.w.live) > 0 || len(f.w.attempts) > 0
		f.w.mu.Unlock()
		if published {
			t.Fatalf("failed attempt published state: live %d attempts %d", len(f.w.live), len(f.w.attempts))
		}
		// Only a later independent request may try again: gate the retry
		// and prove exactly one fresh attempt starts.
		f.failNow.Store(false)
		release2, entered2 := f.hold("/ws/d")
		before := f.openCount("/ws/d")
		res3 := f.getAsync(context.Background(), "/ws/d")
		<-entered2
		if got := f.openCount("/ws/d"); got != before+1 {
			t.Fatalf("retry started %d constructions, want exactly one", got-before)
		}
		close(release2)
		r3 := <-res3
		if r3.err != nil || r3.sc == nil {
			t.Fatalf("later independent attempt: %v", r3.err)
		}
		sc4, err := f.w.get(context.Background(), workspaceScopeInfo("/ws/d"))
		if err != nil || sc4 != r3.sc {
			t.Fatalf("request after the successful retry: %v, want the published retry scope", err)
		}
	})
}

func TestWorkspaceScopesShutdownJoinsConstructionAndDisposesSorted(t *testing.T) {
	t.Run("shutdown joins the in-flight construction before returning", func(t *testing.T) {
		synctest.Test(t, func(tt *testing.T) {
			f := newWorkspaceFixture(tt)
			_, entered := f.hold("/ws/j")
			resJ := f.getAsync(context.Background(), "/ws/j")
			<-entered
			shutdownResult := make(chan error, 1)
			go func() { shutdownResult <- f.w.shutdown() }()
			// The held factory gate, the construction waiter and the
			// shutdown's construction join are all durable blocks, so
			// quiescence here can only mean the shutdown has reached its
			// join — and it must not have returned yet.
			synctest.Wait()
			select {
			case err := <-shutdownResult:
				tt.Fatalf("shutdown returned before the in-flight construction converged: %v", err)
			default:
			}
			f.stopOwner()
			if r := <-resJ; !errors.Is(r.err, context.Canceled) || r.sc != nil {
				tt.Fatalf("held construction after owner cancellation: %v %p", r.err, r.sc)
			}
			if err := <-shutdownResult; err != nil {
				tt.Fatalf("joined shutdown: %v", err)
			}
			if _, err := f.w.get(context.Background(), workspaceScopeInfo("/ws/new")); !errors.Is(err, ErrClosed) {
				tt.Fatalf("get after shutdown admission closed: %v, want ErrClosed", err)
			}
			if got := f.trace.all(); slices.Contains(got, "close:/ws/j") {
				tt.Fatalf("an unpublished construction was disposed: %q", got)
			}
		})
	})

	t.Run("live workspaces close in sorted order and errors join", func(t *testing.T) {
		f := newWorkspaceFixture(t)
		for _, key := range []string{"/ws/2", "/ws/1"} {
			if _, err := f.w.get(context.Background(), workspaceScopeInfo(key)); err != nil {
				t.Fatalf("get %s: %v", key, err)
			}
		}
		err := f.w.shutdown()
		if !errors.Is(err, errWorkspaceClo) {
			t.Fatalf("shutdown error = %v, want the joined closer failures", err)
		}
		got := f.trace.all()
		first, second := slices.Index(got, "close:/ws/1"), slices.Index(got, "close:/ws/2")
		if first < 0 || second < 0 || first > second {
			t.Fatalf("workspace close order = %q, want /ws/1 before /ws/2", got)
		}
		if err2 := f.w.shutdown(); !errors.Is(err2, errWorkspaceClo) {
			t.Fatalf("repeated shutdown: %v, want the same shared result", err2)
		}
		if _, err := f.w.get(context.Background(), workspaceScopeInfo("/ws/1")); !errors.Is(err, ErrClosed) {
			t.Fatalf("get after shutdown: %v, want ErrClosed", err)
		}
	})
}

func TestInvocationCarriesOneRevisionAcrossNestedDependencyCalls(t *testing.T) {
	c := mustComposition(t,
		Plugin{ID: "beta", Scope: ScopeWorkspace, Provides: []CapabilitySpec{Spec[worker]("beta.work")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"beta.work": betaWorker{}}}, nil
		}},
		Plugin{ID: "alpha", Scope: ScopeWorkspace, Requires: []CapabilitySpec{Spec[worker]("beta.work")}, Provides: []CapabilitySpec{Spec[worker]("alpha.work")}, Open: func(_ context.Context, _ ScopeInfo, deps Bindings) (Instance, error) {
			dep, err := Bind[worker](deps, "beta.work")
			if err != nil {
				return Instance{}, err
			}
			return Instance{Values: map[string]any{"alpha.work": alphaWorker{dep: dep}}}, nil
		}},
	)
	ws := mustOpenScope(t, c, context.Background(), workspaceScopeInfo("/ws/config"), nil)
	view, err := selectCapabilities([]*scope{ws}, []string{"alpha.work"})
	if err != nil {
		t.Fatalf("selectCapabilities: %v", err)
	}
	a, err := Bind[worker](view, "alpha.work")
	if err != nil {
		t.Fatalf("Bind selected: %v", err)
	}
	second, err := selectCapabilities([]*scope{ws}, []string{"alpha.work"})
	if err != nil {
		t.Fatalf("second selection: %v", err)
	}
	a2, err := Bind[worker](second, "alpha.work")
	if err != nil {
		t.Fatalf("second Bind: %v", err)
	}
	if a != a2 {
		t.Fatalf("two selected views supplied different instances, want the reused ones")
	}
	if _, err := Bind[worker](view, "beta.work"); !errors.Is(err, ErrComposition) {
		t.Errorf("Bind unselected dependency: %v, want ErrComposition", err)
	}
	if _, err := selectCapabilities([]*scope{ws}, []string{"nope.work"}); !errors.Is(err, ErrComposition) {
		t.Errorf("select unknown capability: %v, want ErrComposition", err)
	}

	revisionOne := &configuration{generation: 1, plugins: map[string]json.RawMessage{
		"alpha": json.RawMessage(`{"x":1}`),
		"beta":  json.RawMessage(`{"y":2}`),
	}}
	revisionTwo := &configuration{generation: 2, plugins: map[string]json.RawMessage{
		"alpha": json.RawMessage(`{"x":9}`),
	}}
	invOne := Invocation{snapshot: revisionOne}
	invTwo := Invocation{snapshot: revisionTwo}
	if got, want := a.Work(invOne), `a:1 acfg:{"x":1} | b:1 bcfg:{"y":2}`; got != want {
		t.Fatalf("revision 1 nested call = %q, want %q", got, want)
	}
	if got, want := a.Work(invTwo), `a:2 acfg:{"x":9} | b:2 bcfg:none`; got != want {
		t.Fatalf("revision 2 on the same instance = %q, want %q", got, want)
	}

	var zero Invocation
	if zero.Revision() != "" || zero.Config("alpha") != nil {
		t.Fatalf("zero Invocation = %q, %v", zero.Revision(), zero.Config("alpha"))
	}
	handed := invOne.Config("alpha")
	handed[2] = '0'
	if string(revisionOne.plugins["alpha"]) != `{"x":1}` {
		t.Fatalf("Config handout aliases the snapshot: %s", revisionOne.plugins["alpha"])
	}
	if invOne.Config("missing") != nil {
		t.Errorf("Config for an unconfigured plugin = %v, want nil", invOne.Config("missing"))
	}
}

func TestAcceptSettingsAppliesTheNoSettingsRule(t *testing.T) {
	for _, row := range []struct {
		name string
		raw  json.RawMessage
		ok   bool
	}{
		{"absent", nil, true},
		{"empty", json.RawMessage(``), true},
		{"whitespace", json.RawMessage("  \n"), true},
		{"empty object", json.RawMessage(`{}`), true},
		{"spaced empty object", json.RawMessage(` { } `), true},
		{"null", json.RawMessage(`null`), false},
		{"populated object", json.RawMessage(`{"a":1}`), false},
		{"array", json.RawMessage(`[]`), false},
		{"number", json.RawMessage(`1`), false},
	} {
		err := acceptSettings(Plugin{ID: "plain"}, row.raw)
		if row.ok && err != nil {
			t.Errorf("%s: %v, want acceptance", row.name, err)
		}
		if !row.ok && !errors.Is(err, ErrComposition) {
			t.Errorf("%s: %v, want ErrComposition", row.name, err)
		}
	}
	var seen json.RawMessage
	validator := func(raw json.RawMessage) error {
		seen = raw
		if string(raw) == `null` {
			return nil
		}
		return errValidator
	}
	if err := acceptSettings(Plugin{ID: "cfg", ValidateConfig: validator}, json.RawMessage(`null`)); err != nil {
		t.Fatalf("validator input: %v", err)
	}
	if string(seen) != `null` {
		t.Fatalf("validator saw %s, want the owned input unchanged", seen)
	}
	if err := acceptSettings(Plugin{ID: "cfg", ValidateConfig: validator}, json.RawMessage(`{"a":1}`)); !errors.Is(err, errValidator) {
		t.Fatalf("validator failure = %v, want the source identity", err)
	}
	// A non-nil validator receives an independent clone: in-place scratch
	// never reaches the caller's bytes.
	caller := json.RawMessage(`{"a":"12345"}`)
	if err := acceptSettings(Plugin{ID: "cfg", ValidateConfig: func(got json.RawMessage) error {
		tamperScratch(got, "12345", "xxxxx")
		return nil
	}}, caller); err != nil {
		t.Fatalf("cloned validator input: %v", err)
	}
	if string(caller) != `{"a":"12345"}` {
		t.Fatalf("caller bytes after validator scratch = %s, want unchanged", caller)
	}
}
