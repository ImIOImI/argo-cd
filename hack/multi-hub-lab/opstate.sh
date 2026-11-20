#!/usr/bin/env bash
# Each hub's view of lab-app's last operation (phase, startedAt, who started
# it, multihub Info, message), the mirrored operation on the spoke's Lease
# (C3), and the hook Jobs on the spoke (one per hook run).
set -u
for h in hub-a hub-b; do
  kubectl --context "kind-$h" -n argocd get application lab-app -o json 2>/dev/null | python3 -c '
import json, sys
h = sys.argv[1]
a = json.load(sys.stdin)
s = a.get("status", {}).get("operationState") or {}
op = s.get("operation", {})
info = ",".join("%s=%s" % (i["name"], i["value"]) for i in op.get("info") or [])
who = "auto" if op.get("initiatedBy", {}).get("automated") else op.get("initiatedBy", {}).get("username", "?")
print(h, s.get("phase"), "startedAt=" + str(s.get("startedAt")), "finishedAt=" + str(s.get("finishedAt")),
      "by=" + who, "rev=" + (s.get("syncResult") or {}).get("revision", "")[:7], ("info=" + info) if info else "",
      "op-pending=" + str("operation" in a), "|", s.get("message"))
' "$h"
done
kubectl --context kind-spoke -n argocd-coordination get lease multi-hub-lab.lab-app -o json 2>/dev/null | python3 -c '
import json, sys, base64, gzip
l = json.load(sys.stdin)
enc = l["metadata"].get("annotations", {}).get("multihub.argoproj.io/operation")
spec = l["spec"]
print("lease holder=" + (spec.get("holderIdentity") or "-"), "gen=" + str(spec.get("leaseTransitions")), "renew=" + str(spec.get("renewTime")))
if not enc:
    print("mirror (none)"); sys.exit()
head, _, state = gzip.decompress(base64.b64decode(enc)).partition(b"\n")
h, s = json.loads(head), json.loads(state)
print("mirror hub=%s gen=%s running=%s phase=%s startedAt=%s | %s" % (h["hub"], h["generation"], h["running"], s.get("phase"), s.get("startedAt"), s.get("message")))
'
echo "hook jobs:"
kubectl --context kind-spoke -n lab get jobs --no-headers -o custom-columns=NAME:.metadata.name,CREATED:.metadata.creationTimestamp,DONE:.status.succeeded 2>/dev/null | grep '^lab-presync-' | sort -k2
