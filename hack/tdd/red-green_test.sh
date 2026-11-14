#!/usr/bin/env bash
# Tests for red-green.sh. Builds a throwaway git repo holding a tiny Go
# module with a known history, then checks red-green.sh accepts a proper
# red -> green pair and rejects everything else.
#
#   hack/tdd/red-green_test.sh
set -uo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
checker="$here/red-green.sh"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

pass=0
fail=0
check() { # check <name> <expected-exit: 0|nonzero> <cmd...>
  local name=$1 want=$2
  shift 2
  local out code
  out=$("$@" 2>&1)
  code=$?
  if { [ "$want" = 0 ] && [ "$code" = 0 ]; } || { [ "$want" = nonzero ] && [ "$code" != 0 ]; }; then
    pass=$((pass + 1))
    echo "ok   $name"
  else
    fail=$((fail + 1))
    echo "FAIL $name (exit $code, want $want)"
    printf '%s\n' "$out" | sed 's/^/     | /'
  fi
}
expect_output() { # expect_output <name> <regex> <cmd...>
  local name=$1 re=$2
  shift 2
  local out
  out=$("$@" 2>&1)
  if printf '%s' "$out" | grep -Eq "$re"; then
    pass=$((pass + 1))
    echo "ok   $name"
  else
    fail=$((fail + 1))
    echo "FAIL $name (output does not match /$re/)"
    printf '%s\n' "$out" | sed 's/^/     | /'
  fi
}

# --- fixture repo -----------------------------------------------------------
repo="$work/repo"
mkdir -p "$repo/calc"
git -C "$repo" init -q
git -C "$repo" config user.email tdd@example.invalid
git -C "$repo" config user.name tdd-fixture
cat > "$repo/go.mod" <<'EOF'
module example.invalid/fixture

go 1.21
EOF
commit() { git -C "$repo" add -A && git -C "$repo" commit -qm "$1" && git -C "$repo" rev-parse HEAD; }

# base: a package with no tests
cat > "$repo/calc/calc.go" <<'EOF'
package calc
EOF
base=$(commit "base")

# build-red: a test that references a function that doesn't exist (build failure)
cat > "$repo/calc/calc_test.go" <<'EOF'
package calc

import "testing"

func TestAdd(t *testing.T) {
	if Add(2, 3) != 5 {
		t.Fatalf("Add(2,3) = %d, want 5", Add(2, 3))
	}
}
EOF
build_red=$(commit "test(red): Add, no stub")

# red: same test plus a compiling stub, so the failure is an assertion
cat > "$repo/calc/calc.go" <<'EOF'
package calc

// Add is a stub.
func Add(a, b int) int { return 0 }
EOF
red=$(commit "test(red): Add, with stub")

# green: implementation
cat > "$repo/calc/calc.go" <<'EOF'
package calc

func Add(a, b int) int { return a + b }
EOF
green=$(commit "feat: Add")

# unrelated: a later commit on a side branch (not a descendant of red)
git -C "$repo" checkout -q -b side "$base"
cat > "$repo/calc/other.go" <<'EOF'
package calc
EOF
side=$(commit "side")
git -C "$repo" checkout -q -

run() { REPO="$repo" "$checker" "$@"; }

# --- cases ------------------------------------------------------------------
check "accepts red (assertion failure) -> green" 0 run "$red" "$green" ./calc
expect_output "reports the failing test on red" 'RED .*TestAdd' run "$red" "$green" ./calc
expect_output "reports green passing" 'GREEN .*ok' run "$red" "$green" ./calc
check "accepts a -run filter" 0 run "$red" "$green" ./calc '^TestAdd$'

check "rejects a build failure as red by default" nonzero run "$build_red" "$green" ./calc
expect_output "explains build failure" 'build fail' run "$build_red" "$green" ./calc
check "accepts a build failure with ALLOW_BUILD_FAILURE=1" 0 env ALLOW_BUILD_FAILURE=1 REPO="$repo" "$checker" "$build_red" "$green" ./calc

check "rejects red that passes (green, green)" nonzero run "$green" "$green" ./calc
check "rejects green that fails (red, red)" nonzero run "$red" "$red" ./calc
check "rejects reversed order (green before red)" nonzero run "$green" "$red" ./calc
check "rejects green that is not a descendant of red" nonzero run "$red" "$side" ./calc
check "rejects a -run filter that matches no tests on red" nonzero run "$red" "$green" ./calc '^TestNothing$'
check "rejects unknown commits" nonzero run deadbeef "$green" ./calc
check "rejects missing arguments" nonzero run "$red"

# the checker must not leave worktrees or touch the repo's checkout
check "leaves no worktrees behind" 0 test "$(git -C "$repo" worktree list | wc -l)" -eq 1
check "leaves the working tree clean" 0 test -z "$(git -C "$repo" status --porcelain)"

echo
echo "$pass passed, $fail failed"
[ "$fail" -eq 0 ]
