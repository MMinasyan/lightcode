A rules file (AGENTS.md or CLAUDE.md) may exist at two levels: global (~/.lightcode/) and project root. Both are loaded and concatenated. The rules file is where project-specific knowledge lives: architecture, conventions, build commands, key file locations.

The following default prompt sections can be replaced by adding a heading with the same name to the rules file (case-insensitive, any heading level):
- safety
- tone
- task_execution
- language

If a heading matching one of these names appears in the rules file, the default section is skipped and the rules file version is used instead.

What belongs in the rules file:
- Project structure outline: key directories, what each contains, important files
- Build and test commands
- Conventions specific to this project (naming, patterns, libraries in use)
- Personal preferences about how you want work done

What does not belong in the rules file:
- Things the system prompt already handles (tool usage, core rules, identity)
- General coding knowledge you already have

When working on a project for the first time, explore the directory structure and write a concise outline of key directories and files into the rules file. Keep this outline updated as the project evolves. An approximate outline of what matters is more useful than a raw file listing.