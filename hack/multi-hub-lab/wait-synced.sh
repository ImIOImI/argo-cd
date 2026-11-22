#!/usr/bin/env bash
# Blocks until lab-app is Synced/Succeeded on every hub, or any reports a
# failure, or ~15 min pass. Prints the final state and the spoke's resources.
set -uo pipefail

s() { kubectl --context "kind-$1" -n argocd get application lab-app \
        -o jsonpath='{.status.sync.status}/{.status.operationState.phase}' 2>/dev/null; }

hubs=$("$(dirname "$0")"/lab-hubs.sh)
for _ in $(seq 300); do
  states=""; done_all=1
  for h in $hubs; do st=$(s "$h"); states="$states $h=$st"; [ "$st" = Synced/Succeeded ] || done_all=0; done
  [ "$done_all" = 1 ] && break
  case "$states" in *Failed*|*Error*) break ;; esac
  sleep 3
done
echo "$states"
for h in $hubs; do
  kubectl --context "kind-$h" -n argocd get application lab-app \
    -o jsonpath='{range .status.conditions[*]}{.type}: {.message}{"\n"}{end}' | cut -c1-300
done
kubectl --context kind-spoke -n lab get cm,deploy,jobs
