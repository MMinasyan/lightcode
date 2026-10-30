// Package builtin assembles Lightcode's shipped optional plugin
// registration set: the bundled model adaptation and the child-session task
// tool. The durable SQLite store, the jobs service, and the seven standard
// tools are baseline services the Runtime owner constructs directly, so they
// are not part of this set. It is ordinary composition data with no
// configuration logic and no additional Core authority; a custom build
// supplies its own plugin slice through the same runtime.Open.
package builtin

import (
	"github.com/MMinasyan/lightcode/internal/plugins/adaptation"
	"github.com/MMinasyan/lightcode/internal/plugins/tasks"
	"github.com/MMinasyan/lightcode/runtime"
)

// Plugins returns the shipped registration set in registration order.
func Plugins() []runtime.Plugin {
	return []runtime.Plugin{adaptation.Plugin(), tasks.Plugin()}
}
