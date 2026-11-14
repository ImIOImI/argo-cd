#!/usr/bin/env bash
# Turns automated sync on with self-heal on or off for lab-app (prune stays on).
# Defaults to both hubs; HUBS narrows it.
#   ./selfheal.sh off
#   HUBS=hub-a ./selfheal.sh on
set -euo pipefail

case "${1:-}" in
  on) v=true ;; off) v=false ;;
  *) echo "usage: $0 on|off" >&2; exit 2 ;;
esac
for h in ${HUBS:-hub-a hub-b}; do
  kubectl --context "kind-$h" -n argocd patch application lab-app --type merge \
    -p "{\"spec\":{\"syncPolicy\":{\"automated\":{\"prune\":true,\"selfHeal\":${v}}}}}"
done
