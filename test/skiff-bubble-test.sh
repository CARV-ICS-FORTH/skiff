#!/usr/bin/env bash
# The bubble script refuses to start without socat, before it creates anything.
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd)
empty=$(mktemp -d)
trap 'rm -rf "$empty"' EXIT

rc=0
out=$(PATH="$empty" HOME="$empty" /bin/bash "$script_dir/../scripts/skiff-bubble.sh" 1 2>&1) || rc=$?
[ "$rc" -eq 1 ] || { echo "skiff-bubble-test: exit $rc without socat, expected 1" >&2; exit 1; }
case "$out" in
    *"socat is required"*) ;;
    *) echo "skiff-bubble-test: no reason given: $out" >&2; exit 1 ;;
esac
[ -z "$(ls -A "$empty")" ] || { echo "skiff-bubble-test: created files before failing" >&2; exit 1; }
echo "skiff-bubble-test: ok"
