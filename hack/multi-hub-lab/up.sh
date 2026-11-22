#!/usr/bin/env bash
# Two Argo CD hubs (hub-a, hub-b) managing the same Application on one spoke.
# Stock upstream Argo CD on both: this lab measures how the hubs fight today,
# before any multi-hub coordination exists.
#
# Needs: docker, kind, kubectl.
set -euo pipefail

ARGOCD_VERSION="${ARGOCD_VERSION:-v3.6.1}"
REPO_URL="${REPO_URL:-https://github.com/ImIOImI/argo-cd.git}"
REPO_REVISION="${REPO_REVISION:-spike/multi-hub}"
APP_PATH="hack/multi-hub-lab/app"
# Different poll intervals so the hubs learn about a new commit at different
# times. That gap is the revision-skew window the spike is about.
HUB_A_RECONCILE="${HUB_A_RECONCILE:-30s}"
HUB_B_RECONCILE="${HUB_B_RECONCILE:-180s}"
INSTALLATION_ID="multi-hub-lab"

log() { printf '\n== %s\n' "$*"; }

create_cluster() {
  local name=$1
  if kind get clusters 2>/dev/null | grep -qx "$name"; then
    log "cluster $name exists"
  else
    log "creating cluster $name"
    kind create cluster --name "$name" --wait 120s
  fi
}

install_argocd() {
  local hub=$1 reconcile=$2 ctx="kind-$1"
  log "installing Argo CD $ARGOCD_VERSION on $hub (reconcile $reconcile)"
  kubectl --context "$ctx" create namespace argocd --dry-run=client -o yaml | kubectl --context "$ctx" apply -f -
  kubectl --context "$ctx" apply -n argocd --server-side --force-conflicts \
    -f "https://raw.githubusercontent.com/argoproj/argo-cd/${ARGOCD_VERSION}/manifests/install.yaml"
  # Identical installationID + tracking method on both hubs, so each treats the
  # spoke's resources as its own (the precondition for sharing an app).
  kubectl --context "$ctx" -n argocd patch configmap argocd-cm --type merge -p "{
    \"data\": {
      \"installationID\": \"${INSTALLATION_ID}\",
      \"application.resourceTrackingMethod\": \"annotation\",
      \"timeout.reconciliation\": \"${reconcile}\"
    }}"
  # The lab app lives in the (large) argo-cd fork. The first full fetch takes
  # longer than the default ~90s git/exec timeout, so raise both; later
  # fetches are incremental.
  kubectl --context "$ctx" -n argocd patch configmap argocd-cmd-params-cm --type merge \
    -p '{"data":{"controller.repo.server.timeout.seconds":"900"}}'
  kubectl --context "$ctx" -n argocd set env deployment/argocd-repo-server ARGOCD_EXEC_TIMEOUT=15m
  kubectl --context "$ctx" -n argocd rollout restart statefulset/argocd-application-controller
  kubectl --context "$ctx" -n argocd rollout restart deployment/argocd-repo-server
  kubectl --context "$ctx" -n argocd rollout status statefulset/argocd-application-controller --timeout=300s
  kubectl --context "$ctx" -n argocd rollout status deployment/argocd-repo-server --timeout=300s
}

spoke_credentials() {
  log "creating argocd-manager service account on spoke"
  kubectl --context kind-spoke apply -f - <<'EOF'
apiVersion: v1
kind: ServiceAccount
metadata: {name: argocd-manager, namespace: kube-system}
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata: {name: argocd-manager}
roleRef: {apiGroup: rbac.authorization.k8s.io, kind: ClusterRole, name: cluster-admin}
subjects: [{kind: ServiceAccount, name: argocd-manager, namespace: kube-system}]
---
apiVersion: v1
kind: Secret
metadata:
  name: argocd-manager-token
  namespace: kube-system
  annotations: {kubernetes.io/service-account.name: argocd-manager}
type: kubernetes.io/service-account-token
EOF
  for _ in $(seq 30); do
    SPOKE_TOKEN=$(kubectl --context kind-spoke -n kube-system get secret argocd-manager-token -o jsonpath='{.data.token}' | base64 -d)
    [ -n "$SPOKE_TOKEN" ] && break
    sleep 1
  done
  SPOKE_CA=$(kubectl --context kind-spoke config view --raw --minify -o jsonpath='{.clusters[0].cluster.certificate-authority-data}')
  # The hubs reach the spoke over the shared "kind" docker network.
  SPOKE_SERVER="https://spoke-control-plane:6443"
}

register_spoke_and_app() {
  local hub=$1 ctx="kind-$1"
  log "registering spoke + lab-app on $hub"
  kubectl --context "$ctx" apply -f - <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: cluster-spoke
  namespace: argocd
  labels: {argocd.argoproj.io/secret-type: cluster}
stringData:
  name: spoke
  server: ${SPOKE_SERVER}
  config: '{"bearerToken":"${SPOKE_TOKEN}","tlsClientConfig":{"caData":"${SPOKE_CA}"}}'
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: lab-app
  namespace: argocd
  annotations:
    argocd.argoproj.io/manifest-generate-paths: .
spec:
  project: default
  source:
    repoURL: ${REPO_URL}
    targetRevision: ${REPO_REVISION}
    path: ${APP_PATH}
  destination:
    name: spoke
    namespace: lab
  syncPolicy:
    automated: {prune: true, selfHeal: true}
    syncOptions: [CreateNamespace=true]
EOF
}

main() {
  create_cluster spoke
  create_cluster hub-a
  create_cluster hub-b
  install_argocd hub-a "$HUB_A_RECONCILE"
  install_argocd hub-b "$HUB_B_RECONCILE"
  spoke_credentials
  register_spoke_and_app hub-a
  register_spoke_and_app hub-b
  log "done. Watch with ./observe.sh, then push a change with ./bump.sh"
}

# Sourced by add-hub.sh for the functions only.
if [ "${BASH_SOURCE[0]}" = "$0" ]; then main; fi
