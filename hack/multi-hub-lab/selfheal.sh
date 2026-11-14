#!/usr/bin/env bash
# Turns automated self-heal on or off for lab-app on both hubs (prune stays on).
#   ./selfheal.sh off
#   ./selfheal.sh on
set -euo pipefail

case "${1:-}" in
  on) v=true ;; off) v=false ;;
  *) echo "usage: $0 on|off" >&2; exit 2 ;;
esac
for h in hub-a hub-b; do
  kubectl --context "kind-$h" -n argocd patch application lab-app --type merge \
    -p "{\"spec\":{\"syncPolicy\":{\"automated\":{\"prune\":true,\"selfHeal\":${v}}}}}"
done
