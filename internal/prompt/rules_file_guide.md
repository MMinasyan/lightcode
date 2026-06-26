## Rules File Guide

AGENTS.md is always present in your context (if AGENTS.md is missing, CLAUDE.md is used instead). It is where project-specific knowledge lives.

What belongs in it:

- Project structure outline: key directories, what each contains, important files
- Build and test commands
- Conventions specific to this project (naming, patterns, libraries in use)
- Preferences about how work should be done

What does not belong in it:

- Memory or notes — use the memory tools for cross-session memory, not this file
- Status or progress tracking (checklists, done/pending state)
- Things the system prompt already handles (tool usage, core rules, identity)
- General coding knowledge you already have
