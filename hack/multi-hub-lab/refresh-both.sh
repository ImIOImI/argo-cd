#!/usr/bin/env bash
# Asks both hubs for a hard refresh of lab-app at (nearly) the same moment,
# so they discover a new commit together. Run right after bump.sh to make
# both hubs want a *full* sync (hooks included) of the same new revision:
# the R0 situation, on demand.
#   ./bump.sh && ./refresh-both.sh
set -euo pipefail
for h in hub-a hub-b; do
  kubectl --context "kind-$h" -n argocd annotate application lab-app \
    argocd.argoproj.io/refresh=hard --overwrite &
done
wait
echo "hard refresh requested on both hubs at $(date -u +%H:%M:%SZ)"
