#!/usr/bin/env bash
# After AFTER (UTC, e.g. 2026-11-22T03:00:00): per hub, the sync operations
# it started for each lab app ("Initialized new operation", one per
# operation), and per app namespace on the spoke, the PreSync hook Jobs
# created (one per hook run).
#   SINCE=15m ./ops-count.sh 2026-11-22T03:00:00
set -uo pipefail
after="${1:?usage: $0 AFTER}"
for h in $("$(dirname "$0")"/lab-hubs.sh); do
  kubectl --context "kind-$h" -n argocd logs statefulset/argocd-application-controller --since="${SINCE:-30m}" 2>/dev/null \
    | grep '"Initialized new operation' | python3 -c '
import json, sys, collections
h, after = sys.argv[1:3]
c = collections.Counter()
for line in sys.stdin:
    try: e = json.loads(line)
    except ValueError: continue
    if e.get("time", "") >= after and e.get("application", "").startswith("lab-") and e.get("application") != "lab-root":
        c[e["application"]] += 1
print(f"{h}: operations={sum(c.values())}", dict(sorted(c.items())))' "$h" "$after"
done
kubectl --context kind-spoke get jobs -A -o json | python3 -c '
import json, sys, collections
after = sys.argv[1]
c = collections.Counter()
for j in json.load(sys.stdin)["items"]:
    m = j["metadata"]
    if m["name"].startswith("lab-presync-") and m["creationTimestamp"] >= after:
        c[m["namespace"]] += 1
print(f"hook jobs created: {sum(c.values())} in {len(c)} namespaces;", "per namespace:", dict(sorted(c.items())))' "$after"
