#!/usr/bin/env bash
# Deletes the lab app's namespace on the spoke, so both hubs see every resource
# missing at the same moment and both want a full sync (hooks included).
# Recreates the R0 situation on demand. Prints the UTC time of the wipe.
set -euo pipefail
kubectl --context kind-spoke delete namespace lab --wait=true
echo "wiped spoke namespace lab at $(date -u +%H:%M:%SZ)"
