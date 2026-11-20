#!/usr/bin/env bash
# Stops or starts a hub's application controller: the "hub dies" lever for C3.
# stop deletes the pod immediately (no graceful release of anything).
#   ./controller.sh hub-a stop
#   ./controller.sh hub-a start
set -euo pipefail
h="${1:?usage: $0 HUB stop|start}" action="${2:?usage: $0 HUB stop|start}"
ctx="kind-$h"
case "$action" in
  stop)
    kubectl --context "$ctx" -n argocd scale statefulset argocd-application-controller --replicas=0 >/dev/null
    kubectl --context "$ctx" -n argocd delete pod argocd-application-controller-0 --grace-period=0 --force >/dev/null 2>&1 || true
    echo "$h controller stopped at $(date -u +%H:%M:%SZ)" ;;
  start)
    kubectl --context "$ctx" -n argocd scale statefulset argocd-application-controller --replicas=1 >/dev/null
    kubectl --context "$ctx" -n argocd rollout status statefulset argocd-application-controller --timeout=180s >/dev/null
    echo "$h controller running at $(date -u +%H:%M:%SZ)" ;;
  *) echo "usage: $0 HUB stop|start" >&2; exit 2 ;;
esac
