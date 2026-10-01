#!/usr/bin/env bash
# Exercise the production batch script with controlled Slurm commands/children.
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd)
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
mkdir -p "$test_dir/bin" "$test_dir/repo/scripts"

cat > "$test_dir/bin/scontrol" <<'EOF'
#!/usr/bin/env bash
[ "${TEST_EMPTY_NODES:-0}" = 1 ] || echo controller
EOF
cat > "$test_dir/bin/squeue" <<'EOF'
#!/usr/bin/env bash
exit 0
EOF
cat > "$test_dir/bin/apptainer" <<'EOF'
#!/usr/bin/env bash
case "$*" in *docker://docker.io/chazapis/skiff-bubble:latest*) touch "$HOME/.skiff/published-image-used" ;; esac
exit 0
EOF
cat > "$test_dir/bin/srun" <<'EOF'
#!/usr/bin/env bash
case "$*" in
    *'getent ahostsv4'*) echo 127.0.0.1 ;;
    *'skiff.sh check'*)
        # What a node needs (host programs, a scratch directory) is checked on the node.
        [ "${TEST_NODE_CHECK:-ok}" = ok ] || { echo "controller: required program(s) not found in PATH: socat" >&2; exit 1; }
        exit 0
        ;;
    *'skiff.sh bubble'*)
        # The bubble of a node is the stand-in below, not the real script (which
        # skiff.slurm has sourced for its functions).
        exec "$FAKE_BUBBLE"
        ;;
    *) exit 0 ;;
esac
EOF
cat > "$test_dir/fake-bubble" <<'EOF'
#!/usr/bin/env bash
if [ "$TEST_CHILD_MODE" = early ]; then exit 9; fi
mkdir -p "$HOME/.skiff/tls"
echo "$SKIFF_JOB_INSTANCE" >> "$HOME/.skiff/instances"
touch "$HOME/.skiff/kubeconfig" "$HOME/.skiff/tls/server-ca.crt"
if [ "$TEST_CHILD_MODE" = term ]; then exec sleep 30; fi
sleep 0.1
exit "$TEST_CHILD_MODE"
EOF
cp "$script_dir/../scripts/skiff.sh" "$test_dir/repo/scripts/"
chmod +x "$test_dir/bin/"* "$test_dir/fake-bubble"
export FAKE_BUBBLE="$test_dir/fake-bubble"
export PATH="$test_dir/bin:$PATH"
export SLURM_JOB_NODELIST=controller SLURM_SUBMIT_DIR="$test_dir/repo" SKIFF_DEV=0
# The state directory is the user's ~/.skiff and cannot be changed: each run gets a
# home of its own. The scratch directory is a setting; it is kept out of the real /tmp.
export SKIFF_TMP_DIR="$test_dir/scratch"

check_result() {
    local label="$1" expected="$2" actual="$3"
    [ "$actual" -eq "$expected" ] || { echo "$label: exit $actual, expected $expected" >&2; exit 1; }
    [ "$(cat "$HOME/.skiff/job-results/$SLURM_JOB_ID.exit")" = "$expected" ] || {
        echo "$label: batch result marker lost exit status" >&2; exit 1;
    }
}

# An invalid configuration stops the job before any bubble starts, and says why.
export SLURM_JOB_ID=8999 HOME="$test_dir/home-invalid" TEST_CHILD_MODE=0 SKIFF_DEV=0 TEST_EMPTY_NODES=0
rc=0
SKIFF_POD_MTU=1500 bash "$script_dir/../scripts/skiff.slurm" > "$test_dir/invalid.log" 2>&1 || rc=$?
[ "$rc" -eq 1 ] || { echo "invalid configuration: exit $rc, expected 1" >&2; exit 1; }
grep -q 'SKIFF_POD_MTU (1500) leaves no room for VXLAN' "$test_dir/invalid.log" || { echo "invalid configuration: no reason given" >&2; cat "$test_dir/invalid.log" >&2; exit 1; }
if grep -q 'Starting Controller' "$test_dir/invalid.log"; then echo "invalid configuration: a bubble was started" >&2; exit 1; fi
# ... and so does a job whose skiff.sh is nowhere to be found.
mv "$test_dir/repo/scripts/skiff.sh" "$test_dir/skiff.sh.away"
rc=0
SLURM_JOB_ID=8998 HOME="$test_dir/home-nohelper" bash "$script_dir/../scripts/skiff.slurm" > "$test_dir/nohelper.log" 2>&1 || rc=$?
[ "$rc" -eq 1 ] && grep -q 'skiff.sh not found' "$test_dir/nohelper.log" || { echo "missing helper: exit $rc" >&2; cat "$test_dir/nohelper.log" >&2; exit 1; }
mv "$test_dir/skiff.sh.away" "$test_dir/repo/scripts/skiff.sh"

# A node that lacks a host program, or whose scratch directory cannot be used, stops the
# job before any bubble starts, naming the problem.
rc=0
SLURM_JOB_ID=8997 HOME="$test_dir/home-badnode" TEST_NODE_CHECK=missing bash "$script_dir/../scripts/skiff.slurm" > "$test_dir/badnode.log" 2>&1 || rc=$?
[ "$rc" -eq 1 ] || { echo "bad node: exit $rc, expected 1" >&2; cat "$test_dir/badnode.log" >&2; exit 1; }
grep -q 'required program(s) not found in PATH: socat' "$test_dir/badnode.log" && grep -q 'nothing was started' "$test_dir/badnode.log" || { echo "bad node: no reason given" >&2; cat "$test_dir/badnode.log" >&2; exit 1; }
if grep -q 'Starting Controller' "$test_dir/badnode.log"; then echo "bad node: a bubble was started" >&2; exit 1; fi
# ... and an invalid scratch directory is refused with the rest of the configuration.
rc=0
SLURM_JOB_ID=8996 HOME="$test_dir/home-badtmp" SKIFF_TMP_DIR=relative/scratch bash "$script_dir/../scripts/skiff.slurm" > "$test_dir/badtmp.log" 2>&1 || rc=$?
[ "$rc" -eq 1 ] && grep -q 'SKIFF_TMP_DIR.*absolute path' "$test_dir/badtmp.log" || { echo "bad scratch directory: exit $rc" >&2; cat "$test_dir/badtmp.log" >&2; exit 1; }

job=9000
for mode in 7 0 early empty term; do
    job=$((job + 1))
    export SLURM_JOB_ID="$job" HOME="$test_dir/home-$mode" TEST_CHILD_MODE="$mode"
    export SKIFF_DEV=0
    [ "$mode" != 7 ] || unset SKIFF_DEV
    export TEST_EMPTY_NODES=0
    [ "$mode" != empty ] || export TEST_EMPTY_NODES=1
    rc=0
    if [ "$mode" = term ]; then
        bash "$script_dir/../scripts/skiff.slurm" > "$test_dir/$mode.log" 2>&1 &
        supervisor_pid=$!
        for attempt in $(seq 1 50); do
            if grep -q 'Supervising job steps' "$test_dir/$mode.log"; then break; fi
            sleep 0.1
        done
        grep -q 'Supervising job steps' "$test_dir/$mode.log" || { cat "$test_dir/$mode.log"; exit 1; }
        kill -TERM "$supervisor_pid"
        wait "$supervisor_pid" || rc=$?
        expected=143
    else
        bash "$script_dir/../scripts/skiff.slurm" > "$test_dir/$mode.log" 2>&1 || rc=$?
        case "$mode" in 7) expected=7 ;; early) expected=9 ;; *) expected=1 ;; esac
    fi
    check_result "$mode" "$expected" "$rc"
    # Every job's bubbles are told the job's own volume directory name.
    if [ "$mode" != empty ] && [ "$mode" != early ]; then
        grep -Eq "^${SLURM_JOB_ID}-[0-9a-f]{8}\$" "$HOME/.skiff/instances" || { echo "$mode: the bubble was not given this job's volume directory name: $(cat "$HOME/.skiff/instances" 2>&1)" >&2; exit 1; }
    fi
    if [ "$mode" = 7 ] && [ ! -f "$HOME/.skiff/published-image-used" ]; then
        echo "Default deployment did not select the published image" >&2; exit 1
    fi
done
