package runtime

import (
	"context"
	"encoding/json"
	"os"
	"sync"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
	"github.com/MMinasyan/lightcode/internal/config"
)

// ConfigurationSource supplies and replaces one complete configuration
// document. Read reports a missing document as an error wrapping
// fs.ErrNotExist; Write is an atomic replacement that fails with the previous
// bytes unchanged.
type ConfigurationSource interface {
	Read(ctx context.Context) ([]byte, error)
	Write(ctx context.Context, data []byte) error
}

// CredentialSource supplies credential observations and managed updates
// without mutating the process environment. Capture is the one observation
// each configuration build publishes; Set and Remove own only the selected
// managed location and refuse externally defined names; SubprocessEnv
// returns the environment a child process may receive with excluded names
// removed.
type CredentialSource interface {
	Capture(ctx context.Context, names []string) (map[string]CredentialValue, error)
	Set(ctx context.Context, name, value string) error
	Remove(ctx context.Context, name string) error
	SubprocessEnv(ctx context.Context, excluded []string) ([]string, error)
}

// CredentialValue is one source-coherent credential observation.
type CredentialValue = config.CredentialValue

// Locations are the independently optional Runtime locations. An omitted
// location performs no lookup there.
type Locations struct {
	GlobalRulesDir     string
	DiscoveryCacheDir  string
	WorkspacePolicyDir string
	WireDebugDir       string
}

// PolicyInputs are the captured permission inputs one evaluator factory
// receives: the Workspace and its captured global and Workspace-policy raw
// values.
type PolicyInputs struct {
	Workspace       string
	Global          json.RawMessage
	WorkspacePolicy json.RawMessage
}

// fileConfiguration is one explicit-path configuration source.
type fileConfiguration struct {
	path string
}

// NewFileConfigurationSource returns a configuration source over the explicit
// path. Reads never create the document; writes are atomic replacements.
func NewFileConfigurationSource(path string) ConfigurationSource {
	return &fileConfiguration{path: path}
}

func (s *fileConfiguration) Read(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return os.ReadFile(s.path) // #nosec G703 -- the path is the host's explicitly selected configuration location.
}

func (s *fileConfiguration) Write(_ context.Context, data []byte) error {
	return atomicfs.Write(s.path, data, 0o600)
}

// valueConfiguration is the locked value-backed configuration source: one
// supplied document in memory, replaced atomically under its own lock.
type valueConfiguration struct {
	mu   sync.Mutex
	data []byte
}

// NewValueConfigurationSource returns a configuration source backed by the
// supplied bytes. Writes replace the held bytes; readers receive an owned
// copy.
func NewValueConfigurationSource(data []byte) ConfigurationSource {
	return &valueConfiguration{data: append([]byte(nil), data...)}
}

func (s *valueConfiguration) Read(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.data...), nil
}

func (s *valueConfiguration) Write(_ context.Context, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = append([]byte(nil), data...)
	return nil
}

// environmentCredentials is one explicit dotenv file backed by the process
// environment for shell precedence and child-environment projection.
type environmentCredentials struct {
	env *config.ManagedEnv
}

// NewEnvironmentCredentialSource returns a credential source over the
// explicit dotenv path. Capture reads the file on every call; the process
// environment wins and stays unmanaged. Set and Remove update only the file,
// refusing externally defined names.
func NewEnvironmentCredentialSource(path string) CredentialSource {
	return &environmentCredentials{env: config.NewManagedEnv(path)}
}

func (s *environmentCredentials) Capture(ctx context.Context, names []string) (map[string]CredentialValue, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.env.Capture(names)
}

func (s *environmentCredentials) Set(_ context.Context, name, value string) error {
	return s.env.TrySet(name, value)
}

func (s *environmentCredentials) Remove(_ context.Context, name string) error {
	return s.env.TryRemove(name)
}

func (s *environmentCredentials) SubprocessEnv(ctx context.Context, excluded []string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return s.env.SubprocessEnv(excluded), nil
}
