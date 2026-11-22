#!/usr/bin/env bash
# Prints the lab's hubs, space-separated: LAB_HUBS if set, otherwise every
# kind cluster named hub-* (sorted). Other scripts loop over this, so the lab
# works with any number of hubs (add one with ./add-hub.sh hub-c).
if [ -n "${LAB_HUBS:-}" ]; then echo "$LAB_HUBS"; exit 0; fi
kind get clusters 2>/dev/null | grep '^hub-' | sort | tr '\n' ' ' | sed 's/ $//'
