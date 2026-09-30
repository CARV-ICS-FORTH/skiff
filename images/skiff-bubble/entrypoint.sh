#!/bin/bash
set -euo pipefail

# Redirect all stdout and stderr to /var/log/entrypoint.log while keeping terminal output
mkdir -p /var/log /tmp/.skiff/.apptainer/tmp /root/.skiff/.apptainer/cache
exec > >(tee -a /var/log/entrypoint.log) 2>&1

# Default values if not provided
HOST_IP=${HOST_IP:-$(ip route get 1 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="src") print $(i+1); exit}' || echo "127.0.0.1")}
SKIFF_ROLE=${SKIFF_ROLE:-controller}
PLAID_UPLINK=${PLAID_UPLINK:-tap0}
: "${PLAID_UPLINK_ADDRESS:?outer slirp guest address is required}"

# The cluster's network configuration is decided and checked once, by skiff.slurm,
# and arrives here resolved (see scripts/skiff-config.sh). Everything below uses
# these values; none is spelled out again.
: "${SKIFF_POD_CIDR:?the pod network is required (SKIFF_POD_CIDR)}"
: "${SKIFF_SERVICE_CIDR:?the Service network is required (SKIFF_SERVICE_CIDR)}"
: "${SKIFF_CLUSTER_DOMAIN:?the cluster domain is required (SKIFF_CLUSTER_DOMAIN)}"
: "${SKIFF_CLUSTER_DNS:?the cluster DNS address is required (SKIFF_CLUSTER_DNS)}"
: "${SKIFF_API_SERVICE_IP:?the API Service address is required (SKIFF_API_SERVICE_IP)}"
: "${SKIFF_POD_MTU:?the pod MTU is required (SKIFF_POD_MTU)}"
: "${SKIFF_OVERLAY_HANDOFF:?the overlay socket handoff path is required (SKIFF_OVERLAY_HANDOFF)}"
# Optional: the pods directory. Empty means Skifflet's own default.
SKIFF_PODS_DIR="${SKIFF_PODS_DIR:-}"

echo "Starting Skiff Bubble..."
echo "  Public IP:     ${HOST_IP}"
echo "  Interface:     tap0"
echo "  Role:          ${SKIFF_ROLE}"
echo "Effective network configuration:"
echo "  Pod network:      ${SKIFF_POD_CIDR}"
echo "  Service network:  ${SKIFF_SERVICE_CIDR} (API ${SKIFF_API_SERVICE_IP}, DNS ${SKIFF_CLUSTER_DNS})"
echo "  Cluster domain:   ${SKIFF_CLUSTER_DOMAIN}"
echo "  Pod MTU:          ${SKIFF_POD_MTU}"
echo "  Pods directory:   ${SKIFF_PODS_DIR:-(default)}"

# Enable IP forwarding
sysctl -w net.ipv4.ip_forward=1

# Wait for tap0 interface to appear (created by slirp4netns)
echo "Waiting for tap0 interface..."
TAP_WAIT=0
TAP_TIMEOUT=30
while ! ip link show tap0 >/dev/null 2>&1; do
    if [ "$TAP_WAIT" -ge "$TAP_TIMEOUT" ]; then
        echo "ERROR: Timed out waiting for tap0 interface after ${TAP_TIMEOUT}s" >&2
        exit 1
    fi
    sleep 0.1
    TAP_WAIT=$((TAP_WAIT + 1))
done

# Ensure it is up
ip link set tap0 up

# Add Host IP as secondary address to tap0
ip addr add "${HOST_IP}/32" dev tap0 2>/dev/null || true
if ! ip -4 addr show dev "$PLAID_UPLINK" | grep -Fq "$PLAID_UPLINK_ADDRESS/"; then
    echo "ERROR: uplink $PLAID_UPLINK does not have $PLAID_UPLINK_ADDRESS" >&2
    exit 1
fi

K3S_PID=""
CSR_SIGN_PID=""

# Start K3s if Controller
if [ "$SKIFF_ROLE" = "controller" ]; then
    echo "Starting K3s Server..."
    k3s server \
      --bind-address 0.0.0.0 \
      --advertise-address "${HOST_IP}" \
      --tls-san "${HOST_IP}" \
      --tls-san 0.0.0.0 \
      --tls-san 127.0.0.1 \
      --tls-san localhost \
      --cluster-cidr "$SKIFF_POD_CIDR" \
      --service-cidr "$SKIFF_SERVICE_CIDR" \
      --cluster-domain "$SKIFF_CLUSTER_DOMAIN" \
      --cluster-dns "$SKIFF_CLUSTER_DNS" \
      --kube-controller-manager-arg=allocate-node-cidrs=true \
      --kube-controller-manager-arg=node-monitor-grace-period=10s \
      --kube-controller-manager-arg=node-monitor-period=2s \
      --disable-agent \
      --disable servicelb \
      --disable traefik \
      --disable local-storage \
      --disable metrics-server \
      --disable-cloud-controller \
      --kubelet-arg=resolv-conf=/etc/resolv.conf \
      --write-kubeconfig-mode 600 \
      --egress-selector-mode=disabled \
      --kube-apiserver-arg=kubelet-certificate-authority=/var/lib/rancher/k3s/server/tls/server-ca.crt \
      --kube-apiserver-arg=kubelet-preferred-address-types=InternalIP,ExternalIP,Hostname \
      >> /var/log/k3s.log 2>&1 &
    K3S_PID=$!
    
    # Wait for K3s to initialize kubeconfig
    echo "Waiting for K3s to initialize..."
    K3S_WAIT=0
    K3S_TIMEOUT=120
    while [ ! -f /etc/rancher/k3s/k3s.yaml ]; do
        if [ "$K3S_WAIT" -ge "$K3S_TIMEOUT" ]; then
            echo "ERROR: Timed out waiting for k3s server to initialize after ${K3S_TIMEOUT}s" >&2
            exit 1
        fi
        sleep 1
        K3S_WAIT=$((K3S_WAIT + 1))
    done
    
    # Export server-ca.crt to shared directory for node certificates FIRST (CA key is NOT exported)
    echo "Copying server-ca.crt to /var/lib/skiff/tls..."
    mkdir -p /var/lib/skiff/tls
    cp /var/lib/rancher/k3s/server/tls/server-ca.crt /var/lib/skiff/tls/server-ca.crt.tmp
    chmod 644 /var/lib/skiff/tls/server-ca.crt.tmp
    mv /var/lib/skiff/tls/server-ca.crt.tmp /var/lib/skiff/tls/server-ca.crt

    # Copy kubeconfig to shared directory AFTER server-ca is ready
    echo "Copying kubeconfig to /var/lib/skiff..."
    cp /etc/rancher/k3s/k3s.yaml /var/lib/skiff/kubeconfig.tmp
    # Replace 0.0.0.0 or 127.0.0.1 in kubeconfig server URL with actual HOST_IP
    sed -i "s|https://0.0.0.0:6443|https://${HOST_IP}:6443|g" /var/lib/skiff/kubeconfig.tmp
    sed -i "s|https://127.0.0.1:6443|https://${HOST_IP}:6443|g" /var/lib/skiff/kubeconfig.tmp
    chmod 600 /var/lib/skiff/kubeconfig.tmp
    mv /var/lib/skiff/kubeconfig.tmp /var/lib/skiff/kubeconfig

    # Start background CSR signing loop on controller node IMMEDIATELY
    echo "Starting kubelet CSR signing loop..."
    (
        while true; do
            shopt -s nullglob
            for csr_file in /var/lib/skiff/.certs/*/kubelet.csr; do
                [ -f "$csr_file" ] || continue
                cert_dir=$(dirname "$csr_file")
                crt_file="${cert_dir}/kubelet.crt"
                cnf_file="${cert_dir}/kubelet.cnf"
                
                # If crt_file exists, check if it was signed by current CA
                if [ -f "$crt_file" ]; then
                    if ! openssl verify -CAfile /var/lib/rancher/k3s/server/tls/server-ca.crt "$crt_file" >/dev/null 2>&1; then
                        rm -f "$crt_file"
                    fi
                fi

                if [ ! -f "$crt_file" ]; then
                    if [ -f "/var/lib/rancher/k3s/server/tls/server-ca.key" ] && [ -f "/var/lib/rancher/k3s/server/tls/server-ca.crt" ]; then
                        echo "Signing kubelet CSR in ${cert_dir}..."
                        EXT_ARGS=""
                        if [ -f "$cnf_file" ]; then
                            EXT_ARGS="-extfile $cnf_file -extensions v3_req"
                        fi
                        # shellcheck disable=SC2086
                        if openssl x509 -req -days 365 -set_serial $(date +%s%N 2>/dev/null || date +%s) \
                          -CA /var/lib/rancher/k3s/server/tls/server-ca.crt \
                          -CAkey /var/lib/rancher/k3s/server/tls/server-ca.key \
                          -in "$csr_file" -out "${crt_file}.tmp" $EXT_ARGS; then
                            chmod 600 "${crt_file}.tmp"
                            mv "${crt_file}.tmp" "$crt_file"
                            echo "Successfully signed kubelet cert in ${cert_dir}"
                        else
                            echo "ERROR: Failed to sign kubelet CSR in ${cert_dir}" >&2
                            rm -f "${crt_file}.tmp"
                        fi
                    fi
                fi
            done
            sleep 1
        done
    ) &
    CSR_SIGN_PID=$!

    # Make CoreDNS inherit the bubble resolver instead of the cluster DNS service IP (async in background).
    (
        echo "Configuring CoreDNS to use the bubble resolver in background..."
        COREDNS_WAIT=0
        COREDNS_TIMEOUT=180
        while [ "$COREDNS_WAIT" -lt "$COREDNS_TIMEOUT" ]; do
            if k3s kubectl -n kube-system get deployment coredns >/dev/null 2>&1 && k3s kubectl -n kube-system get configmap coredns >/dev/null 2>&1; then
                k3s kubectl -n kube-system patch deployment coredns --type=merge -p '{"spec":{"template":{"spec":{"dnsPolicy":"Default"}}}}'
                CURRENT_COREFILE="$(k3s kubectl -n kube-system get configmap coredns -o jsonpath='{.data.Corefile}')"
                UPDATED_COREFILE="$(printf '%s\n' "$CURRENT_COREFILE" | sed -E 's#forward \. (/etc/resolv\.conf|([0-9]{1,3}\.){3}[0-9]{1,3})#forward . /etc/resolv.conf#g')"
                if [ -n "$UPDATED_COREFILE" ] && [ "$CURRENT_COREFILE" != "$UPDATED_COREFILE" ]; then
                    cat <<EOF | k3s kubectl apply -f -
apiVersion: v1
kind: ConfigMap
metadata:
  name: coredns
  namespace: kube-system
data:
  Corefile: |
$(printf '%s\n' "$UPDATED_COREFILE" | sed 's/^/    /')
EOF
                fi
                k3s kubectl -n kube-system rollout restart deployment coredns
                echo "CoreDNS configured with bubble resolver successfully"
                break
            fi
            sleep 2
            COREDNS_WAIT=$((COREDNS_WAIT + 2))
        done
    ) &
fi

# Wait for kubeconfig and server-ca, ensuring server-ca matches kubeconfig's cluster CA
echo "Waiting for /var/lib/skiff/kubeconfig and matching server-ca..."
CA_WAIT=0
CA_TIMEOUT=120
while [ "$CA_WAIT" -lt "$CA_TIMEOUT" ]; do
  if [ -f /var/lib/skiff/kubeconfig ] && [ -f /var/lib/skiff/tls/server-ca.crt ]; then
    KUBECONFIG_CA_HASH=$(grep 'certificate-authority-data:' /var/lib/skiff/kubeconfig 2>/dev/null | awk '{print $2}' | base64 -d 2>/dev/null | sha256sum | awk '{print $1}')
    SERVER_CA_HASH=$(sha256sum /var/lib/skiff/tls/server-ca.crt 2>/dev/null | awk '{print $1}')
    if [ -n "$KUBECONFIG_CA_HASH" ] && [ -n "$SERVER_CA_HASH" ] && [ "$KUBECONFIG_CA_HASH" = "$SERVER_CA_HASH" ]; then
      break
    fi
  fi
  sleep 1
  CA_WAIT=$((CA_WAIT + 1))
done

if [ "$CA_WAIT" -ge "$CA_TIMEOUT" ]; then
  echo "ERROR: Timed out waiting for /var/lib/skiff/kubeconfig and matching server-ca after ${CA_TIMEOUT}s" >&2
  exit 1
fi

# Generate per-node webhook certificate for skifflet with node IP SAN via CSR signed by controller
NODE_NAME="$(hostname)"
NODE_CERT_DIR="/var/lib/skiff/.certs/${NODE_NAME}"
mkdir -p "${NODE_CERT_DIR}"

cat > "${NODE_CERT_DIR}/kubelet.cnf" <<EOF
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name

[req_distinguished_name]

[v3_req]
basicConstraints = CA:FALSE
keyUsage = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth, clientAuth
subjectAltName = @alt_names

[alt_names]
IP.1 = 127.0.0.1
IP.2 = ${HOST_IP}
EOF

if [ ! -f "${NODE_CERT_DIR}/kubelet.key" ]; then
    openssl genrsa -out "${NODE_CERT_DIR}/kubelet.key.tmp" 2048
    chmod 600 "${NODE_CERT_DIR}/kubelet.key.tmp"
    mv "${NODE_CERT_DIR}/kubelet.key.tmp" "${NODE_CERT_DIR}/kubelet.key"
fi
chmod 600 "${NODE_CERT_DIR}/kubelet.key"

rm -f "${NODE_CERT_DIR}/kubelet.crt"
openssl req -new -key "${NODE_CERT_DIR}/kubelet.key" -subj "/CN=skifflet" \
  -out "${NODE_CERT_DIR}/kubelet.csr.tmp" -config "${NODE_CERT_DIR}/kubelet.cnf"
mv "${NODE_CERT_DIR}/kubelet.csr.tmp" "${NODE_CERT_DIR}/kubelet.csr"

echo "Waiting for controller to sign kubelet certificate for ${NODE_NAME}..."
CSR_WAIT=0
CSR_TIMEOUT=120
while [ ! -f "${NODE_CERT_DIR}/kubelet.crt" ]; do
    if [ "$CSR_WAIT" -ge "$CSR_TIMEOUT" ]; then
        echo "ERROR: Timed out waiting for controller to sign kubelet certificate for ${NODE_NAME}" >&2
        exit 1
    fi
    sleep 1
    CSR_WAIT=$((CSR_WAIT + 1))
done
chmod 600 "${NODE_CERT_DIR}/kubelet.crt"

# Wait for kube-dns service (Controller creates it via K3s, Nodes wait for it)
echo "Waiting for kube-dns service..."
export KUBECONFIG=/var/lib/skiff/kubeconfig
DNS_WAIT=0
DNS_TIMEOUT=120
while ! k3s kubectl get service -n kube-system kube-dns >/dev/null 2>&1; do
    if [ "$DNS_WAIT" -ge "$DNS_TIMEOUT" ]; then
        echo "ERROR: Timed out waiting for kube-dns service after ${DNS_TIMEOUT}s" >&2
        exit 1
    fi
    sleep 1
    DNS_WAIT=$((DNS_WAIT + 1))
done

# The addresses of the API and DNS Services were derived from the Service network
# before anything started; the cluster is the authority, so check what it created.
for service in "default kubernetes $SKIFF_API_SERVICE_IP" "kube-system kube-dns $SKIFF_CLUSTER_DNS"; do
    read -r service_ns service_name service_want <<< "$service"
    service_got="$(k3s kubectl get service -n "$service_ns" "$service_name" -o jsonpath='{.spec.clusterIP}')"
    if [ "$service_got" != "$service_want" ]; then
        echo "ERROR: Service $service_ns/$service_name has address $service_got, but the configuration says $service_want" >&2
        exit 1
    fi
done
echo "Service addresses verified: API ${SKIFF_API_SERVICE_IP}, DNS ${SKIFF_CLUSTER_DNS}"

start_skifflet() {
    echo "Starting skifflet..."
    PAUSE_IMAGE_OPT=()
    if [ -n "${PAUSE_IMAGE:-}" ]; then
        PAUSE_IMAGE_OPT=(--pause-image="${PAUSE_IMAGE}")
    fi

    KUBECONFIG=/var/lib/skiff/kubeconfig \
    APISERVER_KEY_LOCATION="${NODE_CERT_DIR}/kubelet.key" \
    APISERVER_CERT_LOCATION="${NODE_CERT_DIR}/kubelet.crt" \
    VKUBELET_ADDRESS="${HOST_IP}" \
    skifflet \
      --apptainer=plaidtainer \
      --nodename="$(hostname)" \
      --disable-taint=true \
      --cluster-domain="$SKIFF_CLUSTER_DOMAIN" \
      ${SKIFF_PODS_DIR:+--pods-dir="$SKIFF_PODS_DIR"} \
      "${PAUSE_IMAGE_OPT[@]}" \
      >> /var/log/skifflet.log 2>&1 &
    SKIFFLET_PID=$!
}
start_skifflet

start_plaidd() {
    echo "Starting plaidd..."
    mkdir -p /run/plaid
    plaidd \
      --kubeconfig=/var/lib/skiff/kubeconfig \
      --node-name="$(hostname)" \
      --cluster-cidr="$SKIFF_POD_CIDR" \
      --service-cidr="$SKIFF_SERVICE_CIDR" \
      --gateway-mode=tap \
      --uplink="$PLAID_UPLINK" \
      --uplink-address="$PLAID_UPLINK_ADDRESS" \
      --vxlan-bind="$HOST_IP" \
      --overlay-handoff="$SKIFF_OVERLAY_HANDOFF" \
      --resolver="$SKIFF_CLUSTER_DNS" \
      --mtu="$SKIFF_POD_MTU" \
      >> /var/log/plaidd.log 2>&1 &
    PLAIDD_PID=$!
}
start_plaidd

# skifflet registers the Node first; Plaid then receives its /24 and creates plaid0.
PLAID_WAIT=0
while ! plaidctl status >/dev/null 2>&1; do
    if ! kill -0 "$PLAIDD_PID" 2>/dev/null || [ "$PLAID_WAIT" -ge 180 ]; then
        echo "ERROR: Plaid did not become ready; see /var/log/plaidd.log" >&2
        exit 1
    fi
    sleep 1
    PLAID_WAIT=$((PLAID_WAIT + 1))
done

start_kube_proxy() {
    echo "Starting kube-proxy..."
    kube-proxy \
      --kubeconfig /var/lib/skiff/kubeconfig \
      --proxy-mode iptables \
      --cluster-cidr "$SKIFF_POD_CIDR" \
      --masquerade-all=true \
      --hostname-override "$(hostname)" \
      --conntrack-max-per-core=0 \
      --conntrack-tcp-timeout-established=0 \
      --conntrack-tcp-timeout-close-wait=0 \
      >> /var/log/kube-proxy.log 2>&1 &
    KUBE_PROXY_PID=$!
}
start_kube_proxy

# Both the API Service and cluster DNS must work before the bubble reports
# startup success. Neither check is needed for the direct API bootstrap above.
echo "Waiting for Kubernetes API Service and cluster DNS..."
NETWORK_WAIT=0
while true; do
    API_CODE="$(curl -ksS --connect-timeout 2 --max-time 5 -o /dev/null -w '%{http_code}' "https://${SKIFF_API_SERVICE_IP}/version" 2>/dev/null || true)"
    DNS_ANSWER="$(timeout 3 drill -Q "@${SKIFF_CLUSTER_DNS}" "kubernetes.default.svc.${SKIFF_CLUSTER_DOMAIN}" A 2>/dev/null | awk '{print $NF}' || true)"
    if [ "$API_CODE" != "000" ] && [ -n "$API_CODE" ] && printf '%s\n' "$DNS_ANSWER" | grep -qx "$SKIFF_API_SERVICE_IP"; then
        echo "Kubernetes API Service and cluster DNS are ready"
        break
    fi
    if ! kill -0 "$PLAIDD_PID" 2>/dev/null || ! kill -0 "$KUBE_PROXY_PID" 2>/dev/null || [ "$NETWORK_WAIT" -ge 180 ]; then
        echo "ERROR: API Service or cluster DNS did not become ready (API=$API_CODE, DNS=$DNS_ANSWER)" >&2
        exit 1
    fi
    sleep 2
    NETWORK_WAIT=$((NETWORK_WAIT + 2))
done

shutdown_daemons() {
    trap - EXIT INT TERM
    echo "Terminating cluster daemons..."
    kill -TERM "$SKIFFLET_PID" "$PLAIDD_PID" "$KUBE_PROXY_PID" 2>/dev/null || true
    if [ -n "$K3S_PID" ]; then
        kill -TERM "$K3S_PID" 2>/dev/null || true
    fi
    if [ -n "$CSR_SIGN_PID" ]; then
        kill -TERM "$CSR_SIGN_PID" 2>/dev/null || true
    fi
    sleep 2
    kill -KILL "$SKIFFLET_PID" "$PLAIDD_PID" "$KUBE_PROXY_PID" 2>/dev/null || true
    if [ -n "$K3S_PID" ]; then
        kill -KILL "$K3S_PID" 2>/dev/null || true
    fi
    if [ -n "$CSR_SIGN_PID" ]; then
        kill -KILL "$CSR_SIGN_PID" 2>/dev/null || true
    fi
    wait "$SKIFFLET_PID" "$PLAIDD_PID" "$KUBE_PROXY_PID" 2>/dev/null || true
    if [ -n "$K3S_PID" ]; then
        wait "$K3S_PID" 2>/dev/null || true
    fi
    if [ -n "$CSR_SIGN_PID" ]; then
        wait "$CSR_SIGN_PID" 2>/dev/null || true
    fi
    kill -TERM 1 2>/dev/null || true
    kill -KILL 1 2>/dev/null || true
}
trap shutdown_daemons EXIT INT TERM

# If explicit arguments were passed, run them; otherwise run daemon supervisor loop
if [ "$#" -gt 0 ]; then
    exec "$@"
fi

echo "All daemons started successfully. Supervising cluster processes..."
while true; do
    if [ "$SKIFF_ROLE" = "controller" ] && [ -n "$K3S_PID" ]; then
        if ! kill -0 "$K3S_PID" 2>/dev/null; then
            echo "FATAL: k3s server (PID $K3S_PID) exited unexpectedly!" >&2
            exit 1
        fi
    fi
    if ! kill -0 "$SKIFFLET_PID" 2>/dev/null; then
        echo "FATAL: skifflet (PID $SKIFFLET_PID) exited unexpectedly!" >&2
        exit 1
    fi
    if ! kill -0 "$PLAIDD_PID" 2>/dev/null; then
        echo "FATAL: plaidd (PID $PLAIDD_PID) exited unexpectedly!" >&2
        exit 1
    fi
    if ! kill -0 "$KUBE_PROXY_PID" 2>/dev/null; then
        echo "FATAL: kube-proxy (PID $KUBE_PROXY_PID) exited unexpectedly!" >&2
        exit 1
    fi
    sleep 2
done
