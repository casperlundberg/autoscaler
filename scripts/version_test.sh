#!/usr/bin/env bash
#
# Checks scripts/version.sh against real repositories with real tags, because
# the thing it gets wrong would be a version string on a deployed binary that
# names the wrong release, and nothing downstream could tell.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
version="$here/version.sh"
failures=0

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

expect() { # description, actual, expected
  if [[ "$2" == "$3" ]]; then
    printf '  ok   %s\n' "$1"
  else
    printf '  FAIL %s\n       got      %s\n       expected %s\n' "$1" "$2" "$3"
    failures=$((failures + 1))
  fi
}

repo="$work/repo"
git init -q -b main "$repo"
git -C "$repo" config user.email test@example.com
git -C "$repo" config user.name test
commit() { echo "$1" >> "$repo/file"; git -C "$repo" add file; git -C "$repo" commit -q -m "$1"; }
short() { git -C "$repo" rev-parse --short=7 HEAD; }

commit one
commit two
expect "before any release it is a development build of 0.0.0, counting every commit" \
  "$("$version" "$repo")" "0.0.0-dev.2+$(short)"

git -C "$repo" tag -a v1.2.0 -m 1.2.0
expect "on a release tag it is that release" "$("$version" "$repo")" "1.2.0"

commit three
commit four
commit five
expect "after a release it is a development build of the next patch, sorting after the release" \
  "$("$version" "$repo")" "1.2.1-dev.3+$(short)"

echo dirty >> "$repo/file"
expect "uncommitted changes are marked in the build metadata" \
  "$("$version" "$repo")" "1.2.1-dev.3+$(short).dirty"
git -C "$repo" checkout -q -- file

echo scratch > "$repo/untracked"
expect "an untracked file counts as a change, as it does for Go's own build stamp" \
  "$("$version" "$repo")" "1.2.1-dev.3+$(short).dirty"
rm "$repo/untracked"

git -C "$repo" tag -a v1.3.0-rc.1 -m rc
expect "a pre-release tag is not a release to count from" \
  "$("$version" "$repo")" "1.2.1-dev.3+$(short)"

git -C "$repo" tag -a v1.10.0 -m 1.10.0 HEAD~1
expect "the nearest release is found by ancestry, not by sorting names" \
  "$("$version" "$repo")" "1.10.1-dev.1+$(short)"

git -C "$repo" checkout -q v1.2.0
echo dirty >> "$repo/file"
expect "a release tag with uncommitted changes is not that release" \
  "$("$version" "$repo")" "1.2.0+$(short).dirty"

if [[ $failures -gt 0 ]]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "version.sh: all checks passed"
