#!/usr/bin/env bash
# Changes the shared refresh period in Git (bootstrap/argocd-cm.yaml) and
# pushes: the one-line change an operator makes. Both hubs' root apps sync it.
#   ./set-period.sh 120s
set -euo pipefail
p="${1:?usage: $0 DURATION}"
cd "$(dirname "$0")"
sed -i -E "s/^(  timeout.reconciliation: )\"[^\"]*\"/\1\"${p}\"/" bootstrap/argocd-cm.yaml
git add bootstrap/argocd-cm.yaml
git commit -q -m "lab: shared refresh period ${p}"
git push -q
echo "pushed refresh period ${p} at $(date -u +%H:%M:%SZ)"
