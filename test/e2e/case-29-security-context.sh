#!/usr/bin/env bash
# Case 29: the securityContext of a pod (T38). The user and group a container runs as are
# applied, and the container stays in the pod's network (not the bubble's, which it once
# silently joined when it ran as another user than root). The fields Skifflet does not apply
# are accepted, so that charts that set them run unchanged, and the pod is told which in a
# Warning event. A volume is writable by a user that is not root without fsGroup. A container
# runs as the user its image says (USER) when the pod does not say (T44), and runAsNonRoot is
# the kubelet's: a container that would run as root is refused. A container that runs as root
# has the capabilities a container runtime gives, or those the pod asks for, in its main process
# and in what is run in it (T38-capabilities).
set -euo pipefail

E2E_CASE="29"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 29)
NODE_A=$(e2e_node_a)

e2e_require_nodes 1

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --wait=false"
e2e_cleanup_register "e2e_preserve_failure $NS"

in_container() { local pod="$1"; shift; e2e_kubectl exec "$pod" -n "$NS" -c app -- "$@"; }

# ---------------------------------------------------------------------------
# T38-nonroot-network: a container that runs as a user other than root is in the pod's
# network namespace: it has the pod's address on eth0 and none of the bubble's interfaces,
# can bind a port that the bubble itself holds (the kubelet's 10250), and is the target of
# exec (which checks that the process is in the pod's network).
# ---------------------------------------------------------------------------
nonroot_network() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-user
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  securityContext: {runAsUser: 1000, runAsGroup: 2000}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
EOF2
    e2e_wait_pod_phase "$NS" e2e29-user Running 120
    local ip addrs all
    ip=$(e2e_kubectl get pod e2e29-user -n "$NS" -o jsonpath='{.status.podIP}')
    e2e_assert_eq "exec reaches the container, as the user and group asked" "$(in_container e2e29-user id -u):$(in_container e2e29-user id -g)" "1000:2000"
    addrs=$(in_container e2e29-user ip -4 -o addr show dev eth0)
    e2e_assert_contains "the container has the pod's address on eth0" "$addrs" "inet ${ip}/"
    all=$(in_container e2e29-user ip -4 -o addr)
    e2e_assert_eq "and none of the bubble's interfaces" "$(printf '%s' "$all" | grep -c -E 'tap0|plaid0' || true)" 0
    e2e_assert_contains "it can bind a port that the bubble holds (the kubelet's 10250)" "$(in_container e2e29-user python3 -c 'import socket; s = socket.socket(); s.bind(("0.0.0.0", 10250)); print("bound")')" bound
    e2e_kubectl delete pod e2e29-user -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T38-nonroot-network nonroot_network

# ---------------------------------------------------------------------------
# T38-fields-not-applied: a pod with the fields of a hardened chart runs, as the user and
# group it names, writes to its volume as that user, and is told which fields were accepted
# and not applied; runAsNonRoot on an image that runs as root is refused, as the kubelet does.
# ---------------------------------------------------------------------------
fields_not_applied() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-hardened
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  securityContext:
    runAsUser: 1000
    runAsGroup: 2000
    fsGroup: 3000
    supplementalGroups: [4000]
    seccompProfile: {type: RuntimeDefault}
  volumes:
  - {name: data, emptyDir: {}}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    securityContext:
      readOnlyRootFilesystem: true
      allowPrivilegeEscalation: false
      capabilities: {drop: [ALL]}
    volumeMounts:
    - {name: data, mountPath: /data}
---
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-nonroot
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    securityContext: {runAsNonRoot: true}
EOF2
    e2e_wait_pod_phase "$NS" e2e29-hardened Running 120
    # runAsNonRoot, with no user in the pod and an image that runs as root, is refused: the container
    # waits with CreateContainerConfigError and the kubelet's message (and the pod does not run)
    nonroot_state() { e2e_kubectl get pod e2e29-nonroot -n "$NS" -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}: {.status.containerStatuses[0].state.waiting.message}' 2>/dev/null; }
    nonroot_refused() { [[ "$(nonroot_state)" == CreateContainerConfigError:* ]]; }
    e2e_wait_condition 60 "runAsNonRoot on an image that runs as root is refused" nonroot_refused
    e2e_assert_contains "with the kubelet's message" "$(nonroot_state)" "container has runAsNonRoot and image will run as root"
    e2e_assert_eq "the pod runs as the user and group it names" "$(in_container e2e29-hardened id -u):$(in_container e2e29-hardened id -g)" "1000:2000"
    e2e_assert_eq "that user writes to its volume (fsGroup is not needed)" "$(in_container e2e29-hardened sh -c 'echo ok > /data/file && cat /data/file')" ok

    event() { e2e_kubectl get events -n "$NS" --field-selector involvedObject.name=e2e29-hardened,reason=SecurityContextNotApplied -o jsonpath='{.items[0].message}' 2>/dev/null; }
    has_event() { [ -n "$(event)" ]; }
    e2e_wait_condition 30 "the event that says what was not applied" has_event
    local message
    message=$(event)
    for field in fsGroup supplementalGroups seccompProfile readOnlyRootFilesystem; do
        e2e_assert_contains "the event names $field" "$message" "$field"
    done
    e2e_assert_eq "and not what was applied" "$(printf '%s' "$message" | grep -c -E 'runAsUser: |runAsGroup: |capabilities' || true)" 0
    e2e_assert_eq "a pod without such fields gets no event" "$(e2e_kubectl get events -n "$NS" --field-selector involvedObject.name=e2e29-nonroot,reason=SecurityContextNotApplied -o name | wc -l | tr -d ' ')" 0
    e2e_kubectl delete pod e2e29-hardened e2e29-nonroot -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T38-fields-not-applied fields_not_applied


# ---------------------------------------------------------------------------
# T38-capabilities: a container that runs as root has the default set of a container runtime
# (and cannot mount, or change its address), a pod can ask for more or for less, and a command
# run in it (exec, probes) has what the container has, whatever it was entered with: a program
# of the image gets nothing more from being run by exec.
# ---------------------------------------------------------------------------
capabilities() {
    local name sc
    for name in default added none privileged; do
        case "$name" in
            default) sc='{}' ;;
            added) sc='{capabilities: {add: [NET_ADMIN]}}' ;;
            none) sc='{capabilities: {drop: [ALL]}}' ;;
            privileged) sc='{privileged: true}' ;;
        esac
        cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-caps-${name}
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    securityContext: ${sc}
    readinessProbe:
      exec: {command: ["sh", "-c", "cat /proc/self/status > /tmp/probe-status"]}
      periodSeconds: 1
EOF2
    done
    for name in default added none privileged; do
        e2e_wait_pod_ready "$NS" "e2e29-caps-${name}" 120
    done
    caps() { in_container "$1" sh -c "sed -n 's/^$2:[[:space:]]*//p' $3"; }
    local want_default=00000000a80425fb want_added=00000000a80435fb want_none=0000000000000000
    for name in default added none; do
        local want="want_${name}"
        e2e_assert_eq "$name: what the container's main process may have" "$(caps "e2e29-caps-$name" CapBnd /proc/1/status)" "${!want}"
        e2e_assert_eq "$name: what its main process has" "$(caps "e2e29-caps-$name" CapEff /proc/1/status)" "${!want}"
        e2e_assert_eq "$name: what an exec may have" "$(caps "e2e29-caps-$name" CapBnd /proc/self/status)" "${!want}"
        e2e_assert_eq "$name: what an exec has" "$(caps "e2e29-caps-$name" CapEff /proc/self/status)" "${!want}"
        e2e_assert_eq "$name: what a probe has" "$(caps "e2e29-caps-$name" CapEff /tmp/probe-status)" "${!want}"
    done
    local full
    full=$(caps e2e29-caps-privileged CapEff /proc/1/status)
    e2e_assert_eq "privileged: an exec has what the main process has" "$(caps e2e29-caps-privileged CapEff /proc/self/status)" "$full"
    e2e_assert_eq "privileged: has more than the default" "$(( 0x$full & (1 << 21) ))" "$(( 1 << 21 ))"

    can() { if in_container "$1" sh -c "$2" > /dev/null 2>&1; then echo yes; else echo no; fi; }
    local mount='mkdir -p /m && mount -t tmpfs none /m' address='ip addr add 10.99.0.1/32 dev eth0'
    e2e_assert_eq "default: an exec cannot mount" "$(can e2e29-caps-default "$mount")" no
    e2e_assert_eq "default: an exec cannot change the pod's address" "$(can e2e29-caps-default "$address")" no
    local unreadable='touch /tmp/f && chmod 000 /tmp/f && cat /tmp/f'
    e2e_assert_eq "default: it reads a file whatever its mode, as root" "$(can e2e29-caps-default "$unreadable")" yes
    e2e_assert_eq "none: it does not" "$(can e2e29-caps-none "$unreadable")" no
    e2e_assert_eq "added: an exec can change the pod's address" "$(can e2e29-caps-added "$address")" yes
    e2e_assert_eq "added: and it cannot mount" "$(can e2e29-caps-added "$mount")" no
    e2e_assert_eq "privileged: an exec can mount" "$(can e2e29-caps-privileged "$mount")" yes

    # (The command's exit status, and what a command that is not there reports, are as without the step.)
    local status=0
    in_container e2e29-caps-default sh -c 'exit 7' > /dev/null 2>&1 || status=$?
    e2e_assert_eq "default: an exec reports its command's status" "$status" 7
    e2e_assert_contains "default: and a command that is not there" "$(in_container e2e29-caps-default no-such-command 2>&1 || true)" "no-such-command"

    e2e_kubectl delete pod e2e29-caps-default e2e29-caps-added e2e29-caps-none e2e29-caps-privileged -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T38-capabilities capabilities

# ---------------------------------------------------------------------------
# T38-exec-environment: what a pod puts in its environment reaches the commands run in it and
# nothing that runs before they are in the container (Skifflet's nsenter has all the bubble's
# capabilities, and plaidtainer, which takes the variable below as an order). As root and not.
# ---------------------------------------------------------------------------
exec_environment() {
    local name sc
    for name in root user; do
        case "$name" in
            root) sc='{}' ;;
            user) sc='{runAsUser: 1000, runAsGroup: 2000}' ;;
        esac
        cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-env-${name}
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    securityContext: ${sc}
    env:
    - {name: PLAID_DIRECT_WATCHDOG, value: "1"}
    - {name: E2E_VALUE, value: "two words"}
EOF2
        e2e_wait_pod_phase "$NS" "e2e29-env-$name" Running 120
        e2e_assert_eq "$name: the command runs, with the pod's environment" \
            "$(in_container "e2e29-env-$name" sh -c 'printf "%s:%s" "$E2E_VALUE" "$PLAID_DIRECT_WATCHDOG"')" "two words:1"
        local status=0
        in_container "e2e29-env-$name" sh -c 'exit 7' || status=$?
        e2e_assert_eq "$name: and its exit status comes back" "$status" 7
        e2e_kubectl delete pod "e2e29-env-$name" -n "$NS" --timeout=60s > /dev/null
    done
}
e2e_subcase_run T38-exec-environment exec_environment

# ---------------------------------------------------------------------------
# T38-exec-root: what is run in a container is started from Skifflet's descriptors, also when its
# main process has changed its root.
# ---------------------------------------------------------------------------
exec_root() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-root
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command:
    - python3
    - -c
    - |
      import os, shutil, time
      j = "/tmp/jail"
      os.makedirs(j + "/proc/self/fd"); os.makedirs(j + "/lib"); os.makedirs(j + "/bin")
      shutil.copy("/bin/busybox", j + "/bin/busybox")
      for f in os.listdir("/lib"):
          if f.startswith("ld-musl"): shutil.copy("/lib/" + f, j + "/lib/" + f)
      with open(j + "/proc/self/fd/3", "w") as p: p.write("#!/bin/busybox sh\n/bin/busybox touch /marker\n")
      os.chmod(j + "/proc/self/fd/3", 0o755)
      open("/tmp/jail-ready", "w").close()
      os.chroot(j); os.chdir("/")
      time.sleep(3600)
EOF2
    e2e_wait_pod_phase "$NS" e2e29-root Running 120
    local i; for i in $(seq 1 30); do
        sleep 1
        in_container e2e29-root /bin/busybox true 2>/dev/null && break
    done
    # The command runs in the container's root, which is now the directory it chose.
    e2e_assert_eq "a command runs in the root the container chose" "$(in_container e2e29-root /bin/busybox sh -c 'echo inside')" inside
    e2e_assert_eq "only the command was run" "$(in_container e2e29-root /bin/busybox sh -c '[ -e /marker ] && echo ran || echo not')" not
    e2e_kubectl delete pod e2e29-root -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T38-exec-root exec_root

# ---------------------------------------------------------------------------
# T38-exec-own-userns: what nsenter starts in a container that has a user namespace of its own stays
# in that namespace. The main process makes one, as root or as another user, and changes its root.
# ---------------------------------------------------------------------------
own_userns_pod() {  # own_userns_pod NAME SECURITY_CONTEXT
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    securityContext: $2
    command:
    - python3
    - -c
    - |
      import ctypes, os, shutil, time
      libc = ctypes.CDLL(None, use_errno=True)
      uid, gid = os.getuid(), os.getgid()
      if libc.unshare(0x10000000 | 0x20000) != 0: raise OSError(ctypes.get_errno(), "unshare")
      open("/proc/self/setgroups", "w").write("deny")
      open("/proc/self/uid_map", "w").write("0 %d 1" % uid)
      open("/proc/self/gid_map", "w").write("0 %d 1" % gid)
      j = "/tmp/userns-root"
      for d in ("/proc/self/fd", "/lib", "/bin", "/tp"): os.makedirs(j + d)
      shutil.copy("/bin/busybox", j + "/bin/busybox")
      for f in os.listdir("/lib"):
          if f.startswith("ld-musl"): shutil.copy("/lib/" + f, j + "/lib/" + f)
      if libc.mount(b"/proc", (j + "/tp").encode(), None, 0x1000 | 0x4000, None) != 0: raise OSError(ctypes.get_errno(), "mount")
      with open(j + "/proc/self/fd/3", "w") as p: p.write("#!/bin/busybox sh\n/bin/busybox readlink /tp/self/ns/user > /marker\n")
      os.chmod(j + "/proc/self/fd/3", 0o755)
      own = os.readlink("/proc/self/ns/user")
      os.chroot(j); os.chdir("/")
      print("READY own=" + own, flush=True)
      while not os.path.exists("/marker"): time.sleep(0.2)
      time.sleep(0.2)
      print("MARKER=" + open("/marker").read().strip(), flush=True)
      time.sleep(3600)
EOF2
}
own_userns_ready() { e2e_kubectl logs "$1" -n "$NS" -c app | e2e_grep_q READY; }
own_userns() {
    local pod log own marker
    for pod in e2e29-userns-user e2e29-userns-root; do
        if [ "$pod" = e2e29-userns-user ]; then own_userns_pod "$pod" '{runAsUser: 1000, runAsGroup: 1000}'; else own_userns_pod "$pod" '{}'; fi
        e2e_wait_pod_phase "$NS" "$pod" Running 120
        e2e_wait_condition 60 "$pod has made its namespace" own_userns_ready "$pod"
        in_container "$pod" /bin/busybox true > /dev/null 2>&1 || true
        sleep 3
        log=$(e2e_kubectl logs "$pod" -n "$NS" -c app)
        own=$(sed -n 's/^READY own=//p' <<< "$log")
        marker=$(sed -n 's/^MARKER=//p' <<< "$log")
        e2e_log "$pod: its namespace $own, what ran there: ${marker:-nothing}"
        [ -n "$own" ] || e2e_fail "$pod did not make a user namespace: $log"
        [ -z "$marker" ] || e2e_assert_eq "$pod: what nsenter ran stayed in the container's user namespace" "$marker" "$own"
        e2e_kubectl delete pod "$pod" -n "$NS" --timeout=60s > /dev/null
    done
}
e2e_subcase_run T38-exec-own-userns own_userns

# ---------------------------------------------------------------------------
# T38-source-address: the bridge takes a pod's packets with the pod's own address as their source.
# ---------------------------------------------------------------------------
source_address() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-src-to
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command:
    - python3
    - -c
    - |
      import socket
      s = socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_ICMP)
      print("READY", flush=True)
      while True:
          d, a = s.recvfrom(2048)
          print("FROM", a[0], flush=True)
---
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-src-from
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
EOF2
    e2e_wait_pod_phase "$NS" e2e29-src-to Running 120
    e2e_wait_pod_phase "$NS" e2e29-src-from Running 120
    local to from log other
    to=$(e2e_kubectl -n "$NS" get pod e2e29-src-to -o jsonpath='{.status.podIP}')
    from=$(e2e_kubectl -n "$NS" get pod e2e29-src-from -o jsonpath='{.status.podIP}')
    other="${from%.*}.250"
    e2e_wait_condition 60 "the receiver is ready" source_receiver_ready
    in_container e2e29-src-from python3 -c "
import socket, struct
def checksum(b):
    t = sum(struct.unpack('!%dH' % (len(b) // 2), b))
    t = (t >> 16) + (t & 0xffff)
    return ~(t + (t >> 16)) & 0xffff
icmp = struct.pack('!BBHHH', 8, 0, 0, 1, 1) + b'probe-probe-probe-'
icmp = icmp[:2] + struct.pack('!H', checksum(icmp)) + icmp[4:]
s = socket.socket(socket.AF_INET, socket.SOCK_RAW, socket.IPPROTO_RAW)
for src in ('$other', '$from'):
    h = struct.pack('!BBHHHBBH4s4s', 0x45, 0, 20 + len(icmp), 1, 0, 64, 1, 0, socket.inet_aton(src), socket.inet_aton('$to'))
    h = h[:10] + struct.pack('!H', checksum(h)) + h[12:]
    for _ in range(3):
        s.sendto(h + icmp, ('$to', 0))
" > /dev/null
    sleep 2
    log=$(e2e_kubectl logs e2e29-src-to -n "$NS" -c app)
    e2e_assert_contains "a packet with the pod's own address arrives" "$log" "FROM $from"
    e2e_assert_eq "packets with another source address arrive" "$(grep -c "FROM $other" <<< "$log" || true)" 0
    e2e_kubectl delete pod e2e29-src-to e2e29-src-from -n "$NS" --timeout=60s > /dev/null
}
source_receiver_ready() { e2e_kubectl logs e2e29-src-to -n "$NS" -c app | e2e_grep_q READY; }
e2e_subcase_run T38-source-address source_address

# ---------------------------------------------------------------------------
# T44-image-user: a container runs as the user its image says (USER), with the group of its
# passwd entry, when the pod does not say; the pod's runAsUser comes first; runAsNonRoot is
# satisfied by the image's user; and a container that is not root binds a port below 1024
# (the pod's namespace lets it, as a container runtime does).
# ---------------------------------------------------------------------------
image_user() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-imageuser
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_USER_IMAGE}
    command: ["sleep", "3600"]
---
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-imageuser-pod
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  containers:
  - name: app
    image: ${FIXTURE_USER_IMAGE}
    command: ["sleep", "3600"]
    securityContext: {runAsUser: 1000, runAsGroup: 2000}
---
apiVersion: v1
kind: Pod
metadata:
  name: e2e29-imageuser-nonroot
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  securityContext: {runAsNonRoot: true}
  containers:
  - name: app
    image: ${FIXTURE_USER_IMAGE}
    command: ["sleep", "3600"]
EOF2
    e2e_wait_pod_phase "$NS" e2e29-imageuser Running 120
    e2e_wait_pod_phase "$NS" e2e29-imageuser-pod Running 120
    e2e_wait_pod_phase "$NS" e2e29-imageuser-nonroot Running 120
    e2e_assert_eq "the image's user, by name, and its group (the group of its passwd entry)" "$(in_container e2e29-imageuser id -un):$(in_container e2e29-imageuser id -u):$(in_container e2e29-imageuser id -g)" "appuser:4321:4322"
    e2e_assert_eq "the pod's user and group over the image's" "$(in_container e2e29-imageuser-pod id -u):$(in_container e2e29-imageuser-pod id -g)" "1000:2000"
    e2e_assert_eq "runAsNonRoot is satisfied by the image's user" "$(in_container e2e29-imageuser-nonroot id -u)" 4321
    e2e_assert_contains "a container that is not root binds port 80" "$(in_container e2e29-imageuser python3 -c 'import socket; s = socket.socket(); s.bind(("0.0.0.0", 80)); print("bound")')" bound
    e2e_kubectl delete pod e2e29-imageuser e2e29-imageuser-pod e2e29-imageuser-nonroot -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T44-image-user image_user

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
