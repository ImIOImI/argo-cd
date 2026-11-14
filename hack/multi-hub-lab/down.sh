#!/usr/bin/env bash
set -uo pipefail
for c in hub-a hub-b spoke; do kind delete cluster --name "$c"; done
