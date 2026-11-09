package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/model"
)

// resolverScript is one mutable resolver fixture: handlers, open/close counters
// and a stable extension identity.
type resolverScript struct {
	mu      sync.Mutex
	list    func() json.RawMessage
	resolve func(string) json.RawMessage
	opens   atomic.Int64
	closes  atomic.Int64
}

func (s *resolverScript) set(list func() json.RawMessage, resolve func(string) json.RawMessage) {
	s.mu.Lock()
	s.list, s.resolve = list, resolve
	s.mu.Unlock()
}

func (s *resolverScript) native() extension.Native {
	return extension.Native{
		Declaration: extension.Declaration{
			ID:           "guestdefs",
			Version:      "1",
			ConfigSchema: []byte(`{"type":"object","required":["enabled"],"properties":{"enabled":{"type":"boolean"},"tag":{"type":"integer"}},"additionalProperties":false}`),
			Components: []extension.Component{{
				ID:       "guestdefs-c",
				Lifetime: extension.LifetimeHarness,
				Provides: []string{"guestdefs.definitions"},
			}},
			Capabilities: []extension.Capability{extension.AgentDefinitionsCapability("guestdefs.definitions")},
		},
		Open: func(ctx context.Context, _ extension.InstanceContext) (extension.Instance, error) {
			if err := ctx.Err(); err != nil {
				return extension.Instance{}, err
			}
			s.opens.Add(1)
			return extension.Instance{
				Capabilities: map[string]extension.Implementation{
					"guestdefs.definitions": {Service: map[string]extension.Handler{
						"list": func(context.Context, extension.CallContext, json.RawMessage) (extension.Result, error) {
							s.mu.Lock()
							list := s.list
							s.mu.Unlock()
							return extension.Result{Value: list()}, nil
						},
						"resolve": func(_ context.Context, _ extension.CallContext, arguments json.RawMessage) (extension.Result, error) {
							var request struct {
								ID string `json:"id"`
							}
							if err := json.Unmarshal(arguments, &request); err != nil {
								return extension.Result{}, err
							}
							s.mu.Lock()
							resolve := s.resolve
							s.mu.Unlock()
							return extension.Result{Value: resolve(request.ID)}, nil
						},
					}},
				},
				Close: func(context.Context, extension.CallContext) error {
					s.closes.Add(1)
					return nil
				},
			}, nil
		},
	}
}

func validResolverList() json.RawMessage {
	value, _ := json.Marshal([]extension.AgentDefinitionSummary{{ID: "one", Description: "one"}})
	return value
}

func validResolverInputs() json.RawMessage {
	value, _ := json.Marshal(extension.AgentDefinitionResolution{Inputs: harness.AgentInputs{
		Model:        model.ModelRef{Provider: "prov", Model: "m"},
		Instructions: "resolver instruction",
		AssemblyMode: "simple",
		Tools:        []string{"read_file"},
	}.Owned()})
	return value
}

func resolverConfigSection(tag string) string {
	return `{"enabled":true` + tag + `}`
}

func resolverConfigDocument(section string) string {
	return strings.TrimSuffix(providerConfigFile("One"), "}") + `,"extensions":{"guestdefs":` + section + `}}`
}

// TestResolverProjectionRejectsMalformedDuplicateAndForgedDocuments proves the
// compiled schema and shared semantic decoders reject malformed list shapes,
// duplicate list IDs, semantic-only invalid resolved inputs and forged source
// metadata, with a fresh candidate's resources released.
func TestResolverProjectionRejectsMalformedDuplicateAndForgedDocuments(t *testing.T) {
	for _, row := range []struct {
		name    string
		list    json.RawMessage
		resolve json.RawMessage
	}{
		{"malformed list document", json.RawMessage(`{"bad":true}`), validResolverInputs()},
		{"duplicate list ids", json.RawMessage(`[{"id":"one","description":"one"},{"id":"one","description":"two"}]`), validResolverInputs()},
		{"forged source metadata", json.RawMessage(`[{"id":"one","description":"one","extension_id":"agents"}]`), validResolverInputs()},
		{"invalid resolved inputs", validResolverList(), json.RawMessage(`{"inputs":{"instructions":"","assembly_mode":"full","tools":[],"capabilities":["dup","dup"],"constraints":{"readonly":false,"write_dir":""},"strategy":{"id":""}},"description":"one","subagent":false}`)},
	} {
		t.Run(row.name, func(t *testing.T) {
			h := newServiceHarness(t)
			writeServiceFile(t, h.configPath, resolverConfigDocument(resolverConfigSection("")))
			script := &resolverScript{}
			script.set(func() json.RawMessage { return row.list }, func(string) json.RawMessage { return row.resolve })
			svc := h.preparedService(context.Background(), script.native())
			_, err := svc.publish(context.Background())
			testassert.Require(t, errors.Is(err, ErrConfiguration), "publish = %v, want ErrConfiguration", err)
			testassert.Require(t, svc.current() == nil, "rejected projection published a snapshot")
			testassert.Require(t, script.opens.Load() == 1 && script.closes.Load() == 1, "fresh resolver resources = %d opens/%d closes, want one each", script.opens.Load(), script.closes.Load())
		})
	}
}

// TestResolverProjectionFailureCleanupPreservesAdoptedAndReleasesFresh proves
// a failed projection keeps an adopted live resource set and releases only a
// fresh candidate set; a later valid replacing reload retires the adopted set.
func TestResolverProjectionFailureCleanupPreservesAdoptedAndReleasesFresh(t *testing.T) {
	h := newServiceHarness(t)
	writeServiceFile(t, h.configPath, resolverConfigDocument(resolverConfigSection("")))
	script := &resolverScript{}
	script.set(validResolverList, func(string) json.RawMessage { return validResolverInputs() })
	svc := h.preparedService(context.Background(), script.native())
	first, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	testassert.Require(t, script.opens.Load() == 1 && script.closes.Load() == 0, "initial resources = %d/%d", script.opens.Load(), script.closes.Load())
	firstResources := first.snapshot.resources

	// Unchanged section: publication adopts the live set; a failed projection
	// must not retire it.
	script.set(func() json.RawMessage {
		return json.RawMessage(`[{"id":"one","description":"one"},{"id":"one","description":"two"}]`)
	}, func(string) json.RawMessage { return validResolverInputs() })
	_, err = svc.publish(context.Background())
	testassert.Require(t, errors.Is(err, ErrConfiguration), "adopted projection failure = %v", err)
	testassert.Require(t, svc.current() == first.snapshot && first.snapshot.resources == firstResources && script.closes.Load() == 0, "adopted set was retired by a failed projection")

	// Changed active section: a fresh set is prepared; failure releases only
	// the fresh set and keeps the live revision.
	writeServiceFile(t, h.configPath, resolverConfigDocument(resolverConfigSection(`,"tag":1`)))
	_, err = svc.publish(context.Background())
	testassert.Require(t, errors.Is(err, ErrConfiguration), "fresh projection failure = %v", err)
	testassert.Require(t, svc.current() == first.snapshot && script.opens.Load() == 2 && script.closes.Load() == 1, "fresh failed set = %d opens/%d closes, want the live set kept and the fresh set released", script.opens.Load(), script.closes.Load())

	// A valid replacing reload retires the adopted first set at commit.
	script.set(validResolverList, func(string) json.RawMessage { return validResolverInputs() })
	second, err := svc.publish(context.Background())
	testassert.NoError(t, err)
	testassert.Require(t, svc.current() == second.snapshot && script.opens.Load() == 3 && script.closes.Load() == 2, "replacing reload = %d opens/%d closes, want the prior set retired", script.opens.Load(), script.closes.Load())
}
