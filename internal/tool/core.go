package tool

import (
	"github.com/MMinasyan/lightcode/internal/config"
)

// CoreTools returns the shared core file tools (read_file, write_file,
// edit_file, apply_patch), each pre-wrapped with permission. The factory is
// the single source of truth for these four tools; both the main agent
// (agent.go) and the subagent (task.go) build their tool registries from
// it so apply_patch enters the system through the same construction path
// as every other core tool. execute_pending stays inline at each call site
// because it has no snapshot/tracker wiring and is a one-liner.
func CoreTools(
	store SnapshotStore,
	tracker *FileTracker,
	cfg config.ToolsConfig,
	root string,
	check CheckFunc,
	ask AskFunc,
) map[string]Tool {
	return map[string]Tool{
		"read_file":   WrapWithPermissionAtRoot(NewReadFileAtRoot(cfg, tracker, root), root, check, ask),
		"write_file":  WrapWithPermissionAtRoot(NewWriteFileWithSnapshotAtRoot(store, tracker, cfg, root), root, check, ask),
		"edit_file":   WrapWithPermissionAtRoot(NewEditFileWithSnapshotAtRoot(store, tracker, cfg, root), root, check, ask),
		"apply_patch": WrapWithPermissionAtRoot(NewApplyPatchWithSnapshotAtRoot(store, tracker, cfg, root), root, check, ask),
	}
}
