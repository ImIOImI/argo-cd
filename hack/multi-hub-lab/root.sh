#!/usr/bin/env bash
# Bootstraps both hubs with the root app (bootstrap/root.yaml): the one
# `kubectl apply` per hub; after that the root manages itself and the shared
# argocd-cm keys from Git.
#   ./root.sh
set -euo pipefail
cd "$(dirname "$0")"
for h in ${HUBS:-hub-a hub-b}; do
  kubectl --context "kind-$h" apply -f bootstrap/root.yaml
done
