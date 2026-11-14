#!/usr/bin/env bash
# Sets (KEY=VALUE) or unsets (KEY-) multihub settings on the application
# controller of the given hubs (default both) and waits for the rollout.
#   ./multihub-env.sh ARGOCD_MULTIHUB_LOG_DENIALS=all
#   HUBS=hub-b ./multihub-env.sh ARGOCD_MULTIHUB_EVENTS=false ARGOCD_MULTIHUB_LOG_DENIALS-
set -euo pipefail
[ $# -gt 0 ] || { echo "usage: $0 KEY=VALUE|KEY- ..." >&2; exit 2; }
for h in ${HUBS:-hub-a hub-b}; do
  kubectl --context "kind-$h" -n argocd set env statefulset/argocd-application-controller "$@"
  kubectl --context "kind-$h" -n argocd rollout status statefulset/argocd-application-controller --timeout=300s
done
