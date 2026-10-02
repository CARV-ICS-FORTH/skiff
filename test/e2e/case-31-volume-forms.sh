#!/usr/bin/env bash
# Case 31: the forms of volumes and ports that charts use (T41): a projected volume of a
# ConfigMap, a Secret and a service-account token (the token reaches the API), a mount with a
# subPath, an emptyDir in memory (accepted; it is a directory on the node's disk, and says
# so in the documentation) and a hostPort (accepted, not published on the host).
set -euo pipefail

E2E_CASE="31"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/common.sh"
source "$SCRIPT_DIR/lib/fixture.sh"
source "$SCRIPT_DIR/lib/runtime.sh"

NS=$(e2e_namespace 31)
NODE_A=$(e2e_node_a)
IP_A=$(runtime_node_ip "$NODE_A")

e2e_require_nodes 2

e2e_kubectl create namespace "$NS"
e2e_cleanup_register "e2e_kubectl delete namespace $NS --ignore-not-found --wait=false"
e2e_cleanup_register "e2e_preserve_failure $NS"

volume_forms() {
    e2e_kubectl create configmap cm31 -n "$NS" --from-literal=a=1 > /dev/null
    e2e_kubectl create secret generic sec31 -n "$NS" --from-literal=b=2 > /dev/null
    cat <<EOF2 | e2e_kubectl apply -f - > /dev/null
apiVersion: v1
kind: Pod
metadata: {name: e2e31-forms, namespace: ${NS}}
spec:
  nodeName: ${NODE_A}
  terminationGracePeriodSeconds: 1
  volumes:
  - name: proj
    projected:
      sources:
      - configMap: {name: cm31}
      - secret: {name: sec31}
      - serviceAccountToken: {path: tok, expirationSeconds: 3600, audience: api}
  - {name: mem, emptyDir: {medium: Memory, sizeLimit: 8Mi}}
  - {name: disk, emptyDir: {}}
  containers:
  - name: app
    image: ${FIXTURE_IMAGE}
    command: ["sleep", "3600"]
    ports: [{containerPort: 8099, hostPort: 8099}]
    volumeMounts:
    - {name: proj, mountPath: /proj}
    - {name: mem, mountPath: /mem}
    - {name: disk, mountPath: /whole}
    - {name: disk, mountPath: /sub, subPath: inner}
EOF2
    e2e_wait_pod_phase "$NS" e2e31-forms Running 120
    inpod() { e2e_kubectl exec e2e31-forms -n "$NS" -c app -- "$@"; }
    e2e_assert_eq "a projected ConfigMap" "$(inpod cat /proj/a)" 1
    e2e_assert_eq "a projected Secret" "$(inpod cat /proj/b)" 2
    e2e_assert_eq "a projected service-account token is there" "$(inpod sh -c 'test -s /proj/tok && echo yes || echo no')" yes
    e2e_assert_eq "the default token reaches the API server" "$(inpod sh -c 'curl -sk -o /dev/null -w "%{http_code}" -H "Authorization: Bearer $(cat /var/run/secrets/kubernetes.io/serviceaccount/token)" https://kubernetes.default.svc/version')" 200
    inpod sh -c 'mkdir -p /whole/inner && echo through-subpath > /sub/file && echo whole > /whole/top'
    e2e_assert_eq "a subPath mount is that directory of the volume" "$(inpod cat /whole/inner/file)" through-subpath
    e2e_assert_eq "and sees nothing outside it" "$(inpod sh -c 'ls /sub | tr "\n" " "')" "file "
    e2e_assert_eq "an emptyDir in memory is accepted and usable" "$(inpod sh -c 'echo ok > /mem/f && cat /mem/f')" ok
    e2e_assert_eq "a hostPort is accepted and not published on the host" "$(runtime_ssh_node "$IP_A" "ss -ltn | awk '\$4 ~ /:8099\$/' | wc -l" | tr -d ' ')" 0
    e2e_kubectl delete pod e2e31-forms -n "$NS" --timeout=60s > /dev/null
}
e2e_subcase_run T41-volume-forms volume_forms

[ "$E2E_FAILED" -eq 0 ] && e2e_pass "Case $E2E_CASE complete" || { e2e_log "Case $E2E_CASE FAILED"; exit 1; }
