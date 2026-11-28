#!/usr/bin/env bash
# Reproduces R4 (research/2026-11-22-hub-membership.md) on lab-app: a refresh
# records a new commit as "compared" without syncing it, and the next sync of
# that commit comes out PARTIAL (hooks skipped) instead of full.
#   1. HUB syncs lab-app with a 120s PreSync hook;
#   2. mid-hook, a new commit is pushed and HUB refreshes (the sync is still
#      running, so auto-sync can't act: the new commit is compared, not synced);
#   3. once the sync finishes, HUB's next auto-sync of the new commit is
#      reported as full or partial, with the hook Jobs per revision.
#   HUB=hub-a ./repro-r4.sh
set -uo pipefail
cd "$(dirname "$0")"
hub="${HUB:-hub-a}"; ctx="kind-$hub"
t0=$(date -u +%Y-%m-%dT%H:%M:%S)
st() { kubectl --context "$ctx" -n argocd get application lab-app -o jsonpath='compared={.status.sync.revision} status={.status.sync.status} op={.status.operationState.phase} op-rev={.status.operationState.syncResult.revision}'; }

./hook-sleep.sh 120 >/dev/null && ./bump.sh && ./refresh.sh "$hub"
./wait-opmsg.sh "$hub" 'waiting for completion of hook' || exit 1
sleep 10
./bump.sh && sleep 5 && ./refresh.sh "$hub"
sleep 25
echo "mid-sync: $(st)" | cut -c1-200
for _ in $(seq 120); do
  p=$(kubectl --context "$ctx" -n argocd get application lab-app -o jsonpath='{.status.operationState.phase}')
  [ "$p" != Running ] && break; sleep 3
done
echo "first sync done: $(st)" | cut -c1-200
sleep 90
echo "after: $(st)" | cut -c1-200
echo "== lab-app operations since $t0 on $hub"
kubectl --context "$ctx" -n argocd logs statefulset/argocd-application-controller --since=15m \
  | grep '"application":"lab-app"' | grep '"Initialized new operation' | python3 -c '
import json, sys, re
t0 = sys.argv[1]
for line in sys.stdin:
    e = json.loads(line)
    if e["time"] < t0: continue
    m = e["msg"]; rev = re.search(r"Revision:([0-9a-f]{7})", m)
    print(" ", e["time"][11:19], "rev", rev.group(1) if rev else "?", "full" if "Resources:[]SyncOperationResource{}" in m else "PARTIAL (no hooks)")' "$t0"
echo "== lab-app hook Jobs since $t0"
kubectl --context kind-spoke -n lab get jobs --no-headers -o custom-columns=N:.metadata.name,C:.metadata.creationTimestamp | awk -v t="$t0" '$2 >= t'
