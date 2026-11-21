#!/usr/bin/env bash
# Counts each hub's refreshes of the lab apps between AFTER and BEFORE (UTC,
# e.g. 2026-11-21T10:00:00), from the application controller's
# "Reconciliation completed" lines with a comparison (comparison-level >= 1):
#   full   = this hub compared (rendered + diffed) itself
#   shared = this hub skipped and used another hub's result (C7)
# Status-only refreshes (level 0) aren't counted.
#   SINCE=40m ./refresh-counts.sh 2026-11-21T10:00:00 2026-11-21T10:15:00
set -uo pipefail
after="${1:?usage: $0 AFTER BEFORE}" before="${2:?usage: $0 AFTER BEFORE}"
for h in hub-a hub-b; do
  kubectl --context "kind-$h" -n argocd logs statefulset/argocd-application-controller --since="${SINCE:-60m}" 2>/dev/null \
    | grep '"Reconciliation completed"' \
    | python3 -c '
import json, sys, collections
h, after, before = sys.argv[1:4]
per = collections.defaultdict(lambda: [0, 0])
for line in sys.stdin:
    try: e = json.loads(line)
    except ValueError: continue
    t = e.get("time", "")
    if not (after <= t < before): continue
    app = e.get("application", "")
    if not (app == "lab-app" or app.startswith("lab-")): continue
    if int(e.get("comparison-level", 0)) < 1: continue
    per[app][1 if e.get("multihub.shared") else 0] += 1
full = sum(v[0] for v in per.values()); shared = sum(v[1] for v in per.values())
print(f"{h}: full={full} shared={shared} apps={len(per)}")
for app in sorted(per): print(f"  {app:10} full={per[app][0]:3} shared={per[app][1]:3}")
' "$h" "$after" "$before"
done
