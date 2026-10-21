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

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
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
	testassert.NoError(t, err)
	return c
}

func mustOpenScope(t *testing.T, c *composition, ctx context.Context, info ScopeInfo, ancestors []*scope) *scope {
	t.Helper()
	sc, err := c.openScope(ctx, info, ancestors)
	testassert.Require(t, err == nil, "openScope(%s %q): %v", info.Kind, info.Workspace, err)
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
			testassert.Require(t, errors.Is(err, ErrComposition), "newComposition error = %v, want ErrComposition", err)
			testassert.Equal(t, opened, 0)
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
		{
			got := len(c.plan[kind])
			testassert.Require(t, got == 1, "%s plan holds %d plugins, want 1", kind, got)
		}
	}
	ids := append([]string(nil), c.capabilityIDs...)
	slices.Sort(ids)
	{
		want := []string{"ag.byvalue", "ag.g", "hook.iface", "hook.ptr", "hook.value", "op.g"}
		testassert.Require(t, slices.Equal(ids, want), "capability universe = %v, want every export including the ordinary short-scope ones", ids)
	}
	forbidden := append(append([]Plugin(nil), plugins...), hookPlugin("op-hook", ScopeOperation, Spec[hookValue]("hook.op")))
	{
		_, err := newComposition(forbidden)
		testassert.Require(t, errors.Is(err, ErrComposition), "newComposition with an Operation-scoped hook export = %v, want ErrComposition", err)
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
	testassert.Equal(t, len(c.plan[ScopeWorkspace]), 2)
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
	{
		got := trace.all()
		testassert.Require(t, reflect.DeepEqual(got, want), "construction order = %q, want %q", got, want)
	}
	testassert.NoError(t, sc.close())
	want = append(want, "close:ties", "close:late", "close:early")
	{
		got := trace.all()
		testassert.Require(t, reflect.DeepEqual(got, want), "close order = %q, want reverse of construction %q", got, want)
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

	testassert.Require(t, len(captured.entries) == 3 && len(ws.bindings.entries) == 1, "consumer received %d entries (want its 3 declared), scope exports %d (want its 1)", len(captured.entries), len(ws.bindings.entries))
	{
		g, err := Bind[greeter](captured, "rt.g")
		testassert.Require(t, err == nil && g.Greet() == "HI", "Bind assignable dependency: err = %v, value = %v", err, g)
	}
	{
		n, err := Bind[int](captured, "rt.n")
		testassert.Require(t, err == nil && n == 0, "Bind zero concrete value: err = %v, value = %d, want the ordinary zero value accepted", err, n)
	}
	{
		_, err := Bind[counted](captured, "rt.c")
		testassert.NoError(t, err)
	}
	{
		_, err := Bind[greeter](captured, "rt.hidden")
		testassert.Check(t, errors.Is(err, ErrComposition), "Bind undeclared capability: %v, want ErrComposition (no ambient lookup)", err)
	}
	{
		_, err := Bind[greeter](captured, "unneeded.g")
		testassert.Check(t, errors.Is(err, ErrComposition), "Bind another plugin's export: %v, want ErrComposition (binding isolation)", err)
	}
	{
		_, err := Bind[counted](captured, "rt.g")
		testassert.Check(t, errors.Is(err, ErrComposition), "Bind wrong requested type: %v, want ErrComposition", err)
	}
	{
		_, err := Bind[greeter](Bindings{}, "rt.g")
		testassert.Check(t, errors.Is(err, ErrComposition), "Bind on zero Bindings: %v, want ErrComposition", err)
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
			testassert.NoError(t, err)
			if row.cancelFirst {
				cancel()
			}
			sc, err := c.openScope(ctx, runtimeScopeInfo(), nil)
			testassert.Require(t, sc == nil, "failed construction returned a scope")
			for _, want := range row.wantIs {
				testassert.Check(t, errors.Is(err, want), "error = %v, want identity %v", err, want)
			}
			{
				got := trace.all()
				testassert.Require(t, reflect.DeepEqual(got, append([]string(nil), row.wantEvents...)), "disposal trace = %q, want %q", got, row.wantEvents)
			}
		})
	}
}

func TestScopeAcceptsZeroTypedAndCoreSeamValues(t *testing.T) {
	c := mustComposition(t,
		Plugin{ID: "ok", Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[int]("ok.n"), Spec[counted]("ok.c"), Spec[harness.Storage]("ok.store"), Spec[harness.JobStopper]("ok.stop")}, Open: func(context.Context, ScopeInfo, Bindings) (Instance, error) {
			return Instance{Values: map[string]any{"ok.n": 0, "ok.c": (*counterService)(nil), "ok.store": stubCoreStorage{}, "ok.stop": (*stubStopper)(nil)}}, nil
		}},
	)
	testassert.Require(t, len(c.capabilityIDs) == 2 && c.capabilityIDs[0] == "ok.n" && c.capabilityIDs[1] == "ok.c" && len(c.coreExports) == 1 && c.coreExports[0].id == "ok.store" && c.coreExports[0].plugin == "ok" && c.coreExports[0].scope == ScopeRuntime && len(c.jobStoppers) == 1 && c.jobStoppers[0].id == "ok.stop" && c.jobStoppers[0].plugin == "ok" && c.jobStoppers[0].scope == ScopeRuntime, "capability universe = %q, want the ordinary IDs without the Core seam exports; Core storage declarations = %+v, want the one Runtime export; job-stop declarations = %+v, want the one Runtime export", c.capabilityIDs, c.coreExports, c.jobStoppers)
	sc := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	{
		_, ok := sc.storages["ok.store"].(stubCoreStorage)
		testassert.Require(t, ok, "Core storage resolution = %#v, want the supplied stub", sc.storages)
	}
	{
		_, ok := sc.jobStoppers["ok.stop"]
		testassert.Require(t, ok, "job-stop resolution = %#v, want the supplied value", sc.jobStoppers)
	}
	{
		_, leaked := sc.bindings.entries["ok.store"]
		testassert.Require(t, !leaked, "Core storage export leaked into ordinary bindings")
	}
	{
		_, leaked := sc.bindings.entries["ok.stop"]
		testassert.Require(t, !leaked, "job-stop export leaked into ordinary bindings")
	}
	{
		_, err := selectCapabilities([]*scope{sc}, []string{"ok.store"})
		testassert.Check(t, errors.Is(err, ErrComposition), "selecting the Core storage export: %v, want ErrComposition", err)
	}
	{
		_, err := selectCapabilities([]*scope{sc}, []string{"ok.stop"})
		testassert.Check(t, errors.Is(err, ErrComposition), "selecting the job-stop export: %v, want ErrComposition", err)
	}
	{
		_, err := selectCapabilities([]*scope{sc}, []string{"ok.n", "ok.c"})
		testassert.Check(t, err == nil, "selecting ordinary exports: %v", err)
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
			{
				_, err := c.openScope(context.Background(), row.info, nil)
				testassert.Require(t, errors.Is(err, ErrComposition), "openScope error = %v, want ErrComposition", err)
			}
		})
	}
	{
		_, err := c.openScope(context.Background(), ScopeInfo{Kind: ScopeOperation, DataDir: "/owner/data", Workspace: "/ws", SessionID: "s", OperationID: "o"}, nil)
		testassert.NoError(t, err)
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
	testassert.NoError(t, err)
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
		synctest.Test(t, func(t *testing.T) {
			f := openGuardFixture(t, context.Background())
			gctx, released, done := startBlockedCall(t, f)
			defer func() {
				if released != nil {
					close(released)
				}
			}()
			closeResult := make(chan error, 1)
			go func() { closeResult <- f.ws.close() }()
			synctest.Wait()
			testassert.Require(t, errors.Is(gctx.Err(), context.Canceled), "scope closure did not cancel the admitted call context")
			select {
			case err := <-closeResult:
				t.Fatalf("close completed without joining the admitted call: %v", err)
			default:
			}
			{
				got := f.trace.all()
				testassert.Require(t, slices.Contains(got, "call-start") && !slices.Contains(got, "close:cons"), "state during the blocked call = %q, want the admitted call not yet disposed", got)
			}
			close(released)
			released = nil
			<-done
			{
				err := <-closeResult
				testassert.Require(t, errors.Is(err, errCloser), "close = %v, want the cleanup error", err)
			}
			want := []string{"call-start", "greet:HI", "call-end", "close:cons"}
			{
				got := f.trace.all()
				testassert.Require(t, reflect.DeepEqual(got, want), "guard trace = %q, want %q", got, want)
			}
			{
				_, _, err := f.ws.enter(context.Background())
				testassert.Check(t, errors.Is(err, ErrClosed), "enter after closure: %v, want ErrClosed", err)
			}
			{
				_, err := Bind[int](f.ws.bindings, "cons.out")
				testassert.Check(t, errors.Is(err, ErrClosed), "binding from the closed supplying scope: %v, want ErrClosed", err)
			}
			{
				_, err := Bind[greeter](f.rt.bindings, "dep.g")
				testassert.Check(t, err == nil, "binding from the still-open supplying scope: %v", err)
			}
			{
				err := f.rt.close()
				testassert.Check(t, err == nil, "runtime close: %v", err)
			}
		})
	})

	t.Run("owner cancellation before admission makes the scope unavailable", func(t *testing.T) {
		owner, cancelOwner := context.WithCancel(context.Background())
		f := openGuardFixture(t, owner)
		cancelOwner()
		{
			_, _, err := f.ws.enter(context.Background())
			testassert.Check(t, errors.Is(err, ErrClosed), "enter on an owner-canceled scope: %v, want ErrClosed", err)
		}
		{
			_, _, err := f.rt.enter(context.Background())
			testassert.Check(t, errors.Is(err, ErrClosed), "enter on an owner-canceled ancestor scope: %v, want ErrClosed", err)
		}
		{
			_, err := Bind[int](f.ws.bindings, "cons.out")
			testassert.Check(t, errors.Is(err, ErrClosed), "Bind from an owner-canceled supplying scope: %v, want ErrClosed", err)
		}
		{
			_, err := Bind[greeter](f.consDeps, "dep.g")
			testassert.Check(t, errors.Is(err, ErrClosed), "Bind from an owner-canceled ancestor supplying scope: %v, want ErrClosed", err)
		}
		// Rejected admissions registered no work, so closure joins and
		// disposes synchronously with no registered call to wait on.
		{
			err := f.ws.close()
			testassert.Check(t, errors.Is(err, errCloser), "close after rejected admissions: %v, want prompt completion with the shared cleanup error", err)
		}
	})

	t.Run("canceled caller on a live scope keeps its own error", func(t *testing.T) {
		f := openGuardFixture(t, context.Background())
		caller, cancelCaller := context.WithCancel(context.Background())
		cancelCaller()
		{
			_, _, err := f.ws.enter(caller)
			testassert.Check(t, errors.Is(err, context.Canceled), "enter from a canceled caller on a live scope: %v, want context.Canceled", err)
		}
		// Rejected admissions registered no work, so closure joins and
		// disposes synchronously with no registered call to wait on.
		{
			err := f.ws.close()
			testassert.Check(t, errors.Is(err, errCloser), "close after rejected admission: %v, want prompt completion with the shared cleanup error", err)
		}
		{
			err := f.rt.close()
			testassert.Check(t, err == nil, "runtime close: %v", err)
		}
	})

	t.Run("canceled caller keeps its identity on a scope derived from it", func(t *testing.T) {
		// The owner context derives from the caller context, as every
		// execution's scope derives from the canceled execution context.
		caller, cancelCaller := context.WithCancel(context.Background())
		owner, cancelOwner := context.WithCancel(caller)
		defer cancelOwner()
		f := openGuardFixture(t, owner)
		cancelCaller() // cancels both the caller and the scope derived from it
		{
			_, _, err := f.ws.enter(caller)
			testassert.Check(t, errors.Is(err, context.Canceled), "enter from a canceled caller on its canceled derived scope: %v, want context.Canceled", err)
		}
		{
			_, _, err := f.rt.enter(caller)
			testassert.Check(t, errors.Is(err, context.Canceled), "enter from a canceled caller on a canceled derived ancestor scope: %v, want context.Canceled", err)
		}
		// A live caller on the same canceled scope still sees its own closed
		// identity.
		{
			_, _, err := f.ws.enter(context.Background())
			testassert.Check(t, errors.Is(err, ErrClosed), "enter from a live caller on the canceled derived scope: %v, want ErrClosed", err)
		}
		// Rejected admissions registered no work, so closure joins and
		// disposes synchronously with no registered call to wait on.
		{
			err := f.ws.close()
			testassert.Check(t, errors.Is(err, errCloser), "close after rejected admissions: %v, want prompt completion with the shared cleanup error", err)
		}
		{
			err := f.rt.close()
			testassert.Check(t, err == nil, "runtime close: %v", err)
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
		testassert.Require(t, r1.err == nil && r2.err == nil && r1.sc == r2.sc && r1.sc != nil, "shared construction errors: %v, %v; same-key callers received %p and %p, want one shared scope", r1.err, r2.err, r1.sc, r2.sc)
		testassert.Equal(t, f.openCount("/ws/x"), 1)
	})

	t.Run("other keys proceed while one is held", func(t *testing.T) {
		f := newWorkspaceFixture(t)
		release, entered := f.hold("/ws/a")
		resA := f.getAsync(context.Background(), "/ws/a")
		<-entered
		scB, err := f.w.get(context.Background(), workspaceScopeInfo("/ws/b"))
		testassert.Require(t, err == nil && scB != nil, "unrelated key: %v", err)
		close(release)
		rA := <-resA
		testassert.Require(t, rA.err == nil && rA.sc != scB, "held key: %v %p", rA.err, rA.sc)
	})

	t.Run("initiator cancellation preserves the attempt for surviving waiters", func(t *testing.T) {
		f := newWorkspaceFixture(t)
		release, entered := f.hold("/ws/c")
		ctx1, cancel1 := context.WithCancel(context.Background())
		res1 := f.getAsync(ctx1, "/ws/c")
		<-entered
		cancel1()
		res2 := f.getAsync(context.Background(), "/ws/c")
		{
			r := <-res1
			testassert.Require(t, errors.Is(r.err, context.Canceled) && r.sc == nil, "canceled initiator: %v %p", r.err, r.sc)
		}
		close(release)
		r2 := <-res2
		testassert.Require(t, r2.err == nil && r2.sc != nil, "surviving waiter: %v", r2.err)
		sc3, err := f.w.get(context.Background(), workspaceScopeInfo("/ws/c"))
		testassert.Require(t, err == nil && sc3 == r2.sc, "later same-key request: %v %p vs %p, want the published scope", err, sc3, r2.sc)
		testassert.Equal(t, f.openCount("/ws/c"), 1)
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
		testassert.Require(t, errors.Is(r1.err, errFactory) && errors.Is(r2.err, errFactory) && r1.sc == nil && r2.sc == nil, "shared failure: %v vs %v", r1.err, r2.err)
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
		{
			got := f.openCount("/ws/d")
			testassert.Require(t, got == before+1, "retry started %d constructions, want exactly one", got-before)
		}
		close(release2)
		r3 := <-res3
		testassert.Require(t, r3.err == nil && r3.sc != nil, "later independent attempt: %v", r3.err)
		sc4, err := f.w.get(context.Background(), workspaceScopeInfo("/ws/d"))
		testassert.Require(t, err == nil && sc4 == r3.sc, "request after the successful retry: %v, want the published retry scope", err)
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
			{
				r := <-resJ
				testassert.Require(tt, errors.Is(r.err, context.Canceled) && r.sc == nil, "held construction after owner cancellation: %v %p", r.err, r.sc)
			}
			testassert.NoError(tt, <-shutdownResult)
			{
				_, err := f.w.get(context.Background(), workspaceScopeInfo("/ws/new"))
				testassert.Require(tt, errors.Is(err, ErrClosed), "get after shutdown admission closed: %v, want ErrClosed", err)
			}
			{
				got := f.trace.all()
				testassert.Require(tt, !slices.Contains(got, "close:/ws/j"), "an unpublished construction was disposed: %q", got)
			}
		})
	})

	t.Run("live workspaces close in sorted order and errors join", func(t *testing.T) {
		f := newWorkspaceFixture(t)
		for _, key := range []string{"/ws/2", "/ws/1"} {
			{
				_, err := f.w.get(context.Background(), workspaceScopeInfo(key))
				testassert.Require(t, err == nil, "get %s: %v", key, err)
			}
		}
		err := f.w.shutdown()
		testassert.Require(t, errors.Is(err, errWorkspaceClo), "shutdown error = %v, want the joined closer failures", err)
		got := f.trace.all()
		first, second := slices.Index(got, "close:/ws/1"), slices.Index(got, "close:/ws/2")
		testassert.Require(t, first >= 0 && second >= 0 && first <= second, "workspace close order = %q, want /ws/1 before /ws/2", got)
		{
			_, err := f.w.get(context.Background(), workspaceScopeInfo("/ws/1"))
			testassert.Require(t, errors.Is(err, ErrClosed), "get after shutdown: %v, want ErrClosed", err)
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
	testassert.NoError(t, err)
	a, err := Bind[worker](view, "alpha.work")
	testassert.NoError(t, err)
	second, err := selectCapabilities([]*scope{ws}, []string{"alpha.work"})
	testassert.NoError(t, err)
	a2, err := Bind[worker](second, "alpha.work")
	testassert.NoError(t, err)
	testassert.Equal(t, a, a2)
	{
		_, err := Bind[worker](view, "beta.work")
		testassert.Check(t, errors.Is(err, ErrComposition), "Bind unselected dependency: %v, want ErrComposition", err)
	}
	{
		_, err := selectCapabilities([]*scope{ws}, []string{"nope.work"})
		testassert.Check(t, errors.Is(err, ErrComposition), "select unknown capability: %v, want ErrComposition", err)
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
	{
		got, want := a.Work(invOne), `a:1 acfg:{"x":1} | b:1 bcfg:{"y":2}`
		testassert.Equal(t, got, want)
	}
	{
		got, want := a.Work(invTwo), `a:2 acfg:{"x":9} | b:2 bcfg:none`
		testassert.Equal(t, got, want)
	}

	var zero Invocation
	if zero.Revision() != "" || zero.Config("alpha") != nil {
		t.Fatalf("zero Invocation = %q, %v", zero.Revision(), zero.Config("alpha"))
	}
	handed := invOne.Config("alpha")
	handed[2] = '0'
	testassert.Require(t, string(revisionOne.plugins["alpha"]) == `{"x":1}`, "Config handout aliases the snapshot: %s", revisionOne.plugins["alpha"])
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
		testassert.Check(t, !row.ok || err == nil, "%s: %v, want acceptance", row.name, err)
		testassert.Check(t, row.ok || errors.Is(err, ErrComposition), "%s: %v, want ErrComposition", row.name, err)
	}
	var seen json.RawMessage
	validator := func(raw json.RawMessage) error {
		seen = raw
		if string(raw) == `null` {
			return nil
		}
		return errValidator
	}
	testassert.NoError(t, acceptSettings(Plugin{ID: "cfg", ValidateConfig: validator}, json.RawMessage(`null`)))
	testassert.Require(t, string(seen) == `null`, "validator saw %s, want the owned input unchanged", seen)
	{
		err := acceptSettings(Plugin{ID: "cfg", ValidateConfig: validator}, json.RawMessage(`{"a":1}`))
		testassert.Require(t, errors.Is(err, errValidator), "validator failure = %v, want the source identity", err)
	}
	// A non-nil validator receives an independent clone: in-place scratch
	// never reaches the caller's bytes.
	caller := json.RawMessage(`{"a":"12345"}`)
	testassert.NoError(t, acceptSettings(Plugin{ID: "cfg", ValidateConfig: func(got json.RawMessage) error {
		tamperScratch(got, "12345", "xxxxx")
		return nil
	}}, caller))
	testassert.Require(t, string(caller) == `{"a":"12345"}`, "caller bytes after validator scratch = %s, want unchanged", caller)
}

// noopTool is a concrete Tool implementation for declaration tests; no
// factory needs to run for its declaration metadata to exist.
type noopTool struct{}

func (noopTool) Normalize(ToolContext, model.ToolCall) (json.RawMessage, error) { return nil, nil }

func (noopTool) Prepare(context.Context, ToolContext, model.ToolCall) harness.PreparedTool {
	return harness.PreparedTool{}
}

// staticToolDescription returns a pure describe function for one declared
// tool ID, carrying a minimal valid definition under that same name.
func staticToolDescription(id string) func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
	return func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
		return ToolDescription{Definition: model.ToolDefinition{Name: id, Parameters: json.RawMessage(`{}`)}}, nil
	}
}

// countingOpen returns a plugin Open that counts factory runs.
func countingOpen(counter *int) func(context.Context, ScopeInfo, Bindings) (Instance, error) {
	return func(context.Context, ScopeInfo, Bindings) (Instance, error) {
		*counter++
		return Instance{}, nil
	}
}

// TestCompositionRejectsToolDeclarationWithoutDescription proves the Tool
// Provides row: a declaration carrying the Tool interface type without its
// pure description function — via the bare Spec[Tool] provides form or a nil
// ToolSpec function — rejects the whole set before any factory runs, while
// the same interface stays an ordinary Requires declaration: with a valid
// ToolSpec provider the Requires-only set composes.
func TestCompositionRejectsToolDeclarationWithoutDescription(t *testing.T) {
	noop := func(context.Context, ScopeInfo, Bindings) (Instance, error) { return Instance{}, nil }
	for _, row := range []struct {
		name    string
		plugins []Plugin
	}{
		{"Spec[Tool] provides", []Plugin{{ID: "tools", Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[Tool]("tool.x")}, Open: noop}}},
		{"ToolSpec with a nil function", []Plugin{{ID: "tools", Scope: ScopeRuntime, Provides: []CapabilitySpec{ToolSpec("tool.x", nil)}, Open: noop}}},
	} {
		t.Run(row.name, func(t *testing.T) {
			{
				_, err := newComposition(row.plugins)
				testassert.Require(t, errors.Is(err, ErrComposition), "newComposition error = %v, want ErrComposition", err)
			}
		})
	}
	// The ordinary sibling: requiring the Tool interface is the ordinary
	// Requires declaration. The consumer's own export is a non-tool type, so
	// the missing-description guard stays isolated to the rejection rows.
	consumer := Plugin{ID: "consumer", Scope: ScopeRuntime, Provides: []CapabilitySpec{Spec[greeter]("consumer.greet")},
		Requires: []CapabilitySpec{Spec[Tool]("tool.x")},
		Open: func(_ context.Context, _ ScopeInfo, bindings Bindings) (Instance, error) {
			if _, err := Bind[Tool](bindings, "tool.x"); err != nil {
				return Instance{}, err
			}
			return Instance{Values: map[string]any{"consumer.greet": loudGreeter{word: "hi"}}}, nil
		}}
	provider := Plugin{ID: "tools", Scope: ScopeRuntime,
		Provides: []CapabilitySpec{ToolSpec("tool.x", staticToolDescription("tool.x"))}, Open: noop}
	{
		_, err := newComposition([]Plugin{provider, consumer})
		testassert.NoError(t, err)
	}
}

// TestToolDeclarationsComposeAtAllScopes proves a tool provider may use any
// of the four scopes and its declaration ID joins the tool universe in
// registration order without running any factory.
func TestToolDeclarationsComposeAtAllScopes(t *testing.T) {
	describe := func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
		return ToolDescription{}, nil
	}
	var opened int
	open := countingOpen(&opened)
	plugins := make([]Plugin, 0, 4)
	wantIDs := make([]string, 0, 4)
	for kind, id := range map[ScopeKind]string{
		ScopeRuntime:   "tool.rt",
		ScopeWorkspace: "tool.ws",
		ScopeOperation: "tool.op",
		ScopeAgent:     "tool.ag",
	} {
		plugins = append(plugins, Plugin{ID: "p-" + string(kind), Scope: kind,
			Provides: []CapabilitySpec{ToolSpec(id, describe)}, Open: open})
		wantIDs = append(wantIDs, id)
	}
	c := mustComposition(t, plugins...)
	for kind := range map[ScopeKind]string{
		ScopeRuntime: "", ScopeWorkspace: "", ScopeOperation: "", ScopeAgent: "",
	} {
		{
			got := len(c.plan[kind])
			testassert.Require(t, got == 1, "%s plan holds %d plugins, want 1", kind, got)
		}
	}
	slices.Sort(wantIDs)
	gotIDs := append([]string(nil), c.toolIDs...)
	slices.Sort(gotIDs)
	testassert.Require(t, slices.Equal(gotIDs, wantIDs), "tool universe = %v, want every declared tool ID %v", gotIDs, wantIDs)
	testassert.Equal(t, opened, 0)
}

// TestToolSpecDescribeValidatesAndCopies proves the recorded description
// contract: the describe function receives the same Invocation and
// constraints unchanged, its returned definition is validated and copied
// with the ID/name match enforced, its error propagates, and the returned
// bytes are owned by the caller.
func TestToolSpecDescribeValidatesAndCopies(t *testing.T) {
	var gotInvocation Invocation
	var gotConstraints ToolConstraints
	source := model.ToolDefinition{Name: "tool.x", Description: "reads", Parameters: json.RawMessage(`{"a":1}`)}
	describe := func(invocation Invocation, constraints ToolConstraints, _ harness.SessionIdentity) (ToolDescription, error) {
		gotInvocation, gotConstraints = invocation, constraints
		return ToolDescription{Definition: source, Available: true, DefaultHidden: true}, nil
	}
	spec := ToolSpec("tool.x", describe)

	wantInvocation := Invocation{}
	wantConstraints := ToolConstraints{Readonly: true, WriteDir: "/w"}
	description, err := spec.describe(wantInvocation, wantConstraints, harness.SessionIdentity{})
	testassert.NoError(t, err)
	if gotInvocation.Revision() != wantInvocation.Revision() || gotConstraints != wantConstraints {
		t.Fatalf("describe inputs = (%q, %+v), want the supplied invocation and constraints echoed", gotInvocation.Revision(), gotConstraints)
	}
	testassert.Require(t, description.Available && description.DefaultHidden && description.Definition.Description == "reads" && description.Definition.Name == "tool.x", "description = %+v, want the flags and validated definition passed through", description)
	// The returned definition is an owned copy in both directions.
	source.Parameters = json.RawMessage(`{"a":2}`)
	testassert.Require(t, string(description.Definition.Parameters) == `{"a":1}`, "returned parameters = %s, want the copied bytes", description.Definition.Parameters)
	description.Definition.Parameters = json.RawMessage(`{"a":3}`)
	testassert.Require(t, string(source.Parameters) == `{"a":2}`, "source parameters = %s, want the returned copy owned by the caller", source.Parameters)

	mismatch := ToolSpec("tool.y", func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
		return ToolDescription{Definition: model.ToolDefinition{Name: "other", Parameters: json.RawMessage(`{}`)}}, nil
	})
	{
		_, err := mismatch.describe(Invocation{}, ToolConstraints{}, harness.SessionIdentity{})
		testassert.Require(t, err != nil && strings.Contains(err.Error(), "tool.y"), "mismatched describe error = %v, want the ID/name match rejection", err)
	}
	invalid := ToolSpec("tool.x", func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
		return ToolDescription{Definition: model.ToolDefinition{Name: "tool.x", Parameters: json.RawMessage(`[1]`)}}, nil
	})
	{
		_, err := invalid.describe(Invocation{}, ToolConstraints{}, harness.SessionIdentity{})
		testassert.Require(t, err != nil, "invalid parameters accepted by the recorded description")
	}
	failing := ToolSpec("tool.x", func(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
		return ToolDescription{}, errValidator
	})
	{
		_, err := failing.describe(Invocation{}, ToolConstraints{}, harness.SessionIdentity{})
		testassert.Require(t, errors.Is(err, errValidator), "describe failure = %v, want the source error", err)
	}
}

// TestCompositionBindsReportWarningPerRuntimeFactory proves the factory-bound
// warning ingress: each Runtime-scoped factory's copied ScopeInfo carries a
// callback that fixes its own registered ID, a Workspace-scoped factory never
// receives one even with a reporter installed, an isolated composition
// without a reporter supplies nil, and a callback whose scope was canceled
// drops the report.
func TestCompositionBindsReportWarningPerRuntimeFactory(t *testing.T) {
	captures := map[string]func(kind, message string){}
	plugin := func(id string, scope ScopeKind) Plugin {
		return Plugin{
			ID:       id,
			Scope:    scope,
			Provides: []CapabilitySpec{Spec[any](id + ".cap")},
			Open: func(_ context.Context, info ScopeInfo, _ Bindings) (Instance, error) {
				captures[id] = info.ReportWarning
				return Instance{Values: map[string]any{id + ".cap": id}}, nil
			},
		}
	}

	// An isolated composition installs no reporter: even a Runtime-scoped
	// factory receives nil.
	isolated := mustComposition(t, plugin("isolated", ScopeRuntime))
	owner, cancel := context.WithCancel(context.Background())
	defer cancel()
	mustOpenScope(t, isolated, owner, runtimeScopeInfo(), nil)
	testassert.Require(t, captures["isolated"] == nil, "an isolated composition supplied a ReportWarning callback")

	// The owner's composition binds each Runtime-scoped factory to its own
	// registered ID; a Workspace-scoped factory never receives the ingress.
	var reported []string
	c := mustComposition(t, plugin("alpha", ScopeRuntime), plugin("beta", ScopeRuntime), plugin("gamma", ScopeWorkspace))
	c.reportWarning = func(pluginID, kind, message string) {
		reported = append(reported, pluginID+"|"+kind+"|"+message)
	}
	runtimeScope := mustOpenScope(t, c, owner, runtimeScopeInfo(), nil)
	mustOpenScope(t, c, owner, workspaceScopeInfo("/ws"), []*scope{runtimeScope})
	testassert.Require(t, captures["alpha"] != nil && captures["beta"] != nil, "a Runtime-scoped factory received no ReportWarning callback")
	testassert.Require(t, captures["gamma"] == nil, "a Workspace-scoped factory received the Runtime warning ingress")
	captures["alpha"]("a_kind", "a_message")
	captures["beta"]("b_kind", "b_message")
	{
		want := []string{"alpha|a_kind|a_message", "beta|b_kind|b_message"}
		testassert.Require(t, slices.Equal(reported, want), "reported = %v, want each factory's own registered ID", reported)
	}

	// The scope's cancellation is the callback's boundary: a canceled scope
	// drops the report.
	cancel()
	captures["alpha"]("late_kind", "late_message")
	{
		want := []string{"alpha|a_kind|a_message", "beta|b_kind|b_message"}
		testassert.Require(t, slices.Equal(reported, want), "a canceled scope's report reached the sink: %v", reported)
	}
}
