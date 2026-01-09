# Cluster-OS Node Container
# Ubuntu-based container with systemd support for testing

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# Install systemd and required packages
RUN apt-get update && apt-get install -y \
    systemd \
    systemd-sysv \
    dbus \
    iproute2 \
    iputils-ping \
    curl \
    wget \
    ca-certificates \
    wireguard-tools \
    iptables \
    net-tools \
    sudo \
    vim \
    less \
    slurm-wlm \
    slurmd \
    slurmctld \
    munge \
    python3 \
    python3-pip \
    build-essential \
    fuse-overlayfs \
    openmpi-bin \
    libopenmpi-dev \
    mpich \
    libmpich-dev \
    libpmix-dev \
    libpmix2 \
    python3-mpi4py \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Install k3s binary (pre-installed for production readiness)
RUN curl -sfL https://get.k3s.io | INSTALL_K3S_SKIP_START=true \
    INSTALL_K3S_SKIP_ENABLE=true \
    INSTALL_K3S_EXEC="--snapshotter=fuse-overlayfs" sh -

# Install kubectl for easier k8s management
RUN curl -LO "https://dl.k8s.io/release/$(curl -L -s https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl" && \
    chmod +x kubectl && \
    mv kubectl /usr/local/bin/

# Configure systemd for container environment
RUN systemctl mask systemd-udevd.service \
    systemd-udevd-control.socket \
    systemd-udevd-kernel.socket \
    systemd-modules-load.service \
    sys-kernel-config.mount \
    sys-kernel-debug.mount \
    sys-kernel-tracing.mount

# Configure D-Bus for SLURM integration
RUN mkdir -p /var/run/dbus && \
    dbus-uuidgen > /etc/machine-id && \
    dbus-uuidgen > /var/lib/dbus/machine-id

# Create cluster-os directories
RUN mkdir -p /var/lib/cluster-os \
    /etc/cluster-os \
    /var/log/cluster-os \
    /usr/local/bin

# Copy node-agent binary
COPY bin/node-agent /usr/local/bin/node-agent
RUN chmod +x /usr/local/bin/node-agent

# Copy default configuration
COPY node/config/node.yaml /etc/cluster-os/node.yaml

# Copy systemd service file
COPY node/systemd/node-agent.service /etc/systemd/system/node-agent.service

# Copy entrypoint script
COPY test/docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh && echo "Entrypoint script copied"

# Enable node-agent service
RUN systemctl enable node-agent.service

# Expose ports
# 7946: Serf gossip (TCP/UDP)
# 7373: Raft consensus
# 51820: WireGuard VPN
EXPOSE 7946/tcp 7946/udp 7373/tcp 51820/udp

# Set entrypoint to run node-agent directly
STOPSIGNAL SIGTERM
ENTRYPOINT ["/entrypoint.sh"]
CMD ["/usr/local/bin/node-agent", "start", "--foreground"]

# Labels
LABEL org.cluster-os.version="dev" \
      org.cluster-os.component="node-agent" \
      org.cluster-os.description="Cluster-OS Node Container with systemd"
