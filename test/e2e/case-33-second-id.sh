#!/usr/bin/env bash
# Case 33: a container can hold a second id (T43).
#
# With a range of subordinate ids the bubble has more ids than the user's own, and so has every
# container in it: a process that starts as root can give a file to another user, and become
# another user (what the official nginx, postgres or redis images do at startup). Without a range
# the cluster runs with one id (see "Without a subordinate id range" in the guide): the kernel
# refuses both, with EINVAL and EPERM, and a pod that needs them cannot run. That is a fact about
# the node, so on a node without a range the result is UNAVAILABLE with that reason, and the run
# ends as PROFILE INCOMPLETE, not as a failure.
set -euo pipefail
E2E_CASE=33
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 33)
NODE_A=$(e2e_node_a)

e2e_require_nodes 2

# The same question the node check asks, put to the node the pod will run on.
has_range=$(runtime_ssh_node "$(runtime_node_ip "$NODE_A")" 'bash -c "source \$HOME/skiff/scripts/skiff.sh; if _skiff_has_id_range \$(id -un); then echo yes; else echo no; fi"' 2>/dev/null | tail -1 || true)
if [ "$has_range" != yes ]; then
    e2e_subcase_unavailable T43-second-id "the user has no subordinate id range on ${NODE_A} (/etc/subuid, /etc/subgid): the cluster runs with a single id, and a container cannot give a file to another user or become one"
    exit 0
fi

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --wait=false"
e2e_cleanup_register "e2e_preserve_failure $NS"

second_id() {
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata:
  name: e2e33-ids
  namespace: ${NS}
spec:
  nodeName: ${NODE_A}
  restartPolicy: Never
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command:
    - python3
    - -c
    - |
      import os
      open("/tmp/f", "w").close()
      os.chown("/tmp/f", 1000, 1000)
      print("owner", os.stat("/tmp/f").st_uid, os.stat("/tmp/f").st_gid)
      os.setgid(65534)
      os.setuid(65534)
      print("now", os.getuid(), os.getgid())
EOF2
    e2e_wait_pod_phase "$NS" e2e33-ids Succeeded 120
    local out
    out=$(e2e_kubectl logs e2e33-ids -n "$NS")
    e2e_assert_contains "the file was given to another user" "$out" "owner 1000 1000"
    e2e_assert_contains "and the process became another user" "$out" "now 65534 65534"
}
e2e_subcase_run T43-second-id second_id

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
