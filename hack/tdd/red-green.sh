#!/usr/bin/env bash
# Proves a TDD red -> green pair of commits:
#   * RED must be an ancestor of GREEN (and not the same commit);
#   * at RED, `go test` on PKG fails, with failing tests, not a build
#     error (ALLOW_BUILD_FAILURE=1 relaxes that);
#   * at GREEN, the same `go test` passes and actually runs tests.
# Each commit is checked out in a throwaway worktree; the caller's checkout
# is never touched.
#
#   hack/tdd/red-green.sh <red-commit> <green-commit> <package> [test-regex]
#   hack/tdd/red-green.sh 93dafdac0 HEAD ./controller/multihub/ '^TestPin'
#
# Env: REPO (default: the git toplevel of $PWD), ALLOW_BUILD_FAILURE=1.
# Exit: 0 = proven, 1 = not a valid red->green pair, 2 = usage / bad input.
set -uo pipefail

usage() {
  echo "usage: $0 <red-commit> <green-commit> <package> [test-regex]" >&2
  exit 2
}
[ $# -ge 3 ] && [ $# -le 4 ] || usage
red_ref=$1 green_ref=$2 pkg=$3 regex=${4:-}
repo="${REPO:-$(git rev-parse --show-toplevel 2>/dev/null)}"
[ -n "$repo" ] || { echo "not in a git repository (set REPO)" >&2; exit 2; }

resolve() {
  git -C "$repo" rev-parse --verify --quiet "$1^{commit}" \
    || { echo "unknown commit: $1" >&2; exit 2; }
}
red=$(resolve "$red_ref") || exit 2
green=$(resolve "$green_ref") || exit 2

fail() { echo "NOT PROVEN: $*" >&2; exit 1; }

[ "$red" != "$green" ] || fail "red and green are the same commit (${red:0:9})"
git -C "$repo" merge-base --is-ancestor "$red" "$green" \
  || fail "green ${green:0:9} is not a descendant of red ${red:0:9}"

tmp="$(mktemp -d)"
cleanup() {
  for wt in "$tmp/red" "$tmp/green"; do
    [ -d "$wt" ] && git -C "$repo" worktree remove --force "$wt" >/dev/null 2>&1
  done
  git -C "$repo" worktree prune >/dev/null 2>&1
  rm -rf "$tmp"
}
trap cleanup EXIT

run_tests() { # run_tests <name> <sha>; sets out, code
  local wt="$tmp/$1"
  git -C "$repo" worktree add --detach --quiet "$wt" "$2" || { echo "worktree failed for $2" >&2; exit 2; }
  local args=(test -count=1)
  [ -n "$regex" ] && args+=(-run "$regex")
  out=$(cd "$wt" && go "${args[@]}" "$pkg" 2>&1)
  code=$?
}

# --- red ---
run_tests red "$red"
red_out=$out
if [ "$code" -eq 0 ]; then
  printf '%s\n' "$red_out" | sed 's/^/  red | /' >&2
  fail "tests pass at red ${red:0:9}; a red commit must fail"
fi
build_failed=0
printf '%s\n' "$red_out" | grep -Eq '\[(build|setup) failed\]' && build_failed=1
failing=$(printf '%s\n' "$red_out" | sed -nE 's/^[[:space:]]*--- FAIL: ([^ ]+).*/\1/p' | sort -u | tr '\n' ' ')
if [ "$build_failed" -eq 1 ] && [ "${ALLOW_BUILD_FAILURE:-}" != 1 ]; then
  printf '%s\n' "$red_out" | sed 's/^/  red | /' >&2
  fail "red ${red:0:9} is a build failure, not a failing test. Add compiling stubs so the tests fail on assertions (or ALLOW_BUILD_FAILURE=1)"
fi
if [ "$build_failed" -eq 0 ] && [ -z "$failing" ]; then
  printf '%s\n' "$red_out" | sed 's/^/  red | /' >&2
  fail "red ${red:0:9} fails but no test reported --- FAIL"
fi
if [ "$build_failed" -eq 1 ]; then
  echo "RED   ${red:0:9} $pkg: build failure (allowed)"
else
  echo "RED   ${red:0:9} $pkg: FAIL ${failing% }"
fi

# --- green ---
run_tests green "$green"
if [ "$code" -ne 0 ]; then
  printf '%s\n' "$out" | sed 's/^/  green | /' >&2
  fail "tests fail at green ${green:0:9}"
fi
if printf '%s\n' "$out" | grep -q 'no tests to run'; then
  printf '%s\n' "$out" | sed 's/^/  green | /' >&2
  fail "no tests ran at green ${green:0:9}"
fi
echo "GREEN ${green:0:9} $pkg: ok"
echo "PROVEN red ${red:0:9} -> green ${green:0:9}"
