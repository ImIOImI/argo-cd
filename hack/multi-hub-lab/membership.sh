#!/usr/bin/env bash
# Shows each hub's view: the HubMembership it has (spec), the hub records on
# the spoke (id, labels, last seen), and from each controller's log the
# latest refusal reason, if any.
set -uo pipefail
for h in $("$(dirname "$0")"/lab-hubs.sh); do
  printf '%s: ' "$h"
  kubectl --context "kind-$h" -n argocd get hubmembership default -o jsonpath='{.spec}' 2>/dev/null || printf 'no HubMembership'
  echo
done
echo "== hub records on the spoke"
kubectl --context kind-spoke -n argocd-coordination get lease -l multihub.argoproj.io/record=hub \
  -o custom-columns='ID:.metadata.annotations.multihub\.argoproj\.io/hub-id,LAST-SEEN:.spec.renewTime,LABELS:.metadata.annotations.multihub\.argoproj\.io/hub-labels' 2>/dev/null
