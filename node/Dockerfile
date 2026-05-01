# Cluster-OS Node Container
# Ubuntu-based container with systemd support for testing

FROM ubuntu:24.04

ENV DEBIAN_FRONTEND=noninteractive

# Install systemd and required packages
# wireguard-tools intentionally omitted: networking is Tailscale on bare metal; not needed in container tests.
RUN apt-get update && apt-get install -y \
    systemd \
    systemd-sysv \
    dbus \
    iproute2 \
    iputils-ping \
    curl \
    wget \
    ca-certificates \
    iptables \
    net-tools \
    sudo \
    vim \
    less \
    jq \
    avahi-daemon \
    avahi-utils \
    libnss-mdns \
    slurm-wlm \
    slurm-client \
    slurmrestd \
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

# Install Tailscale
RUN curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.noarmor.gpg \
        | tee /usr/share/keyrings/tailscale-archive-keyring.gpg >/dev/null && \
    curl -fsSL https://pkgs.tailscale.com/stable/ubuntu/noble.tailscale-keyring.list \
        | tee /etc/apt/sources.list.d/tailscale.list && \
    apt-get update && apt-get install -y tailscale && \
    apt-get clean && rm -rf /var/lib/apt/lists/*

# Install k3s binary (pre-installed for production readiness)
RUN curl -sfL https://get.k3s.io | INSTALL_K3S_SKIP_START=true \
    INSTALL_K3S_SKIP_ENABLE=true \
    INSTALL_K3S_EXEC="--snapshotter=fuse-overlayfs" sh -

# k3s ships kubectl; symlink it under the standard name for convenience
RUN ln -s /usr/local/bin/k3s /usr/local/bin/kubectl

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
    /etc/clusteros \
    /var/log/cluster-os \
    /usr/local/bin

# Copy node-agent binary
COPY bin/node-agent /usr/local/bin/node-agent
RUN chmod +x /usr/local/bin/node-agent

# Copy default configuration
COPY node/config/node.yaml /etc/cluster-os/node.yaml

# Copy systemd service file
COPY node/systemd/node-agent.service /etc/systemd/system/node-agent.service

# Bake in Tailscale credentials (same flow as OS images — OAuth, never-expiring)
# patch/tailscale.env must exist at build time; patch/tailscale-auth is the auth script
COPY patch/tailscale-auth /usr/local/bin/clusteros-tailscale-init
COPY patch/tailscale.env /etc/clusteros/tailscale.env
RUN chmod +x /usr/local/bin/clusteros-tailscale-init && \
    chmod 600 /etc/clusteros/tailscale.env

# Copy entrypoint script
COPY test/docker/entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh && echo "Entrypoint script copied"

# Enable node-agent service
RUN systemctl enable node-agent.service

# Expose ports
# 7946: Serf gossip (TCP/UDP)
# 7373: Raft consensus
# Note: 51820/udp (WireGuard) no longer exposed — networking is Tailscale on bare metal.
EXPOSE 7946/tcp 7946/udp 7373/tcp

# Set entrypoint to run node-agent directly
STOPSIGNAL SIGTERM
ENTRYPOINT ["/entrypoint.sh"]
CMD ["/usr/local/bin/node-agent", "start", "--foreground"]

# Labels
LABEL org.cluster-os.version="dev" \
      org.cluster-os.component="node-agent" \
      org.cluster-os.description="Cluster-OS Node Container with systemd"
