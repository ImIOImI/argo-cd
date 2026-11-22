#!/usr/bin/env bash
# Creates N extra Applications lab-1..lab-N on BOTH hubs, identical to
# lab-app except for the name and destination namespace (lab-<i>), so that
# shared refresh (C7) has many apps to split. N=0 deletes them (without
# cascading: the resources stay on the spoke).
#   ./apps.sh 10
#   ./apps.sh 0
set -euo pipefail
n="${1:?usage: $0 N}"
for h in $("$(dirname "$0")"/lab-hubs.sh); do
  ctx="kind-$h"
  existing=$(kubectl --context "$ctx" -n argocd get applications -o name | grep -E '/lab-[0-9]+$' || true)
  if [ "$n" = 0 ]; then
    [ -n "$existing" ] && kubectl --context "$ctx" -n argocd delete $existing --wait=false
    continue
  fi
  kubectl --context "$ctx" -n argocd get application lab-app -o json | python3 -c '
import json, sys
base = json.load(sys.stdin)
n = int(sys.argv[1])
items = []
for i in range(1, n + 1):
    spec = json.loads(json.dumps(base["spec"]))
    spec["destination"]["namespace"] = f"lab-{i}"
    items.append({"apiVersion": "argoproj.io/v1alpha1", "kind": "Application",
                  "metadata": {"name": f"lab-{i}", "namespace": "argocd",
                               "annotations": base["metadata"].get("annotations", {})},
                  "spec": spec})
print(json.dumps({"apiVersion": "v1", "kind": "List", "items": items}))
' "$n" | kubectl --context "$ctx" apply -f -
done
