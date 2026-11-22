#!/usr/bin/env bash
# Blocks until neither hub has a pending refresh annotation on lab-app (max ~5
# min), i.e. any requested (hard) refresh has finished. Use after a redeploy
# + refresh-both.sh to make sure both repo-server caches are warm.
set -uo pipefail
for h in $("$(dirname "$0")"/lab-hubs.sh); do
  for _ in $(seq 100); do
    a=$(kubectl --context "kind-$h" -n argocd get application lab-app -o jsonpath='{.metadata.annotations.argocd\.argoproj\.io/refresh}')
    [ -z "$a" ] && break
    sleep 3
  done
  kubectl --context "kind-$h" -n argocd get application lab-app -o jsonpath="$h reconciledAt={.status.reconciledAt} {.status.sync.status}{'\n'}"
done
