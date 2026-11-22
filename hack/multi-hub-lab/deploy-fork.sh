#!/usr/bin/env bash
# Builds the argocd binary from this checkout, layers it onto the stock
# v3.6.1 image, and switches every Argo CD component on both hubs to it.
# Applies this checkout's CRDs first (the fork tracks master, which may add fields).
#
# Multihub coordination is enabled per hub with ARGOCD_MULTIHUB_HUB_ID on the
# application controller. MULTIHUB=off leaves it unset (fork binary, stock
# behaviour), which is how to re-run a baseline on the same binary.
#
#   ./deploy-fork.sh
#   MULTIHUB=off ./deploy-fork.sh
set -euo pipefail

BASE_IMAGE="${BASE_IMAGE:-quay.io/argoproj/argocd:v3.6.1}"
MULTIHUB="${MULTIHUB:-on}"
here="$(cd "$(dirname "$0")" && pwd)"
root="$(cd "$here/../.." && pwd)"
sha="$(git -C "$root" rev-parse --short=9 HEAD)"
dirty=""
git -C "$root" diff --quiet HEAD -- . || dirty="-dirty"
tag="argocd-multihub:${sha}${dirty}"

echo "== building argocd ($sha$dirty)"
build="$(mktemp -d)"
trap 'rm -rf "$build"' EXIT
(cd "$root" && CGO_ENABLED=0 go build -o "$build/argocd" ./cmd)
cat > "$build/Dockerfile" <<EOF
FROM ${BASE_IMAGE}
COPY argocd /usr/local/bin/argocd
EOF
docker build -q -t "$tag" "$build"

for hub in $("$(dirname "$0")"/lab-hubs.sh); do
  ctx="kind-$hub"
  echo "== $hub: load $tag"
  kind load docker-image "$tag" --name "$hub"
  kubectl --context "$ctx" apply --server-side --force-conflicts -k "$root/manifests/crds/" >/dev/null
  # Swap every container/initContainer that runs the argocd image; kind-loaded
  # images can't be pulled, so pull only if missing.
  kubectl --context "$ctx" -n argocd get deployment,statefulset -o json \
    | python3 -c '
import json, sys
tag = sys.argv[1]
items = json.load(sys.stdin)["items"]
for obj in items:
    spec = obj["spec"]["template"]["spec"]
    for c in spec.get("containers", []) + spec.get("initContainers", []):
        if "/argoproj/argocd:" in c["image"] or c["image"].startswith("argocd-multihub:"):
            c["image"] = tag
            c["imagePullPolicy"] = "IfNotPresent"
    for k in ("resourceVersion", "uid", "creationTimestamp", "generation", "managedFields"):
        obj["metadata"].pop(k, None)
    obj.pop("status", None)
print(json.dumps({"apiVersion": "v1", "kind": "List", "items": items}))
' "$tag" | kubectl --context "$ctx" apply -f - >/dev/null
  if [ "$MULTIHUB" = on ]; then
    kubectl --context "$ctx" -n argocd set env statefulset/argocd-application-controller ARGOCD_MULTIHUB_HUB_ID="$hub"
  else
    kubectl --context "$ctx" -n argocd set env statefulset/argocd-application-controller ARGOCD_MULTIHUB_HUB_ID-
  fi
  kubectl --context "$ctx" -n argocd rollout status statefulset/argocd-application-controller --timeout=300s
  kubectl --context "$ctx" -n argocd rollout status deployment/argocd-repo-server --timeout=300s
  kubectl --context "$ctx" -n argocd rollout status deployment/argocd-server --timeout=300s
done
echo "== deployed $tag (multihub $MULTIHUB)"
