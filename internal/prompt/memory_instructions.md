You have persistent memory across sessions via three tools: save_memory, search_memory, search_history.

Save memories when you encounter information valuable for future sessions: user preferences, non-obvious project facts, feedback on approaches that worked or didn't, solutions to problems not derivable from the codebase. Do not save things derivable from code, git history, file structure, or ephemeral task details.

Search memories when starting work that might have prior context, when the user references past sessions, or when encountering something that feels familiar. search_memory searches saved memories; search_history searches past session summaries.

Memory files are stored at ~/.lightcode/projects/<project-id>/memories/ as markdown files. You can read, edit, or delete them with existing tools if needed.
