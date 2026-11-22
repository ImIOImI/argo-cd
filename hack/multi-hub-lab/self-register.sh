#!/usr/bin/env bash
# Registers each hub's own cluster (server https://kubernetes.default.svc)
# with labels: multihub.argoproj.io/hub-id=<hub> (convention: matches the
# hub's ARGOCD_MULTIHUB_HUB_ID) and multihub.argoproj.io/role=<ROLE>.
# Hub-specific, so applied per hub, never from the shared bootstrap/.
#   ./self-register.sh                 # every hub, role=hub
#   ROLE=retired HUBS=hub-c ./self-register.sh
set -euo pipefail
role="${ROLE:-hub}"
for h in ${HUBS:-$("$(dirname "$0")"/lab-hubs.sh)}; do
  kubectl --context "kind-$h" apply -f - <<EOF2
apiVersion: v1
kind: Secret
metadata:
  name: in-cluster
  namespace: argocd
  labels:
    argocd.argoproj.io/secret-type: cluster
    multihub.argoproj.io/hub-id: ${h}
    multihub.argoproj.io/role: ${role}
stringData:
  name: in-cluster
  server: https://kubernetes.default.svc
  config: '{"tlsClientConfig":{"insecure":false}}'
EOF2
done
