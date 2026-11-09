package melta

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/MMinasyan/lightcode/harness"
	"github.com/MMinasyan/lightcode/internal/storage"
	"github.com/MMinasyan/lightcode/internal/testassert"
	"github.com/MMinasyan/lightcode/protocol"
)

type captureFailingSource struct {
	CredentialSource
	name string
}

func (s captureFailingSource) Capture(ctx context.Context, names []string) (map[string]CredentialValue, error) {
	for _, name := range names {
		if name == s.name {
			return nil, errors.New("capture unavailable")
		}
	}
	return s.CredentialSource.Capture(ctx, names)
}

// TestDiscoverProviderCredentialCaptureUsesConfigurationClass proves the
// custom discovery read's direct credential capture failure is the owner's
// configuration class, like connect and provider create.
func TestDiscoverProviderCredentialCaptureUsesConfigurationClass(t *testing.T) {
	dir := t.TempDir()
	options := Options{
		DataDir:       dir,
		Configuration: NewValueConfigurationSource([]byte(`{}`)),
		Credentials:   captureFailingSource{CredentialSource: NewEnvironmentCredentialSource(filepath.Join(dir, ".env")), name: "DISCOVERY_KEY"},
		OpenStore: func(context.Context, string) (harness.Storage, func() error, error) {
			return storage.NewMemory(), func() error { return nil }, nil
		},
	}
	owner, err := Open(context.Background(), options)
	testassert.NoError(t, err)
	defer func() { _ = owner.Close(context.Background()) }()
	_, err = owner.DiscoverProvider(context.Background(), testCaller(owner), protocol.DiscoveryRequest{BaseUrl: "https://discover.test/v1", ApiKeyEnv: "DISCOVERY_KEY"})
	testassert.Require(t, errors.Is(err, ErrConfiguration), "discovery capture = %v, want ErrConfiguration", err)
}
