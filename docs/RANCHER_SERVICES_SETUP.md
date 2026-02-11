# Rancher & Services on ClusterOS via Tailscale

> **New clusters**: Longhorn, cert-manager, and Rancher are auto-deployed by `node-agent` when the K3s leader starts. You shouldn't need to install them manually.
>
> **Existing clusters**: Run `sudo cluster-setup-services` on any K3s server node to deploy these services.

This guide covers exposing your ClusterOS K3s cluster through Tailscale — including installing Rancher for a management UI and making services accessible without public IPs or port forwarding.

## Prerequisites

- ClusterOS cluster running with K3s active (`cluster-test k3s` passes)
- Tailscale connected on all nodes (`tailscale status` shows peers)
- `sudo` access on at least one K3s server node

---

## 1. Access K3s from Your Laptop

Since all K3s nodes use Tailscale IPs, you can access the cluster from any machine on the same Tailnet.

### Copy the kubeconfig

On a K3s **server** node:

```bash
# Get the kubeconfig
sudo cat /etc/rancher/k3s/k3s.yaml
```

On your **laptop** (with `kubectl` installed):

```bash
# Get the server node's Tailscale IP
TAILSCALE_IP=100.x.x.x   # replace with actual

# Copy and patch the kubeconfig
ssh clusteros@$TAILSCALE_IP 'sudo cat /etc/rancher/k3s/k3s.yaml' > ~/.kube/clusteros.yaml

# Replace 127.0.0.1 with the Tailscale IP
sed -i "s|127.0.0.1|$TAILSCALE_IP|g" ~/.kube/clusteros.yaml

# Use it
export KUBECONFIG=~/.kube/clusteros.yaml
kubectl get nodes
```

This works because Tailscale already provides encrypted, authenticated connectivity — no Ingress or LoadBalancer needed.

---

## 2. Install Rancher

Rancher gives you a web UI for managing K3s workloads, deploying Helm charts, monitoring, and multi-cluster management.

### Install cert-manager (required by Rancher)

```bash
# On a K3s server node, or from your laptop with KUBECONFIG set:
sudo k3s kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.14.4/cert-manager.yaml

# Wait for cert-manager to be ready
sudo k3s kubectl -n cert-manager rollout status deployment/cert-manager --timeout=120s
sudo k3s kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=120s
```

### Install Rancher via Helm

```bash
# Install Helm if not present
curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash

# Add the Rancher Helm repo
helm repo add rancher-stable https://releases.rancher.com/server-charts/stable
helm repo update

# Get this node's Tailscale IP for the hostname
RANCHER_HOST=$(tailscale ip -4)

# Install Rancher
helm install rancher rancher-stable/rancher \
  --namespace cattle-system --create-namespace \
  --set hostname=$RANCHER_HOST \
  --set bootstrapPassword=admin \
  --set ingress.tls.source=rancher \
  --set replicas=1
```

### Wait for Rancher to start

```bash
sudo k3s kubectl -n cattle-system rollout status deployment/rancher --timeout=300s
```

### Access Rancher UI

Rancher deploys with an Ingress on port 443. Since K3s has no LoadBalancer by default (ServiceLB and Traefik are disabled in ClusterOS), expose it via NodePort:

```bash
# Patch the Rancher ingress to also create a NodePort service
sudo k3s kubectl -n cattle-system expose deployment rancher \
  --type=NodePort --port=443 --target-port=443 --name=rancher-nodeport \
  --dry-run=client -o yaml | sudo k3s kubectl apply -f -

# Get the assigned port
NODE_PORT=$(sudo k3s kubectl -n cattle-system get svc rancher-nodeport \
  -o jsonpath='{.spec.ports[0].nodePort}')

echo "Rancher UI: https://$(tailscale ip -4):$NODE_PORT"
```

Open that URL from any machine on your Tailnet. Log in with the bootstrap password (`admin`) and set a new password.

---

## 3. Expose Services via Tailscale Funnel

[Tailscale Funnel](https://tailscale.com/kb/1223/funnel) lets you expose a service to the **public internet** through Tailscale's infrastructure — no port forwarding, no static IP, no DNS setup needed.

[Tailscale Serve](https://tailscale.com/kb/1242/tailscale-serve) does the same but only within your Tailnet (private).

### Enable HTTPS & Funnel in your Tailnet

1. Go to [Tailscale Admin → DNS](https://login.tailscale.com/admin/dns)
2. Enable **MagicDNS** if not already on
3. Enable **HTTPS Certificates**
4. Go to [Tailscale Admin → ACLs](https://login.tailscale.com/admin/acls) and add Funnel policy:

```json
{
  "nodeAttrs": [
    {
      "target": ["tag:clusteros"],
      "attr": ["funnel"]
    }
  ]
}
```

### Expose a K3s service with Tailscale Serve (private, Tailnet only)

```bash
# Example: expose the Rancher UI (running on NodePort) within your Tailnet
sudo tailscale serve https / http://localhost:$NODE_PORT

# Your service is now available at:
# https://<hostname>.<tailnet-name>.ts.net/
# Accessible from any device on your Tailnet
```

### Expose a K3s service with Tailscale Funnel (public internet)

```bash
# Make it publicly accessible (no auth required to reach)
sudo tailscale funnel https / http://localhost:$NODE_PORT

# Your service is now publicly available at:
# https://<hostname>.<tailnet-name>.ts.net/
# Accessible from anywhere on the internet
```

### Expose multiple services on different paths

```bash
# Rancher on /
sudo tailscale serve https / http://localhost:$NODE_PORT

# A web app on /app
sudo tailscale serve https /app http://localhost:8080

# Check current config
sudo tailscale serve status
```

### Stop serving

```bash
sudo tailscale serve reset
```

---

## 4. Deploy Your Own Service (End-to-End Example)

Here's a complete example deploying a web app and making it accessible.

### Deploy to K3s

```bash
# Create namespace
sudo k3s kubectl create namespace myapp

# Deploy
sudo k3s kubectl -n myapp create deployment web --image=nginx:alpine --replicas=2

# Expose as NodePort
sudo k3s kubectl -n myapp expose deployment web \
  --type=NodePort --port=80 --target-port=80

# Get the port
APP_PORT=$(sudo k3s kubectl -n myapp get svc web \
  -o jsonpath='{.spec.ports[0].nodePort}')

echo "App running on port $APP_PORT"
```

### Make it accessible via Tailscale

```bash
# Private (Tailnet only):
sudo tailscale serve https /myapp http://localhost:$APP_PORT

# Or public:
sudo tailscale funnel https /myapp http://localhost:$APP_PORT
```

### Deploy to SLURM (batch workload)

```bash
# Submit a batch computation
sbatch --wrap='python3 -c "
import time, socket
print(f\"Computing on {socket.gethostname()}...\")
result = sum(i*i for i in range(10_000_000))
print(f\"Result: {result}\")
"' --output=/tmp/compute_%j.out

# Check status
squeue

# View result when done
cat /tmp/compute_*.out
```

---

## 5. Persistent Services via Helm Charts

With Rancher running, you can deploy apps from its catalog. Some useful ones:

| App | What it does | Helm install |
|-----|-------------|--------------|
| **Longhorn** | Distributed storage for PVCs | `helm install longhorn longhorn/longhorn -n longhorn-system --create-namespace` |
| **Prometheus + Grafana** | Monitoring & dashboards | Available in Rancher's built-in monitoring |
| **JupyterHub** | Multi-user notebooks | `helm install jhub jupyterhub/jupyterhub -n jupyter --create-namespace` |

### Install Longhorn (recommended for persistent storage)

```bash
helm repo add longhorn https://charts.longhorn.io
helm repo update

helm install longhorn longhorn/longhorn \
  --namespace longhorn-system --create-namespace \
  --set defaultSettings.defaultDataPath=/var/lib/longhorn

# Wait for it
sudo k3s kubectl -n longhorn-system rollout status deployment/longhorn-driver-deployer --timeout=300s
```

Longhorn provides replicated block storage across your cluster nodes — your PersistentVolumeClaims will survive node failures.

---

## Quick Reference

| Task | Command |
|------|---------|
| Test cluster health | `cluster-test` |
| Test SLURM only | `cluster-test slurm` |
| Test K3s only | `cluster-test k3s` |
| Get kubeconfig | `sudo cat /etc/rancher/k3s/k3s.yaml` |
| List K3s nodes | `sudo k3s kubectl get nodes` |
| List all pods | `sudo k3s kubectl get pods -A` |
| SLURM node status | `sinfo` |
| SLURM job queue | `squeue` |
| Submit SLURM job | `sbatch --wrap='command'` |
| Tailscale serve (private) | `sudo tailscale serve https /path http://localhost:PORT` |
| Tailscale funnel (public) | `sudo tailscale funnel https /path http://localhost:PORT` |
| Stop serving | `sudo tailscale serve reset` |
| Check Tailscale status | `tailscale status` |
