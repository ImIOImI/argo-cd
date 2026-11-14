#!/usr/bin/env bash
# Blocks until lab-app is Synced/Succeeded on both hubs, or either reports a
# failure, or ~15 min pass. Prints the final state and the spoke's resources.
set -uo pipefail

s() { kubectl --context "kind-$1" -n argocd get application lab-app \
        -o jsonpath='{.status.sync.status}/{.status.operationState.phase}' 2>/dev/null; }

for _ in $(seq 300); do
  a=$(s hub-a); b=$(s hub-b)
  case "$a $b" in *Synced/Succeeded*Synced/Succeeded*) break ;; esac
  case "$a $b" in *Failed*|*Error*) break ;; esac
  sleep 3
done
echo "hub-a=$a hub-b=$b"
for h in hub-a hub-b; do
  kubectl --context "kind-$h" -n argocd get application lab-app \
    -o jsonpath='{range .status.conditions[*]}{.type}: {.message}{"\n"}{end}' | cut -c1-300
done
kubectl --context kind-spoke -n lab get cm,deploy,jobs
