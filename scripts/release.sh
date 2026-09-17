#!/usr/bin/env bash
#
# Releases this repository as a semantic version: checks it, tags it, and
# pushes the commit and the tag together.
#
#   scripts/release.sh 1.3.0        (or: make release VERSION=1.3.0)
#
# A release is what a recorded run names, so it refuses anything that would
# make a version mean something other than one tested tree:
#
#   - a version that is not plain MAJOR.MINOR.PATCH, or is not above the last
#   - uncommitted or untracked changes
#   - a branch other than main, or a main behind its remote
#   - a CHANGELOG.md with no "## MAJOR.MINOR.PATCH" section for it
#   - checks that fail (RELEASE_CHECK, default `make check`)
#
# The commit and the tag are pushed atomically, so CI building the commit
# already sees the tag and stamps the image with the release version rather
# than a development one.
set -euo pipefail

version="${1:-}"
check="${RELEASE_CHECK:-make check}"

refuse() { echo "release: $*" >&2; exit 1; }

[[ "$version" =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] \
  || refuse "'$version' is not a release version: give MAJOR.MINOR.PATCH, e.g. 1.3.0"

[[ -z "$(git status --porcelain)" ]] \
  || refuse "the working tree has uncommitted or untracked changes; a release must be exactly one commit"

branch="$(git symbolic-ref --short HEAD 2>/dev/null || true)"
[[ "$branch" == "main" ]] || refuse "releases are cut from main, not from '${branch:-a detached HEAD}'"

git fetch -q --tags origin
if git rev-parse -q --verify refs/remotes/origin/main >/dev/null \
   && ! git merge-base --is-ancestor origin/main HEAD; then
  refuse "main is behind origin/main; pull first, so the release includes what is already published"
fi

git rev-parse -q --verify "refs/tags/v$version" >/dev/null \
  && refuse "v$version is already released"

last="$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --merged HEAD | grep -v -- - | sed 's/^v//' | sort -V | tail -1 || true)"
if [[ -n "$last" ]]; then
  highest="$(printf '%s\n%s\n' "$last" "$version" | sort -V | tail -1)"
  [[ "$highest" == "$version" && "$last" != "$version" ]] \
    || refuse "$version is not above the last release, $last"
fi

[[ -f CHANGELOG.md ]] && grep -qE "^## \[?${version//./\\.}\]?( |$)" CHANGELOG.md \
  || refuse "CHANGELOG.md has no '## $version' section; say what the release changes before releasing it"

echo "release: running checks: $check"
bash -c "$check" || refuse "checks failed; nothing was tagged"

# The changelog section becomes the tag message, so the tag carries its own
# account of what changed.
notes="$(awk -v v="$version" '
  $0 ~ "^## \\[?" v "\\]?( |$)" { on = 1; next }
  on && /^## / { exit }
  on { print }' CHANGELOG.md)"
git tag -a "v$version" -m "$version" -m "$notes"

git push -q --atomic origin HEAD:main "v$version"
echo "release: v$version tagged at $(git rev-parse --short HEAD) and pushed"
