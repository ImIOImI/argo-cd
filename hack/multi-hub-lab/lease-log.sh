#!/usr/bin/env bash
# Prints each hub's structured multihub lease log lines (those with a
# multihub.event field) as compact rows: time level event path holder gen msg.
#   AFTER=2026-11-14T16:30 SINCE=10m ./lease-log.sh
#   RAW=1 ./lease-log.sh    # the JSON lines as logged
set -uo pipefail
SINCE="${SINCE:-10m}"
AFTER="${AFTER:-}"
for h in hub-a hub-b; do
  echo "== $h"
  kubectl --context "kind-$h" -n argocd logs statefulset/argocd-application-controller --since="$SINCE" 2>/dev/null \
    | grep '"multihub.event"' \
    | python3 -c '
import json, sys
after, raw = sys.argv[1], sys.argv[2] == "1"
for line in sys.stdin:
    try: e = json.loads(line)
    except ValueError: continue
    if e.get("time", "") < after: continue
    if raw: print(line.rstrip()); continue
    print(e.get("time","")[11:19], e.get("level","").ljust(5), e.get("multihub.event","").ljust(9),
          e.get("multihub.path","").ljust(9), (e.get("multihub.holder") or "-").ljust(6),
          str(e.get("multihub.generation","-")).ljust(3), e.get("msg",""))
' "$AFTER" "${RAW:-0}"
done
