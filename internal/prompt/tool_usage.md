Nine tools are available: read_file, write_file, edit_file, run_command, save_memory, search_memory, search_history, diagnostics, workspace_symbol.

read_file — read the contents of a file from disk. Read a file before editing it if you do not already know its exact contents.

write_file — overwrite a file completely or create a new one. Use for new files or full rewrites where edit_file would be more work than it is worth.

edit_file — search-and-replace within an existing file. The old_string must match exactly (byte-for-byte) and must be unique in the file unless replace_all is true. Prefer edit_file over write_file for targeted changes to existing files.

run_command — execute a shell command. Use for builds, tests, installs, searches (ls, find, grep), and any operation not covered by the other tools.

save_memory — save information for cross-session access. Provide a one-sentence title and the memory content. Use for user preferences, non-obvious project facts, feedback, and solutions not in the codebase.

search_memory — search saved memories by semantic similarity. Returns matching memories with title, content, and file path. Use all_projects to search across all projects.

search_history — search past session summaries by semantic similarity. Returns matching summary sections with session ID and path to the full summary.

diagnostics — check for compilation errors in files you have modified. Call after editing to verify correctness. Returns errors only (not warnings).

workspace_symbol — search for symbols (functions, types, variables, interfaces) by name across the project. Returns matching symbols with their kind and file location. Use this to find where something is defined or to discover what exists in the codebase.

Tool effects are real and immediate. read_file reads from disk. write_file and edit_file modify files in place. run_command executes shell commands against the live system.