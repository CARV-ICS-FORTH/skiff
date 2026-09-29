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

# Named subcases: a required subcase must be reported, and only a PASS counts.
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
eval "$(declare -f profile_required_cases | sed 's/profile_required_cases/base_required_cases/')"
profile_required_cases() { base_required_cases; printf '%s\n' T99-alpha T99-beta; }
RESULTS[T99-alpha]=PASS RESULTS[T99-beta]=PASS
profile_check_results
unset 'RESULTS[T99-beta]'
if profile_check_results 2>/dev/null; then echo "missing required subcase accepted" >&2; exit 1; fi
RESULTS[T99-beta]=SKIP
if profile_check_results 2>/dev/null; then echo "skipped required subcase accepted" >&2; exit 1; fi
RESULTS[T99-beta]=PASS

merged() { printf "$1" > "$tmp/sub.tsv"; profile_merge_subresults "$tmp/sub.tsv" | tr '\t\n' '=;'; }
[ "$(merged 'A\tPASS\nB\tPASS\n')" = "A=PASS;B=PASS;" ] || { echo "merge of passes" >&2; exit 1; }
[ "$(merged 'A\tPASS\nA\tFAIL\nA\tPASS\n')" = "A=FAIL;" ] || { echo "a failure was overridden" >&2; exit 1; }
[ "$(merged 'A\tPASS\nA\tSKIP\n')" = "A=SKIP;" ] || { echo "a skip was overridden" >&2; exit 1; }
[ "$(merged 'A\tMAYBE\n')" = "A=FAIL;" ] || { echo "unknown result accepted" >&2; exit 1; }
[ "$(merged 'A\tPASS\nbroken line\n')" = "A=PASS;MALFORMED-SUBRESULT=FAIL;" ] || { echo "malformed evidence accepted" >&2; exit 1; }
[ -z "$(profile_merge_subresults "$tmp/missing.tsv")" ] || { echo "missing file produced results" >&2; exit 1; }

# Exercise the real aggregate entry point: stale evidence must be rejected
# before node discovery, deployment or execution of any qualification case.
mkdir "$tmp/results"
echo stale > "$tmp/results/results.json"
if E2E_LOGDIR="$tmp/results" bash "$(dirname "$0")/../run-all.sh" > "$tmp/stale.log" 2>&1; then
    echo "stale qualification evidence was reused" >&2; exit 1
fi
grep -q 'Refusing to reuse existing qualification results' "$tmp/stale.log"
