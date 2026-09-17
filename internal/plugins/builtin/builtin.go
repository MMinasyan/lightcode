// Package builtin assembles Lightcode's shipped plugin registration set: the
// durable SQLite session store, the native file and command tools, the
// bundled model adaptation, and the LSP tools. It is ordinary composition
// data with no configuration logic and no additional Core authority; a custom
// build supplies its own plugin slice through the same runtime.Open.
package builtin

import (
	"github.com/MMinasyan/lightcode/internal/plugins/adaptation"
	"github.com/MMinasyan/lightcode/internal/plugins/lsp"
	"github.com/MMinasyan/lightcode/internal/plugins/sqlite"
	"github.com/MMinasyan/lightcode/internal/plugins/tools"
	"github.com/MMinasyan/lightcode/runtime"
)

// Plugins returns the shipped registration set in registration order.
func Plugins() []runtime.Plugin {
	return []runtime.Plugin{sqlite.Plugin(), tools.Plugin(), adaptation.Plugin(), lsp.Plugin()}
}
