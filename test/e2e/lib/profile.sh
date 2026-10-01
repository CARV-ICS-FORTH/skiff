#!/usr/bin/env bash
# The required results of the qualification profile. A label is either a case
# result (E2E-07) or a named subcase reported by a case through e2e_subcase
# (T-ID-name, for example T07-startup-http). Subcase labels are listed here as
# their features land, so an unexecuted variant cannot hide behind a case PASS.

profile_required_cases() {
    printf '%s\n' \
        E2E-01 E2E-02 E2E-03 E2E-04 E2E-05 E2E-06 \
        E2E-07 E2E-08 E2E-09 E2E-09-skifflet E2E-09-plaidd \
        E2E-09-slirp E2E-09-k3s E2E-09-relay E2E-10 E2E-11 E2E-12 E2E-13 E2E-14 E2E-15 E2E-16 E2E-17 E2E-18 E2E-19 E2E-20 E2E-21 E2E-22 E2E-23 E2E-24 E2E-25 E2E-26 E2E-27 E2E-28 \
        NET-1-plaid-integration NET-2-plaid-standalone NET-3-tap-acceptance NET-4-tap-webhook CANDIDATE E2E-01-teardown \
        T04-logs T04-exec T04-portforward T04-pods-assigned T04-runningpods \
        T04-attach-501 T04-stats T04-metrics \
        T04-logs-missing-pod T04-logs-missing-container \
        T17-exec-environment T17-exec-workdir \
        T07-startup-protects-slow-start T07-startup-failure-restarts T07-startup-failure-kill \
        T07-named-ports T07-exec-probes T07-http-request T08-service-readiness \
        T09-delete-during-probe \
        T17-exec-streams T17-exec-tty T17-exec-right-after-start T17-exec-init-running T17-exec-init-finished \
        T14-exec-client-cancel T14-exec-target-ends T14-exec-resources \
        T10-exec-hooks T10-http-hook T10-completed T11-poststart-failure T11-prestop-failure \
        T11-prestop-hang T11-zero-grace T11-liveness-prestop T26-deletion-progress T27-default-class T27-delayed-provisioning T27-create-failure T28-shared-replacement T28-local-control T29-delete-and-retain T29-cleanup-retry T29-user-owned T30-fresh-job T27-claim-waits T27-waiting-cancel T31-private-tmp T31-reset-on-restart T31-emptydir T34-writable-layer T34-private-layer T34-reset-layer T37-kubelet-endpoints T37-metrics-api \
        T12-default-and-tail T12-partial-line T12-timestamps T12-since T12-limit-bytes T12-invalid T12-follow-tail \
        T13-previous-missing T13-attempts T13-init-attempts \
        T05-unsupported-forms T06-metadata-updates T06-unsupported-updates T06-rollout \
        T15-environment-and-files T15-missing-configuration T16-continuity \
        T20-cycles T20-no-leaks \
        T18-services T18-node-networks T18-startup-report T18-pod-network T18-connectivity T18-runtime-path T32-scratch T32-volume-roots T32-unusable-scratch \
        T21-owner T21-no-forward T21-traffic T36-failure-cause \
        T22-cancel-during-startup T22-helper-killed T22-port-taken T22-second-boot \
        T23-translation T23-named-ports T23-dns T23-bubble T23-large T23-selectorless T23-untranslated \
        T24-pinned T24-refusal T24-unsupported T24-no-kube-proxy T24-counters \
        T25-tcp-udp T25-slirp-state T25-changes T25-no-backends T25-drain T25-bind-collision T25-unsupported T25-reconcile T25-cleanup
}

profile_is_required() {
    local label="$1" required
    while IFS= read -r required; do
        [ "$label" = "$required" ] && return 0
    done < <(profile_required_cases)
    return 1
}

# A run passes when every required label PASSED (a missing or skipped one fails it)
# and no recorded result at all failed: a result that is not required still has to
# be reported honestly, so a failure beside a passing verdict cannot happen.
profile_check_results() {
    local label failed=0
    while IFS= read -r label; do
        if [ "${RESULTS[$label]:-MISSING}" != PASS ]; then
            echo "Required ${label}: ${RESULTS[$label]:-MISSING}" >&2
            failed=1
        fi
    done < <(profile_required_cases)
    for label in "${!RESULTS[@]}"; do
        if [ "${RESULTS[$label]}" = FAIL ]; then
            echo "Recorded ${label}: FAIL" >&2
            failed=1
        fi
    done
    return "$failed"
}

# Merge a subcase result file (lines of "ID<TAB>PASS|FAIL|SKIP") into one
# "ID<TAB>RESULT" line per ID in first-seen order. A failure anywhere wins, then
# a skip; any other value is treated as a failure so malformed evidence cannot pass.
profile_merge_subresults() {
    local file="$1"
    [ -s "$file" ] || return 0
    awk -F '\t' '
        NF != 2 || $1 == "" { bad = 1; next }
        {
            r = ($2 == "PASS" || $2 == "SKIP") ? $2 : "FAIL"
            if (!($1 in res)) { order[++n] = $1; res[$1] = r }
            else if (r == "FAIL" || (r == "SKIP" && res[$1] == "PASS")) res[$1] = r
        }
        END {
            for (i = 1; i <= n; i++) printf "%s\t%s\n", order[i], res[order[i]]
            if (bad) printf "MALFORMED-SUBRESULT\tFAIL\n"
        }' "$file"
}
