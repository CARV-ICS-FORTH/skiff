#!/usr/bin/env bash
# test/e2e/lib/standalone.sh: the standalone Plaid suites (NET-1, NET-2).
#
# Plaid is also used on its own, outside a cluster: plaidd as a daemon on the host, the
# CNI plugin, and plaidtainer for rootless Apptainer containers. That is installed on
# the host (/usr/local/bin, /opt/cni/bin, /run/plaid). These helpers prepare it for a
# test the way a user would, from the candidate under test: the binaries are taken out of
# the very SIF image that was uploaded for the run (the one the cluster runs), recorded
# with their checksums in the result bundle, and removed again afterwards. Nothing else
# on the hosts is changed, and no binary is copied from anywhere else.
#
# Source after common.sh and runtime.sh.

STANDALONE_BINS="plaidd plaid plaidctl plaidtainer"
STANDALONE_BUBBLE_SIF="${HOME}/.skiff/images/skiff-bubble.sif"
# The overlay of the standalone daemons: not the cluster's port, which a running cluster holds.
STANDALONE_VXLAN_PORT=8473
# Node networks of the standalone daemons (the first node's and the second's).
STANDALONE_CIDR_A="10.244.1.0/24"
STANDALONE_CIDR_B="10.244.2.0/24"
STANDALONE_STATE="${E2E_LOGDIR:-/tmp}/standalone"
# Where each node keeps the daemon's PID and log: the same place in every run, so a daemon
# left behind by an aborted run is found and stopped by the next one.
STANDALONE_NODE_DIR="/tmp/skiff-e2e-standalone"

# standalone_install NODE_IP: put the candidate's Plaid on a node, from the uploaded image.
standalone_install() {
    local ip="$1" sif="$STANDALONE_BUBBLE_SIF"
    runtime_ssh_node "$ip" "set -e
        test -f '$sif'
        tmp=\$(mktemp -d)
        for b in $STANDALONE_BINS; do apptainer exec '$sif' cat /usr/local/bin/\$b > \$tmp/\$b 2>/dev/null; done
        apptainer exec '$sif' cat /opt/cni/bin/host-local > \$tmp/host-local 2>/dev/null
        for b in $STANDALONE_BINS; do sudo install -m 0755 \$tmp/\$b /usr/local/bin/\$b; done
        sudo install -d -m 0755 /opt/cni/bin
        sudo install -m 0755 \$tmp/host-local /opt/cni/bin/host-local
        sudo ln -sf /usr/local/bin/plaid /opt/cni/bin/plaid
        sudo install -d -m 0777 /run/plaid
        (cd \$tmp && sha256sum $STANDALONE_BINS host-local)
        rm -rf \$tmp"
}

# standalone_remove NODE_IP: take it off again (what standalone_install put there, and
# the daemon's state).
standalone_remove() {
    local ip="$1"
    standalone_stop_plaidd "$ip"
    runtime_ssh_node "$ip" "for b in $STANDALONE_BINS; do sudo rm -f /usr/local/bin/\$b; done
        sudo rm -f /opt/cni/bin/plaid /opt/cni/bin/host-local
        sudo rm -rf /run/plaid" || true
}

# standalone_prepare IP_A IP_B: install on both nodes and check they got the same bytes.
standalone_prepare() {
    local ip sums first="" sum
    mkdir -p "$STANDALONE_STATE"
    : > "$STANDALONE_STATE/binaries.sha256"
    for ip in "$1" "$2"; do
        sums=$(standalone_install "$ip") || { e2e_fail "standalone: cannot install Plaid on $ip from $STANDALONE_BUBBLE_SIF"; return 1; }
        printf '# %s\n%s\n' "$ip" "$sums" >> "$STANDALONE_STATE/binaries.sha256"
        sum=$(printf '%s\n' "$sums" | sha256sum | cut -d' ' -f1)
        if [ -z "$first" ]; then first="$sum"; elif [ "$sum" != "$first" ]; then e2e_fail "standalone: the two nodes got different binaries"; return 1; fi
    done
    printf '# image\n%s  %s\n' "$(sha256sum "$STANDALONE_BUBBLE_SIF" | cut -d' ' -f1)" "$STANDALONE_BUBBLE_SIF" >> "$STANDALONE_STATE/binaries.sha256"
    e2e_log "standalone Plaid installed on both nodes from the uploaded image (checksums in standalone/binaries.sha256)"
}

# standalone_start_plaidd NODE_IP NODE_CIDR PEER_CIDR PEER_IP: a standalone daemon (slirp mode, no
# Kubernetes) with a route to the other node's network, as an ordinary user.
standalone_start_plaidd() {
    local ip="$1" cidr="$2" peer_cidr="$3" peer_ip="$4" log="$STANDALONE_NODE_DIR/plaidd.log"
    runtime_ssh_node "$ip" "mkdir -p '$STANDALONE_NODE_DIR'
        if [ -f '$STANDALONE_NODE_DIR/plaidd.pid' ]; then kill \$(cat '$STANDALONE_NODE_DIR/plaidd.pid') 2>/dev/null; sleep 1; fi
        rm -rf /run/plaid/*
        setsid nohup plaidd --api-socket=/run/plaid/plaidd.sock --disable-kubernetes \
            --node-cidr=$cidr --cluster-cidr=10.244.0.0/16 --vxlan-port=$STANDALONE_VXLAN_PORT \
            --routes='${peer_cidr}=${peer_ip}' --gateway-mode=slirp --slirp-host-loopback=true \
            > '$log' 2>&1 < /dev/null &
        echo \$! > '$STANDALONE_NODE_DIR/plaidd.pid'
        for i in \$(seq 1 50); do plaidctl status >/dev/null 2>&1 && exit 0; sleep 0.2; done
        tail -5 '$log'; exit 1"
}

# Only the daemon this suite started (by its PID): a running cluster's plaidd is a process of
# the same name, visible on the host too, and must not be touched.
standalone_stop_plaidd() {
    runtime_ssh_node "$1" "pidfile='$STANDALONE_NODE_DIR/plaidd.pid'; if [ -f \$pidfile ]; then kill \$(cat \$pidfile) 2>/dev/null; rm -f \$pidfile; sleep 1; fi; true" || true
}

# standalone_teardown IP_A IP_B: stop the daemons, leave nothing installed. The addresses are
# given, not looked up: the cluster may be gone by now.
standalone_teardown() {
    local ip
    for ip in "$1" "$2"; do standalone_remove "$ip"; done
}
