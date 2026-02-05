# ClusterOS Image Builder
# Builds a ready-to-deploy Ubuntu 24.04 server image with ClusterOS services
#
# Usage:
#   packer init .
#   packer build .
#
# Debug mode:
#   packer build -var="headless=false" .

packer {
  required_plugins {
    qemu = {
      version = ">= 1.0.0"
      source  = "github.com/hashicorp/qemu"
    }
  }
}

# ------------------------------------------------------------------------------
# Variables
# ------------------------------------------------------------------------------

variable "vm_name" {
  type        = string
  default     = "cluster-os-node"
  description = "Name for the output VM image"
}

variable "disk_size" {
  type        = string
  default     = "8G"
  description = "Size of the VM disk (8G fits on most USB drives)"
}

variable "memory" {
  type        = number
  default     = 16384
  description = "Memory in MB for the build VM"
}

variable "cpus" {
  type        = number
  default     = 8
  description = "Number of CPUs for the build VM"
}

variable "ssh_username" {
  type        = string
  default     = "clusteros"
  description = "SSH username for the image"
}

variable "ssh_password" {
  type        = string
  default     = "clusteros"
  description = "SSH password for the image"
}

variable "headless" {
  type        = bool
  default     = true
  description = "Run build without display (set false for debugging)"
}

variable "output_dir" {
  type        = string
  default     = "/data/packer-output"
  description = "Directory for output images"
}

# Tailscale OAuth credentials (from .env)
variable "tailscale_oauth_client_id" {
  type        = string
  default     = env("TAILSCALE_OAUTH_CLIENT_ID")
  description = "Tailscale OAuth Client ID"
}

variable "tailscale_oauth_client_secret" {
  type        = string
  default     = env("TAILSCALE_OAUTH_CLIENT_SECRET")
  sensitive   = true
  description = "Tailscale OAuth Client Secret"
}

variable "tailscale_authkey" {
  type        = string
  default     = env("TAILSCALE_AUTHKEY")
  sensitive   = true
  description = "Tailscale auth key (fallback)"
}

# Cluster authentication key (from .env or auto-generated)
variable "cluster_auth_key" {
  type        = string
  default     = env("CLUSTER_AUTH_KEY")
  sensitive   = true
  description = "Cluster authentication key (base64)"
}

variable "serf_encrypt_key" {
  type        = string
  default     = env("SERF_ENCRYPT_KEY")
  sensitive   = true
  description = "Serf encryption key (base64)"
}

# Ubuntu 24.04 cloud image (pre-installed, no installer needed)
variable "iso_url" {
  type        = string
  default     = "https://cloud-images.ubuntu.com/releases/24.04/release/ubuntu-24.04-server-cloudimg-amd64.img"
  description = "Ubuntu cloud image URL"
}

variable "iso_checksum" {
  type        = string
  default     = "file:https://cloud-images.ubuntu.com/releases/24.04/release/SHA256SUMS"
  description = "Checksum for the cloud image"
}

# ------------------------------------------------------------------------------
# Source: QEMU VM from Ubuntu Cloud Image
# ------------------------------------------------------------------------------

source "qemu" "clusteros" {
  # Cloud image as base (already installed, boots directly)
  disk_image       = true
  iso_url          = var.iso_url
  iso_checksum     = var.iso_checksum
  
  # Output
  output_directory = "${var.output_dir}/${var.vm_name}"
  vm_name          = "${var.vm_name}.qcow2"
  format           = "qcow2"
  disk_size        = var.disk_size
  
  # QEMU settings
  accelerator      = "kvm"
  machine_type     = "q35"
  cpu_model        = "host"
  memory           = var.memory
  cpus             = var.cpus
  
  # Disk - use virtio for performance
  disk_interface   = "virtio"
  disk_compression = true
  
  # Network
  net_device       = "virtio-net"
  
  # Cloud-init via CD-ROM (standard for cloud images)
  cd_files         = ["cloud-init/*"]
  cd_label         = "cidata"
  
  # SSH - cloud-init sets this up
  ssh_username     = var.ssh_username
  ssh_password     = var.ssh_password
  ssh_timeout      = "10m"
  
  # Shutdown
  shutdown_command = "echo '${var.ssh_password}' | sudo -S shutdown -P now"
  
  # Display
  headless         = var.headless
  vnc_bind_address = "0.0.0.0"
  vnc_port_min     = 5900
  vnc_port_max     = 5999
  
  # Serial console for debugging
  qemuargs = [
    ["-serial", "mon:stdio"]
  ]
}

# ------------------------------------------------------------------------------
# Build
# ------------------------------------------------------------------------------

build {
  name    = "clusteros"
  sources = ["source.qemu.clusteros"]

  # Wait for cloud-init to finish
  provisioner "shell" {
    inline = [
      "echo 'Waiting for cloud-init...'",
      "sudo cloud-init status --wait",
      "echo 'Cloud-init complete'"
    ]
  }

  # Copy files to VM
  provisioner "file" {
    source      = "../../bin/node-agent"
    destination = "/tmp/node-agent"
  }

  # Copy the remote installer script (becomes cluster-os-install)
  provisioner "file" {
    source      = "../../remote-install/remote-node-installer.sh"
    destination = "/tmp/cluster-os-install"
  }

  # Create target directory and copy files
  provisioner "shell" {
    inline = ["mkdir -p /tmp/clusteros-files"]
  }

  provisioner "file" {
    source      = "files/config"
    destination = "/tmp/clusteros-files"
  }

  provisioner "file" {
    source      = "files/systemd"
    destination = "/tmp/clusteros-files"
  }

  provisioner "file" {
    source      = "files/netplan"
    destination = "/tmp/clusteros-files"
  }

  provisioner "file" {
    source      = "files/motd"
    destination = "/tmp/clusteros-files"
  }

  provisioner "file" {
    source      = "files/bin"
    destination = "/tmp/clusteros-files"
  }

  provisioner "file" {
    source      = "files/tailscale"
    destination = "/tmp/clusteros-files"
  }

  # Generate and inject cluster credentials during build
  provisioner "shell" {
    inline = [
      "echo 'Configuring cluster credentials...'",
      "mkdir -p /tmp/clusteros-files/secrets",
      
      # Generate cluster auth key if not provided
      "if [ -z '${var.cluster_auth_key}' ]; then",
      "  echo 'Generating new cluster authentication key...'",
      "  CLUSTER_KEY=$(openssl rand -base64 32)",
      "else",
      "  CLUSTER_KEY='${var.cluster_auth_key}'",
      "fi",
      "echo \"$CLUSTER_KEY\" > /tmp/clusteros-files/secrets/cluster.key",
      
      # Use same key for Serf encryption if not provided
      "if [ -z '${var.serf_encrypt_key}' ]; then",
      "  SERF_KEY=\"$CLUSTER_KEY\"",
      "else",
      "  SERF_KEY='${var.serf_encrypt_key}'",
      "fi",
      "echo \"$SERF_KEY\" > /tmp/clusteros-files/secrets/serf.key",
      
      # Create Tailscale env file with OAuth credentials
      "cat > /tmp/clusteros-files/tailscale/tailscale.env << 'EOF'",
      "# Tailscale Configuration for ClusterOS",
      "# Generated during image build",
      "",
      "TAILSCALE_OAUTH_CLIENT_ID=${var.tailscale_oauth_client_id}",
      "TAILSCALE_OAUTH_CLIENT_SECRET=${var.tailscale_oauth_client_secret}",
      "TAILSCALE_AUTHKEY=${var.tailscale_authkey}",
      "TAILSCALE_TAGS=[\"tag:cluster-node\"]",
      "EOF",
      
      "echo 'Credentials configured'"
    ]
  }

  # Run provisioning script
  provisioner "shell" {
    script = "provision.sh"
    environment_vars = [
      "DEBIAN_FRONTEND=noninteractive"
    ]
  }

  # Cleanup
  provisioner "shell" {
    inline = [
      "sudo apt-get autoremove -y",
      "sudo apt-get clean",
      "sudo rm -rf /tmp/* /var/tmp/*",
      "sudo cloud-init clean --logs --seed",
      "sudo rm -f /etc/machine-id",
      "sudo truncate -s 0 /etc/machine-id",
      "sudo sync"
    ]
  }

  # Create additional output formats
  post-processor "shell-local" {
    inline = [
      "echo '=== Build Complete ==='",
      "echo 'Creating raw image for USB/bare-metal...'",
      "qemu-img convert -f qcow2 -O raw '${var.output_dir}/${var.vm_name}/${var.vm_name}.qcow2' '${var.output_dir}/${var.vm_name}/${var.vm_name}.raw'",
      "echo 'Compressing...'",
      "gzip -kf '${var.output_dir}/${var.vm_name}/${var.vm_name}.raw'",
      "echo ''",
      "echo 'Output files:'",
      "ls -lh '${var.output_dir}/${var.vm_name}/'",
      "echo ''",
      "echo 'To test with QEMU:'",
      "echo \"  qemu-system-x86_64 -enable-kvm -m 2048 -hda '${var.output_dir}/${var.vm_name}/${var.vm_name}.qcow2'\""
    ]
  }
}
