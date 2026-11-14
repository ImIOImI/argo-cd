#!/usr/bin/env bash
# One-shot view of coordination state: the revision pin on the spoke's Lease,
# each hub's sync status/revision for lab-app, and the ConfigMap generation.
set -u
kubectl --context kind-spoke -n argocd-coordination get lease multi-hub-lab.lab-app \
  -o jsonpath='{.metadata.annotations}' 2>/dev/null \
  | python3 -c 'import json,sys
raw = sys.stdin.read().strip()
d = json.loads(raw) if raw else {}
pin = {k.split("/")[1]: v for k, v in d.items() if "pinned" in k}
print("pin", pin if pin else "(none)")'
for h in hub-a hub-b; do
  kubectl --context "kind-$h" -n argocd get application lab-app \
    -o jsonpath="$h {.status.sync.status} {.status.sync.revision}{'\n'}"
done
kubectl --context kind-spoke -n lab get configmap lab-config \
  -o jsonpath='spoke generation={.data.generation} rv={.metadata.resourceVersion}{"\n"}'
