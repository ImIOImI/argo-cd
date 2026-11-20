#!/usr/bin/env bash
# Blocks until a PreSync hook Job pod for lab-app is Running on the spoke
# (max ~3 min), and prints its Job name.
set -uo pipefail
for _ in $(seq 900); do
  j=$(kubectl --context kind-spoke -n lab get pods --field-selector=status.phase=Running \
        -o jsonpath='{range .items[*]}{.metadata.labels.job-name}{"\n"}{end}' 2>/dev/null | grep '^lab-presync-' | head -1)
  [ -n "$j" ] && { echo "hook running: $j at $(date -u +%H:%M:%SZ)"; exit 0; }
  sleep 0.2
done
echo "timed out waiting for a running hook" >&2
exit 1
