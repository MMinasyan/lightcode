package runtime_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/runtime"
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
func nextComposedEvent(t *testing.T, sub *runtime.Subscription) (runtime.Event, bool) {
	t.Helper()
	select {
	case event, ok := <-sub.Events():
		return event, ok
	case <-time.After(10 * time.Second):
		t.Fatal("no event arrived within the test budget")
		return runtime.Event{}, false
	}
}

// composedEventKind reads one composed event's discriminator.
func composedEventKind(t *testing.T, event runtime.Event) string {
	t.Helper()
	kind, err := event.Discriminator()
	testassert.NoError(t, err)
	return kind
}

// composedEventGeneration reads one composed configuration event's
// generation.
func composedEventGeneration(t *testing.T, event runtime.Event) string {
	t.Helper()
	body, err := event.AsConfigurationChangedEvent()
	testassert.NoError(t, err)
	return body.ConfigurationRevision.Generation
}

// composedEventJSON renders one composed event for failure diagnostics.
func composedEventJSON(event runtime.Event) string {
	data, err := event.MarshalJSON()
	if err != nil {
		return "<unset>"
	}
	return string(data)
}
