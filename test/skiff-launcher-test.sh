#!/usr/bin/env bash
# Exercise Skiff's own launcher (skiff.sh start, status, stop, run) on this machine, with the
# node's check, network probe and bubble replaced by stand-ins: the real skiff.sh runs them as
# `bash skiff.sh check|network-probe|bubble`, and a `bash` first in PATH answers those three and
# runs everything else.
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd)
test_dir=$(mktemp -d)
trap 'pkill -KILL -f "$test_dir/" 2>/dev/null || true; rm -rf "$test_dir"' EXIT
real_bash=$(command -v bash)
mkdir -p "$test_dir/bin" "$test_dir/apptainer-bin" "$test_dir/repo/scripts" "$test_dir/work"
cp "$script_dir/../scripts/skiff.sh" "$test_dir/repo/scripts/"
echo "9.9.9-test" > "$test_dir/repo/VERSION"
skiff="$test_dir/repo/scripts/skiff.sh"
failures=0
fail() { echo "FAIL $*" >&2; failures=$((failures + 1)); }
pass() { echo "PASS $*"; }

cat > "$test_dir/bin/bash" <<EOF
#!$real_bash
case "\${1:-}:\${2:-}" in
    */skiff.sh:check)
        [ "\${TEST_CHECK:-ok}" = ok ] || { echo "\$(hostname): required program(s) not found in PATH: socat" >&2; exit 1; }
        exit 0 ;;
    */skiff.sh:bubble) exec "$test_dir/fake-bubble" "\$3" ;;
    */skiff.sh:network-probe)
        # The controller's address on its network, or (given it) the route to it.
        if [ -n "\${3:-}" ]; then echo "dev=eth0 mtu=1500 onlink=yes"; else echo "ip=127.0.0.1 dev=eth0 mtu=1500 speed=10000"; fi
        exit 0 ;;
esac
exec "$real_bash" "\$@"
EOF
# The bubble: says what it was given, makes the controller's credentials, and ends as the real one
# does when its lease says stop (the bubble's own test covers the real one).
cat > "$test_dir/fake-bubble" <<EOF
#!$real_bash
mkdir -p "\$HOME/.skiff/tls"
{ echo "id=\$1 role=\$SKIFF_ROLE job=\$SKIFF_JOB_ID instance=\$SKIFF_JOB_INSTANCE lease=\${SKIFF_LEASE:-}"; echo "slurm=\$(compgen -e | grep -c '^SLURM_' || true)"; } >> "\$HOME/.skiff/bubble-env"
echo "id=\$1 pprof=\${PLAID_PPROF_SOCKET:-} other=\${TEST_OTHER:-}" >> "\$HOME/.skiff/bubble-passed"
touch "\$HOME/.skiff/kubeconfig" "\$HOME/.skiff/tls/server-ca.crt"
echo "stand-in bubble \$1 is up"
[ ! -e "\$HOME/.skiff/bubble\$1-exits" ] || exit 9
[ "\${TEST_BUBBLE:-run}" != early ] || exit 9
while true; do
    if [ "\$(cut -d' ' -f1 "\$SKIFF_LEASE" 2>/dev/null)" = stop ]; then echo stopped-by-lease >> "\$HOME/.skiff/bubble-events"; exit 143; fi
    sleep 0.2
done
EOF
# Apptainer: says what it was asked; an instance stop answers as ~/.skiff/apptainer-stop says
# (missing: the instance is gone, as when its bubble stopped it; fails: it cannot be stopped).
cat > "$test_dir/apptainer-bin/apptainer" <<'EOF'
#!/bin/sh
echo "$*" >> "$HOME/.skiff/apptainer-args"
case "$1 $2:$(cat "$HOME/.skiff/apptainer-stop" 2>/dev/null)" in
    "instance stop:missing")
        echo 'Error for command "stop": no instance found' >&2
        printf 'Usage:\n  apptainer [global options...] instance stop [stop options...] [instance]\n'
        exit 255 ;;
    "instance stop:fails")
        echo 'FATAL:   while stopping the instance: permission denied' >&2
        printf 'Usage:\n  apptainer [global options...] instance stop [stop options...] [instance]\n'
        exit 255 ;;
esac
EOF
chmod +x "$test_dir/bin/"* "$test_dir/apptainer-bin/"* "$test_dir/fake-bubble"
export PATH="$test_dir/bin:$test_dir/apptainer-bin:$PATH" SKIFF_TMP_DIR="$test_dir/scratch" SKIFF_LEASE_INTERVAL=1
unset SKIFF_DEV

new_home() {
    export HOME="$test_dir/home-$1"
    mkdir -p "$HOME/.skiff/images"
    case "$(uname -m)" in x86_64) : > "$HOME/.skiff/images/skiff-9.9.9-test-amd64.sif" ;; *) : > "$HOME/.skiff/images/skiff-9.9.9-test-arm64.sif" ;; esac
}
record_job() { sed -n 's/^job=//p' "$HOME/.skiff/launcher/cluster" 2>/dev/null; }
run_skiff() { (cd "$test_dir/work" && bash "$skiff" "$@"); }

# --- start, status, stop ------------------------------------------------------------------
new_home basic
out=$(run_skiff start 2>&1) && rc=0 || rc=$?
job=$(record_job)
if [ "$rc" -eq 0 ] && [[ "$job" =~ ^[0-9]+$ ]] && grep -q "^Started Skiff cluster job $job on " <<< "$out" && grep -q "Its output: $test_dir/work/skiff-cluster-$job.out" <<< "$out"; then
    pass "start returns at once with the job (a number) and its output file in the current directory"
else
    fail "start: rc $rc, record job '$job': $out"
fi
for i in $(seq 1 50); do grep -q 'Supervising the bubbles' "$test_dir/work/skiff-cluster-$job.out" 2>/dev/null && break; sleep 0.1; done
grep -q 'Supervising the bubbles' "$test_dir/work/skiff-cluster-$job.out" && pass "the job runs in the background, its output in the file" || fail "the job did not come up: $(cat "$test_dir/work/skiff-cluster-$job.out")"
grep -q '^checked=' "$HOME/.skiff/launcher/cluster" && grep -q 'Checking the nodes' "$test_dir/work/skiff-cluster-$job.out" && pass "start returned once the nodes had passed their checks" || fail "the record does not say the nodes were checked: $(cat "$HOME/.skiff/launcher/cluster")"
grep -Eq "^id=1 role=controller job=$job instance=$job-[0-9a-f]{8} lease=$HOME/.skiff/launcher/$job.lease\$" "$HOME/.skiff/bubble-env" \
    && pass "the bubble is the controller of the job, with the job's volume directory name and its lease" || fail "the bubble was given: $(cat "$HOME/.skiff/bubble-env")"
out=$(run_skiff status 2>&1) && rc=0 || rc=$?
[ "$rc" -eq 0 ] && grep -q "^Skiff cluster job $job: running since" <<< "$out" && grep -q "supervisor: PID [0-9]* on $(hostname)" <<< "$out" \
    && pass "status shows the running cluster" || fail "status: rc $rc: $out"
lease1=$(cat "$HOME/.skiff/launcher/$job.lease"); sleep 2.2; lease2=$(cat "$HOME/.skiff/launcher/$job.lease")
[[ "$lease1" =~ ^[0-9]+$ && "$lease2" =~ ^[0-9]+$ && "$lease2" -gt "$lease1" ]] && pass "the supervisor renews its lease" || fail "the lease was '$lease1', then '$lease2'"
out=$(run_skiff start 2>&1) && rc=0 || rc=$?
[ "$rc" -eq 1 ] && grep -q "A Skiff cluster is already running: job $job" <<< "$out" && [ "$(record_job)" = "$job" ] \
    && pass "a second start is refused, naming the running job" || fail "second start: rc $rc: $out"
out=$(run_skiff stop 2>&1) && rc=0 || rc=$?
if [ "$rc" -eq 0 ] && grep -q "^Stopped. Its output: $test_dir/work/skiff-cluster-$job.out" <<< "$out"; then pass "stop waits until the cluster is gone"; else fail "stop: rc $rc: $out"; fi
[ ! -e "$HOME/.skiff/launcher/cluster" ] && [ ! -e "$HOME/.skiff/launcher/$job.lease" ] && pass "its record and lease are gone" || fail "the record or the lease is left: $(ls "$HOME/.skiff/launcher")"
grep -qx stopped-by-lease "$HOME/.skiff/bubble-events" 2>/dev/null && pass "the bubble was told to stop by the lease" || fail "the bubble was not stopped by its lease"
[ "$(cat "$HOME/.skiff/job-results/$job.exit" 2>/dev/null)" = 143 ] && grep -q '^observed: received SIGTERM' "$HOME/.skiff/job-results/$job.cause" \
    && pass "the job's exit status and cause are recorded, as a Slurm job's are" || fail "exit '$(cat "$HOME/.skiff/job-results/$job.exit" 2>&1)', cause: $(cat "$HOME/.skiff/job-results/$job.cause" 2>&1)"
grep -qx 'instance stop bubble1' "$HOME/.skiff/apptainer-args" && ! grep -q -- '--all' "$HOME/.skiff/apptainer-args" \
    && pass "the clean-up's sweep stops the bubble's instance by name, nothing else of the user's" || fail "the sweep: $(cat "$HOME/.skiff/apptainer-args" 2>&1)"
grep -q 'Cluster cleanup complete.' "$test_dir/work/skiff-cluster-$job.out" && pass "the output ends with the clean-up" || fail "no clean-up in the output"
out=$(run_skiff status 2>&1) && rc=0 || rc=$?
[ "$rc" -eq 1 ] && grep -q 'No Skiff cluster of skiff.sh start is running' <<< "$out" && pass "status then says there is none" || fail "status after stop: rc $rc: $out"
out=$(run_skiff stop 2>&1) && rc=0 || rc=$?
[ "$rc" -eq 0 ] && grep -q 'No Skiff cluster of skiff.sh start is running' <<< "$out" && pass "stop with nothing running says so" || fail "stop of nothing: rc $rc: $out"

# --- a supervisor that is killed --------------------------------------------------------------
new_home killed
run_skiff start > /dev/null 2>&1 || fail "start (killed)"
job=$(record_job)
for i in $(seq 1 50); do grep -q 'Supervising the bubbles' "$test_dir/work/skiff-cluster-$job.out" 2>/dev/null && break; sleep 0.1; done
pid=$(sed -n 's/^pid=//p' "$HOME/.skiff/launcher/cluster")
kill -KILL "$pid"
sleep 0.5
out=$(run_skiff status 2>&1) && rc=0 || rc=$?
[ "$rc" -eq 2 ] && grep -q "its supervisor (PID $pid on $(hostname)) is gone" <<< "$out" && pass "a killed supervisor is seen as gone" || fail "status of a killed supervisor: rc $rc: $out"
lease1=$(cat "$HOME/.skiff/launcher/$job.lease"); sleep 2.2; lease2=$(cat "$HOME/.skiff/launcher/$job.lease")
[ "$lease1" = "$lease2" ] && pass "and its lease is no longer renewed" || fail "the lease of a killed supervisor changed: $lease1, $lease2"
pkill -KILL -f "$test_dir/fake-bubble" 2>/dev/null || true
sleep 1.1
out=$(run_skiff start 2>&1) && rc=0 || rc=$?
job2=$(record_job)
[ "$rc" -eq 0 ] && grep -q "The supervisor of job $job (PID $pid on $(hostname)) is gone; its record is removed" <<< "$out" && [ -n "$job2" ] && [ "$job2" != "$job" ] \
    && pass "the next start removes the record it left, saying so, and starts" || fail "start after a killed supervisor: rc $rc: $out"
run_skiff stop > /dev/null 2>&1 || fail "stop (killed)"

# --- a start that fails ---------------------------------------------------------------------
new_home badnode
out=$(TEST_CHECK=missing run_skiff start 2>&1) && rc=0 || rc=$?
job=$(ls "$HOME/.skiff/job-results" 2>/dev/null | sed -n 's/\.exit$//p')
if [ "$rc" -eq 1 ] && grep -q "ERROR: Skiff cluster job $job ended as it started (exit status 1)" <<< "$out" && grep -q 'required program(s) not found in PATH: socat' <<< "$out" && grep -q 'nothing was started' <<< "$out"; then
    pass "a node that cannot run Skiff: start fails, naming the job, with the end of its output"
else
    fail "a failing start: rc $rc: $out"
fi
[ ! -e "$HOME/.skiff/launcher/cluster" ] && [ -z "$(ls "$HOME/.skiff/launcher" 2>/dev/null)" ] && [ "$(cat "$HOME/.skiff/job-results/$job.exit" 2>/dev/null)" = 1 ] \
    && pass "and leaves no record or lease, only the job's exit status" || fail "after a failed start: $(ls -A "$HOME/.skiff/launcher" 2>&1), exit '$(cat "$HOME/.skiff/job-results/$job.exit" 2>&1)'"
new_home earlybubble
out=$(TEST_BUBBLE=early run_skiff start 2>&1) && rc=0 || rc=$?
job=$(sed -n 's/^Started Skiff cluster job \([0-9]*\) .*/\1/p' <<< "$out")
for i in $(seq 1 100); do [ -e "$HOME/.skiff/job-results/$job.exit" ] && break; sleep 0.1; done
[ "$(cat "$HOME/.skiff/job-results/$job.exit" 2>/dev/null)" = 9 ] && grep -q 'process:  the process of the bubble on' "$HOME/.skiff/job-results/$job.cause" && [ ! -e "$HOME/.skiff/launcher/cluster" ] \
    && pass "a bubble that ends ends the job, with its status and cause, and the record goes" || fail "a bubble that ended: rc $rc, exit '$(cat "$HOME/.skiff/job-results/$job.exit" 2>&1)': $out"

# --- what is Slurm's is not the cluster's ---------------------------------------------------
new_home slurmvars
SLURM_JOB_ID=77 SLURM_MEM_PER_NODE=5 SLURM_STEP_GPUS=0 run_skiff start > /dev/null 2>&1 || fail "start (Slurm variables)"
job=$(record_job)
for i in $(seq 1 50); do [ -s "$HOME/.skiff/bubble-env" ] && break; sleep 0.1; done
[ "$job" != 77 ] && grep -qx 'slurm=0' "$HOME/.skiff/bubble-env" && pass "a start inside a Slurm allocation passes none of its variables on" || fail "Slurm variables: job $job, $(cat "$HOME/.skiff/bubble-env" 2>&1)"
run_skiff stop > /dev/null 2>&1 || fail "stop (Slurm variables)"

# --- one cluster per user, whichever launcher -----------------------------------------------
new_home slurmjob
cat > "$test_dir/bin/squeue" <<'EOF'
#!/bin/sh
echo 4242
EOF
chmod +x "$test_dir/bin/squeue"
out=$(TEST_FG=1 run_skiff run 2>&1) && rc=0 || rc=$?
rm -f "$test_dir/bin/squeue"
[ "$rc" -eq 1 ] && grep -q 'Another Skiff cluster job is already running (Job IDs: 4242)' <<< "$out" && [ ! -e "$HOME/.skiff/launcher/cluster" ] \
    && pass "a running Slurm job of the user's refuses a cluster of the launcher" || fail "with a Slurm job running: rc $rc: $out"
# A cluster of the launcher on another host, alive by its lease, refuses one here; a lease that is old does not.
new_home elsewhere
mkdir -p "$HOME/.skiff/launcher"
printf 'job=1000\nhost=another-host\npid=1\nnodes=another-host\noutput=/x\nstarted=then\n' > "$HOME/.skiff/launcher/cluster"
date +%s > "$HOME/.skiff/launcher/1000.lease"
out=$(run_skiff start 2>&1) && rc=0 || rc=$?
[ "$rc" -eq 1 ] && grep -q 'already running: job 1000, on another-host' <<< "$out" && pass "a cluster alive on another host (its lease) refuses another" || fail "alive elsewhere: rc $rc: $out"
echo $(( $(date +%s) - 120 )) > "$HOME/.skiff/launcher/1000.lease"
out=$(run_skiff status 2>&1) && rc=0 || rc=$?
[ "$rc" -eq 2 ] && pass "one whose lease is older than its expiry is gone" || fail "an old lease elsewhere: rc $rc: $out"

# --- the logout warning ----------------------------------------------------------------------
new_home logout
cat > "$test_dir/bin/busctl" <<'EOF'
#!/bin/sh
case "$5" in
    KillUserProcesses) echo "b ${FAKE_KILL:-true}" ;;
    KillExcludeUsers) echo "${FAKE_EXCLUDE:-as 1 \"nobody\"}" ;;
    KillOnlyUsers) echo "${FAKE_ONLY:-as 0}" ;;
esac
EOF
chmod +x "$test_dir/bin/busctl"
out=$(run_skiff start 2>&1) || fail "start (logout)"
grep -q 'WARNING: this host stops all your processes when you log out' <<< "$out" && pass "a host that stops a user's processes at logout is warned about" || fail "no logout warning: $out"
run_skiff stop > /dev/null 2>&1 || fail "stop (logout)"
out=$(FAKE_EXCLUDE="as 2 \"root\" \"$(id -un)\"" run_skiff start 2>&1) || fail "start (logout, excluded)"
if grep -q 'WARNING: this host stops' <<< "$out"; then fail "a user excluded from it was warned"; else pass "not for a user it excludes"; fi
run_skiff stop > /dev/null 2>&1 || fail "stop (logout, excluded)"
out=$(FAKE_ONLY='as 1 "someone"' run_skiff start 2>&1) || fail "start (logout, only)"
if grep -q 'WARNING: this host stops' <<< "$out"; then fail "a user it does not apply to was warned"; else pass "nor for one it does not apply to"; fi
run_skiff stop > /dev/null 2>&1 || fail "stop (logout, only)"
out=$(FAKE_KILL=false run_skiff start 2>&1) || fail "start (logout, off)"
if grep -q 'WARNING: this host stops' <<< "$out"; then fail "warned on a host that does not stop processes"; else pass "nor on a host that does not"; fi
run_skiff stop > /dev/null 2>&1 || fail "stop (logout, off)"
rm -f "$test_dir/bin/busctl"

# --- hosts over ssh ----------------------------------------------------------------------------
# ssh runs the command here, as the host would (they share the home), with only the home and
# the host's own PATH, as a login on the host has none of the caller's variables. The hosts have
# no Apptainer of their own: it is in Skiff's tools on the home (where `skiff.sh download` puts
# it), which the system's PATH does not name. A host named unreachable refuses, as ssh without a
# key does.
host_path=""
IFS=: read -ra path_dirs <<< "$PATH"
for d in "${path_dirs[@]}"; do [ -x "$d/apptainer" ] || host_path="${host_path:+$host_path:}$d"; done
cat > "$test_dir/bin/ssh" <<EOF
#!$real_bash
while [ "\${1:-}" = -o ]; do shift 2; done
host=\$1; shift
echo "\$host" >> "\$HOME/.skiff/ssh-hosts"
[ "\$host" != unreachable ] || { echo "\$host: Permission denied (publickey)." >&2; exit 255; }
exec env -i HOME="\$HOME" PATH="$host_path" bash -c "\$*"
EOF
chmod +x "$test_dir/bin/ssh"
machine=$(uname -m); [ "$machine" != arm64 ] || machine=aarch64
new_hosts_home() {
    new_home "$1"
    mkdir -p "$HOME/.skiff/tools/$machine/bin"
    cp "$test_dir/apptainer-bin/apptainer" "$HOME/.skiff/tools/$machine/bin/"
}
new_hosts_home hosts
echo missing > "$HOME/.skiff/apptainer-stop"
out=$(PLAID_PPROF_SOCKET=/scratch/plaidd-pprof.sock TEST_OTHER=kept-here run_skiff start h1 h2 2>&1) && rc=0 || rc=$?
job=$(record_job)
[ "$rc" -eq 0 ] && grep -q "^Started Skiff cluster job $job on h1 h2 (supervisor PID [0-9]* on $(hostname))" <<< "$out" && [ "$(sed -n 's/^nodes=//p' "$HOME/.skiff/launcher/cluster")" = "h1 h2" ] \
    && pass "start with hosts: the job's nodes are the hosts, in order" || fail "start with hosts: rc $rc: $out"
for i in $(seq 1 50); do [ "$(grep -c '^id=' "$HOME/.skiff/bubble-env" 2>/dev/null)" = 2 ] && break; sleep 0.1; done
grep -Eq "^id=1 role=controller job=$job instance=$job-[0-9a-f]{8} lease=$HOME/.skiff/launcher/$job.lease\$" "$HOME/.skiff/bubble-env" \
    && grep -Eq "^id=2 role=node job=$job instance=$job-[0-9a-f]{8} lease=$HOME/.skiff/launcher/$job.lease\$" "$HOME/.skiff/bubble-env" \
    && pass "the first host runs the controller and the second a node, each told the job, its volume directory and its lease over ssh" || fail "the bubbles over ssh were given: $(cat "$HOME/.skiff/bubble-env" 2>&1)"
grep -qx h1 "$HOME/.skiff/ssh-hosts" && grep -qx h2 "$HOME/.skiff/ssh-hosts" && pass "every command of the job ran on its host over ssh" || fail "ssh went to: $(sort -u "$HOME/.skiff/ssh-hosts" 2>&1 | tr '\n' ' ')"
grep -qx 'id=1 pprof=/scratch/plaidd-pprof.sock other=' "$HOME/.skiff/bubble-passed" && grep -qx 'id=2 pprof=/scratch/plaidd-pprof.sock other=' "$HOME/.skiff/bubble-passed" \
    && pass "plaidd's profiling socket reaches the bubbles over ssh, as under Slurm, and nothing else of the caller's" || fail "the bubbles over ssh were passed: $(cat "$HOME/.skiff/bubble-passed" 2>&1)"
[ -s "$HOME/.skiff/launcher/$job.bubble2.pid" ] && pass "a bubble over ssh records its process group" || fail "no process group recorded for bubble 2: $(ls "$HOME/.skiff/launcher")"
out=$(run_skiff stop 2>&1) || fail "stop (hosts): $out"
[ "$(grep -c '^stopped-by-lease$' "$HOME/.skiff/bubble-events" 2>/dev/null)" = 2 ] && pass "stop ends both bubbles through the lease" || fail "the bubbles stopped: $(cat "$HOME/.skiff/bubble-events" 2>&1)"
grep -q 'stand-in bubble 2 is up' "$test_dir/work/skiff-cluster-$job.out" && grep -q 'Cluster cleanup complete' "$test_dir/work/skiff-cluster-$job.out" \
    && pass "the job's output has what the bubble over ssh wrote, and its own" || fail "the job's output: $(cat "$test_dir/work/skiff-cluster-$job.out")"
[ "$(cat "$HOME/.skiff/job-results/$job.exit" 2>/dev/null)" = 143 ] && [ -z "$(ls "$HOME/.skiff/launcher" 2>/dev/null)" ] \
    && pass "and the job ends with 143, leaving nothing in the launcher's directory" || fail "after stop (hosts): exit '$(cat "$HOME/.skiff/job-results/$job.exit" 2>&1)', left: $(ls "$HOME/.skiff/launcher")"
grep -qx 'instance stop bubble1' "$HOME/.skiff/apptainer-args" && grep -qx 'instance stop bubble2' "$HOME/.skiff/apptainer-args" \
    && pass "the sweep stops each host's bubble instance by name, with the Apptainer of Skiff's tools" || fail "the sweep (hosts): $(cat "$HOME/.skiff/apptainer-args" 2>&1)"
if grep -q 'WARNING: the last sweep\|instance stop \[stop options\|no instance found' "$test_dir/work/skiff-cluster-$job.out"; then
    fail "an instance already gone made the sweep say something: $(grep -i 'sweep\|instance' "$test_dir/work/skiff-cluster-$job.out")"
else
    pass "an instance its bubble has stopped already is nothing to say"
fi
# An instance that cannot be stopped is said, on one line for each host; a job that fails keeps
# the bubbles' output files (bubble 2 ends at once, and the job with it).
new_hosts_home sweepfails
echo fails > "$HOME/.skiff/apptainer-stop"
touch "$HOME/.skiff/bubble2-exits"
out=$(run_skiff start h1 h2 2>&1) && rc=0 || rc=$?
job=$(ls "$HOME/.skiff/launcher/"*.lease "$HOME/.skiff/job-results/"*.exit 2>/dev/null | sed -n 's|.*/\([0-9]*\)\.[a-z]*$|\1|p' | sed -n 1p || true)
for i in $(seq 1 150); do [ -e "$HOME/.skiff/job-results/$job.exit" ] && break; sleep 0.1; done
jobout="$test_dir/work/skiff-cluster-$job.out"
host=$(hostname)
[ "$(grep -c '^WARNING: the last sweep' "$jobout")" = 2 ] \
    && grep -qx "WARNING: the last sweep could not stop the bubble's instance bubble1 on $host (apptainer instance stop bubble1: FATAL:   while stopping the instance: permission denied); it may still be running there" "$jobout" \
    && grep -q "^WARNING: the last sweep could not stop the bubble's instance bubble2 on $host " "$jobout" && ! grep -q 'instance stop \[stop options' "$jobout" \
    && pass "an instance the sweep cannot stop is said, on one line for each host" || fail "the sweep's failures (start: rc $rc, $out): $(grep -i 'sweep\|instance' "$jobout" 2>&1)"
[ "$(cat "$HOME/.skiff/job-results/$job.exit" 2>/dev/null)" = 9 ] && grep -q 'stand-in bubble 1 is up' "$HOME/.skiff/launcher/$job.bubble1.out" 2>/dev/null \
    && grep -q 'stand-in bubble 2 is up' "$HOME/.skiff/launcher/$job.bubble2.out" 2>/dev/null && [ -z "$(ls "$HOME/.skiff/launcher/"*.pid 2>/dev/null)" ] \
    && grep -qF "The bubbles' own output is kept in $HOME/.skiff/launcher/$job.bubble*.out" "$jobout" \
    && pass "a job that failed keeps its bubbles' output files, and says where" || fail "after a failed job: exit '$(cat "$HOME/.skiff/job-results/$job.exit" 2>&1)', left: $(ls "$HOME/.skiff/launcher" 2>&1); $(tail -3 "$jobout" 2>&1)"
new_home unreachable
out=$(run_skiff start h1 unreachable 2>&1) && rc=0 || rc=$?
[ "$rc" -eq 1 ] && grep -q 'cannot run a command on unreachable over ssh without a password or a question (ssh says: unreachable: Permission denied (publickey).)' <<< "$out" && [ ! -e "$HOME/.skiff/launcher/cluster" ] \
    && pass "a host that ssh cannot reach is named before anything starts" || fail "an unreachable host: rc $rc: $out"
out=$(run_skiff start h1 h1 2>&1) && rc=0 || rc=$?
[ "$rc" -ne 0 ] && grep -q 'a host is named twice: h1 h1' <<< "$out" && pass "a host named twice is refused" || fail "a host named twice: rc $rc: $out"
rm -f "$test_dir/bin/ssh"

# --- run in the foreground, told to end -----------------------------------------------------
new_home foreground
(cd "$test_dir/work" && exec bash "$skiff" run) > "$test_dir/fg.log" 2>&1 &
fg=$!
for i in $(seq 1 50); do grep -q 'Supervising the bubbles' "$test_dir/fg.log" && break; sleep 0.1; done
job=$(record_job)
kill -TERM "$fg"
rc=0; wait "$fg" || rc=$?
[ "$rc" -eq 143 ] && grep -qx stopped-by-lease "$HOME/.skiff/bubble-events" && [ ! -e "$HOME/.skiff/launcher/cluster" ] && [ "$(cat "$HOME/.skiff/job-results/$job.exit" 2>/dev/null)" = 143 ] \
    && pass "run in the foreground, told to end, stops its bubble and ends with 143" || fail "foreground run: rc $rc: $(tail -5 "$test_dir/fg.log")"

if [ "$failures" -gt 0 ]; then
    echo "skiff-launcher: $failures check(s) failed" >&2
    exit 1
fi
echo "skiff-launcher: all checks passed"
