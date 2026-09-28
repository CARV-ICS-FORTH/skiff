#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/profile.sh"
declare -A RESULTS

while IFS= read -r label; do RESULTS[$label]=PASS; done < <(profile_required_cases)
profile_check_results

RESULTS[E2E-08]=SKIP
if profile_check_results 2>/dev/null; then echo "required SKIP accepted" >&2; exit 1; fi
RESULTS[E2E-08]=PASS

unset 'RESULTS[E2E-10]'
if profile_check_results 2>/dev/null; then echo "missing result accepted" >&2; exit 1; fi
RESULTS[E2E-10]=PASS

RESULTS[E2E-01-teardown]=FAIL
if profile_check_results 2>/dev/null; then echo "failed teardown accepted" >&2; exit 1; fi
RESULTS[E2E-01-teardown]=PASS

RESULTS[NET-2-plaid-all]=FAIL
profile_check_results

E2E_PROFILE=unknown
if profile_required_cases >/dev/null 2>&1; then echo "unknown profile accepted" >&2; exit 1; fi

# Exercise the real aggregate entry point: stale evidence must be rejected
# before node discovery, deployment or execution of any qualification case.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
mkdir "$tmp/results"
echo stale > "$tmp/results/results.json"
if E2E_LOGDIR="$tmp/results" bash "$(dirname "$0")/../run-all.sh" > "$tmp/stale.log" 2>&1; then
    echo "stale qualification evidence was reused" >&2; exit 1
fi
grep -q 'Refusing to reuse existing qualification results' "$tmp/stale.log"
