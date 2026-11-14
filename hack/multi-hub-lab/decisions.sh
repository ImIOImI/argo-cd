#!/usr/bin/env bash
# Prints each hub's auto-sync / multihub lease decisions from controller logs,
# with consecutive repeats collapsed and counted (uniq -c).
#
#   AFTER=2026-11-14T13:19 ./decisions.sh
#   SINCE=10m AFTER=... ALL=1 ./decisions.sh   # also the plain auto-sync skips
set -uo pipefail

SINCE="${SINCE:-10m}"
AFTER="${AFTER:-}"
if [ -n "${ALL:-}" ]; then
  pattern='multihub|Skipping auto-sync|Initiated automated sync|Waiting to start operation|Operation stopped|hard refresh'
else
  pattern='multihub|Skipping auto-sync: (operation lease|settling)|Waiting to start operation|Operation stopped'
fi

for h in hub-a hub-b; do
  echo "== $h"
  kubectl --context "kind-$h" -n argocd logs statefulset/argocd-application-controller --since="$SINCE" 2>/dev/null \
    | grep -E "$pattern" \
    | sed -E 's/.*"msg":"([^"]{0,150}).*"time":"([^"]+)".*/\2 \1/' \
    | awk -v after="$AFTER" '$1 >= after' \
    | uniq -f1 -c
done
