#!/usr/bin/env bash
# Run `scripts/skiff.sh bubble` against a fake slirp4netns API, a fake apptainer and a fake
# socat: what the host publishes, how the overlay socket helper is started and
# supervised (it is a one-shot, not a daemon), what a missing socat does, and that a
# terminated bubble leaves nothing behind.
set -euo pipefail
script_dir=$(cd "$(dirname "$0")" && pwd)
repo_dir=$(cd "$script_dir/.." && pwd)
test_dir=$(mktemp -d)
trap 'kill "$(cat "$test_dir"/fake/pid 2>/dev/null)" "$(cat "$test_dir"/fake/inst-pid 2>/dev/null)" 2>/dev/null || true; rm -rf "$test_dir"' EXIT

LOG=""
fail() { echo "skiff-bubble-test: $*" >&2; [ -z "$LOG" ] || sed 's/^/  | /' "$LOG" >&2; exit 1; }

# Without the host programs (socat among them) the bubble refuses to start, before it
# creates anything.
empty="$test_dir/empty"; mkdir -p "$empty"
rc=0
out=$(PATH="$empty" HOME="$empty" /bin/bash "$script_dir/../scripts/skiff.sh" bubble 1 2>&1) || rc=$?
[ "$rc" -eq 1 ] || fail "exit $rc without socat, expected 1"
case "$out" in *"required program(s) not found in PATH"*socat*) ;; *) fail "no reason given without socat: $out" ;; esac
[ -z "$(ls -A "$empty")" ] || fail "created files before failing without socat"

mkdir -p "$test_dir/bin" "$test_dir/home/.skiff/images"
: > "$test_dir/home/.skiff/images/skiff-bubble.sif"

cat > "$test_dir/bin/apptainer" <<'STUB'
#!/usr/bin/env bash
case "$*" in
    --version) echo "apptainer version 1.5.4" ;;
    *"kill -TERM"*)
        # The request to stop the bubble gracefully (to its entrypoint): it stops, taking its
        # instance with it.
        echo drain >> "$FAKE_DIR/drain-requests"
        kill "$(cat "$FAKE_DIR/inst-pid")" 2>/dev/null
        exit 0 ;;
    *overlay-socket*)
        echo "$*" >> "$FAKE_DIR/helper-args"
        echo $$ > "$FAKE_DIR/helper-pid"
        case "${FAKE_HELPER:-run}" in
            ok) exit 0 ;;
            fail) echo "Error: cannot open the overlay socket" >&2; exit 1 ;;
            *) exec sleep 300 ;;
        esac ;;
    "instance run"*) echo "$*" > "$FAKE_DIR/instance-run"; exit 0 ;;
    "instance list"*)
        # The instance is one of many on the node (the user's others: 400 rows after it, more
        # than a pipe holds, so a reader that stops at the first match makes this die of
        # SIGPIPE); its PID is a real process the test controls.
        [ -e "$FAKE_DIR/instance-gone" ] && exit 0
        if [ -e "$FAKE_DIR/list-flap" ]; then rm -f "$FAKE_DIR/list-flap"; exit 0; fi
        if [ -e "$FAKE_DIR/list-fails" ]; then echo "FATAL: cannot read the instance directory" >&2; exit 1; fi
        printf 'INSTANCE NAME PID IP IMAGE\nbubble1 %s /x/skiff-bubble.sif\n' "$(cat "$FAKE_DIR/inst-pid")"
        for n in $(seq 1 400); do printf 'other%s %s /home/user/.apptainer/some/long/path/to/an/image/that/is/not/the/bubble.sif\n' "$n" "$((100000 + n))"; done ;;
    *pgrep*)
        # Is the entrypoint of the bubble running? pgrep says 1 for "no match".
        [ -e "$FAKE_DIR/entrypoint-gone" ] && exit 1
        if [ -e "$FAKE_DIR/entrypoint-flap" ]; then rm -f "$FAKE_DIR/entrypoint-flap"; exit 1; fi
        [ -e "$FAKE_DIR/exec-fails" ] && exit 255
        exit 0 ;;
    "instance stop"*) echo stop >> "$FAKE_DIR/instance-stop" ;;
esac
exit 0
STUB
cat > "$test_dir/bin/ip" <<'STUB'
#!/usr/bin/env bash
echo "192.0.2.1 dev eth0 src 192.0.2.10 uid 0"
STUB
cat > "$test_dir/bin/slirp4netns" <<STUB
#!/usr/bin/env bash
exec python3 "$script_dir/fake-slirp4netns.py" "\$@"
STUB
# The relays stay up (sleep); as a client of a UNIX socket (the slirp API) it sends its
# standard input and prints the answer, as `socat -t 2 - UNIX-CONNECT:path` does.
cat > "$test_dir/bin/socat" <<'STUB'
#!/usr/bin/env bash
case "$*" in
    *UNIX-CONNECT:*)
        exec python3 -c 'import socket, sys
s = socket.socket(socket.AF_UNIX); s.settimeout(2); s.connect(sys.argv[1].split("UNIX-CONNECT:", 1)[1])
s.sendall(sys.stdin.buffer.read())
try: sys.stdout.buffer.write(s.recv(65536))
except socket.timeout: pass' "$*" ;;
    *) exec sleep 300 ;;
esac
STUB
printf '#!/usr/bin/env bash\nexit 0\n' > "$test_dir/bin/ss"
chmod +x "$test_dir/bin/"*

# The scratch directory is a setting (its default is the node's /tmp/.skiff): here it is
# somewhere else, to see that everything follows it.
export PATH="$test_dir/bin:$PATH" HOME="$test_dir/home" SKIFF_DEV=1 CONTROLLER_IP=192.0.2.10 SKIFF_TMP_DIR="$test_dir/scratch"
export SKIFF_SLIRP_API_TIMEOUT=4
# shellcheck source=../scripts/skiff.sh
source "$repo_dir/scripts/skiff.sh"
skiff_config_validate >/dev/null
skiff_outer_network 1
guest="$SKIFF_OUTER_ADDRESS"

# run_bubble ROLE [ENV=VALUE...]: start the bubble script and wait until it is up or has exited.
run_bubble() {
    local role="$1"; shift
    export FAKE_DIR="$test_dir/fake"
    kill "$(cat "$FAKE_DIR/inst-pid" 2>/dev/null)" 2>/dev/null || true
    rm -rf "$FAKE_DIR"; mkdir -p "$FAKE_DIR"
    rm -f "$HOME"/.skiff/job-results/*.cause
    # The bubble's instance: a process that lives until the test ends it.
    sleep 600 > /dev/null 2>&1 &
    echo $! > "$FAKE_DIR/inst-pid"
    disown
    export FAKE_SLIRP_DIR="$FAKE_DIR"
    LOG="$test_dir/bubble.log"
    env SKIFF_ROLE="$role" "$@" bash "$repo_dir/scripts/skiff.sh" bubble 1 > "$LOG" 2>&1 &
    BUBBLE_PID=$!
    local i
    for i in $(seq 1 60); do
        grep -q 'started successfully' "$LOG" && return 0
        kill -0 "$BUBBLE_PID" 2>/dev/null || return 0
        sleep 0.25
    done
}
wait_exit() {
    local rc=0 i
    for i in $(seq 1 60); do
        kill -0 "$BUBBLE_PID" 2>/dev/null || { wait "$BUBBLE_PID" || rc=$?; EXIT_RC=$rc; return 0; }
        sleep 0.25
    done
    fail "the bubble script did not exit"
}
requests() { python3 - "$FAKE_DIR/requests" <<'PY'
import json, sys
for line in open(sys.argv[1]):
    a = json.loads(line)["arguments"]
    print(a["proto"], a["host_addr"], a["host_port"], a["guest_addr"], a["guest_port"])
PY
}
alive() { kill -0 "$(cat "$1")" 2>/dev/null; }
released() {
    local i
    for i in $(seq 1 20); do
        if ! alive "$FAKE_DIR/pid" && { [ ! -e "$FAKE_DIR/helper-pid" ] || ! alive "$FAKE_DIR/helper-pid"; }; then return 0; fi
        sleep 0.25
    done
    fail "slirp4netns or the overlay helper is still running"
}

# A controller: the kubelet and the API are forwarded to the host's loopback for the
# relays; the overlay's UDP port is not forwarded at all (its socket is the host's).
run_bubble controller
grep -q 'started successfully' "$LOG" || fail "the controller bubble did not start"
expected="tcp 127.0.0.1 10250 $guest 10250
tcp 127.0.0.1 6443 $guest 6443"
[ "$(requests)" = "$expected" ] || { requests >&2; fail "slirp4netns listeners differ from the expected ones (no UDP overlay forward)"; }

# slirp4netns runs at the pod MTU, so that nothing it sends has to be fragmented on its way to pods.
grep -q -- "--mtu=$SKIFF_POD_MTU " "$FAKE_DIR/args" || { cat "$FAKE_DIR/args" >&2; fail "slirp4netns was not started at the pod MTU"; }

# The helper was started once, from the image, with the host address, the port and the
# handoff socket in the scratch directory; the bubble was told where it is, got the
# scratch directory at the same path, and Apptainer's scratch is below it.
[ "$(wc -l < "$FAKE_DIR/helper-args" | tr -d ' ')" = 1 ] || fail "the helper was not started exactly once"
grep -q -- "$HOME/.skiff/images/skiff-bubble.sif /usr/local/bin/plaidctl overlay-socket --bind 192.0.2.10 --port 8472 --handoff $SKIFF_TMP_DIR/overlay.sock" "$FAKE_DIR/helper-args" \
    || { cat "$FAKE_DIR/helper-args" >&2; fail "unexpected helper command line"; }
grep -q -- "--env SKIFF_OVERLAY_HANDOFF=$SKIFF_TMP_DIR/overlay.sock" "$FAKE_DIR/instance-run" || fail "the bubble was not told the handoff path"
grep -q -- "--bind $SKIFF_TMP_DIR:$SKIFF_TMP_DIR " "$FAKE_DIR/instance-run" || fail "the bubble was not given the scratch directory at the same path"
grep -q -- "--env SKIFF_TMP_DIR=$SKIFF_TMP_DIR " "$FAKE_DIR/instance-run" || fail "the bubble was not told the scratch directory"
# A bubble started by hand makes the job's volume directory name; one that is given is kept.
grep -Eq -- "--env SKIFF_JOB_INSTANCE=0-[0-9a-f]{8} " "$FAKE_DIR/instance-run" || { cat "$FAKE_DIR/instance-run" >&2; fail "the bubble was not given a job instance name"; }
for variable in APPTAINER_TMPDIR SINGULARITY_TMPDIR TMPDIR; do
    grep -q -- "--env $variable=$SKIFF_TMP_DIR/.apptainer/tmp " "$FAKE_DIR/instance-run" || fail "$variable is not below the scratch directory"
done
grep -q -- "--bind $HOME/.skiff:/root/.skiff " "$FAKE_DIR/instance-run" || fail "the state directory is not bound as /root/.skiff"
if grep -q "/tmp/.skiff" "$FAKE_DIR/instance-run" "$FAKE_DIR/helper-args"; then fail "the default scratch directory is still used"; fi
grep -q -- "--env SKIFF_SLIRP_API=/root/.skiff/slirp-bubble1.sock" "$FAKE_DIR/instance-run" || fail "the bubble was not told slirp4netns's API socket"

# A helper that is still waiting is not a failure, and terminating the bubble stops it.
sleep 3
kill -0 "$BUBBLE_PID" 2>/dev/null || fail "the bubble stopped while the helper was waiting"
kill -TERM "$BUBBLE_PID"; wait_exit
[ "$EXIT_RC" -eq 143 ] || fail "SIGTERM: exit $EXIT_RC"
released
# Told to end (not failing), the bubble first asked its entrypoint to stop the pods, and
# only then took the relays and slirp4netns away.
[ "$(wc -l < "$FAKE_DIR/drain-requests" 2>/dev/null | tr -d ' ')" = 1 ] || fail "the bubble did not ask its entrypoint to stop gracefully"
grep -q 'Stopping the pods of bubble bubble1' "$LOG" || fail "the graceful stop is not in the log"
[ ! -e "$SKIFF_TMP_DIR/overlay.sock" ] || fail "the handoff socket was left behind"
[ ! -e "$HOME/.skiff/slirp-bubble1.sock" ] || fail "the API socket was left behind"

run_bubble controller SKIFF_JOB_INSTANCE=4711-0123abcd
grep -q -- "--env SKIFF_JOB_INSTANCE=4711-0123abcd " "$FAKE_DIR/instance-run" || fail "the job instance name that skiff.slurm made was not passed on"
kill -TERM "$BUBBLE_PID"; wait_exit; released
# A worker forwards no API port.
run_bubble node FAKE_HELPER=ok
grep -q 'started successfully' "$LOG" || fail "the node bubble did not start"
[ "$(requests)" = "tcp 127.0.0.1 10250 $guest 10250" ] || { requests >&2; fail "node listeners differ from the expected ones"; }

# The helper handing over its socket and exiting cleanly is not a dead daemon.
sleep 5
kill -0 "$BUBBLE_PID" 2>/dev/null || fail "the bubble stopped after the helper exited successfully"
grep -q 'ERROR' "$LOG" && fail "an error was reported after a successful handoff"
kill -TERM "$BUBBLE_PID"; wait_exit; released

# A helper that fails (the port is taken, plaidd refused the socket, nobody asked for it)
# fails the startup with a reason, and takes slirp4netns with it.
run_bubble controller FAKE_HELPER=fail
wait_exit
[ "$EXIT_RC" -ne 0 ] || fail "a failed overlay helper did not stop the bubble"
grep -q 'overlay socket helper' "$LOG" || fail "the failure does not name the overlay socket helper"
released

# A port that is in use for the TCP forwards still stops the bubble, and names the listener.
run_bubble controller FAKE_SLIRP_FAIL=tcp:10250
wait_exit
[ "$EXIT_RC" -ne 0 ] || fail "a forward that could not bind did not stop the bubble"
grep -qi 'kubelet' "$LOG" && grep -q 'Address already in use' "$LOG" || fail "the failure does not name the forward and the reason"
released

# When the bubble's instance ends by itself, the job's output carries the end of the
# bubble's own log, which says which daemon it was.
log_dir="$HOME/.apptainer/instances/logs/$(hostname)/$(id -un)"
mkdir -p "$log_dir"
printf 'earlier line\nFATAL: k3s server (PID 99) exited unexpectedly!\n' > "$log_dir/bubble1.out"
run_bubble controller FAKE_HELPER=ok
sleep 3
kill -0 "$BUBBLE_PID" 2>/dev/null || fail "the bubble stopped while its instance was running among 400 others"
touch "$FAKE_DIR/instance-gone"; kill "$(cat "$FAKE_DIR/inst-pid")"
wait_exit
[ "$EXIT_RC" -eq 1 ] || fail "an instance that ended: exit $EXIT_RC, expected 1"
grep -q "FATAL: k3s server (PID 99) exited unexpectedly" "$LOG" || fail "the job output does not carry the bubble's own log"
# The cause is recorded: the process, what was seen, and the bubble's own last FATAL line.
cause="$HOME/.skiff/job-results/unknown.cause"
[ -f "$cause" ] || fail "no cause was recorded when the instance ended"
grep -q '^process:  Apptainer instance bubble1' "$cause" && grep -q '^observed: gone: not in the instance listing and its PID is not running' "$cause" \
    && grep -q '^detail:   FATAL: k3s server (PID 99)' "$cause" || { cat "$cause" >&2; fail "the recorded cause does not say what ended"; }
grep -q 'Shutdown cause:' "$LOG" || fail "the cause is not in the job output"
rm -f "$log_dir/bubble1.out"

# One bad sample is not a death. A listing that lacks the instance once, a listing that
# fails for a while, a pgrep that misses once, an exec that fails: the instance and its
# PID are alive, the bubble goes on, and the disagreement is logged.
run_bubble controller FAKE_HELPER=ok
touch "$FAKE_DIR/list-flap"; sleep 5
kill -0 "$BUBBLE_PID" 2>/dev/null || fail "one listing without the instance ended the bubble"
grep -q 'probes of Apptainer instance bubble1 disagree' "$LOG" || fail "the disagreeing probes were not logged"
touch "$FAKE_DIR/list-fails"; sleep 9
kill -0 "$BUBBLE_PID" 2>/dev/null || fail "a failing listing ended a bubble whose instance was running"
rm -f "$FAKE_DIR/list-fails"
touch "$FAKE_DIR/entrypoint-flap"; sleep 6
kill -0 "$BUBBLE_PID" 2>/dev/null || fail "one miss of pgrep ended the bubble"
grep -q 'no entrypoint.sh in instance bubble1; checking again' "$LOG" || fail "the first miss of the entrypoint was not logged"
touch "$FAKE_DIR/exec-fails"; sleep 6
kill -0 "$BUBBLE_PID" 2>/dev/null || fail "a failing apptainer exec ended the bubble"
rm -f "$FAKE_DIR/exec-fails"
[ ! -e "$HOME/.skiff/job-results/unknown.cause" ] || fail "a cause was recorded for something that was not a failure"
# Persistent: the entrypoint really is gone (twice in a row), the bubble ends and says why.
touch "$FAKE_DIR/entrypoint-gone"
wait_exit
[ "$EXIT_RC" -eq 1 ] || fail "a missing entrypoint: exit $EXIT_RC, expected 1"
grep -q '^process:  bubble entrypoint (in instance bubble1)' "$HOME/.skiff/job-results/unknown.cause" || fail "the cause does not name the entrypoint"
released

# slirp4netns killed: the cause carries how it ended.
run_bubble controller FAKE_HELPER=ok
kill -KILL "$(cat "$FAKE_DIR/pid")"
wait_exit
[ "$EXIT_RC" -eq 1 ] || fail "slirp4netns killed: exit $EXIT_RC, expected 1"
# A failure does not wait for the pods: nothing is asked of the entrypoint.
[ ! -e "$FAKE_DIR/drain-requests" ] || fail "a failing bubble waited for its pods to stop"
grep -q '^process:  slirp4netns' "$HOME/.skiff/job-results/unknown.cause" && grep -q 'exited with status 137 (signal KILL' "$HOME/.skiff/job-results/unknown.cause" \
    || { cat "$HOME/.skiff/job-results/unknown.cause" >&2; fail "the cause does not say that slirp4netns was killed"; }

# A scratch directory that cannot be created stops the bubble before anything starts,
# with the reason; no slirp4netns, helper or instance was started.
: > "$test_dir/a-file"
run_bubble controller SKIFF_TMP_DIR="$test_dir/a-file/scratch"
wait_exit
[ "$EXIT_RC" -eq 1 ] || fail "an unusable scratch directory: exit $EXIT_RC, expected 1"
grep -q "SKIFF_TMP_DIR ($test_dir/a-file/scratch) cannot be created or written" "$LOG" || fail "no reason given for the scratch directory"
[ ! -e "$FAKE_DIR/pid" ] && [ ! -e "$FAKE_DIR/helper-args" ] && [ ! -e "$FAKE_DIR/instance-run" ] || { ls -la "$FAKE_DIR" >&2; fail "something was started before the scratch directory was checked"; }

echo "skiff-bubble-test: ok"
