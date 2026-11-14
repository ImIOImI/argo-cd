#!/usr/bin/env bash
# Prints every sync operation each hub's controller started, from its logs.
# This is the churn metric: one line = one sync. Spoke polling (observe.sh)
# misses flips that are reverted between samples.
#
#   ./syncs.sh                       # last 60 min
#   SINCE=20m ./syncs.sh             # log window
#   AFTER=2026-11-14T12:00 ./syncs.sh  # only operations at/after this UTC prefix
set -uo pipefail

SINCE="${SINCE:-60m}"
AFTER="${AFTER:-}"

for h in hub-a hub-b; do
  echo "== $h"
  kubectl --context "kind-$h" -n argocd logs statefulset/argocd-application-controller --since="$SINCE" 2>/dev/null \
    | grep '"msg":"Initialized new operation' \
    | sed -E 's/.*Revision:([0-9a-f]{7}).*SelfHealAttemptsCount:([0-9]+).*"time":"([^"]+)".*/\3 rev=\1 selfHealAttempt=\2/' \
    | awk -v after="$AFTER" '$1 >= after'
done
