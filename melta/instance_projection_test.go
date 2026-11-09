package melta

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
)

// TestOwnerInstanceStampsEveryProjection proves the owner's instance identity
// is already present on every revisioned read and on revisioned embedded
// subscription events; no host post-processing is required.
func TestOwnerInstanceStampsEveryProjection(t *testing.T) {
	ctx := context.Background()
	r, e := openProjectionRuntime(t, storage.NewMemory())
	defer closeProjectionRuntime(r)
	instance := r.InstanceID()
	caller := testCaller()

	configuration, err := r.GetConfiguration(ctx, caller)
	testassert.Require(t, err == nil && configuration.ConfigurationRevision.InstanceId == instance, "configuration revision = %+v, want instance %q", configuration.ConfigurationRevision, instance)
	warnings, err := r.GetWarnings(ctx, caller)
	testassert.Require(t, err == nil && warnings.WarningsRevision.InstanceId == instance, "warnings revision = %+v, want instance %q", warnings.WarningsRevision, instance)

	session := projectionSession(t, r, filepath.Join(e.home, "instance-ws"), "solo").Identity.SessionID
	hydration, err := r.GetHydration(ctx, caller, session)
	testassert.Require(t, err == nil && hydration.SessionRevision.InstanceId == instance && hydration.ConfigurationRevision.InstanceId == instance && hydration.WarningsRevision.InstanceId == instance, "hydration revisions = %+v/%+v/%+v, want instance %q", hydration.SessionRevision, hydration.ConfigurationRevision, hydration.WarningsRevision, instance)
	page, err := r.GetHistory(ctx, caller, session, nil)
	testassert.Require(t, err == nil && page.SessionRevision.InstanceId == instance, "history revision = %+v, want instance %q", page.SessionRevision, instance)

	sub, err := r.Subscribe(8)
	testassert.NoError(t, err)
	defer sub.Close()
	if _, err := r.Reload(ctx, caller); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	deadline := time.After(10 * time.Second)
	for {
		select {
		case event, ok := <-sub.Events():
			testassert.Require(t, ok, "the subscription closed before the configuration event")
			kind, err := event.Discriminator()
			testassert.NoError(t, err)
			if kind != "configuration_changed" {
				continue
			}
			value, err := event.AsConfigurationChangedEvent()
			testassert.Require(t, err == nil && value.ConfigurationRevision.InstanceId == instance, "embedded event revision = %+v, want instance %q", value.ConfigurationRevision, instance)
			return
		case <-deadline:
			t.Fatal("no configuration event reached the embedded subscription")
		}
	}
}
