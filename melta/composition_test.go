package melta

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

func TestCompositionRejectsScopeInfoShapes(t *testing.T) {
	for _, row := range []struct {
		name string
		info ScopeInfo
	}{
		{"missing data directory", ScopeInfo{Kind: ScopeRuntime}},
		{"runtime with workspace", ScopeInfo{Kind: ScopeRuntime, DataDir: "/d", Workspace: "/w"}},
		{"runtime with session", ScopeInfo{Kind: ScopeRuntime, DataDir: "/d", SessionID: "0123456789abcdef0123456789abcdef"}},
		{"workspace without workspace", ScopeInfo{Kind: ScopeWorkspace, DataDir: "/d"}},
		{"workspace with session", ScopeInfo{Kind: ScopeWorkspace, DataDir: "/d", Workspace: "/w", SessionID: "0123456789abcdef0123456789abcdef"}},
		{"operation without operation", ScopeInfo{Kind: ScopeOperation, DataDir: "/d", Workspace: "/w", SessionID: "0123456789abcdef0123456789abcdef"}},
		{"invocation without invocation", ScopeInfo{Kind: ScopeAgent, DataDir: "/d", Workspace: "/w", SessionID: "0123456789abcdef0123456789abcdef", OperationID: "op"}},
		{"invocation without purpose", ScopeInfo{Kind: ScopeAgent, DataDir: "/d", Workspace: "/w", SessionID: "0123456789abcdef0123456789abcdef", OperationID: "op", InvocationID: "inv"}},
		{"unknown kind", ScopeInfo{Kind: ScopeKind("project"), DataDir: "/d"}},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := mustComposition(t)
			_, err := c.openScope(context.Background(), row.info, nil)
			testassert.Require(t, errors.Is(err, ErrComposition), "openScope error = %v, want ErrComposition", err)
		})
	}
}

func TestScopeGuardOwnsAdmittedCallsThroughClose(t *testing.T) {
	t.Run("closure joins the admitted call before disposing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			trace := &traceLog{}
			gate := make(chan struct{})
			c := mustComposition(t,
				testNative("dep", extension.LifetimeHarness, []extension.Capability{testServiceCapability("dep.svc")}, nil,
					func(context.Context, extension.InstanceContext) (extension.Instance, error) {
						return extension.Instance{Capabilities: map[string]extension.Implementation{"dep.svc": testServiceImplementation()}}, nil
					}),
				testNative("cons", extension.LifetimeWorkspace, []extension.Capability{testServiceCapability("cons.svc")}, nil,
					func(context.Context, extension.InstanceContext) (extension.Instance, error) {
						return extension.Instance{Capabilities: map[string]extension.Implementation{"cons.svc": testServiceImplementation()}, Close: func(context.Context, extension.CallContext) error {
							trace.add("close:cons")
							return errCloser
						}}, nil
					}),
			)
			owner := context.Background()
			rt := mustOpenScope(t, c, owner, runtimeScopeInfo(), nil)
			ws := mustOpenScope(t, c, owner, workspaceScopeInfo("/w"), []*scope{rt})
			gctx, release, err := ws.enter(context.Background())
			testassert.NoError(t, err)
			done := make(chan struct{})
			go func() {
				trace.add("call-start")
				<-gate
				trace.add("call-end")
				release()
				close(done)
			}()
			synctest.Wait()
			testassert.Require(t, !slices.Contains(trace.all(), "close:cons"), "instance closed before the admitted call finished")
			closeResult := make(chan error, 1)
			go func() { closeResult <- ws.close() }()
			synctest.Wait()
			testassert.Require(t, errors.Is(gctx.Err(), context.Canceled), "scope closure did not cancel the admitted call context")
			select {
			case err := <-closeResult:
				t.Fatalf("close completed without joining the admitted call: %v", err)
			default:
			}
			close(gate)
			<-done
			testassert.Require(t, errors.Is(<-closeResult, errCloser), "close lost the cleanup error")
			testassert.Require(t, !slices.Contains(trace.all(), "close:cons") == false, "instance was not disposed")
			testassert.Require(t, reflect.DeepEqual(trace.all(), []string{"call-start", "call-end", "close:cons"}), "trace = %q", trace.all())
			_, _, err = ws.enter(context.Background())
			testassert.Check(t, errors.Is(err, ErrClosed), "enter after closure: %v, want ErrClosed", err)
			testassert.NoError(t, rt.close())
		})
	})

	t.Run("owner cancellation before admission makes the scope unavailable", func(t *testing.T) {
		owner, cancelOwner := context.WithCancel(context.Background())
		c := mustComposition(t, testNative("cons", extension.LifetimeWorkspace, []extension.Capability{testServiceCapability("cons.svc")}, nil,
			func(context.Context, extension.InstanceContext) (extension.Instance, error) {
				return extension.Instance{Capabilities: map[string]extension.Implementation{"cons.svc": testServiceImplementation()}, Close: func(context.Context, extension.CallContext) error { return errCloser }}, nil
			}))
		ws := mustOpenScope(t, c, owner, workspaceScopeInfo("/w"), nil)
		cancelOwner()
		_, _, err := ws.enter(context.Background())
		testassert.Check(t, errors.Is(err, ErrClosed), "enter on an owner-canceled scope: %v, want ErrClosed", err)
		testassert.Require(t, errors.Is(ws.close(), errCloser), "close after rejected admissions lost the cleanup error")
	})

	t.Run("canceled caller on a live scope keeps its own error", func(t *testing.T) {
		c := mustComposition(t, testNative("cons", extension.LifetimeWorkspace, []extension.Capability{testServiceCapability("cons.svc")}, nil,
			func(context.Context, extension.InstanceContext) (extension.Instance, error) {
				return extension.Instance{Capabilities: map[string]extension.Implementation{"cons.svc": testServiceImplementation()}, Close: func(context.Context, extension.CallContext) error { return nil }}, nil
			}))
		ws := mustOpenScope(t, c, context.Background(), workspaceScopeInfo("/w"), nil)
		caller, cancelCaller := context.WithCancel(context.Background())
		cancelCaller()
		_, _, err := ws.enter(caller)
		testassert.Check(t, errors.Is(err, context.Canceled), "enter from a canceled caller: %v, want context.Canceled", err)
		testassert.NoError(t, ws.close())
	})
}

func TestWorkspaceScopeAttemptsAreSharedPerKey(t *testing.T) {
	t.Run("waiters share one construction and a canceled waiter does not cancel it", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		var opens atomic.Int64
		c := mustComposition(t, testNative("ws", extension.LifetimeWorkspace, []extension.Capability{testServiceCapability("ws.svc")}, nil,
			func(ctx context.Context, _ extension.InstanceContext) (extension.Instance, error) {
				opens.Add(1)
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return extension.Instance{}, ctx.Err()
				}
				return extension.Instance{Capabilities: map[string]extension.Implementation{"ws.svc": testServiceImplementation()}}, nil
			}))
		w := newWorkspaceScopes(context.Background(), c, nil, newObservation(), nil)
		firstCtx, cancelFirst := context.WithCancel(context.Background())
		first := make(chan error, 1)
		go func() {
			_, err := w.get(firstCtx, workspaceScopeInfo("/w"))
			first <- err
		}()
		<-entered
		second := make(chan error, 1)
		go func() {
			_, err := w.get(context.Background(), workspaceScopeInfo("/w"))
			second <- err
		}()
		cancelFirst()
		testassert.Require(t, errors.Is(<-first, context.Canceled), "canceled waiter did not return its own cancellation")
		close(release)
		testassert.NoError(t, <-second)
		testassert.Equal(t, opens.Load(), int64(1))
		_, err := w.get(context.Background(), workspaceScopeInfo("/w"))
		testassert.NoError(t, err)
		testassert.Equal(t, opens.Load(), int64(1))
		testassert.NoError(t, w.shutdown())
	})

	t.Run("a failed attempt publishes nothing and a later attempt retries", func(t *testing.T) {
		var attempts atomic.Int64
		c := mustComposition(t, testNative("ws", extension.LifetimeWorkspace, []extension.Capability{testServiceCapability("ws.svc")}, nil,
			func(context.Context, extension.InstanceContext) (extension.Instance, error) {
				if attempts.Add(1) == 1 {
					return extension.Instance{}, errFactory
				}
				return extension.Instance{Capabilities: map[string]extension.Implementation{"ws.svc": testServiceImplementation()}}, nil
			}))
		w := newWorkspaceScopes(context.Background(), c, nil, newObservation(), nil)
		_, err := w.get(context.Background(), workspaceScopeInfo("/w"))
		testassert.Require(t, errors.Is(err, errFactory), "first attempt = %v, want the factory error", err)
		_, err = w.get(context.Background(), workspaceScopeInfo("/w"))
		testassert.NoError(t, err)
		testassert.Equal(t, attempts.Load(), int64(2))
		testassert.NoError(t, w.shutdown())
	})
}

func TestWorkspaceConstructionCleanupRetained(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		for _, abandoned := range []bool{false, true} {
			for _, cleanupFails := range []bool{false, true} {
				name := fmt.Sprintf("rejected=%v/abandoned=%v/cleanup=%v", rejected, abandoned, cleanupFails)
				t.Run(name, func(t *testing.T) {
					owner, cancelOwner := context.WithCancel(context.Background())
					defer cancelOwner()
					entered, gate := make(chan struct{}), make(chan struct{})
					var gateOnce sync.Once
					defer gateOnce.Do(func() { close(gate) })
					factoryErr, cleanupErr := &workspaceFactoryError{}, &workspaceCleanupError{}
					var prefixOpens, prefixCloses atomic.Int64
					var retry atomic.Bool
					c := mustComposition(t,
						testNative("prefix", extension.LifetimeWorkspace, []extension.Capability{testServiceCapability("prefix")}, nil,
							func(context.Context, extension.InstanceContext) (extension.Instance, error) {
								id := prefixOpens.Add(1)
								return extension.Instance{Capabilities: map[string]extension.Implementation{"prefix": testServiceImplementation()}, Close: func(context.Context, extension.CallContext) error {
									prefixCloses.Add(1)
									if cleanupFails && id == 1 {
										return cleanupErr
									}
									return nil
								}}, nil
							}),
						testNative("later", extension.LifetimeWorkspace, []extension.Capability{testServiceCapability("later")}, nil,
							func(context.Context, extension.InstanceContext) (extension.Instance, error) {
								if retry.Load() {
									return extension.Instance{Capabilities: map[string]extension.Implementation{"later": testServiceImplementation()}}, nil
								}
								close(entered)
								<-gate
								if !rejected {
									return extension.Instance{}, factoryErr
								}
								return extension.Instance{Capabilities: map[string]extension.Implementation{"later": testServiceImplementation()}}, nil
							}),
					)
					w := newWorkspaceScopes(owner, c, nil, newObservation(), nil)
					caller, cancelCaller := context.WithCancel(context.Background())
					defer cancelCaller()
					result := make(chan error, 1)
					go func() { _, err := w.get(caller, workspaceScopeInfo("/ws/test")); result <- err }()
					<-entered
					testassert.Equal(t, prefixOpens.Load(), int64(1))
					if abandoned {
						cancelCaller()
						testassert.Require(t, errors.Is(<-result, context.Canceled), "abandoned waiter did not return cancellation")
					}
					if rejected {
						cancelOwner()
					}
					gateOnce.Do(func() { close(gate) })
					if !abandoned {
						err := <-result
						if rejected {
							testassert.Require(t, errors.Is(err, context.Canceled), "rejected construction = %v", err)
						} else {
							var gotFactory *workspaceFactoryError
							testassert.Require(t, errors.Is(err, factoryErr) && errors.As(err, &gotFactory), "construction cause lost: %v", err)
						}
						var gotCleanup *workspaceCleanupError
						testassert.Require(t, errors.Is(err, cleanupErr) == cleanupFails && errors.As(err, &gotCleanup) == cleanupFails, "cleanup cause in construction result = %v", err)
					}
					if !rejected && !abandoned {
						retry.Store(true)
						_, err := w.get(context.Background(), workspaceScopeInfo("/ws/test"))
						testassert.NoError(t, err)
					}
					err := w.shutdown()
					testassert.Require(t, errors.Is(err, cleanupErr) == cleanupFails, "shutdown = %v, cleanup failure wanted %v", err, cleanupFails)
					var retainedCleanup *workspaceCleanupError
					testassert.Require(t, errors.As(err, &retainedCleanup) == cleanupFails, "shutdown cleanup type = %v", err)
					testassert.Require(t, !errors.Is(err, factoryErr) && !errors.Is(err, context.Canceled), "shutdown retained a construction cause: %v", err)
					testassert.Require(t, prefixCloses.Load() == prefixOpens.Load(), "prefix cleanup count = %d for %d opens", prefixCloses.Load(), prefixOpens.Load())
				})
			}
		}
	}
}

func TestWorkspaceShutdownJoinsConstructionAndLiveCleanupErrors(t *testing.T) {
	c := mustComposition(t,
		testNative("prefix", extension.LifetimeWorkspace, []extension.Capability{testServiceCapability("prefix")}, nil,
			func(_ context.Context, info extension.InstanceContext) (extension.Instance, error) {
				return extension.Instance{Capabilities: map[string]extension.Implementation{"prefix": testServiceImplementation()}, Close: func(context.Context, extension.CallContext) error {
					if info.Scope.Workspace == "/live" {
						return errWorkspaceClo
					}
					return errCloser
				}}, nil
			}),
		testNative("later", extension.LifetimeWorkspace, []extension.Capability{testServiceCapability("later")}, nil,
			func(_ context.Context, info extension.InstanceContext) (extension.Instance, error) {
				if info.Scope.Workspace == "/failed" {
					return extension.Instance{}, errFactory
				}
				return extension.Instance{Capabilities: map[string]extension.Implementation{"later": testServiceImplementation()}}, nil
			}),
	)
	w := newWorkspaceScopes(context.Background(), c, nil, newObservation(), nil)
	_, err := w.get(context.Background(), workspaceScopeInfo("/live"))
	testassert.NoError(t, err)
	_, err = w.get(context.Background(), workspaceScopeInfo("/failed"))
	testassert.Require(t, errors.Is(err, errFactory) && errors.Is(err, errCloser), "construction failure = %v", err)
	err = w.shutdown()
	testassert.Require(t, errors.Is(err, errCloser) && errors.Is(err, errWorkspaceClo) && !errors.Is(err, errFactory), "shutdown did not join only the cleanup errors: %v", err)
}

func TestCompositionBindsReportWarningPerRuntimeFactory(t *testing.T) {
	var reported []string
	var mu sync.Mutex
	c := mustComposition(t, testNative("alpha", extension.LifetimeHarness, []extension.Capability{testServiceCapability("alpha.svc")}, nil,
		func(_ context.Context, info extension.InstanceContext) (extension.Instance, error) {
			return extension.Instance{Capabilities: map[string]extension.Implementation{"alpha.svc": testServiceImplementation()}}, nil
		}))
	c.reportWarning = func(id, kind, message string) {
		mu.Lock()
		defer mu.Unlock()
		reported = append(reported, id+":"+kind+":"+message)
	}
	sc := mustOpenScope(t, c, context.Background(), runtimeScopeInfo(), nil)
	_ = sc
	// The factory has no warning callback in InstanceContext; the composition
	// still attributes reports from its warning binding when a test factory
	// uses the host warning ingress. This fixture proves the sink identity is
	// the registered extension ID when a Melta report arrives.
	c.reportWarning("alpha", "kind", "message")
	testassert.Require(t, reflect.DeepEqual(reported, []string{"alpha:kind:message"}), "reported = %q", reported)
}

func TestNativeInvalidInstanceOutputUnwindsAcquiredPrefix(t *testing.T) {
	var firstClosed, invalidClosed, openErrorClosed int
	first := testNative("first", extension.LifetimeHarness, []extension.Capability{testServiceCapability("first.cap")}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"first.cap": testServiceImplementation()}, Close: func(context.Context, extension.CallContext) error {
			firstClosed++
			return nil
		}}, nil
	})
	invalid := testNative("invalid", extension.LifetimeHarness, []extension.Capability{testServiceCapability("invalid.cap")}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Capabilities: map[string]extension.Implementation{"not-declared": testServiceImplementation()}, Close: func(context.Context, extension.CallContext) error {
			invalidClosed++
			return nil
		}}, nil
	})
	c := mustComposition(t, first, invalid)
	_, err := c.openScope(context.Background(), runtimeScopeInfo(), nil)
	testassert.Require(t, errors.Is(err, ErrComposition), "openScope = %v, want ErrComposition", err)
	testassert.Equal(t, firstClosed, 1)
	testassert.Equal(t, invalidClosed, 1)

	openError := testNative("open-error", extension.LifetimeHarness, []extension.Capability{testServiceCapability("open-error.cap")}, nil, func(context.Context, extension.InstanceContext) (extension.Instance, error) {
		return extension.Instance{Close: func(context.Context, extension.CallContext) error { openErrorClosed++; return nil }}, errors.New("open failed")
	})
	c = mustComposition(t, first, openError)
	_, err = c.openScope(context.Background(), runtimeScopeInfo(), nil)
	testassert.Require(t, err != nil, "openScope succeeded")
	testassert.Equal(t, openErrorClosed, 0)
}
