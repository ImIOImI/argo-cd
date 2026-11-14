#!/usr/bin/env bash
# Starts a user-style sync operation on one hub (what "Sync" in the UI writes):
# the app's own source at its targetRevision, initiated by a user. Leaves
# auto-sync settings alone.
#   ./manual-sync.sh hub-b
set -euo pipefail
hub="${1:?usage: $0 HUB}"
kubectl --context "kind-$hub" -n argocd patch application lab-app --type merge -p \
  '{"operation":{"sync":{"prune":true},"initiatedBy":{"username":"lab-manual-sync"}}}'
echo "manual sync requested on $hub at $(date -u +%H:%M:%SZ)"
