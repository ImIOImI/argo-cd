#!/usr/bin/env bash
# Asks one hub for a hard refresh of lab-app, so it discovers a new commit
# now (and, being first, takes the lease).
#   ./bump.sh && ./refresh.sh hub-a
set -euo pipefail
h="${1:?usage: $0 HUB}"
kubectl --context "kind-$h" -n argocd annotate application lab-app \
  argocd.argoproj.io/refresh=hard --overwrite >/dev/null
echo "hard refresh requested on $h at $(date -u +%H:%M:%SZ)"
