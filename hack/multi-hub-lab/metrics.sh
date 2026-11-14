#!/usr/bin/env bash
# Prints argocd_multihub_* metrics from each hub's application controller
# (port-forwards the controller's metrics port 8082 for a moment).
set -uo pipefail
port=18082
for h in hub-a hub-b; do
  echo "== $h"
  kubectl --context "kind-$h" -n argocd port-forward statefulset/argocd-application-controller "$port:8082" >/dev/null 2>&1 &
  pf=$!
  for _ in $(seq 20); do curl -sf "localhost:$port/metrics" >/dev/null && break; sleep 0.5; done
  curl -s "localhost:$port/metrics" | grep '^argocd_multihub_' || echo "(no argocd_multihub_ series)"
  kill "$pf" 2>/dev/null; wait "$pf" 2>/dev/null || true
done
