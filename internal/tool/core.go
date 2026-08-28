package tool

import (
	"github.com/MMinasyan/lightcode/internal/config"
)

// CoreTools returns the shared core file tools (read_file, write_file,
// edit_file, apply_patch), each pre-wrapped with permission. The factory is
// the single source of truth for these four tools; both the main agent
// (agent.go) and the subagent (task.go) build their tool registries from
// it so apply_patch enters the system through the same construction path
// as every other core tool.
func CoreTools(
	store SnapshotStore,
	tracker *FileTracker,
	cfg config.ToolsConfig,
	root string,
	check CheckFunc,
	ask AskFunc,
) map[string]Tool {
	return CoreToolsWithOptions(store, tracker, cfg, root, check, ask, CapabilityOptions{})
}

func CoreToolsWithOptions(
	store SnapshotStore,
	tracker *FileTracker,
	cfg config.ToolsConfig,
	root string,
	check CheckFunc,
	ask AskFunc,
	opts CapabilityOptions,
) map[string]Tool {
	tools := CoreToolListWithOptions(store, tracker, cfg, root, check, ask, opts)
	out := make(map[string]Tool, len(tools))
	for _, tool := range tools {
		out[tool.Name()] = tool
	}
	return out
}

func CoreToolList(
	store SnapshotStore,
	tracker *FileTracker,
	cfg config.ToolsConfig,
	root string,
	check CheckFunc,
	ask AskFunc,
) []Tool {
	return CoreToolListWithOptions(store, tracker, cfg, root, check, ask, CapabilityOptions{})
}

func CoreToolListWithOptions(
	store SnapshotStore,
	tracker *FileTracker,
	cfg config.ToolsConfig,
	root string,
	check CheckFunc,
	ask AskFunc,
	opts CapabilityOptions,
) []Tool {
	return []Tool{
		WrapWithPermissionAtRoot(NewReadFileAtRoot(cfg, tracker, root), root, check, ask),
		WrapWithPermissionAtRootWithOptions(NewWriteFileWithSnapshotAtRoot(store, tracker, cfg, root), root, check, ask, opts),
		WrapWithPermissionAtRootWithOptions(NewEditFileWithSnapshotAtRoot(store, tracker, cfg, root), root, check, ask, opts),
		WrapWithPermissionAtRootWithOptions(NewApplyPatchWithSnapshotAtRoot(store, tracker, cfg, root), root, check, ask, opts),
	}
}
