package melta_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Syntheresis/melta/extension"
	"github.com/Syntheresis/melta/harness"
	"github.com/Syntheresis/melta/internal/testassert"
	"github.com/Syntheresis/melta/melta"
	"github.com/Syntheresis/melta/runtime"
)

// The shared fixtures of the external composed suite: the event readers over
// one live Subscription, and the bundled-credential isolation. Package
// runtime's own tests keep their copies: the two packages cannot share
// unexported test helpers.

// isolateBundledCredentials empties every api_key_env declared by the bundled
// catalog so a captured build never starts a network attempt.
func isolateBundledCredentials(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join("..", "internal", "catalog", "builtin"))
	testassert.NoError(t, err)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join("..", "internal", "catalog", "builtin", entry.Name()))
		if err != nil {
			t.Fatalf("read bundled provider %s: %v", entry.Name(), err)
		}
		var provider struct {
			Transport struct {
				APIKeyEnv string `json:"api_key_env"`
			} `json:"transport"`
		}
		if err := json.Unmarshal(data, &provider); err != nil {
			t.Fatalf("decode bundled provider %s: %v", entry.Name(), err)
		}
		if provider.Transport.APIKeyEnv != "" {
			t.Setenv(provider.Transport.APIKeyEnv, "")
		}
	}
}

// nextComposedEvent waits for one event on one live Subscription.
func nextComposedEvent(t *testing.T, sub *melta.Subscription) (melta.Event, bool) {
	t.Helper()
	select {
	case event, ok := <-sub.Events():
		return event, ok
	case <-time.After(10 * time.Second):
		t.Fatal("no event arrived within the test budget")
		return melta.Event{}, false
	}
}

// composedEventKind reads one composed event's discriminator.
func composedEventKind(t *testing.T, event melta.Event) string {
	t.Helper()
	kind, err := event.Discriminator()
	testassert.NoError(t, err)
	return kind
}

// composedEventGeneration reads one composed configuration event's
// generation.
func composedEventGeneration(t *testing.T, event melta.Event) string {
	t.Helper()
	body, err := event.AsConfigurationChangedEvent()
	testassert.NoError(t, err)
	return body.ConfigurationRevision.Generation
}

// composedEventJSON renders one composed event for failure diagnostics.
func composedEventJSON(event melta.Event) string {
	data, err := event.MarshalJSON()
	if err != nil {
		return "<unset>"
	}
	return string(data)
}

// composedCaller issues the authenticated user's whole-owner grant.
func composedCaller(t *testing.T, r *melta.Melta) melta.Caller {
	t.Helper()
	caller, err := r.Caller(melta.CallerContext{Origin: harness.InputOriginUser, Scope: melta.CallerScope{Kind: melta.CallerScopeHarness}})
	testassert.NoError(t, err)
	return caller
}

// composedOptions builds explicit construction inputs over one test home.
func composedOptions(dataDir, configPath, home string, plugins []extension.Native) melta.Options {
	dataRoot := filepath.Join(home, ".lightcode")
	return melta.Options{
		DataDir:       dataDir,
		Configuration: melta.NewFileConfigurationSource(configPath),
		Credentials:   melta.NewEnvironmentCredentialSource(filepath.Join(dataRoot, ".env")),
		Locations: melta.Locations{
			GlobalRulesDir:     dataRoot,
			DiscoveryCacheDir:  filepath.Join(dataRoot, "cache", "discovery"),
			WorkspacePolicyDir: filepath.Join(dataRoot, "projects"),
		},
		Extensions: plugins,
	}
}

// openComposed opens the public owner over one test home.
func openComposed(ctx context.Context, dataDir, configPath, home string, plugins []extension.Native) (*melta.Melta, error) {
	return melta.Open(ctx, composedOptions(dataDir, configPath, home, plugins))
}

// writeAgentDefinitionsForTest inserts the Agents section into one
// configuration document.
func writeAgentDefinitionsForTest(t *testing.T, configPath, content string) {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		data = []byte(`{}`)
	} else {
		testassert.NoError(t, err)
	}
	var root map[string]json.RawMessage
	testassert.NoError(t, json.Unmarshal(data, &root))
	var extensions map[string]json.RawMessage
	if len(root["extensions"]) != 0 {
		testassert.NoError(t, json.Unmarshal(root["extensions"], &extensions))
	}
	if extensions == nil {
		extensions = map[string]json.RawMessage{}
	}
	var section map[string]json.RawMessage
	if len(extensions["agents"]) != 0 {
		testassert.NoError(t, json.Unmarshal(extensions["agents"], &section))
	}
	if section == nil {
		section = map[string]json.RawMessage{}
	}
	section["enabled"] = json.RawMessage(`true`)
	section["definitions"] = json.RawMessage(content)
	extensions["agents"], err = json.Marshal(section)
	testassert.NoError(t, err)
	root["extensions"], err = json.Marshal(extensions)
	testassert.NoError(t, err)
	encoded, err := json.Marshal(root)
	testassert.NoError(t, err)
	testassert.NoError(t, os.WriteFile(configPath, encoded, 0o600))
}

// openComposedHost opens the public owner with a protocol host release
// callback so closing the owner also withdraws the mounted listener.
func openComposedHost(ctx context.Context, dataDir, configPath, home string, plugins []extension.Native) (*melta.Melta, *runtime.ProtocolHost, error) {
	host := runtime.NewProtocolHost()
	options := composedOptions(dataDir, configPath, home, plugins)
	options.BeforeRelease = host.BeforeRelease
	owner, err := melta.Open(ctx, options)
	return owner, host, err
}
