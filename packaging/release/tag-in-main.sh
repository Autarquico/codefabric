#!/bin/sh
# Fail unless COMMIT is an ancestor of (or equal to) the tip of REF. Fail closed on anything unresolvable.
# Usage: tag-in-main.sh COMMIT [REF]      (REF defaults to origin/main; run it in the repository)
set -eu

[ $# -ge 1 ] && [ $# -le 2 ] || { echo "usage: tag-in-main.sh COMMIT [REF]" >&2; exit 2; }
commit=$1
ref=${2:-origin/main}

git rev-parse --verify --quiet "$commit^{commit}" >/dev/null || { echo "tag-in-main: $commit is not a commit" >&2; exit 2; }
git rev-parse --verify --quiet "$ref^{commit}" >/dev/null || { echo "tag-in-main: $ref does not resolve" >&2; exit 2; }
git merge-base --is-ancestor "$commit" "$ref" || { echo "tag-in-main: $commit is not in $ref" >&2; exit 1; }
echo "tag-in-main: $commit is in $ref"
