# Freshness gate for generated artifacts, shared by `make check-protocol` and
# `make check-wails-bindings`: the regenerated worktree bytes must match the
# staged (index) state exactly, and no new untracked output may appear under
# the paths the caller names. The caller supplies those paths; this checker
# holds no built-in path list, staging mutation or build invocation.
set -eu

if [ "$#" -eq 0 ]; then
	echo "usage: check-generated-files.sh PATH..." >&2
	exit 1
fi

git diff --exit-code -- "$@"

untracked=$(git ls-files --others --exclude-standard -- "$@")
if [ -n "$untracked" ]; then
	echo "untracked generated artifacts:" >&2
	printf '%s\n' "$untracked" >&2
	exit 1
fi
