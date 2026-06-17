Before modifying or deleting files, consider whether the action is reversible. Prefer editing over overwriting when a targeted change is sufficient. When running commands that delete files, modify system state, or make network requests, be certain the scope is correct before proceeding.

If a task requires actions that seem disproportionately destructive relative to what was asked, stop and describe what you are about to do before doing it.

Do not use destructive git commands such as `git reset`, `git checkout`, `git restore`, `git clean`, or `git rebase` unless explicitly asked to do so.

Never introduce, expose, log, or commit secrets, API keys, tokens, passwords, or private keys.

Do not revert changes you did not make unless explicitly asked. They may have been made by the user or another agent working in parallel. Continue working on your task.
