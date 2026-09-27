#!/usr/bin/env bash
# test/e2e/lib/fixture.sh — pinned fixture image references and Pod/Job YAML helpers.
# Source after common.sh.

# ---------------------------------------------------------------------------
# Fixture image — local SIF file deployed by `make e2e-images`.
# Build and deploy with:   make e2e-images   (then make develop to sync scripts)
# The SIF is placed on both VMs at ~/.skiff/images/skiff-e2e-fixture.sif
# by the e2e-images target, which also stamps FIXTURE_IMAGE_TAG below.
if [ -f "${HOME}/.skiff/fixture.env" ]; then
    # shellcheck source=/dev/null
    . "${HOME}/.skiff/fixture.env"
fi
FIXTURE_IMAGE_TAG="${FIXTURE_IMAGE_TAG:-3b2c5f4}"
FIXTURE_IMAGE="${FIXTURE_IMAGE:-/var/lib/skiff/images/skiff-e2e-fixture.sif}"
FIXTURE_REGISTRY="${FIXTURE_REGISTRY:-/var/lib/skiff/images}"


# ---------------------------------------------------------------------------
# Pod / Job YAML helpers
# Each function prints YAML to stdout; pipe into: kubectl apply -f -
# ---------------------------------------------------------------------------

# fixture_pod NS NAME NODE MODE [EXTRA_YAML]
# MODE: server | client | check | restart-counter | log-emitter | term-handler | blocker
fixture_pod() {
    local ns="$1" name="$2" node="$3" mode="$4"
    local extra="${5:-}"
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${ns}
  labels:
    app: ${name}
    e2e-fixture: "true"
    e2e-mode: ${mode}
spec:
  nodeName: ${node}
  restartPolicy: Never
  containers:
  - name: fixture
    image: ${FIXTURE_IMAGE}
    imagePullPolicy: IfNotPresent
    args: ["${mode}"]
${extra}
EOF
}

# fixture_server_pod NS NAME NODE PORT
# Long-lived HTTP server with readiness probe and TERM handler.
fixture_server_pod() {
    local ns="$1" name="$2" node="$3" port="${4:-8080}"
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${ns}
  labels:
    app: ${name}
    e2e-fixture: "true"
    e2e-mode: server
spec:
  nodeName: ${node}
  containers:
  - name: fixture
    image: ${FIXTURE_IMAGE}
    imagePullPolicy: IfNotPresent
    args: ["server", "--port", "${port}"]
    ports:
    - containerPort: ${port}
    readinessProbe:
      httpGet:
        path: /ready
        port: ${port}
      initialDelaySeconds: 1
      periodSeconds: 2
EOF
}

# fixture_readiness_pod NS NAME NODE PORT
# Server that starts unready; flip via POST /ready-on.
fixture_readiness_pod() {
    local ns="$1" name="$2" node="$3" port="${4:-8080}"
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${ns}
  labels:
    app: ${name}
    e2e-fixture: "true"
    e2e-mode: readiness-server
spec:
  nodeName: ${node}
  containers:
  - name: fixture
    image: ${FIXTURE_IMAGE}
    imagePullPolicy: IfNotPresent
    args: ["server", "--port", "${port}", "--start-unready"]
    ports:
    - containerPort: ${port}
    readinessProbe:
      httpGet:
        path: /ready
        port: ${port}
      initialDelaySeconds: 1
      periodSeconds: 2
EOF
}

# fixture_client_job NS NAME NODE SERVER_URL [EXTRA_ARGS...]
fixture_client_job() {
    local ns="$1" name="$2" node="$3" url="$4"
    shift 4
    local extra_args=""
    for a in "$@"; do extra_args="${extra_args}    - \"${a}\"\n"; done
    cat <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${name}
  namespace: ${ns}
  labels:
    e2e-fixture: "true"
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        app: ${name}
        e2e-fixture: "true"
    spec:
      nodeName: ${node}
      restartPolicy: Never
      containers:
      - name: fixture
        image: ${FIXTURE_IMAGE}
        imagePullPolicy: IfNotPresent
        args:
        - "client"
        - "${url}"
$(printf '%b' "$extra_args")
EOF
}

# fixture_checksum_job NS NAME NODE URL EXPECTED_SHA256
fixture_checksum_job() {
    local ns="$1" name="$2" node="$3" url="$4" sha="$5"
    cat <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: ${name}
  namespace: ${ns}
  labels:
    e2e-fixture: "true"
spec:
  backoffLimit: 0
  template:
    metadata:
      labels:
        app: ${name}
        e2e-fixture: "true"
    spec:
      nodeName: ${node}
      restartPolicy: Never
      containers:
      - name: fixture
        image: ${FIXTURE_IMAGE}
        imagePullPolicy: IfNotPresent
        args: ["checksum", "${url}", "${sha}"]
EOF
}

# fixture_clusterip_service NS NAME SELECTOR_APP PORT
fixture_clusterip_service() {
    local ns="$1" name="$2" selector="$3" port="${4:-8080}"
    cat <<EOF
apiVersion: v1
kind: Service
metadata:
  name: ${name}
  namespace: ${ns}
spec:
  selector:
    app: ${selector}
  ports:
  - port: ${port}
    targetPort: ${port}
EOF
}

# ---------------------------------------------------------------------------
# Helpers for verifying PodIP subnet membership
# ---------------------------------------------------------------------------

# e2e_assert_podip_in_subnet LABEL IP CIDR
# Checks that IP belongs to the node's PodCIDR reported by Kubernetes.
e2e_assert_podip_not_loopback() {
    local label="$1" ip="$2"
    if [[ "$ip" == 127.* ]]; then
        e2e_fail "${label}: PodIP ${ip} is loopback"
    else
        e2e_pass "${label}: PodIP ${ip} is not loopback"
    fi
}

e2e_assert_podip_in_node_cidr() {
    local label="$1" ip="$2" node="$3"
    local cidr
    cidr=$(e2e_kubectl get node "$node" -o jsonpath='{.spec.podCIDR}' 2>/dev/null || echo "")
    if [ -z "$cidr" ]; then
        e2e_fail "${label}: Could not get podCIDR for node ${node}"
        return 1
    fi
    # Simple prefix check for /24: compare first 3 octets
    local prefix
    prefix=$(echo "$cidr" | cut -d. -f1-3)
    if [[ "$ip" == "${prefix}."* ]]; then
        e2e_pass "${label}: PodIP ${ip} in node CIDR ${cidr}"
    else
        e2e_fail "${label}: PodIP ${ip} not in node CIDR ${cidr}"
    fi
}
