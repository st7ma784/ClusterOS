# Deploying Apps on ClusterOS with Helm Charts

This guide covers writing, deploying, and managing applications on the ClusterOS
Kubernetes cluster (k3s) using Helm. It assumes the cluster is running and you have
`kubectl` and `helm` available (both pre-installed by `apply-patch.sh`).

---

## Quick Start

```bash
# On any cluster node — kubectl and helm are pre-installed
sudo k3s kubectl get nodes      # verify cluster is healthy
helm version                    # should print v3.x
```

If running from your laptop, [copy the kubeconfig first](#accessing-from-your-laptop).

---

## Table of Contents

1. [Core Concepts](#core-concepts)
2. [Accessing from Your Laptop](#accessing-from-your-laptop)
3. [Using Rancher App Catalog (GUI)](#using-rancher-app-catalog-gui)
4. [Installing Community Helm Charts](#installing-community-helm-charts)
5. [Writing Your Own Helm Chart](#writing-your-own-helm-chart)
6. [Persistent Storage with Longhorn](#persistent-storage-with-longhorn)
7. [Exposing Services via nginx Ingress](#exposing-services-via-nginx-ingress)
8. [Common App Recipes](#common-app-recipes)
9. [Updating and Rolling Back](#updating-and-rolling-back)
10. [Debugging Helm Releases](#debugging-helm-releases)

---

## Core Concepts

| Concept | What it is |
|---------|-----------|
| **Chart** | A Helm package — templates + default values |
| **Release** | A deployed instance of a chart (name + namespace) |
| **Values** | Configuration that overrides chart defaults |
| **Repository** | A collection of charts hosted at a URL |
| **StorageClass** | `longhorn` is the default — provides replicated PVCs |
| **Ingress** | nginx-ingress DaemonSet routes HTTP/HTTPS on every node |
| **NodePort** | Services exposed on every node at a fixed port (30000–32767) |

ClusterOS uses NodePort + nginx ingress (not LoadBalancer) for service exposure.
MetalLB is installed but non-functional due to port conflict — ignore it.

---

## Accessing from Your Laptop

```bash
LEADER_IP=100.102.126.31   # your cluster leader's Tailscale IP

# Copy and patch kubeconfig (replaces 127.0.0.1 with the real IP)
ssh clusteros@$LEADER_IP 'sudo cat /etc/rancher/k3s/k3s.yaml' \
  | sed "s|127.0.0.1|$LEADER_IP|g" \
  | sed "s|default|clusteros|g" \
  > ~/.kube/clusteros.yaml

export KUBECONFIG=~/.kube/clusteros.yaml
kubectl get nodes -o wide
```

Install `helm` on your laptop if needed:
```bash
curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
```

---

## Using Rancher App Catalog (GUI)

> **Note**: Rancher Helm install currently fails due to [Issue #1](KNOWN_ISSUES.md).
> Access Rancher directly at `https://NODE_IP:30444` once it's fixed, or use the
> CLI approach below.

Once Rancher is running:
1. Navigate to `https://NODE_IP:30444` → log in (default: `admin`/`admin`)
2. Select your cluster from the home screen
3. **Apps** → **Charts** — browse the built-in catalog
4. Click a chart → **Install** → fill in values → **Install**
5. Monitor under **Workloads** → **Deployments**

For custom Helm repos in Rancher:
- **Apps** → **Repositories** → **Create**
- Enter name and chart repository URL
- Charts from that repo appear in the catalog

---

## Installing Community Helm Charts

### Add a chart repository

```bash
# Common repos
helm repo add stable        https://charts.helm.sh/stable
helm repo add bitnami       https://charts.bitnami.com/bitnami
helm repo add jupyterhub    https://hub.jupyter.org/helm-chart/
helm repo add prometheus    https://prometheus-community.github.io/helm-charts
helm repo add grafana       https://grafana.github.io/helm-charts
helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx

helm repo update   # fetch latest chart index
```

### Search for a chart

```bash
helm search repo bitnami/postgresql
helm search hub wordpress   # searches Artifact Hub (public index)
```

### Inspect chart defaults before installing

```bash
# Show all configurable values
helm show values bitnami/postgresql

# Show chart README
helm show readme bitnami/postgresql
```

### Install a chart

```bash
# Minimal install — uses all defaults
helm install mydb bitnami/postgresql \
  --namespace myapp --create-namespace

# Install with value overrides
helm install mydb bitnami/postgresql \
  --namespace myapp --create-namespace \
  --set auth.postgresPassword=secret123 \
  --set primary.service.type=NodePort \
  --set primary.service.nodePorts.postgresql=30432

# Install from a values file (recommended for complex configs)
helm install mydb bitnami/postgresql \
  --namespace myapp --create-namespace \
  -f my-postgres-values.yaml
```

### List installed releases

```bash
helm list -A          # all namespaces
helm list -n myapp    # specific namespace
```

---

## Writing Your Own Helm Chart

### Create the chart scaffold

```bash
helm create myapp
```

This creates:
```
myapp/
├── Chart.yaml          # chart metadata (name, version, description)
├── values.yaml         # default configuration values
├── templates/
│   ├── deployment.yaml
│   ├── service.yaml
│   ├── ingress.yaml
│   ├── _helpers.tpl    # reusable template snippets
│   └── NOTES.txt       # printed after install
└── .helmignore
```

### Chart.yaml — chart metadata

```yaml
# myapp/Chart.yaml
apiVersion: v2
name: myapp
description: My application on ClusterOS
type: application
version: 0.1.0        # chart version (bump on chart changes)
appVersion: "1.0.0"   # your app's version
```

### values.yaml — configurable defaults

```yaml
# myapp/values.yaml
replicaCount: 2

image:
  repository: nginx
  tag: alpine
  pullPolicy: IfNotPresent

service:
  type: NodePort
  port: 80
  nodePort: 31234   # fixed NodePort so the landing page can link to it

ingress:
  enabled: true
  path: /myapp      # accessible at http://NODE_IP/myapp

persistence:
  enabled: false
  size: 1Gi
  storageClass: longhorn

resources:
  requests:
    cpu: 100m
    memory: 128Mi
  limits:
    cpu: 500m
    memory: 512Mi
```

### templates/deployment.yaml

```yaml
# myapp/templates/deployment.yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: {{ include "myapp.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "myapp.labels" . | nindent 4 }}
spec:
  replicas: {{ .Values.replicaCount }}
  selector:
    matchLabels:
      {{- include "myapp.selectorLabels" . | nindent 6 }}
  template:
    metadata:
      labels:
        {{- include "myapp.selectorLabels" . | nindent 8 }}
    spec:
      containers:
      - name: {{ .Chart.Name }}
        image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
        imagePullPolicy: {{ .Values.image.pullPolicy }}
        ports:
        - containerPort: {{ .Values.service.port }}
        {{- if .Values.persistence.enabled }}
        volumeMounts:
        - name: data
          mountPath: /data
        {{- end }}
        resources:
          {{- toYaml .Values.resources | nindent 10 }}
      {{- if .Values.persistence.enabled }}
      volumes:
      - name: data
        persistentVolumeClaim:
          claimName: {{ include "myapp.fullname" . }}-data
      {{- end }}
```

### templates/service.yaml

```yaml
# myapp/templates/service.yaml
apiVersion: v1
kind: Service
metadata:
  name: {{ include "myapp.fullname" . }}
  namespace: {{ .Release.Namespace }}
  labels:
    {{- include "myapp.labels" . | nindent 4 }}
spec:
  type: {{ .Values.service.type }}
  selector:
    {{- include "myapp.selectorLabels" . | nindent 4 }}
  ports:
  - port: {{ .Values.service.port }}
    targetPort: {{ .Values.service.port }}
    {{- if and (eq .Values.service.type "NodePort") .Values.service.nodePort }}
    nodePort: {{ .Values.service.nodePort }}
    {{- end }}
```

### templates/ingress.yaml

ClusterOS uses nginx-ingress with path-based routing and regex rewrites:

```yaml
# myapp/templates/ingress.yaml
{{- if .Values.ingress.enabled }}
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ include "myapp.fullname" . }}
  namespace: {{ .Release.Namespace }}
  annotations:
    nginx.ingress.kubernetes.io/use-regex: "true"
    nginx.ingress.kubernetes.io/rewrite-target: /$2
spec:
  ingressClassName: nginx
  rules:
  - http:
      paths:
      - path: {{ .Values.ingress.path }}(/|$)(.*)
        pathType: ImplementationSpecific
        backend:
          service:
            name: {{ include "myapp.fullname" . }}
            port:
              number: {{ .Values.service.port }}
{{- end }}
```

### templates/pvc.yaml (optional)

```yaml
# myapp/templates/pvc.yaml
{{- if .Values.persistence.enabled }}
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: {{ include "myapp.fullname" . }}-data
  namespace: {{ .Release.Namespace }}
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: {{ .Values.persistence.storageClass }}
  resources:
    requests:
      storage: {{ .Values.persistence.size }}
{{- end }}
```

### Validate and install

```bash
# Lint — catches template errors
helm lint myapp/

# Dry run — renders templates without applying
helm install myrelease myapp/ \
  --namespace myapp --create-namespace \
  --dry-run --debug

# Install for real
helm install myrelease myapp/ \
  --namespace myapp --create-namespace

# With value overrides
helm install myrelease myapp/ \
  --namespace myapp --create-namespace \
  --set replicaCount=3 \
  --set persistence.enabled=true
```

---

## Persistent Storage with Longhorn

Longhorn is the default StorageClass. PVCs are replicated across nodes.

### Use in your chart

In `values.yaml`:
```yaml
persistence:
  storageClass: longhorn
  size: 5Gi
```

In your PVC template:
```yaml
storageClassName: {{ .Values.persistence.storageClass }}
```

### Access modes

| Mode | Use case |
|------|---------|
| `ReadWriteOnce` | Single pod (databases, stateful apps) |
| `ReadWriteMany` | Multiple pods simultaneously — requires NFS or Longhorn RWX |

For `ReadWriteMany`, use `storageClassName: longhorn` with `accessModes: [ReadWriteMany]`
(Longhorn supports RWX via NFS share from v1.4+).

### Check storage health

```bash
sudo k3s kubectl -n longhorn-system get pods
# longhorn-manager on every node should be Running

# List volumes
sudo k3s kubectl -n longhorn-system get volumes.longhorn.io

# Open UI
http://NODE_IP/longhorn
```

---

## Exposing Services via nginx Ingress

nginx-ingress runs as a DaemonSet on every node (`hostNetwork: true`), so your service
is reachable on every node's IP once an Ingress resource exists.

### Path-based routing (recommended)

All ClusterOS services use `/path` routing with regex rewrites:

```yaml
annotations:
  nginx.ingress.kubernetes.io/use-regex: "true"
  nginx.ingress.kubernetes.io/rewrite-target: /$2
# path: /myapp(/|$)(.*)
```

This strips the prefix before forwarding to your app. If your app serves at `/`, use this.
If your app serves at its own prefix (e.g. Grafana with `GF_SERVER_ROOT_URL=/grafana`),
omit the rewrite-target and just use `path: /grafana`.

### Host-based routing (requires DNS)

```yaml
spec:
  rules:
  - host: myapp.100-102-126-31.nip.io    # resolves to 100.102.126.31
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: myapp
            port:
              number: 80
```

### HTTPS with cert-manager

```yaml
metadata:
  annotations:
    cert-manager.io/cluster-issuer: selfsigned
spec:
  tls:
  - hosts: [myapp.100-102-126-31.nip.io]
    secretName: myapp-tls
  rules:
  - host: myapp.100-102-126-31.nip.io
    ...
```

Create a self-signed ClusterIssuer if not present:
```bash
cat <<'EOF' | sudo k3s kubectl apply -f -
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: selfsigned
spec:
  selfSigned: {}
EOF
```

---

## Common App Recipes

### PostgreSQL (Bitnami)

```bash
helm install postgres bitnami/postgresql \
  --namespace data --create-namespace \
  --set auth.postgresPassword=mypassword \
  --set primary.persistence.storageClass=longhorn \
  --set primary.persistence.size=10Gi \
  --set primary.service.type=NodePort \
  --set primary.service.nodePorts.postgresql=30432
```

Connect from any node:
```bash
psql -h NODE_IP -p 30432 -U postgres
```

### Redis (Bitnami)

```bash
helm install redis bitnami/redis \
  --namespace data --create-namespace \
  --set auth.password=myredispass \
  --set master.service.type=NodePort \
  --set master.service.nodePorts.redis=30379 \
  --set master.persistence.storageClass=longhorn
```

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
      storageClass: longhorn
    capacity: 10Gi
  defaultUrl: "/lab"
hub:
  config:
    JupyterHub:
      admin_access: true
    Authenticator:
      admin_users: [admin]
    DummyAuthenticator:
      password: clusteros
  extraConfig:
    00-auth: |
      c.JupyterHub.authenticator_class = 'dummy'
EOF

helm install jupyterhub jupyterhub/jupyterhub \
  --namespace jupyter --create-namespace \
  -f jhub-values.yaml \
  --wait --timeout 10m
```

Add ingress:
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

Access at `http://NODE_IP/jupyter` or `http://NODE_IP:30888`.

### Grafana + Prometheus

```bash
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm repo update

helm install kube-prom prometheus-community/kube-prometheus-stack \
  --namespace monitoring --create-namespace \
  --set grafana.service.type=NodePort \
  --set grafana.service.nodePort=30300 \
  --set grafana.adminPassword=clusteros \
  --set grafana.grafana\.ini.server.root_url="%(protocol)s://%(domain)s/grafana" \
  --set grafana.grafana\.ini.server.serve_from_sub_path=true \
  --set prometheus.prometheusSpec.storageSpec.volumeClaimTemplate.spec.storageClassName=longhorn \
  --set prometheus.prometheusSpec.storageSpec.volumeClaimTemplate.spec.resources.requests.storage=20Gi
```

Add Grafana ingress:
```bash
cat <<'EOF' | sudo k3s kubectl apply -f -
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: grafana
  namespace: monitoring
spec:
  ingressClassName: nginx
  rules:
  - http:
      paths:
      - path: /grafana
        pathType: Prefix
        backend:
          service:
            name: kube-prom-grafana
            port:
              number: 80
EOF
```

Access Grafana at `http://NODE_IP/grafana` (admin/clusteros).

### MinIO Object Storage

```bash
helm repo add minio https://charts.min.io/
helm repo update

helm install minio minio/minio \
  --namespace storage --create-namespace \
  --set rootUser=minioadmin \
  --set rootPassword=minioadmin123 \
  --set mode=standalone \
  --set persistence.storageClass=longhorn \
  --set persistence.size=50Gi \
  --set service.type=NodePort \
  --set service.nodePort=30900 \
  --set consoleService.type=NodePort \
  --set consoleService.nodePort=30901
```

---

## Updating and Rolling Back

### Upgrade a release

```bash
# Upgrade with new values
helm upgrade myrelease myapp/ \
  --namespace myapp \
  --set replicaCount=4

# Upgrade from updated chart (community chart)
helm repo update
helm upgrade mydb bitnami/postgresql --namespace data
```

### Roll back

```bash
# List release history
helm history myrelease -n myapp

# Roll back to previous version
helm rollback myrelease -n myapp

# Roll back to specific revision
helm rollback myrelease 2 -n myapp
```

### Uninstall

```bash
helm uninstall myrelease -n myapp
# Note: PVCs are NOT deleted by default — data persists.
# To also delete PVCs:
sudo k3s kubectl -n myapp delete pvc --all
```

---

## Debugging Helm Releases

### Check release status

```bash
helm status myrelease -n myapp
helm get all myrelease -n myapp     # full rendered manifests + values
helm get values myrelease -n myapp  # values used in this release
```

### Render templates locally without installing

```bash
helm template myrelease myapp/ -f my-values.yaml > rendered.yaml
cat rendered.yaml  # inspect for errors before applying
```

### Pod is not starting

```bash
# List pods
sudo k3s kubectl -n myapp get pods

# Describe pod (shows Events — usually the root cause)
sudo k3s kubectl -n myapp describe pod <pod-name>

# Pod logs
sudo k3s kubectl -n myapp logs <pod-name>
sudo k3s kubectl -n myapp logs <pod-name> --previous   # last crashed container

# Follow logs live
sudo k3s kubectl -n myapp logs -f deployment/myapp
```

### Common pod failures

| Status | Likely cause | Fix |
|--------|-------------|-----|
| `ImagePullBackOff` | Image doesn't exist or registry unreachable | Check image name/tag; verify internet access |
| `CrashLoopBackOff` | App crashes on startup | Check `logs --previous`; look for missing env/config |
| `Pending` | No node has enough resources or PVC not bound | Check `describe pod`; check `get pvc` |
| `Init:0/1` | Init container waiting | Check init container logs: `logs <pod> -c <init-container-name>` |
| `OOMKilled` | Memory limit too low | Increase `resources.limits.memory` |

### PVC stuck in Pending

```bash
sudo k3s kubectl -n myapp describe pvc <pvc-name>
# Look for Events — usually "no storage class" or Longhorn not ready

# Check Longhorn is healthy
sudo k3s kubectl -n longhorn-system get pods | grep -v Running
```

### Ingress not routing

```bash
# Check ingress exists
sudo k3s kubectl -n myapp get ingress

# Check ingress is accepted by controller (look for ADDRESS)
sudo k3s kubectl -n myapp describe ingress myapp

# Check nginx-ingress controller logs
sudo k3s kubectl -n ingress-nginx logs -l app.kubernetes.io/name=ingress-nginx | tail -30

# Verify the path regex is correct
curl -v http://NODE_IP/myapp/
```

### Helm release stuck in failed state

```bash
# Force uninstall if release is stuck
helm uninstall myrelease -n myapp --no-hooks

# If that fails, delete the secret helm uses to track the release
sudo k3s kubectl -n myapp delete secret -l owner=helm,name=myrelease
```

---

## Quick Reference

```bash
# Repo management
helm repo add <name> <url>
helm repo update
helm repo list

# Chart info
helm search repo <keyword>
helm show values <repo/chart>

# Install / upgrade
helm install <release> <chart> -n <ns> --create-namespace -f values.yaml
helm upgrade <release> <chart> -n <ns> -f values.yaml
helm upgrade --install <release> <chart> -n <ns>   # idempotent: install or upgrade

# Status
helm list -A
helm status <release> -n <ns>
helm history <release> -n <ns>

# Debug
helm template <release> <chart> -f values.yaml
helm get all <release> -n <ns>
helm lint <chart-dir>/

# Rollback / remove
helm rollback <release> -n <ns>
helm uninstall <release> -n <ns>
```
