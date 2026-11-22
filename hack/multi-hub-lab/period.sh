#!/usr/bin/env bash
# Shows each hub's refresh period: the argocd-cm value, the field manager
# that owns it, and the hub-specific url (to show it is untouched).
set -uo pipefail
for h in $("$(dirname "$0")"/lab-hubs.sh); do
  kubectl --context "kind-$h" -n argocd get cm argocd-cm -o json --show-managed-fields | python3 -c '
import json, sys
cm = json.load(sys.stdin); h = sys.argv[1]
owners = [m["manager"] for m in cm["metadata"].get("managedFields", [])
          if "f:timeout.reconciliation" in json.dumps(m.get("fieldsV1", {}))]
print(h, "timeout.reconciliation=" + cm["data"].get("timeout.reconciliation", "-"),
      "owned-by=" + ",".join(owners), "url=" + cm["data"].get("url", "-"))' "$h"
done
