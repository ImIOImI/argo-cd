#!/usr/bin/env bash
# Blocks until lab-app's multihub lease on the spoke is held by HUB (max ~60s).
#   ./wait-lease.sh hub-a
set -uo pipefail
hub="${1:?usage: $0 HUB}"
for _ in $(seq 300); do
  h=$(kubectl --context kind-spoke -n argocd-coordination get lease multi-hub-lab.lab-app \
        -o jsonpath='{.spec.holderIdentity}' 2>/dev/null)
  [ "$h" = "$hub" ] && { echo "lease held by $hub at $(date -u +%H:%M:%SZ)"; exit 0; }
  sleep 0.2
done
echo "timed out waiting for $hub to hold the lease (holder: ${h:-none})" >&2
exit 1
