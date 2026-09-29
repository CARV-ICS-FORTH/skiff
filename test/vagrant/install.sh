#!/bin/bash
export DEBIAN_FRONTEND=noninteractive

ROLE=$(cat /etc/vagrant_role)
echo "Provisioning as $ROLE..."

# 0. Enlarge LVM volume to use all available partition space
if [ -b /dev/mapper/ubuntu--vg-ubuntu--lv ]; then
    echo "Enlarging LVM root partition to use 100% of free Volume Group space..."
    lvextend -r -l +100%FREE /dev/mapper/ubuntu--vg-ubuntu--lv || true
fi

# 1. Disable Security
echo "Disabling security features..."
systemctl stop ufw
systemctl disable ufw
# AppArmor
systemctl stop apparmor
systemctl disable apparmor
# SELinux is not installed by default on Ubuntu, but just in case
if command -v setenforce &> /dev/null; then
    setenforce 0
    sed -i 's/^SELINUX=.*/SELINUX=disabled/' /etc/selinux/config
fi

# Fix /etc/hosts (Remove 127.0.x.x entries that confuse Slurm)
sed -i '/127.0.1.1/d' /etc/hosts
sed -i '/127.0.2.1/d' /etc/hosts

# Disable AppArmor (Kernel parameter required)
sed -i 's/GRUB_CMDLINE_LINUX_DEFAULT="/GRUB_CMDLINE_LINUX_DEFAULT="apparmor=0 /' /etc/default/grub
update-grub

# Fix Routes: Persistent Fix to remove default route on 10.0.2.x (NAT)
# so that 192.168.64.x is preferred.
cat <<EOF > /usr/local/bin/fix-routes.sh
#!/bin/bash
# Find interface with 10.0.2.x default route
default_dev_nat=\$(ip route | grep "default via" | grep "10.0.2" | awk '{print \$5}' | head -n1)
if [ -n "\$default_dev_nat" ]; then
    echo "Removing default route for \$default_dev_nat (NAT)..."
    ip route del default dev \$default_dev_nat
fi
EOF
chmod +x /usr/local/bin/fix-routes.sh

cat <<EOF > /etc/systemd/system/fix-routes.service
[Unit]
Description=Fix Routing for Cluster IP Priority
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/fix-routes.sh
RemainAfterExit=true

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now fix-routes.service

# 2. Update System & Install Basics
# Fix slow SSH
echo "UseDNS no" >> /etc/ssh/sshd_config
# Fix slow apt (ipv6)
echo 'Acquire::ForceIPv4 "true";' > /etc/apt/apt.conf.d/99force-ipv4
systemctl restart ssh

# Allow unprivileged users to use ICMP sockets
echo "net.ipv4.ping_group_range = 0 2147483647" > /etc/sysctl.d/99-ping.conf
sysctl -p /etc/sysctl.d/99-ping.conf

apt-get update
apt-get install -y software-properties-common
add-apt-repository -y ppa:apptainer/ppa
apt-get update
apt-get install -y git slurm-wlm munge nfs-common avahi-daemon libnss-mdns apptainer slirp4netns socat

# kubectl for host-level access to the cluster (the same release as the bubble's Kubernetes)
KUBECTL_VERSION=v1.35.0
KUBECTL_ARCH=$(dpkg --print-architecture)
curl -fsSLo /usr/local/bin/kubectl "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${KUBECTL_ARCH}/kubectl"
echo "$(curl -fsSL "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${KUBECTL_ARCH}/kubectl.sha256")  /usr/local/bin/kubectl" | sha256sum -c -
chmod +x /usr/local/bin/kubectl

# Enable mDNS
# Configure Avahi to only listen on eth0 (Cluster Network) to avoid checking out 10.0.2.15 (NAT)
sed -i 's/#allow-interfaces=eth0/allow-interfaces=eth0/' /etc/avahi/avahi-daemon.conf
if ! grep -q "allow-interfaces=eth0" /etc/avahi/avahi-daemon.conf; then
    # If it wasn't commented out, append it to [server] section (simple hack)
    # or just replace the line if it exists
    echo "allow-interfaces=eth0" >> /etc/avahi/avahi-daemon.conf
fi

systemctl enable avahi-daemon
systemctl start avahi-daemon
systemctl restart avahi-daemon

# Time Sync (Crucial for Munge)
echo "Setting up Time Sync..."
apt-get install -y chrony
systemctl restart chrony

# 3. NFS Setup
if [ "$ROLE" == "controller" ]; then
    echo "Setting up NFS Server..."
    apt-get install -y nfs-kernel-server
    mkdir -p /home
    echo "/home *(rw,sync,no_root_squash,no_subtree_check)" >> /etc/exports
    exportfs -a
    systemctl restart nfs-kernel-server
else
    echo "Setting up NFS Client..."
    echo "Waiting for controller.local..."
    until ping -c1 controller.local &>/dev/null; do :; done
    mount controller.local:/home /home
    echo "controller.local:/home /home nfs defaults 0 0" >> /etc/fstab
fi

# 4. Munge Setup (Shared Key)
echo "Setting up Munge..."
echo -n "skiff-test-munge-key-1234567890123456" > /etc/munge/munge.key
chown munge:munge /etc/munge/munge.key
chmod 400 /etc/munge/munge.key
systemctl restart munge

# 5. Slurm Setup
echo "Setting up Slurm..."
# Create configuration
cat <<EOF > /etc/slurm/slurm.conf
ClusterName=skiff
SlurmctldHost=controller(controller.local)
AuthType=auth/munge
ProctrackType=proctrack/linuxproc
ReturnToService=2
SlurmctldPidFile=/var/run/slurm/slurmctld.pid
SlurmctldPort=6817
SlurmdPidFile=/var/run/slurm/slurmd.pid
SlurmdPort=6818
SlurmdSpoolDir=/var/lib/slurm/slurmd
SlurmUser=slurm
StateSaveLocation=/var/lib/slurm/slurmctld
SwitchType=switch/none
TaskPlugin=task/none
# TIMERS
InactiveLimit=0
MinJobAge=300
KillWait=30
Waittime=0
# NODES
NodeName=controller NodeAddr=controller.local CPUs=2 RealMemory=1500 State=UNKNOWN
NodeName=node       NodeAddr=node.local       CPUs=2 RealMemory=1500 State=UNKNOWN
# PARTITIONS
PartitionName=debug Nodes=ALL Default=YES MaxTime=INFINITE State=UP
EOF

mkdir -p /var/lib/slurm/slurmd /var/lib/slurm/slurmctld /var/run/slurm
chown -R slurm:slurm /var/lib/slurm /var/run/slurm

# Enable Services
if [ "$ROLE" == "controller" ]; then
    systemctl enable slurmctld
    systemctl start slurmctld
    systemctl enable slurmd
    systemctl start slurmd
else
    systemctl enable slurmd
    systemctl start slurmd
fi

# Add dynamic host resolution service for Slurm
cat <<EOF > /usr/local/bin/resolve-hosts.sh
#!/bin/bash

# Wait for avahi-daemon to be active
while [ "\$(systemctl is-active avahi-daemon)" != "active" ]; do
    sleep 1
done

resolve_ip() {
    local host="\$1"
    # Try getent hosts first
    local ip=\$(getent hosts "\$host" | awk '{print \$1}' | grep -v '^10.0.2.' | grep -v '^127.' | grep -v ':' | head -n1)
    if [ -n "\$ip" ]; then
        echo "\$ip"
        return 0
    fi
    # Fall back to nslookup
    ip=\$(nslookup "\$host" 2>/dev/null | awk '/Address:/ {print \$2}' | grep -v '^10.0.2.' | grep -v '^127.' | grep -v ':' | head -n1)
    if [ -n "\$ip" ]; then
        echo "\$ip"
        return 0
    fi
    return 1
}

# Loop indefinitely until both hosts are resolved
while true; do
    CONTROLLER_IP=\$(resolve_ip controller.local)
    NODE_IP=\$(resolve_ip node.local)
    
    if [ -n "\$CONTROLLER_IP" ] && [ -n "\$NODE_IP" ]; then
        # Remove existing entries
        sed -i -E '/^\S+\s+(controller|node)(\s|\.|$)/d' /etc/hosts

        # Write updated entries
        echo "\$CONTROLLER_IP controller controller.local" >> /etc/hosts
        echo "\$NODE_IP node node.local" >> /etc/hosts

        # Restart Slurm to pick up changes
        systemctl restart slurmctld slurmd 2>/dev/null || true
        break
    fi
    sleep 5
done
EOF
chmod +x /usr/local/bin/resolve-hosts.sh

cat <<EOF > /etc/systemd/system/resolve-hosts.service
[Unit]
Description=Resolve Slurm Hostnames in /etc/hosts
After=avahi-daemon.service
Wants=avahi-daemon.service

[Service]
Type=simple
ExecStart=/usr/local/bin/resolve-hosts.sh

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable --now resolve-hosts.service

echo "Provisioning complete."
