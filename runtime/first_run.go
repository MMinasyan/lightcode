package runtime

import (
	"errors"
	"io/fs"
	"os"

	"github.com/MMinasyan/lightcode/internal/atomicfs"
)

// mainConfigSkeleton is the application's first-run configuration: it
// enables the supplied optional extensions without choosing a model. Only a
// missing document is ever created; an existing user file is never rewritten.
const mainConfigSkeleton = `{
  "providers": {},
  "extensions": {"agents": {"enabled": true}, "adaptation": {"enabled": true}}
}
`

// InitializeFirstRunConfiguration creates the application's first-run
// configuration at path when it is absent, exclusively, and leaves any
// existing document untouched. Callers invoke it before Open; the owner itself
// never creates a missing document.
func InitializeFirstRunConfiguration(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, err := atomicfs.CreateExclusive(path, []byte(mainConfigSkeleton), 0o600)
	return err
}
