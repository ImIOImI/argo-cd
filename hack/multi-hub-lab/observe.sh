#!/usr/bin/env bash
# Samples both hubs and the spoke every INTERVAL seconds and prints one line per
# sample. Also appended to observe.log (gitignored) for later analysis.
#
# Columns:
#   cm_gen / cm_rv   value + resourceVersion of the ConfigMap on the spoke.
#                    A resourceVersion that keeps climbing with no new commit = churn.
#   hooks            PreSync Jobs that exist on the spoke (one per hook run).
#   a_* / b_*        each hub's sync status, the revision it compares to, and
#                    its last operation phase.
set -uo pipefail

INTERVAL="${INTERVAL:-2}"
LOG="$(dirname "$0")/observe.log"

hub() {
  kubectl --context "kind-$1" -n argocd get application lab-app -o \
    jsonpath='{.status.sync.status}{" "}{.status.sync.revision}{" "}{.status.operationState.phase}' 2>/dev/null \
    | awk '{printf "%-9s %.7s %-10s", $1, $2, $3}'
}

printf '%-8s %-6s %-8s %-5s | %-28s | %-28s\n' time cm_gen cm_rv hooks "hub-a sync/rev/op" "hub-b sync/rev/op" | tee -a "$LOG"
while true; do
  cm=$(kubectl --context kind-spoke -n lab get configmap lab-config \
    -o jsonpath='{.data.generation}{" "}{.metadata.resourceVersion}' 2>/dev/null || echo "- -")
  hooks=$(kubectl --context kind-spoke -n lab get jobs --no-headers 2>/dev/null | grep -c '^lab-presync-')
  printf '%-8s %-6s %-8s %-5s | %-28s | %-28s\n' \
    "$(date +%H:%M:%S)" ${cm} "$hooks" "$(hub hub-a)" "$(hub hub-b)" | tee -a "$LOG"
  sleep "$INTERVAL"
done
