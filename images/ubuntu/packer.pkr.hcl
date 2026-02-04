packer {
  required_plugins {
    qemu = {
      version = ">= 1.0.0"
      source  = "github.com/hashicorp/qemu"
    }
  }
}

variable "iso_url" {
  type    = string
  default = "https://releases.ubuntu.com/24.04/ubuntu-24.04.3-live-server-amd64.iso"
}

variable "iso_checksum" {
  type    = string
  default = "sha256:c3514bf0056180d09376462a7a1b4f213c1d6e8ea67fae5c25099c6fd3d8274b"
}

variable "vm_name" {
  type    = string
  default = "cluster-os-node"
}

variable "disk_size" {
  type    = string
  default = "20G"
}

variable "memory" {
  type    = string
  default = "4096"
}

variable "cpus" {
  type    = string
  default = "2"
}

variable "ssh_username" {
  type    = string
  default = "clusteros"
}

variable "ssh_password" {
  type    = string
  default = "clusteros"
}

variable "cluster_key" {
  type    = string
  default = ""
}

source "qemu" "ubuntu-2404" {
  iso_url          = var.iso_url
  iso_checksum     = var.iso_checksum
  output_directory = "/data/packer-output/${var.vm_name}"
  shutdown_command = "echo '${var.ssh_password}' | sudo -S shutdown -P now"
  disk_size        = var.disk_size
  format           = "qcow2"
  
  # Use KVM hardware acceleration for much faster builds
  # KVM is available on this system (/dev/kvm found)
  # KVM builds are fast (10-20 minutes) vs TCG (1-2 hours)
  accelerator      = "kvm"

  # VM resources
  memory           = var.memory
  cpus             = var.cpus

  # Network
  net_device       = "virtio-net"
  disk_interface   = "virtio"

  # SSH settings
  ssh_username     = var.ssh_username
  ssh_password     = var.ssh_password
  ssh_timeout      = "30m"  # Timeout for KVM builds (90m was for TCG)

  # VM name
  vm_name          = "${var.vm_name}.qcow2"

  # Boot configuration - Ubuntu autoinstall
  boot_wait        = "10s"  # Boot wait for KVM acceleration
  boot_command     = [
    "<esc><wait>",
    "e<wait>",
    "<down><down><down><end>",
    " autoinstall ds=nocloud-net\\;s=http://{{ .HTTPIP }}:{{ .HTTPPort }}/",
    "<f10><wait>"
  ]

  # HTTP directory for autoinstall files
  http_directory   = "http"

  # Headless mode (set to false for debugging)
  headless         = true

  # VNC for debugging if needed
  vnc_bind_address = "0.0.0.0"
  vnc_port_min     = 5900
  vnc_port_max     = 5900
}

build {
  name = "cluster-os"

  sources = ["source.qemu.ubuntu-2404"]

  # Wait for cloud-init to complete
  provisioner "shell" {
    inline = [
      "echo 'Waiting for cloud-init to complete...'",
      "cloud-init status --wait",
      "echo 'Cloud-init complete'"
    ]
  }

  # Update system
  provisioner "shell" {
    inline = [
      "sudo apt-get update",
      "sudo DEBIAN_FRONTEND=noninteractive apt-get upgrade -y"
    ]
  }

  # Install base dependencies
  provisioner "shell" {
    inline = [
      "sudo DEBIAN_FRONTEND=noninteractive apt-get install -y \\",
      "  wireguard wireguard-tools \\",
      "  curl wget ca-certificates gnupg \\",
      "  jq munge slurm-wlm slurm-client \\",
      "  net-tools iproute2 iputils-ping \\",
      "  systemd-resolved systemd-timesyncd \\",
      "  openssh-server build-essential"
    ]
  }

  # Install k3s binary
  provisioner "shell" {
    inline = [
      "curl -sfL https://get.k3s.io | INSTALL_K3S_SKIP_START=true INSTALL_K3S_SKIP_ENABLE=true sh -",
      "sudo systemctl disable k3s || true",
      "sudo systemctl disable k3s-agent || true"
    ]
  }

  # Copy node-agent binary
  provisioner "file" {
    source      = "../../bin/node-agent"
    destination = "/tmp/node-agent"
  }

  # Install node-agent
  provisioner "shell" {
    inline = [
      "sudo mv /tmp/node-agent /usr/local/bin/node-agent",
      "sudo chmod +x /usr/local/bin/node-agent",
      "sudo mkdir -p /etc/cluster-os",
      "sudo mkdir -p /var/lib/cluster-os",
      "sudo mkdir -p /var/log/cluster-os"
    ]
  }

  # Copy systemd service file
  provisioner "file" {
    source      = "systemd/node-agent.service"
    destination = "/tmp/node-agent.service"
  }

  # Install systemd service
  provisioner "shell" {
    inline = [
      "sudo mv /tmp/node-agent.service /etc/systemd/system/node-agent.service",
      "sudo systemctl daemon-reload",
      "sudo systemctl enable node-agent.service"
    ]
  }

  # Copy cluster configuration if provided
  provisioner "shell" {
    inline = [
      "if [ -n '${var.cluster_key}' ]; then",
      "  echo '${var.cluster_key}' | sudo tee /etc/cluster-os/cluster.key",
      "  sudo chmod 600 /etc/cluster-os/cluster.key",
      "fi"
    ]
  }

  # Setup WiFi configuration
  provisioner "file" {
    source      = "netplan/01-clusteros-network.yaml"
    destination = "/tmp/01-clusteros-network.yaml"
  }

  provisioner "shell" {
    inline = [
      "sudo mv /tmp/01-clusteros-network.yaml /etc/netplan/01-clusteros-network.yaml",
      "sudo chmod 600 /etc/netplan/01-clusteros-network.yaml"
    ]
  }

  # Clean up
  provisioner "shell" {
    inline = [
      "sudo apt-get autoremove -y",
      "sudo apt-get clean",
      "sudo rm -rf /tmp/*",
      "sudo rm -rf /var/tmp/*",
      "sudo cloud-init clean --logs",
      "sudo sync"
    ]
  }

  # Create converted formats for different use cases
  post-processor "shell-local" {
    inline = [
      "echo 'Creating raw disk image for USB boot...'",
      "qemu-img convert -f qcow2 -O raw /data/packer-output/${var.vm_name}/${var.vm_name}.qcow2 /data/packer-output/${var.vm_name}/${var.vm_name}.raw",
      "echo 'Creating compressed archive...'",
      "cd /data/packer-output/${var.vm_name} && gzip -k ${var.vm_name}.raw",
      "echo 'Build complete!'",
      "ls -lh /data/packer-output/${var.vm_name}/"
    ]
  }
}
