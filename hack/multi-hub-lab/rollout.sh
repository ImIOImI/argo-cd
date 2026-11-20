#!/usr/bin/env bash
# Makes the next sync roll the lab Deployment, with new pods turning Ready
# only after SECONDS: the sync then waits that long in wave 0 before wave 1.
# Commits without pushing; follow with ./bump.sh, which pushes.
#   ./rollout.sh 120 && ./bump.sh
set -euo pipefail
secs="${1:?usage: $0 READY_DELAY_SECONDS}"
cd "$(dirname "$0")"
f=app/deployment.yaml
cur=$(sed -n 's/^        lab\/rollout: "\([0-9]*\)"/\1/p' "$f")
sed -i "s/^        lab\/rollout: \"${cur}\"/        lab\/rollout: \"$((cur + 1))\"/" "$f"
sed -i -E "s/(initialDelaySeconds: )[0-9]+/\1${secs}/" "$f"
git add "$f"
git commit -q -m "lab: roll the Deployment (rollout $((cur + 1)), ready after ${secs}s)"
echo "rollout $((cur + 1)), ready after ${secs}s"
