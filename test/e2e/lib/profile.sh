#!/usr/bin/env bash

E2E_PROFILE="${E2E_PROFILE:-skiff-tap-alpha}"

profile_required_cases() {
    case "$E2E_PROFILE" in
        skiff-tap-alpha)
            printf '%s\n' \
                E2E-01 E2E-02 E2E-03 E2E-04 E2E-05 E2E-06 \
                E2E-07 E2E-08 E2E-09 E2E-09-skifflet E2E-09-plaidd \
                E2E-09-slirp E2E-09-relay E2E-10 E2E-11 E2E-12 \
                NET-3-tap-acceptance NET-4-tap-webhook CANDIDATE E2E-01-teardown
            ;;
        *) echo "unknown E2E profile: $E2E_PROFILE" >&2; return 1 ;;
    esac
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
