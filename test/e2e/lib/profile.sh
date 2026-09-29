#!/usr/bin/env bash
# The required results of the qualification profile. A label is either a case
# result (E2E-07) or a named subcase reported by a case through e2e_subcase
# (T-ID-name, for example T07-startup-http). Subcase labels are listed here as
# their features land, so an unexecuted variant cannot hide behind a case PASS.

profile_required_cases() {
    printf '%s\n' \
        E2E-01 E2E-02 E2E-03 E2E-04 E2E-05 E2E-06 \
        E2E-07 E2E-08 E2E-09 E2E-09-skifflet E2E-09-plaidd \
        E2E-09-slirp E2E-09-relay E2E-10 E2E-11 E2E-12 E2E-13 E2E-14 \
        NET-3-tap-acceptance NET-4-tap-webhook CANDIDATE E2E-01-teardown \
        T04-logs T04-exec T04-portforward T04-pods-assigned T04-runningpods \
        T04-attach-501 T04-stats-501 T04-metrics-501 \
        T04-logs-missing-pod T04-logs-missing-container \
        T17-exec-environment T17-exec-workdir \
        T07-startup-protects-slow-start T07-startup-failure-restarts T07-startup-failure-kill \
        T07-named-ports T07-exec-probes T07-http-request T08-service-readiness \
        T09-delete-during-probe \
        T17-exec-streams T17-exec-tty T17-exec-right-after-start T17-exec-init-running T17-exec-init-finished \
        T14-exec-client-cancel T14-exec-target-ends T14-exec-resources
}

profile_is_required() {
    local label="$1" required
    while IFS= read -r required; do
        [ "$label" = "$required" ] && return 0
    done < <(profile_required_cases)
    return 1
}

profile_check_results() {
    local label failed=0
    while IFS= read -r label; do
        if [ "${RESULTS[$label]:-MISSING}" != PASS ]; then
            echo "Required ${label}: ${RESULTS[$label]:-MISSING}" >&2
            failed=1
        fi
    done < <(profile_required_cases)
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
