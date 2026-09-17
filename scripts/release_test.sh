#!/usr/bin/env bash
#
# Checks scripts/release.sh against a real repository and a real remote. A
# release is a tag other people build from, so the refusals matter as much as
# the tag: each case below is a way to publish a version that does not mean
# what it says.
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
release="$here/release.sh"
failures=0

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

ok()   { printf '  ok   %s\n' "$1"; }
fail() { printf '  FAIL %s\n' "$1"; failures=$((failures + 1)); }

# refuses <description> <pattern the refusal must mention> <version>
refuses() {
  local what=$1 pattern=$2 version=$3 out
  if out="$(cd "$repo" && RELEASE_CHECK=true "$release" "$version" 2>&1)"; then
    fail "$what (it released)"
  elif grep -qi -- "$pattern" <<< "$out"; then
    ok "$what"
  else
    fail "$what (refused, but without saying '$pattern': $out)"
  fi
}

git init -q --bare -b main "$work/origin.git"
repo="$work/repo"
git clone -q "$work/origin.git" "$repo" 2>/dev/null
git -C "$repo" config user.email test@example.com
git -C "$repo" config user.name test
git -C "$repo" checkout -q -b main
printf '# Changelog\n\n## 1.0.0\n\nFirst.\n' > "$repo/CHANGELOG.md"
git -C "$repo" add CHANGELOG.md
git -C "$repo" commit -q -m first
git -C "$repo" push -q origin main

refuses "a version that is not MAJOR.MINOR.PATCH" "MAJOR.MINOR.PATCH" "1.0"
refuses "a pre-release version" "MAJOR.MINOR.PATCH" "1.0.0-rc.1"
refuses "a release the changelog does not describe" "CHANGELOG" "1.1.0"

if out="$(cd "$repo" && RELEASE_CHECK=true "$release" 1.0.0 2>&1)" \
   && git -C "$work/origin.git" rev-parse -q --verify refs/tags/v1.0.0 >/dev/null; then
  ok "a clean, described, pushed version is tagged and the tag reaches the remote"
else
  fail "releasing 1.0.0: $out"
fi
if [[ "$(git -C "$work/origin.git" cat-file -t v1.0.0 2>/dev/null)" == "tag" ]]; then
  ok "the tag is annotated, so it records who released it and when"
else
  fail "the tag is not annotated"
fi

printf '\n## 0.9.0\n\nOlder.\n' >> "$repo/CHANGELOG.md"
git -C "$repo" commit -q -am "describe 0.9.0"
refuses "a version not above the last release" "above" "0.9.0"
refuses "a version already released" "already" "1.0.0"

printf '\n## 1.1.0\n\nNext.\n' >> "$repo/CHANGELOG.md"
refuses "a working tree with uncommitted changes" "uncommitted" "1.1.0"
git -C "$repo" commit -q -am "describe 1.1.0"

git -C "$repo" checkout -q -b topic
refuses "a release from a branch other than main" "main" "1.1.0"
git -C "$repo" checkout -q main

other="$work/other"
git clone -q "$work/origin.git" "$other" 2>/dev/null
git -C "$other" config user.email test@example.com
git -C "$other" config user.name test
echo more > "$other/elsewhere"
git -C "$other" add elsewhere
git -C "$other" commit -q -m "someone else's change"
git -C "$other" push -q origin main
refuses "a main that is behind the remote" "behind" "1.1.0"

git -C "$repo" pull -q --rebase origin main
if out="$(cd "$repo" && RELEASE_CHECK=false "$release" 1.1.0 2>&1)"; then
  fail "a release whose checks fail was published"
elif grep -qi "checks" <<< "$out" && ! git -C "$work/origin.git" rev-parse -q --verify refs/tags/v1.1.0 >/dev/null; then
  ok "a release whose checks fail is not tagged"
else
  fail "failing checks: $out"
fi

if [[ $failures -gt 0 ]]; then
  echo "$failures check(s) failed"
  exit 1
fi
echo "release.sh: all checks passed"
