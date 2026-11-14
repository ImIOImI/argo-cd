#!/usr/bin/env bash
# Pushes one new commit that changes the lab app (generation N -> N+1).
# Run from inside the fork's checkout, on the branch the hubs track.
set -euo pipefail

cd "$(dirname "$0")"
cm=app/configmap.yaml
cur=$(sed -n 's/^  generation: "\([0-9]*\)"/\1/p' "$cm")
next=$((cur + 1))
sed -i "s/^  generation: \"${cur}\"/  generation: \"${next}\"/" "$cm"
git add "$cm"
git commit -q -m "lab: bump generation to ${next}"
git push -q
echo "pushed generation ${next} at $(date +%H:%M:%S)"
