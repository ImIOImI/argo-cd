#!/usr/bin/env bash
# Sets the periodic refresh period (argocd-cm timeout.reconciliation) on the
# given hubs (default both) and restarts their application controllers.
#   ./reconcile.sh 60s
set -euo pipefail
d="${1:?usage: $0 DURATION}"
for h in ${HUBS:-$("$(dirname "$0")"/lab-hubs.sh)}; do
  ctx="kind-$h"
  kubectl --context "$ctx" -n argocd patch configmap argocd-cm --type merge -p "{\"data\":{\"timeout.reconciliation\":\"$d\"}}" >/dev/null
  kubectl --context "$ctx" -n argocd rollout restart statefulset/argocd-application-controller >/dev/null
  kubectl --context "$ctx" -n argocd rollout status statefulset/argocd-application-controller --timeout=300s >/dev/null
  echo "$h: timeout.reconciliation=$d, controller restarted at $(date -u +%H:%M:%SZ)"
done
