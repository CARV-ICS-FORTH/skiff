#!/usr/bin/env bash
# Exercise the production batch script with controlled Slurm commands/children.
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd)
test_dir=$(mktemp -d)
trap 'rm -rf "$test_dir"' EXIT
mkdir -p "$test_dir/bin" "$test_dir/repo/scripts"

cat > "$test_dir/bin/scontrol" <<'EOF'
#!/usr/bin/env bash
case "$*" in
    "show node"*) echo "NodeName=controller Arch=x86_64 CoresPerSocket=4 CPUAlloc=0 CPUTot=8 CPULoad=0.10" ;;
    *) [ "${TEST_EMPTY_NODES:-0}" = 1 ] || echo controller ;;
esac
EOF
# squeue lists nothing, or with TEST_STEPS=1 the steps of the job (what `squeue -s` does).
cat > "$test_dir/bin/squeue" <<'EOF'
#!/usr/bin/env bash
case " $* " in
    *" -s "*) if [ "${TEST_STEPS:-0}" = 1 ]; then printf '%s.batch\n%s.extern\n%s.0\n' "$SLURM_JOB_ID" "$SLURM_JOB_ID" "$SLURM_JOB_ID"; fi ;;
esac
exit 0
EOF
# scancel keeps what it is asked and, as a signal to the tasks of a step does, stops the bubble.
cat > "$test_dir/bin/scancel" <<'EOF'
#!/usr/bin/env bash
mkdir -p "$HOME/.skiff"; echo "$*" >> "$HOME/.skiff/scancel-args"
if [ -s "$HOME/.skiff/bubble-pid" ]; then kill -TERM "$(cat "$HOME/.skiff/bubble-pid")" 2>/dev/null || true; fi
exit 0
EOF
# apptainer converts the image archive to the SIF (the bubbles are stand-ins and run nothing).
cat > "$test_dir/bin/apptainer" <<'EOF'
#!/usr/bin/env bash
case "$1" in build) echo "$*" >> "$HOME/.skiff/conversions"; : > "$3" ;; esac
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
        # skiff.slurm has sourced for its functions). What the step was asked for is kept.
        mkdir -p "$HOME/.skiff"; echo "$*" >> "$HOME/.skiff/step-args"
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
if [ "$TEST_CHILD_MODE" = term ]; then echo $$ > "$HOME/.skiff/bubble-pid"; exec sleep 30; fi
sleep 0.1
exit "$TEST_CHILD_MODE"
EOF
cp "$script_dir/../scripts/skiff.sh" "$test_dir/repo/scripts/"
# a release: the version beside the scripts' folder, and its image archive in each home (below)
echo "9.9.9-test" > "$test_dir/repo/VERSION"
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
for mode in 7 0 early empty term termsteps; do
    job=$((job + 1))
    export SLURM_JOB_ID="$job" HOME="$test_dir/home-$mode" TEST_CHILD_MODE="$mode" TEST_STEPS=0
    # (termsteps: the job is told to end while its steps are listed by Slurm, as they are on a cluster)
    if [ "$mode" = termsteps ]; then export TEST_CHILD_MODE=term TEST_STEPS=1; fi
    export SKIFF_DEV=0
    [ "$mode" != 7 ] || unset SKIFF_DEV
    export TEST_EMPTY_NODES=0
    [ "$mode" != empty ] || export TEST_EMPTY_NODES=1
    mkdir -p "$HOME/.skiff/images"; : > "$HOME/.skiff/images/skiff-9.9.9-test.tar"
    rc=0
    if [ "$mode" = term ] || [ "$mode" = termsteps ]; then
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
    # Told to end with its steps listed, the job signals the tasks of each step (TERM, so the
    # bubble drains its pods) and not the srun that started it; never the batch or extern step.
    if [ "$mode" = termsteps ]; then
        grep -qx -- "--signal=TERM ${SLURM_JOB_ID}.0" "$HOME/.skiff/scancel-args" || { echo "$mode: the step was not signalled: $(cat "$HOME/.skiff/scancel-args" 2>&1)" >&2; exit 1; }
        if grep -qE 'batch|extern' "$HOME/.skiff/scancel-args"; then echo "$mode: a step that is not a bubble was signalled" >&2; exit 1; fi
    elif [ "$mode" = term ] && [ -e "$HOME/.skiff/scancel-args" ]; then
        echo "$mode: a step was signalled although Slurm listed none" >&2; exit 1
    fi
    # Every job's bubbles are told the job's own volume directory name.
    if [ "$mode" != empty ] && [ "$mode" != early ]; then
        grep -Eq "^${SLURM_JOB_ID}-[0-9a-f]{8}\$" "$HOME/.skiff/instances" || { echo "$mode: the bubble was not given this job's volume directory name: $(cat "$HOME/.skiff/instances" 2>&1)" >&2; exit 1; }
    fi
    # The step of a node's bubble asks for all the CPUs of the node (a step has one unless it
    # asks), because they are what the node advertises and the pods can use.
    if [ "$mode" != empty ] && [ "$mode" != early ]; then
        grep -q -- '--exclusive --cpus-per-task=8 bash' "$HOME/.skiff/step-args" || { echo "$mode: the bubble's step did not ask for the node's CPUs: $(cat "$HOME/.skiff/step-args" 2>&1)" >&2; exit 1; }
    fi
    # The job runs the release's image: the SIF of its version, made from the archive.
    if [ "$mode" != empty ]; then
        grep -qx "build --force $HOME/.skiff/images/skiff-9.9.9-test.sif docker-archive://$HOME/.skiff/images/skiff-9.9.9-test.tar" "$HOME/.skiff/conversions" \
            && grep -q "^Image: $HOME/.skiff/images/skiff-9.9.9-test.sif\$" "$test_dir/$mode.log" \
            || { echo "$mode: the job did not prepare the release's image: $(cat "$HOME/.skiff/conversions" 2>&1)" >&2; exit 1; }
    fi
done
