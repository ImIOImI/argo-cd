#!/usr/bin/env bash
set -uo pipefail
for c in $("$(dirname "$0")"/lab-hubs.sh) spoke; do kind delete cluster --name "$c"; done
