#!/usr/bin/env bash
# Saves raw evidence for a research note into DIR:
#   <hub>-controller-autosync.jsonl  auto-sync decisions + started operations
#   <hub>-application.yaml           the Application (status, history)
#   spoke-jobs.txt                   hook Jobs on the spoke (one per hook run)
#   observe.log                      observe.sh samples, if present
#   ./collect.sh ../../research/data/<date>-<slug>
set -euo pipefail

dir="${1:?usage: $0 DIR}"
SINCE="${SINCE:-60m}"
mkdir -p "$dir"
for h in hub-a hub-b; do
  kubectl --context "kind-$h" -n argocd logs statefulset/argocd-application-controller --since="$SINCE" 2>/dev/null \
    | grep -E '"msg":"(Initiated automated sync|Skipping auto-sync|Initialized new operation)' \
    > "$dir/$h-controller-autosync.jsonl" || true
  kubectl --context "kind-$h" -n argocd get application lab-app -o yaml > "$dir/$h-application.yaml"
done
kubectl --context kind-spoke -n lab get jobs -o wide > "$dir/spoke-jobs.txt"
[ -f "$(dirname "$0")/observe.log" ] && cp "$(dirname "$0")/observe.log" "$dir/observe.log"
# Credentials should never appear here; refuse to leave them if they do.
if grep -ilE 'bearer|token|password' "$dir"/* >/dev/null; then
  echo "possible credential in $dir; review before committing" >&2
  exit 1
fi
echo "saved to $dir"
