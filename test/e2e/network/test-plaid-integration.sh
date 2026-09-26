#!/usr/bin/env bash
# Integration test for Plaid on a Linux system or Docker container.
set -euo pipefail

export CNI_PATH="${CNI_PATH:-/opt/cni/bin}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

if command -v plaidd >/dev/null 2>&1; then
    BIN_DIR="$(dirname "$(command -v plaidd)")"
elif [ -d "${ROOT_DIR}/plaid/bin" ]; then
    BIN_DIR="${ROOT_DIR}/plaid/bin"
elif [ -d "${ROOT_DIR}/bin" ]; then
    BIN_DIR="${ROOT_DIR}/bin"
else
    echo "=== 1. Building Plaid binaries ==="
    make -C "${ROOT_DIR}" build
    BIN_DIR="${ROOT_DIR}/plaid/bin"
fi
RUN_DIR=$(mktemp -d /tmp/plaid-test-XXXXXX)
SOCKET_PATH="${RUN_DIR}/plaidd.sock"

cleanup() {
    echo "=== Cleaning up ==="
    if [ -n "${PLAIDD_PID:-}" ] && kill -0 "${PLAIDD_PID}" 2>/dev/null; then
        kill "${PLAIDD_PID}" 2>/dev/null || true
        wait "${PLAIDD_PID}" 2>/dev/null || true
    fi
    ip netns del testns1 2>/dev/null || true
    ip netns del testns2 2>/dev/null || true
    rm -rf "${RUN_DIR}"
    echo "=== Cleanup complete ==="
}
trap cleanup EXIT

echo "=== 2. Creating Network Namespaces ==="
ip netns add testns1
ip netns add testns2

echo "=== 3. Starting plaidd in background ==="
"${BIN_DIR}/plaidd" \
    --api-socket="${SOCKET_PATH}" \
    --node-cidr="10.244.1.0/24" \
    --cluster-cidr="10.244.0.0/16" \
    --vxlan-port=8473 \
    --enable-slirp=false &
PLAIDD_PID=$!

# Wait for API socket to become active
for i in {1..20}; do
    if [ -S "${SOCKET_PATH}" ]; then
        break
    fi
    sleep 0.1
done

if [ ! -S "${SOCKET_PATH}" ]; then
    echo "ERROR: plaidd API socket not ready"
    exit 1
fi
echo "plaidd is running with PID ${PLAIDD_PID}"

echo "=== 4. Adding endpoints using plaid CNI ==="
CNI_CONF=$(cat <<EOF
{
  "cniVersion": "0.4.0",
  "name": "cbr0",
  "type": "plaid",
  "socketPath": "${SOCKET_PATH}",
  "mtu": 1500
}
EOF
)

# Configure testns1 with 10.244.1.4
echo "${CNI_CONF}" | CNI_COMMAND=ADD \
    CNI_CONTAINERID=cont-ns1 \
    CNI_NETNS=/var/run/netns/testns1 \
    CNI_IFNAME=eth0 \
    CNI_ARGS="IP=10.244.1.4;K8S_POD_NAME=pod-ns1;K8S_POD_NAMESPACE=default" \
    "${BIN_DIR}/plaid" >/dev/null

# Assign IP to tap device inside testns1
ip netns exec testns1 ip addr add 10.244.1.4/24 dev eth0 2>/dev/null || true
ip netns exec testns1 ip link set eth0 up

# Configure testns2 with 10.244.1.5
echo "${CNI_CONF}" | CNI_COMMAND=ADD \
    CNI_CONTAINERID=cont-ns2 \
    CNI_NETNS=/var/run/netns/testns2 \
    CNI_IFNAME=eth0 \
    CNI_ARGS="IP=10.244.1.5;K8S_POD_NAME=pod-ns2;K8S_POD_NAMESPACE=default" \
    "${BIN_DIR}/plaid" >/dev/null

# Assign IP to tap device inside testns2
ip netns exec testns2 ip addr add 10.244.1.5/24 dev eth0 2>/dev/null || true
ip netns exec testns2 ip link set eth0 up

echo "=== 5. Testing Ping between testns1 and testns2 ==="
if ip netns exec testns1 ping -c 3 -W 2 10.244.1.5; then
    echo "SUCCESS: Ping between pods via plaidd passed!"
else
    echo "FAIL: Ping between pods failed"
    exit 1
fi

echo "=== 6. Testing CNI DEL ==="
echo "${CNI_CONF}" | CNI_COMMAND=DEL \
    CNI_CONTAINERID=cont-ns1 \
    CNI_IFNAME=eth0 \
    CNI_ARGS="K8S_POD_NAME=pod-ns1;K8S_POD_NAMESPACE=default" \
    "${BIN_DIR}/plaid" >/dev/null

echo "SUCCESS: Integration test completed successfully!"
