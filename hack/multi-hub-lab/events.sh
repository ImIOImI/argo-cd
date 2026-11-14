#!/usr/bin/env bash
# Kubernetes Events about lab-app on each hub (multihub ones only unless ALL=1).
set -uo pipefail
for h in hub-a hub-b; do
  echo "== $h"
  kubectl --context "kind-$h" -n argocd get events \
    --field-selector involvedObject.kind=Application,involvedObject.name=lab-app \
    --sort-by=.lastTimestamp -o custom-columns=LAST:.lastTimestamp,TYPE:.type,REASON:.reason,COUNT:.count,MESSAGE:.message \
    | { if [ -n "${ALL:-}" ]; then cat; else grep -E 'LAST|Multihub'; fi; }
done
