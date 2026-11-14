#!/usr/bin/env bash
# Prints each hub's effective multihub configuration, from the controller's
# "multihub coordination enabled" startup log line.
set -uo pipefail
for h in hub-a hub-b; do
  printf '%s ' "$h"
  kubectl --context "kind-$h" -n argocd logs statefulset/argocd-application-controller 2>/dev/null \
    | grep -m1 'multihub coordination enabled' \
    | python3 -c 'import json,sys; e=json.loads(sys.stdin.read()); print({k:v for k,v in e.items() if k.startswith("multihub.")})' \
    || echo "(multihub not enabled)"
done
