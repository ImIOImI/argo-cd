#!/usr/bin/env bash
# Sets how long the lab PreSync hook runs, and commits that (without pushing:
# hooks don't count toward sync status, so follow with ./bump.sh, which pushes
# both commits). A long hook gives time to kill a hub mid-hook (C3).
#   ./hook-sleep.sh 120 && ./bump.sh
set -euo pipefail
secs="${1:?usage: $0 SECONDS}"
cd "$(dirname "$0")"
f=app/presync-hook.yaml
sed -i -E "s/(sleep )[0-9]+/\1${secs}/" "$f"
git add "$f"
git commit -q -m "lab: PreSync hook sleeps ${secs}s" || echo "hook already sleeps ${secs}s"
grep -o 'sleep [0-9]*' "$f"
