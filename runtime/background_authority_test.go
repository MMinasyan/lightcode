package runtime

// Background authority over the composed Runtime: one declared probe tool
// drives the Runtime's bound background object through the real preparation,
// permission evaluation and child admission producers, and the same object is
// driven directly for its lifetime rules.

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/agents"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// childLaunchConfig is one keyless configured provider plus the optional deny
// rule for the probe's declared child target.
func childLaunchConfig(endpoint string, deny bool) string {
	doc := `{"providers":{"prov":` + nominalProvMember(endpoint, "",
		`{"m":{"name":"M","context_window":262144}}`) + `}`
	if deny {
		doc += `,"permissions":{"rules":[{"permission":"child.launch","target":"child_agent","access":"deny"}]}`
	}
	return doc + `}`
}

const childLaunchAgents = `{
	"prober": {"model": "prov/m", "system_prompt": "simple", "tools": ["probe"]},
	"child_agent": {"model": "prov/m", "system_prompt": "simple", "subagent": true, "description": "child"}
}`

// childProbe is one declared probe tool whose preparation and execution
// phases drive the bound background object.
type childProbe struct {
	mu       sync.Mutex
	phase    string
	executed atomic.Int32

	background  BackgroundServices
	permissions []harness.PermissionRequest

	launchErr error
	otherErr  error
	jobErr    error

	spawnRan atomic.Bool
	started  chan struct{}
	done     chan struct{}
}

func probeLaunch(background BackgroundServices, agentType string) error {
	if background == nil {
		return errors.New("probe: no background services")
	}
	_, err := background.LaunchChild(ChildRequest{
		AgentType:     agentType,
		Content:       []model.ContentPart{{Kind: model.PartText, Text: "child probe"}},
		OperationID:   "probe-child-op",
		MaxConcurrent: 4,
		OutputLimit:   15360,
	})
	return err
}

func (p *childProbe) probeJob(background BackgroundServices) error {
	if background == nil {
		return errors.New("probe: no background services")
	}
	return background.StartJob("0a1b2c3d", func(context.Context, func(string) error) error {
		p.spawnRan.Store(true)
		return nil
	})
}

func (p *childProbe) recordLaunch(err error) {
	p.mu.Lock()
	p.launchErr = err
	p.mu.Unlock()
}

func (p *childProbe) recordOther(err error) {
	p.mu.Lock()
	p.otherErr = err
	p.mu.Unlock()
}

func (p *childProbe) recordJob(err error) {
	p.mu.Lock()
	p.jobErr = err
	p.mu.Unlock()
}

func (p *childProbe) recorded() (launchErr, otherErr, jobErr error, background BackgroundServices) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.launchErr, p.otherErr, p.jobErr, p.background
}

func (*childProbe) describe(Invocation, ToolConstraints, harness.SessionIdentity) (ToolDescription, error) {
	definition, err := model.NewToolDefinition(model.ToolDefinition{
		Name:        "probe",
		Description: "background authority probe",
		Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
	})
	if err != nil {
		return ToolDescription{}, err
	}
	return ToolDescription{Definition: definition, Available: true}, nil
}

func (p *childProbe) Normalize(tc ToolContext, _ model.ToolCall) (json.RawMessage, error) {
	p.mu.Lock()
	phase := p.phase
	p.mu.Unlock()
	if phase == "normalize" {
		p.recordLaunch(probeLaunch(tc.Background, "child_agent"))
		p.recordJob(p.probeJob(tc.Background))
	}
	return json.RawMessage(`{}`), nil
}

func (p *childProbe) Prepare(_ context.Context, tc ToolContext, call model.ToolCall) harness.PreparedTool {
	p.mu.Lock()
	phase := p.phase
	p.background = tc.Background
	p.mu.Unlock()
	if phase == "prepare" {
		p.recordLaunch(probeLaunch(tc.Background, "child_agent"))
		p.recordJob(p.probeJob(tc.Background))
	}
	permissions := []harness.PermissionRequest{{Permission: harness.PermissionChildLaunch, Target: "child_agent"}}
	p.mu.Lock()
	p.permissions = permissions
	p.mu.Unlock()
	return harness.PreparedTool{
		Permissions: permissions,
		Execute: func(ctx context.Context) harness.ToolOutcome {
			p.executed.Add(1)
			switch phase {
			case "execute":
				p.recordLaunch(probeLaunch(tc.Background, "child_agent"))
			case "other_target":
				p.recordLaunch(probeLaunch(tc.Background, "other_agent"))
			case "changed_data":
				p.mu.Lock()
				p.permissions[0].Target = "other_agent"
				p.mu.Unlock()
				p.recordOther(probeLaunch(tc.Background, "other_agent"))
				p.recordLaunch(probeLaunch(tc.Background, "child_agent"))
			case "nil_spawn":
				p.recordJob(tc.Background.StartJob("0a1b2c3d", nil))
			case "park":
				if p.started != nil {
					close(p.started)
				}
				<-ctx.Done()
				p.recordLaunch(probeLaunch(tc.Background, "child_agent"))
				p.recordJob(p.probeJob(tc.Background))
				if p.done != nil {
					close(p.done)
				}
			}
			return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: call.ID, Status: model.ResultSuccess, Content: "probe returned"}}
		},
	}
}

func childProbePlugin(probe *childProbe) Plugin {
	return Plugin{
		ID:       "probe",
		Scope:    ScopeRuntime,
		Provides: []CapabilitySpec{ToolSpec("probe", probe.describe)},
		Open: func(context.Context, ScopeInfo) (Instance, error) {
			return Instance{Values: map[string]any{"probe": probe}}, nil
		},
	}
}

// childSessionCount reads every Session header and counts the children.
func childSessionCount(t *testing.T, r *Runtime, store harness.Storage) int {
	t.Helper()
	ids, err := store.ListSessionIDs(context.Background())
	testassert.NoError(t, err)
	children := 0
	for _, id := range ids {
		header, err := r.harness.ReadSessionHeader(context.Background(), id)
		if err != nil {
			continue
		}
		if header.Identity.ParentSessionID != "" {
			children++
		}
	}
	return children
}

// childParent reads the one child Session header's parent identity.
func childParent(t *testing.T, r *Runtime, store harness.Storage) string {
	t.Helper()
	ids, err := store.ListSessionIDs(context.Background())
	testassert.NoError(t, err)
	for _, id := range ids {
		header, err := r.harness.ReadSessionHeader(context.Background(), id)
		if err != nil {
			continue
		}
		if header.Identity.ParentSessionID != "" {
			return header.Identity.ParentSessionID
		}
	}
	return ""
}

// TestBackgroundAuthorityThroughComposedRuntime exercises the bound object
// through the actual composed Runtime: authority exists only inside Execute,
// the plan's declared target is the only launchable one, a denied plan never
// executes, the frozen pairs cannot be broadened by the producer, and the
// object refuses every call after settlement.
func TestBackgroundAuthorityThroughComposedRuntime(t *testing.T) {
	for _, row := range []struct {
		name          string
		phase         string
		deny          bool
		wantExecuted  int32
		wantChildren  int
		wantLaunchErr error
		wantOtherErr  error
		wantJobErr    error
	}{
		{name: "normalize", phase: "normalize", wantExecuted: 1, wantLaunchErr: ErrClosed, wantJobErr: ErrClosed},
		{name: "prepare", phase: "prepare", deny: true, wantLaunchErr: ErrClosed, wantJobErr: ErrClosed},
		{name: "denied", phase: "denied", deny: true},
		{name: "execute", phase: "execute", wantExecuted: 1, wantChildren: 1},
		{name: "other_target", phase: "other_target", wantExecuted: 1, wantLaunchErr: ErrClosed},
		{name: "changed_data", phase: "changed_data", wantExecuted: 1, wantChildren: 1, wantOtherErr: ErrClosed},
		{name: "nil_spawn", phase: "nil_spawn", wantExecuted: 1, wantJobErr: harness.ErrInvalid},
	} {
		t.Run(row.name, func(t *testing.T) {
			ctx := context.Background()
			e := newOwnerEnv(t)
			store := storage.NewMemory()
			probe := &childProbe{phase: row.phase}
			e.server.setScript(func(_ context.Context, body string) []string {
				if lastMessageRole(body) == "tool" || probe.executed.Load() != 0 {
					return nil
				}
				return toolCallTurnEvents("probe-call", "probe", `{}`)
			})
			writeServiceFile(t, e.configPath, childLaunchConfig(e.server.URL, row.deny))
			writeServiceFile(t, agents.PathForConfig(e.configPath), childLaunchAgents)
			e.fixtureStore(store)
			r, err := e.open(ctx, childProbePlugin(probe))
			testassert.NoError(t, err)
			defer func() { testassert.NoError(t, r.Close(context.Background())) }()

			session, err := r.createSession(ctx, filepath.Join(e.home, "probe-ws"), "prober")
			testassert.NoError(t, err)
			sessionID := session.Identity.SessionID
			submitThroughRuntime(t, r, sessionID, "probe-parent-op", "run probe")
			awaitOperation(t, r, sessionID, "probe-parent-op", harness.OperationSuccess)

			testassert.Equal(t, probe.executed.Load(), row.wantExecuted)
			children := childSessionCount(t, r, store)
			testassert.Equal(t, children, row.wantChildren)
			launchErr, otherErr, jobErr, background := probe.recorded()
			if row.wantLaunchErr == nil {
				testassert.NoError(t, launchErr)
			} else {
				testassert.Require(t, errors.Is(launchErr, row.wantLaunchErr), "launch error = %v, want %v", launchErr, row.wantLaunchErr)
			}
			if row.wantOtherErr == nil {
				testassert.NoError(t, otherErr)
			} else {
				testassert.Require(t, errors.Is(otherErr, row.wantOtherErr), "other-target launch error = %v, want %v", otherErr, row.wantOtherErr)
			}
			if row.wantJobErr == nil {
				testassert.NoError(t, jobErr)
			} else {
				testassert.Require(t, errors.Is(jobErr, row.wantJobErr), "job start error = %v, want %v", jobErr, row.wantJobErr)
			}
			testassert.Require(t, !probe.spawnRan.Load(), "a refused start ran the spawn callback")
			if row.name == "execute" {
				parent := childParent(t, r, store)
				testassert.Require(t, parent == sessionID, "child parent = %q, want the calling Session %q", parent, sessionID)
			}

			// After settlement the stored object grants no authority and
			// creates nothing.
			testassert.Require(t, background != nil, "Prepare received no background services object")
			storedLaunchErr := probeLaunch(background, "child_agent")
			testassert.Require(t, errors.Is(storedLaunchErr, ErrClosed), "retained launch = %v, want ErrClosed", storedLaunchErr)
			storedJobErr := background.StartJob("0a1b2c3d", func(context.Context, func(string) error) error {
				return errors.New("the retained object must not run a spawn")
			})
			testassert.Require(t, errors.Is(storedJobErr, ErrClosed), "retained job start = %v, want ErrClosed", storedJobErr)
			testassert.Equal(t, childSessionCount(t, r, store), children)
		})
	}
}

// TestBackgroundAuthorityRefusesCanceledOrigin proves cancellation ends
// authority: an execution woken by Interrupt refuses both methods with the
// closed identity, and no child or job origin is created.
func TestBackgroundAuthorityRefusesCanceledOrigin(t *testing.T) {
	ctx := context.Background()
	e := newOwnerEnv(t)
	store := storage.NewMemory()
	probe := &childProbe{phase: "park", started: make(chan struct{}), done: make(chan struct{})}
	e.server.setScript(func(_ context.Context, body string) []string {
		if lastMessageRole(body) == "tool" {
			return nil
		}
		return toolCallTurnEvents("probe-call", "probe", `{}`)
	})
	writeServiceFile(t, e.configPath, childLaunchConfig(e.server.URL, false))
	writeServiceFile(t, agents.PathForConfig(e.configPath), childLaunchAgents)
	e.fixtureStore(store)
	r, err := e.open(ctx, childProbePlugin(probe))
	testassert.NoError(t, err)
	defer func() { testassert.NoError(t, r.Close(context.Background())) }()

	session, err := r.createSession(ctx, filepath.Join(e.home, "probe-ws"), "prober")
	testassert.NoError(t, err)
	sessionID := session.Identity.SessionID
	submitThroughRuntime(t, r, sessionID, "probe-parent-op", "run probe")
	select {
	case <-probe.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the admitted execution never parked")
	}
	err = r.withHarness(context.Background(), func(ctx context.Context, h *harness.Harness) error {
		return h.Interrupt(ctx, sessionID)
	})
	testassert.NoError(t, err)
	awaitOperation(t, r, sessionID, "probe-parent-op", harness.OperationInterruption)
	select {
	case <-probe.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the parked execution never resumed after cancellation")
	}

	launchErr, otherErr, jobErr, _ := probe.recorded()
	testassert.Require(t, errors.Is(launchErr, ErrClosed), "post-cancellation launch = %v, want ErrClosed", launchErr)
	testassert.Require(t, errors.Is(jobErr, ErrClosed), "post-cancellation job start = %v, want ErrClosed", jobErr)
	testassert.NoError(t, otherErr)
	testassert.Require(t, !probe.spawnRan.Load(), "a canceled-origin job start ran the spawn callback")
	testassert.Equal(t, childSessionCount(t, r, store), 0)
}

// TestBoundCallLifetime drives the bound object directly, without a Harness:
// the frozen pairs govern enforcement, close cancels before joining, and the
// accepting boundary owns the outcome bytes.
func TestBoundCallLifetime(t *testing.T) {
	t.Run("freezes", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &boundBackground{bridge: &backgroundBridge{}, sessionID: testSessionID}
			original := []harness.PermissionRequest{{Permission: harness.PermissionChildLaunch, Target: "child_agent"}}
			var launchErr error
			plan := harness.PreparedTool{
				Permissions: original,
				Execute: func(context.Context) harness.ToolOutcome {
					launchErr = probeLaunch(b, "other_agent")
					return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "ran"}}
				},
			}
			wrapped := b.bindPlan(plan)
			original[0].Target = "other_agent"
			testassert.Require(t, len(wrapped.Permissions) == 1 && wrapped.Permissions[0].Target == "child_agent", "returned plan permissions = %+v, want the frozen child_agent pair", wrapped.Permissions)
			wrapped.Execute(context.Background())
			testassert.Require(t, errors.Is(launchErr, ErrClosed), "other-target launch = %v, want ErrClosed before any Harness call", launchErr)
		})
	})

	t.Run("joins", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &boundBackground{bridge: &backgroundBridge{}, sessionID: testSessionID}
			entered := make(chan struct{})
			canceled := make(chan struct{})
			release := make(chan struct{})
			plan := harness.PreparedTool{
				Permissions: []harness.PermissionRequest{{Permission: harness.PermissionChildLaunch, Target: "child_agent"}},
				Execute: func(context.Context) harness.ToolOutcome {
					go func() {
						ctx, done, err := b.enter()
						if err != nil {
							t.Errorf("entered call refused: %v", err)
							return
						}
						defer done()
						close(entered)
						<-ctx.Done()
						close(canceled)
						<-release
					}()
					<-entered
					return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "ran"}}
				},
			}
			wrapped := b.bindPlan(plan)
			done := make(chan struct{})
			go func() {
				wrapped.Execute(context.Background())
				close(done)
			}()
			synctest.Wait()
			select {
			case <-canceled:
			default:
				t.Fatal("the derived context was not canceled during the join")
			}
			select {
			case <-done:
				t.Fatal("the wrapper returned before joining the entered call")
			default:
			}
			{
				_, _, err := b.enter()
				testassert.Require(t, errors.Is(err, ErrClosed), "entry during the join = %v, want ErrClosed", err)
			}
			close(release)
			<-done
		})
	})

	t.Run("owns_outcome", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			b := &boundBackground{bridge: &backgroundBridge{}, sessionID: testSessionID}
			metadata := json.RawMessage(`{"kept":1}`)
			entered := make(chan struct{})
			edited := make(chan struct{})
			plan := harness.PreparedTool{
				Permissions: []harness.PermissionRequest{{Permission: harness.PermissionChildLaunch, Target: "child_agent"}},
				Execute: func(context.Context) harness.ToolOutcome {
					go func() {
						ctx, done, err := b.enter()
						if err != nil {
							t.Errorf("entered call refused: %v", err)
							return
						}
						defer done()
						close(entered)
						<-ctx.Done()
						metadata[0] = '['
						close(edited)
					}()
					<-entered
					return harness.ToolOutcome{Evidence: harness.ExecutionObserved, Metadata: metadata, Result: model.ToolResult{CallID: "call-1", Status: model.ResultSuccess, Content: "ran"}}
				},
			}
			wrapped := b.bindPlan(plan)
			outcome := wrapped.Execute(context.Background())
			<-edited
			testassert.Require(t, string(outcome.Metadata) == `{"kept":1}`, "returned metadata = %s, want the copied original bytes", outcome.Metadata)
		})
	})
}
