#!/usr/bin/env bash
# Prints each hub's effective multihub configuration, from the controller's
# "multihub coordination enabled" startup log line.
set -u
for h in hub-a hub-b; do
  line=$(kubectl --context "kind-$h" -n argocd logs statefulset/argocd-application-controller 2>/dev/null \
    | grep -m1 'multihub coordination enabled')
  if [ -z "$line" ]; then
    echo "$h (multihub not enabled)"
    continue
  fi
  printf '%s ' "$h"
  python3 -c 'import json,sys; e=json.loads(sys.argv[1]); print({k:v for k,v in e.items() if k.startswith("multihub.")})' "$line"
done
