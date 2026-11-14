#!/usr/bin/env bash
# Rolls lab-app back on ONE hub to a status.history entry, mirroring what the
# API server's Rollback does (server/application/application.go, (*Server).Rollback):
#   1. auto-sync must be off on that hub (the server refuses otherwise), so this
#      turns it off, as an operator would before rolling back;
#   2. writes a sync Operation for that history entry's revision + source with
#      the apply strategy, prune=false, the app's syncOptions.
#
#   ./rollback.sh hub-b 0      # roll hub-b back to history id 0
set -euo pipefail

hub="${1:?usage: $0 HUB HISTORY_ID}"
id="${2:?usage: $0 HUB HISTORY_ID}"
ctx="kind-$hub"

kubectl --context "$ctx" -n argocd patch application lab-app --type json \
  -p '[{"op":"remove","path":"/spec/syncPolicy/automated"}]'

patch=$(kubectl --context "$ctx" -n argocd get application lab-app -o json | python3 -c '
import json, sys
app = json.load(sys.stdin)
hid = int(sys.argv[1])
h = next((x for x in app["status"].get("history", []) if x["id"] == hid), None)
if h is None:
    sys.exit(f"no history entry with id {hid}")
op = {"sync": {
        "revision": h["revision"],
        "prune": False,
        "syncStrategy": {"apply": {}},
        "source": h["source"],
        "syncOptions": app["spec"].get("syncPolicy", {}).get("syncOptions", []),
      },
      "initiatedBy": {"username": "lab-rollback"}}
print(json.dumps({"operation": op}))
' "$id")

kubectl --context "$ctx" -n argocd patch application lab-app --type merge -p "$patch"
echo "rolled $hub back to history id $id at $(date -u +%H:%M:%SZ)"
