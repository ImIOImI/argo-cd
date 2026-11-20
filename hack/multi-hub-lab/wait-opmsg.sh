#!/usr/bin/env bash
# Blocks until HUB's lab-app operation message matches REGEX (max ~5 min).
#   ./wait-opmsg.sh hub-a 'waiting for healthy state'
set -uo pipefail
h="${1:?usage: $0 HUB REGEX}" re="${2:?usage: $0 HUB REGEX}"
for _ in $(seq 1500); do
  m=$(kubectl --context "kind-$h" -n argocd get application lab-app -o jsonpath='{.status.operationState.phase} {.status.operationState.message}' 2>/dev/null)
  echo "$m" | grep -Eq "$re" && { echo "$h: $m at $(date -u +%H:%M:%SZ)"; exit 0; }
  sleep 0.2
done
echo "timed out; $h: $m" >&2
exit 1
