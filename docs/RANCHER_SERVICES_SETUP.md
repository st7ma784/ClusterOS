# Kubernetes Services on ClusterOS: Rancher, Longhorn & Ingress

> **Auto-deployed**: When the K3s leader starts, node-agent automatically deploys
> slurmdbd, MetalLB, nginx-ingress, Longhorn, cert-manager, Rancher, and SLURM REST.
> You typically do not need to install these manually.

This guide covers:
- What services are deployed and how to reach them
- Using Longhorn for persistent storage
- Deploying your own services with proper ingress
- Exposing services via Tailscale Serve/Funnel
- Verifying service visibility across all node LANs

---

## 1. Pre-deployed Services Reference

All services are accessible from any node's LAN IP or Tailscale IP (100.x.x.x).
nginx-ingress runs as a DaemonSet with `hostNetwork: true`, binding ports 80/443 on
every node — so the same URLs work regardless of which node you hit.

| Service | URL | NodePort | Notes |
|---------|-----|----------|-------|
| **Landing page** | `http://NODE_IP/` | 80 / 30080 | Auto-discovers all NodePort services |
| **Rancher UI** | `https://NODE_IP:30444` | 30444 | Admin user/pass set on first login |
| **Rancher (via ingress)** | `http://NODE_IP/rancher` | 80 | Redirects to `https://NODE_IP:30444` |
| **Longhorn UI** | `http://NODE_IP/longhorn` | 80 / 30900 | Distributed block storage |
| **SLURM REST API** | `http://NODE_IP/slurm` | 30819 | slurmrestd + munge auth |
| **K3s API** | `https://NODE_IP:6443` | — | TLS, use kubeconfig |

> Replace `NODE_IP` with the node's LAN IP (e.g. `192.168.1.x`) or Tailscale IP (`100.x.x.x`).

---

## 2. Access the Cluster from Your Laptop

### Copy the kubeconfig

On a K3s **server** (leader) node:

```bash
sudo cat /etc/rancher/k3s/k3s.yaml
```

On your **laptop**:

```bash
TAILSCALE_IP=100.x.x.x   # replace with leader's Tailscale IP

# Copy and patch server URL
ssh clusteros@$TAILSCALE_IP 'sudo cat /etc/rancher/k3s/k3s.yaml' \
  | sed "s|127.0.0.1|$TAILSCALE_IP|g" > ~/.kube/clusteros.yaml

export KUBECONFIG=~/.kube/clusteros.yaml
kubectl get nodes -o wide
```

---

## 3. Rancher Management UI

Rancher is auto-deployed by node-agent and accessible at `https://NODE_IP:30444`.

### First login

1. Open `https://NODE_IP:30444` (accept the self-signed cert warning)
2. Set a password when prompted
3. The cluster is pre-imported — you'll see it on the home screen

### Import a cluster already running K3s

If you have additional K3s clusters, import them from Rancher:

1. **Home** → **Import Existing** → **Generic**
2. Copy the `kubectl apply` command Rancher shows
3. Run it on the target cluster's server node
4. The cluster appears in Rancher within ~2 minutes

### Deploy via Rancher App Catalog

From the Rancher UI:
1. Select your cluster → **Apps** → **Charts**
2. Browse or search for a chart (e.g. "JupyterHub", "Grafana")
3. Click **Install**, fill in values, click **Install** again
4. Monitor the deployment under **Workloads** → **Deployments**

---

## 4. Longhorn Distributed Storage

Longhorn provides replicated block storage across all cluster nodes. PersistentVolumeClaims
backed by Longhorn survive node failures (data is replicated 2–3×).

### Verify Longhorn is ready

```bash
sudo k3s kubectl -n longhorn-system get pods
# All pods should be Running. Key pods: longhorn-manager-*, longhorn-driver-deployer-*
```

### Access the Longhorn UI

Navigate to `http://NODE_IP/longhorn` from any machine on the node's LAN or Tailnet.

From the UI you can:
- View volume health and replication status
- Increase/decrease replica count per volume
- Create volume snapshots and backups
- Add extra disks (node-agent auto-registers disks found at `/mnt/clusteros/disk-N`)

### Set Longhorn as the default StorageClass

```bash
# Verify Longhorn is the default
sudo k3s kubectl get storageclass
# longhorn should show (default)

# If not, set it:
sudo k3s kubectl patch storageclass longhorn \
  -p '{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}'
```

### Use a PersistentVolumeClaim (PVC)

```yaml
# pvc-example.yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-data
  namespace: myapp
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: longhorn
  resources:
    requests:
      storage: 5Gi
```

```bash
sudo k3s kubectl apply -f pvc-example.yaml

# Watch until bound
sudo k3s kubectl -n myapp get pvc my-data -w
```

Once `STATUS` is `Bound`, mount it in a Pod:

```yaml
# pod-with-pvc.yaml
apiVersion: v1
kind: Pod
metadata:
  name: app
  namespace: myapp
spec:
  containers:
  - name: app
    image: nginx:alpine
    volumeMounts:
    - name: data
      mountPath: /data
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: my-data
```

### Longhorn backup to S3 (optional)

1. Longhorn UI → **Settings** → **Backup Target**
2. Set `s3://your-bucket@region/path`
3. Set **Backup Target Credential Secret** with AWS keys
4. Create recurring backups per volume under **Volumes** → **Volume** → **Recurring Jobs**

---

## 5. Deploy Your Own Service

### Step 1: Deploy to K3s

```bash
# Create a namespace
sudo k3s kubectl create namespace myapp

# Deploy (replace nginx:alpine with your image)
sudo k3s kubectl -n myapp create deployment web \
  --image=nginx:alpine --replicas=2

# Expose as ClusterIP first, then as NodePort
sudo k3s kubectl -n myapp expose deployment web \
  --type=NodePort --port=80 --target-port=80 --name=web

# Find the NodePort assigned
sudo k3s kubectl -n myapp get svc web
# NAME   TYPE       CLUSTER-IP    EXTERNAL-IP   PORT(S)        AGE
# web    NodePort   10.43.x.x     <none>        80:3XXXX/TCP   10s
```

### Step 2: Add an ingress rule (path-based routing via nginx)

This makes your service available at `http://NODE_IP/myapp` on every node:

```bash
cat <<'EOF' | sudo k3s kubectl apply -f -
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: web-ingress
  namespace: myapp
  annotations:
    nginx.ingress.kubernetes.io/use-regex: "true"
    nginx.ingress.kubernetes.io/rewrite-target: /$2
spec:
  ingressClassName: nginx
  rules:
  - http:
      paths:
      - path: /myapp(/|$)(.*)
        pathType: ImplementationSpecific
        backend:
          service:
            name: web
            port:
              number: 80
EOF
```

Test from any LAN machine:

```bash
curl http://NODE_IP/myapp/
```

### Step 3 (optional): HTTPS with cert-manager

cert-manager is pre-deployed. Add a `ClusterIssuer` for self-signed certs:

```bash
cat <<'EOF' | sudo k3s kubectl apply -f -
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned
spec:
  selfSigned: {}
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: web-ingress-tls
  namespace: myapp
  annotations:
    cert-manager.io/cluster-issuer: selfsigned
spec:
  ingressClassName: nginx
  tls:
  - hosts: [NODE_IP.nip.io]
    secretName: web-tls
  rules:
  - host: NODE_IP.nip.io
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: web
            port:
              number: 80
EOF
```

For production HTTPS via Let's Encrypt (requires a public domain):

```bash
cat <<'EOF' | sudo k3s kubectl apply -f -
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-prod
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: you@example.com
    privateKeySecretRef:
      name: letsencrypt-prod
    solvers:
    - http01:
        ingress:
          class: nginx
EOF
```

---

## 6. Tailscale Serve and Funnel

**Tailscale Serve** — expose a service privately within your Tailnet only.
**Tailscale Funnel** — expose a service publicly on the internet (requires Funnel enabled in Tailscale ACLs).

### Enable Funnel in Tailscale ACLs

In the [Tailscale Admin console](https://login.tailscale.com/admin/acls), add:

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

Also enable **MagicDNS** and **HTTPS Certificates** under Admin → DNS.

### Expose a service privately (Tailnet only)

```bash
# Get the NodePort for your service
NODE_PORT=$(sudo k3s kubectl -n myapp get svc web \
  -o jsonpath='{.spec.ports[0].nodePort}')

# Serve it at https://<hostname>.<tailnet>.ts.net/myapp
sudo tailscale serve https /myapp http://localhost:$NODE_PORT

# Check what's being served
sudo tailscale serve status
```

### Expose via nginx ingress (recommended for multi-service)

Since nginx-ingress binds port 80 on the host, point Tailscale Serve at port 80:

```bash
# All ingress paths become accessible at https://<hostname>.<tailnet>.ts.net/
sudo tailscale serve https / http://localhost:80

# Now all your ingress paths work:
#   /myapp  → your app
#   /longhorn → Longhorn UI
#   /slurm  → SLURM REST
```

### Expose a service publicly (internet)

```bash
# Requires Funnel enabled in Tailscale ACLs (see above)
sudo tailscale funnel https / http://localhost:80

# Your cluster's landing page is now publicly reachable at:
# https://<hostname>.<tailnet>.ts.net/
```

### Expose Rancher publicly

Rancher runs on HTTPS/30444, not on port 80. Forward it to a Tailscale path:

```bash
sudo tailscale serve https /rancher https://localhost:30444
# Warning: Rancher uses absolute redirects internally — open the URL directly
# rather than through a path prefix to avoid redirect loops.

# Better: serve the Rancher port directly
sudo tailscale serve --bg https+insecure:443 https://localhost:30444
```

### Stop serving

```bash
sudo tailscale serve reset    # remove all serve config
sudo tailscale funnel reset   # remove all funnel config
```

---

## 7. LAN Visibility: Verify Services Are Reachable on Every Node

nginx-ingress runs as a DaemonSet with `hostNetwork: true`, so it binds ports 80 and
443 directly on every cluster node. This means every service behind ingress is reachable
from the LAN of every node, not just the leader.

### Quick check from any node

```bash
NODE_IP=$(hostname -I | awk '{print $1}')

echo "=== Checking services on $NODE_IP ==="

# Landing page (should return HTML with service list)
curl -sf http://$NODE_IP/ | grep -o '<title>.*</title>' && echo "OK: landing page" \
  || echo "FAIL: landing page"

# Longhorn UI
curl -sf -o /dev/null -w "%{http_code}" http://$NODE_IP/longhorn/ \
  | grep -qE '^(200|301|302)' && echo "OK: Longhorn" || echo "FAIL: Longhorn"

# SLURM REST API
curl -sf -o /dev/null -w "%{http_code}" http://$NODE_IP/slurm \
  | grep -qE '^(200|301|302|401)' && echo "OK: SLURM REST" || echo "FAIL: SLURM REST"

# Rancher redirect
curl -sf -o /dev/null -w "%{http_code}" http://$NODE_IP/rancher \
  | grep -qE '^(200|301|302)' && echo "OK: Rancher redirect" || echo "FAIL: Rancher redirect"

# Rancher direct HTTPS
curl -sfk -o /dev/null -w "%{http_code}" https://$NODE_IP:30444/ \
  | grep -qE '^(200|301|302)' && echo "OK: Rancher HTTPS" || echo "FAIL: Rancher HTTPS"
```

### Cluster-wide LAN visibility sweep

Run this on the leader to check services are reachable from every node's LAN IP:

```bash
#!/bin/bash
# Check that all services respond on every cluster node's LAN IP.

SERVICES=(
  "http://NODE/            Landing page"
  "http://NODE/longhorn/   Longhorn UI"
  "http://NODE/slurm       SLURM REST"
  "http://NODE/rancher     Rancher redirect"
  "https://NODE:30444/     Rancher HTTPS"
)

# Get all node IPs from k3s (uses LAN/Tailscale IPs registered with kubelet)
NODES=$(sudo k3s kubectl get nodes \
  -o jsonpath='{range .items[*]}{.status.addresses[?(@.type=="InternalIP")].address}{"\n"}{end}')

for NODE_IP in $NODES; do
  echo ""
  echo "=== Node: $NODE_IP ==="
  for entry in "${SERVICES[@]}"; do
    URL=$(echo "$entry" | sed "s|NODE|$NODE_IP|" | awk '{print $1}')
    NAME=$(echo "$entry" | awk '{print $2, $3}')
    CODE=$(curl -sfk -o /dev/null -w "%{http_code}" --connect-timeout 5 "$URL" 2>/dev/null)
    if echo "$CODE" | grep -qE '^(200|301|302|401)'; then
      printf "  %-30s %s\n" "$NAME" "OK ($CODE)"
    else
      printf "  %-30s %s\n" "$NAME" "FAIL (got: ${CODE:-timeout})"
    fi
  done
done
```

Save as `scripts/check-service-visibility.sh` and run with `bash scripts/check-service-visibility.sh`.

### What to check if a service is not visible

**Port 80/443 not responding on a node:**

```bash
# Is nginx-ingress DaemonSet pod running on this node?
sudo k3s kubectl -n ingress-nginx get pods -o wide | grep <node-name>

# Is the pod using hostNetwork?
sudo k3s kubectl -n ingress-nginx get pod <pod-name> \
  -o jsonpath='{.spec.hostNetwork}'
# Should print: true

# Is port 80 actually bound?
ss -tlnp | grep ':80'

# Is the INPUT rule present?
iptables -L INPUT -n | grep 'dpt:80'
```

**NodePort range not reachable from LAN:**

```bash
# Check that NodePort INPUT rule exists
iptables -L INPUT -n | grep '30000:32767'

# If missing, restart node-agent (it rebuilds firewall rules on startup)
sudo systemctl restart node-agent
```

**Service responds on Tailscale IP but not on LAN IP:**

nginx-ingress binds on all interfaces (`0.0.0.0`), so if the LAN IP is unreachable it's
a firewall/routing issue, not an nginx issue.

```bash
# Check UFW allows port 80 from LAN
sudo ufw status | grep 80

# Temporarily test without UFW
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
```

---

## 8. Persistent Services via Helm

With Rancher running, deploy from its built-in catalog or via Helm directly.

### JupyterHub

```bash
helm repo add jupyterhub https://hub.jupyter.org/helm-chart/
helm repo update

cat <<'EOF' > jhub-values.yaml
proxy:
  service:
    type: NodePort
    nodePorts:
      http: 30888
singleuser:
  storage:
    type: dynamic
    dynamic:
      storageClass: longhorn   # backed by Longhorn PVCs
    capacity: 10Gi
EOF

helm install jupyterhub jupyterhub/jupyterhub \
  --namespace jupyter --create-namespace \
  -f jhub-values.yaml
```

Add an ingress rule so JupyterHub is reachable at `/jupyter`:

```bash
cat <<'EOF' | sudo k3s kubectl apply -f -
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: jupyterhub
  namespace: jupyter
  annotations:
    nginx.ingress.kubernetes.io/use-regex: "true"
    nginx.ingress.kubernetes.io/rewrite-target: /$2
spec:
  ingressClassName: nginx
  rules:
  - http:
      paths:
      - path: /jupyter(/|$)(.*)
        pathType: ImplementationSpecific
        backend:
          service:
            name: proxy-public
            port:
              number: 80
EOF
```

### Grafana + Prometheus

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

helm install prometheus prometheus-community/kube-prometheus-stack \
  --namespace monitoring --create-namespace \
  --set grafana.service.type=NodePort \
  --set grafana.service.nodePort=30300 \
  --set prometheus.service.type=NodePort \
  --set prometheus.service.nodePort=30090
```

---

## Quick Reference

| Task | Command |
|------|---------|
| Check all services | `cluster dash` |
| List K3s nodes | `sudo k3s kubectl get nodes -o wide` |
| List all pods | `sudo k3s kubectl get pods -A` |
| List all services + ports | `sudo k3s kubectl get svc -A` |
| List all ingress rules | `sudo k3s kubectl get ingress -A` |
| Rancher UI | `https://NODE_IP:30444` |
| Longhorn UI | `http://NODE_IP/longhorn` |
| SLURM REST | `http://NODE_IP/slurm` |
| Landing page | `http://NODE_IP/` |
| Tailscale serve (private) | `sudo tailscale serve https / http://localhost:80` |
| Tailscale funnel (public) | `sudo tailscale funnel https / http://localhost:80` |
| Stop Tailscale serving | `sudo tailscale serve reset` |
| Check firewall rules | `iptables -L INPUT -n \| grep -E '80\|443\|30000'` |
| LAN visibility sweep | `bash scripts/check-service-visibility.sh` |
