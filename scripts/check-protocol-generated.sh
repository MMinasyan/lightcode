# Freshness gate for the generated protocol artifacts, shared by
# `make check-protocol` and the isolated regression test: the regenerated
# worktree bytes must match the staged (index) state exactly, and no new
# untracked output may appear under the artifact paths.
set -eu

git diff --exit-code -- protocol/protocol.gen.go frontend/src/generated/protocol

untracked=$(git ls-files --others --exclude-standard -- protocol/protocol.gen.go frontend/src/generated/protocol)
if [ -n "$untracked" ]; then
	echo "untracked generated protocol artifacts:" >&2
	printf '%s\n' "$untracked" >&2
	exit 1
fi
