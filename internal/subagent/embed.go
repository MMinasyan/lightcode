package subagent

import "embed"

//go:embed types/*.md
var builtinFS embed.FS
