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

# A failure of a label that is not required still fails the run.
RESULTS[NOT-REQUIRED]=FAIL
if profile_check_results 2>/dev/null; then echo "failure beside a passing profile accepted" >&2; exit 1; fi
RESULTS[NOT-REQUIRED]=SKIP
profile_check_results
unset 'RESULTS[NOT-REQUIRED]'

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

# A required result that cannot run on this host (a capability it needs is missing) is neither a
# pass nor a failure: the run is incomplete (status 3), unless something failed too.
RESULTS[T99-beta]=UNAVAILABLE
status=0; profile_check_results 2>/dev/null || status=$?
[ "$status" -eq 3 ] || { echo "an unavailable required result gave status $status, want 3" >&2; exit 1; }
RESULTS[E2E-08]=FAIL
status=0; profile_check_results 2>/dev/null || status=$?
[ "$status" -eq 1 ] || { echo "a failure beside an unavailable result gave status $status, want 1" >&2; exit 1; }
RESULTS[E2E-08]=PASS
RESULTS[E2E-08]=SKIP
status=0; profile_check_results 2>/dev/null || status=$?
[ "$status" -eq 1 ] || { echo "a skip beside an unavailable result gave status $status, want 1" >&2; exit 1; }
RESULTS[E2E-08]=PASS
E2E_LOGDIR="$tmp"
printf 'T99-beta\tneeds root on the node\n' > "$tmp/unavailable.tsv"
[ "$(profile_unavailable_summary)" = "T99-beta (needs root on the node)" ] || { echo "unavailable summary: $(profile_unavailable_summary)" >&2; exit 1; }
RESULTS[T99-beta]=PASS
[ -z "$(profile_unavailable_summary)" ] || { echo "a passing result listed as unavailable" >&2; exit 1; }
[ "$(merged 'A\tPASS\nA\tUNAVAILABLE\n')" = "A=UNAVAILABLE;" ] || { echo "an unavailable result was lost" >&2; exit 1; }
[ "$(merged 'A\tUNAVAILABLE\nA\tPASS\n')" = "A=UNAVAILABLE;" ] || { echo "an unavailable result was overridden by a pass" >&2; exit 1; }
[ "$(merged 'A\tUNAVAILABLE\nA\tSKIP\n')" = "A=SKIP;" ] || { echo "a skip was overridden by an unavailable result" >&2; exit 1; }
[ "$(merged 'A\tUNAVAILABLE\nA\tFAIL\n')" = "A=FAIL;" ] || { echo "a failure was overridden by an unavailable result" >&2; exit 1; }

# What a smoke run leaves out is said: an UNAVAILABLE result with its reason, a skipped one.
unset RESULTS; declare -A RESULTS
RESULTS[E2E-02]=PASS
[ -z "$(profile_not_run_summary)" ] || { echo "a run that ran everything has something not run" >&2; exit 1; }
RESULTS[T46-gpu-node]=UNAVAILABLE RESULTS[E2E-33]=SKIP
printf 'T46-gpu-node\tno node of the cluster has GPUs\n' > "$tmp/unavailable.tsv"
want="E2E-33 (skipped); T46-gpu-node (no node of the cluster has GPUs)"
[ "$(profile_not_run_summary)" = "$want" ] || { echo "not-run summary: $(profile_not_run_summary)" >&2; exit 1; }
