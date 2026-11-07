// Package builtin assembles Lightcode's shipped optional native extension
// registration set: the bundled model adaptation and the child-session task
// tool. The durable SQLite store, the jobs service, and the seven standard
// tools are baseline services the Runtime owner constructs directly, so they
// are not part of this set. It is ordinary composition data with no
// configuration logic and no additional Core authority; a custom build
// supplies its own native slice through the same runtime.Open.
package builtin

import (
	"github.com/MMinasyan/lightcode/extension"
	"github.com/MMinasyan/lightcode/internal/plugins/adaptation"
	"github.com/MMinasyan/lightcode/internal/plugins/tasks"
)

// Plugins returns the shipped registration set in registration order.
func Plugins() []extension.Native {
	return []extension.Native{adaptation.Native(), tasks.Native()}
}

// DefaultCapabilityIDs returns the shipped application's default capability
// selection: the bundled model adaptation contribution. A custom host supplies
// its own ordinary selection through runtime.Options.
func DefaultCapabilityIDs() []string {
	return []string{adaptation.CapabilityID}
}
