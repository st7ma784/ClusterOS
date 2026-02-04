# Kubernetes config plan — Ollama + Anthropic adapter

Goal

Deploy Ollama serving a quantized model on GPU node(s) and expose an internal Anthropic/Claude-compatible HTTP endpoint via an adapter.

Assumptions

- Kubernetes cluster with NVIDIA device plugin installed.
- One or more GPU nodes labelled for inference, e.g. `node-role.kubernetes.io/inference=true`.
- Cluster has a working Container Registry accessible by the cluster.
- Tailscale provides internal connectivity between cluster nodes (restrict external exposure).

Artifacts to create

- `ollama-deployment.yaml` — Deployment for Ollama with GPU resource request
- `ollama-service.yaml` — ClusterIP service to expose Ollama inside the cluster
- `model-pvc.yaml` — PersistentVolumeClaim for model storage
- `adapter-deployment.yaml` — Anthropic adapter deployment
- `deploy-ollama.sh` — small script to apply manifests

Example manifests (skeletons)

ollama Deployment (GPU)
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ollama
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ollama
  template:
    metadata:
      labels:
        app: ollama
    spec:
      nodeSelector:
        node-role.kubernetes.io/inference: "true"
      containers:
      - name: ollama
        image: ollama/ollama:latest
        ports:
        - containerPort: 11434
        resources:
          limits:
            nvidia.com/gpu: 1
            memory: "24Gi"
            cpu: "4"
        volumeMounts:
        - name: models
          mountPath: /models
        env:
        - name: OLLAMA_MODEL_DIR
          value: /models
      volumes:
      - name: models
        persistentVolumeClaim:
          claimName: ollama-models-pvc
```

ollama Service
```yaml
apiVersion: v1
kind: Service
metadata:
  name: ollama
spec:
  selector:
    app: ollama
  ports:
  - protocol: TCP
    port: 11434
    targetPort: 11434
  type: ClusterIP
```

PersistentVolumeClaim (example)
```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ollama-models-pvc
spec:
  accessModes: ["ReadWriteOnce"]
  resources:
    requests:
      storage: 200Gi
  storageClassName: standard
```

Anthropic adapter Deployment
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ollama-anthropic-adapter
spec:
  replicas: 1
  selector:
    matchLabels:
      app: ollama-adapter
  template:
    metadata:
      labels:
        app: ollama-adapter
    spec:
      containers:
      - name: adapter
        image: <registry>/ollama-anthropic-adapter:latest
        env:
        - name: OLLAMA_URL
          value: http://ollama:11434
        - name: AUTH_TOKEN
          valueFrom:
            secretKeyRef:
              name: ollama-adapter-secret
              key: token
        ports:
        - containerPort: 8080
```

Deploy script (local convenience)

```bash
#!/usr/bin/env bash
set -euo pipefail
kubectl apply -f model-pvc.yaml
kubectl apply -f ollama-deployment.yaml
kubectl apply -f ollama-service.yaml
kubectl apply -f adapter-deployment.yaml
```

Operational notes

- Use a `DaemonSet` instead of `Deployment` only if you want Ollama on every node.
- For HA consider running 1 primary Ollama instance and other replicas for scaling reads; model loading duplicates VRAM usage so prefer 1 replica per GPU node.
- Use `nodeSelector` and `tolerations` to ensure scheduling on GPU nodes.
- Secure adapter with TLS and token auth; restrict access via NetworkPolicy and Tailscale ACLs.

Next steps I can perform

- Render final YAMLs with your exact values and a `deploy-ollama.sh` file in the repository.
- Build the adapter container image (FastAPI) and push to your registry.
- Optionally create a Helm chart for repeatable deployments.