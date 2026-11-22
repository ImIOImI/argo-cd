#!/usr/bin/env bash
# Adds another hub to a running lab: a kind cluster with stock Argo CD (same
# installationID and tracking method as the others), the spoke registered,
# and lab-app. Then put the fork on it and match the others:
#   ./add-hub.sh hub-c
#   ./deploy-fork.sh                  # every hub, incl. the new one
#   HUBS=hub-c ./root.sh              # shared argocd-cm keys from Git
#   ./apps.sh 10                      # the extra apps, on every hub
set -euo pipefail
hub="${1:?usage: $0 HUB (e.g. hub-c)}"
case "$hub" in hub-*) ;; *) echo "hub names must start with hub-" >&2; exit 2 ;; esac
cd "$(dirname "$0")"
# shellcheck source=up.sh
source ./up.sh
create_cluster "$hub"
install_argocd "$hub" "${RECONCILE:-120s}"
spoke_credentials
register_spoke_and_app "$hub"
log "$hub added"
