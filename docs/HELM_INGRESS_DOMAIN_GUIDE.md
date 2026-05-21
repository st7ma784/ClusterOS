# Helm Charts, Ingress, and the Cluster Domain

This guide covers how to write Helm charts (or deploy community charts) on ClusterOS
that need the cluster domain and nginx ingress annotations — without getting burned by
Helm's annotation escaping rules or the nip.io vs custom-domain split.

---

## How ClusterOS Publishes the Domain

```
/etc/clusteros/cloudflare.env          ← source of truth on each node
  CLOUDFLARE_DOMAIN=example.com
  CLOUDFLARE_TUNNEL_TOKEN=<token>
          │
          ▼ readCloudflareDomain()
  Go daemon (node-agent) — publishClusterConfigMap()
          │
          ├─► clusteros-config (kube-system)      — pods, scripts, kubectl
          └─► clusteros-helm-values (fleet-default) — Fleet valuesFrom
```

When no domain is configured the cluster falls back to `<tailscale-ip>.nip.io`
with NodePorts (30080/30443/30444).

### `clusteros-config` — for pods and scripts

```bash
k3s kubectl get cm clusteros-config -n kube-system -o yaml
```

```yaml
data:
  domain: "example.com"       # empty string when no domain configured
  rancher-host: "rancher.example.com"
  mode: "domain"              # "domain" | "nip"
  ssl-redirect: "false"       # "false" when Cloudflare in front; "true" otherwise
```

### `clusteros-helm-values` — for Fleet `valuesFrom`

Published into **both** Fleet namespaces — use whichever matches your GitRepo:

```bash
# GitRepos targeting the local cluster (most common on ClusterOS)
k3s kubectl get cm clusteros-helm-values -n fleet-local -o yaml

# GitRepos targeting remote/external clusters
k3s kubectl get cm clusteros-helm-values -n fleet-default -o yaml
```

```yaml
data:
  values.yaml: |
    global:
      clusterDomain: "example.com"
      clusterMode: "domain"
      rancherHost: "rancher.example.com"
      sslRedirect: "false"
      ingressAnnotations:
        nginx.ingress.kubernetes.io/ssl-redirect: "false"
        nginx.ingress.kubernetes.io/force-ssl-redirect: "false"
```

Fleet merges this into every chart it deploys when `fleet.yaml` references it.

---

## Reading the Domain at Install Time

### Shell / Makefile

```bash
# From the node filesystem (on the node itself)
DOMAIN=$(grep '^CLOUDFLARE_DOMAIN=' /etc/clusteros/cloudflare.env 2>/dev/null \
         | cut -d= -f2)

# From the cluster (works from any machine with kubectl access)
DOMAIN=$(k3s kubectl get cm clusteros-config -n kube-system \
           -o jsonpath='{.data.domain}' 2>/dev/null)

# Build the hostname with nip.io fallback
if [ -n "$DOMAIN" ]; then
  HOST="myapp.${DOMAIN}"
else
  NODE_IP=$(k3s kubectl get nodes \
    -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
  HOST="${NODE_IP//./-}.nip.io"
fi
```

---

## The Annotation Escaping Problem

nginx ingress annotations contain dots AND slashes in their keys:

```
nginx.ingress.kubernetes.io/backend-protocol: HTTPS
```

Helm's `--set` flag uses dots as path separators. Passing annotation keys via `--set`:

```bash
# BROKEN — Helm parses dots as nested keys: nginx → ingress → kubernetes → io → ...
helm install myapp ./chart \
  --set "ingress.annotations.nginx.ingress.kubernetes.io/backend-protocol=HTTPS"

# UNRELIABLE — backslash-dot only works if the string reaches Helm's parser
# unmodified, but shell quoting + extraAnnotations add extra parsing layers
helm install myapp ./chart \
  --set-string "ingress.annotations.nginx\.ingress\.kubernetes\.io/backend-protocol=HTTPS"
```

Helm does support `\.` as a literal-dot escape in its own parser, but:
- The shell may consume the backslash before Helm sees it (varies by shell and quoting)
- Community charts that expose annotations via `extraAnnotations` run the value
  through a second template parse, dropping the escapes
- k3s uses `exec.Command` (no shell), so `\.` reaches Helm's parser intact —
  but the `extraAnnotations` layer still silently drops it

**The node-agent code itself gave up on `--set-string` for annotations** and always
follows up with a `kubectl patch` to reliably apply them.

---

## The Right Patterns

### Pattern 1 — Values file (preferred for all cases)

Write a `values.yaml` file instead of using `--set` for anything containing annotation
keys. YAML has no escaping issue with dotted keys.

```bash
DOMAIN=$(k3s kubectl get cm clusteros-config -n kube-system \
           -o jsonpath='{.data.domain}' 2>/dev/null)
HOST="${DOMAIN:+myapp.${DOMAIN}}"
if [ -z "$HOST" ]; then
  NODE_IP=$(k3s kubectl get nodes \
    -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
  HOST="${NODE_IP//./-}.nip.io"
fi

cat > /tmp/myapp-values.yaml <<EOF
ingress:
  enabled: true
  ingressClassName: nginx
  host: "${HOST}"
  annotations:
    nginx.ingress.kubernetes.io/ssl-redirect: "false"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "120"
    nginx.ingress.kubernetes.io/affinity: "cookie"
    nginx.ingress.kubernetes.io/session-cookie-name: "MYAPP_STICKY"
EOF

helm install myapp stable/myapp \
  -n myapp --create-namespace \
  -f /tmp/myapp-values.yaml \
  --wait --timeout 5m0s
```

For HTTPS backends (Rancher, cert-protected services):

```yaml
  annotations:
    nginx.ingress.kubernetes.io/backend-protocol: "HTTPS"
    nginx.ingress.kubernetes.io/proxy-ssl-verify: "off"
    nginx.ingress.kubernetes.io/ssl-redirect: "false"
```

### Pattern 2 — Post-install kubectl patch (reliable for any chart)

Let helm install whatever it can, then patch annotations reliably after:

```bash
helm install myapp stable/myapp -n myapp --create-namespace --wait

# kubectl handles dotted annotation keys natively — no escaping needed
k3s kubectl annotate ingress myapp -n myapp \
  nginx.ingress.kubernetes.io/backend-protocol=HTTPS \
  nginx.ingress.kubernetes.io/proxy-ssl-verify=off \
  nginx.ingress.kubernetes.io/ssl-redirect=false \
  --overwrite

# Patch the hostname separately using JSON patch (safe for any value)
k3s kubectl patch ingress myapp -n myapp --type=json \
  -p="[{\"op\":\"replace\",\"path\":\"/spec/rules/0/host\",\"value\":\"${HOST}\"}]"
```

### Pattern 3 — Disable chart ingress, apply yours separately

Full control: set `ingress.enabled=false` in the chart and apply your own Ingress
resource with exactly the annotations you need.

```bash
helm install myapp stable/myapp -n myapp --create-namespace \
  --set ingress.enabled=false \
  --wait

k3s kubectl apply -f - <<EOF
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: myapp
  namespace: myapp
  annotations:
    nginx.ingress.kubernetes.io/ssl-redirect: "false"
    nginx.ingress.kubernetes.io/affinity: "cookie"
    nginx.ingress.kubernetes.io/session-cookie-name: "MYAPP_INGRESSCOOKIE"
    nginx.ingress.kubernetes.io/session-cookie-expires: "172800"
    nginx.ingress.kubernetes.io/session-cookie-max-age: "172800"
spec:
  ingressClassName: nginx
  rules:
  - host: myapp.${DOMAIN}
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: myapp
            port:
              number: 80
EOF
```

### Pattern 4 — From Go (node-agent style)

When writing a service deployer in Go, write a temp values file rather than building
`--set` args for annotated resources. `exec.Command` bypasses the shell so `\.`
reaches Helm's parser, but chart `extraAnnotations` layers still silently drop them.

```go
func deployMyApp(nodeIP, domain string) error {
    host := "myapp." + domain
    if domain == "" {
        host = strings.ReplaceAll(nodeIP, ".", "-") + ".nip.io"
    }

    // Values file: YAML never has annotation-key escaping problems
    values := fmt.Sprintf(`
ingress:
  enabled: true
  ingressClassName: nginx
  host: %q
  annotations:
    nginx.ingress.kubernetes.io/ssl-redirect: "false"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "120"
`, host)

    f, err := os.CreateTemp("", "myapp-values-*.yaml")
    if err != nil {
        return err
    }
    defer os.Remove(f.Name())
    f.WriteString(values)
    f.Close()

    cmd := exec.Command("helm", "install", "myapp", "stable/myapp",
        "--namespace", "myapp", "--create-namespace",
        "-f", f.Name(),
        "--wait", "--timeout", "5m0s",
        "--kubeconfig", "/etc/rancher/k3s/k3s.yaml",
    )
    cmd.Env = append(os.Environ(), "KUBECONFIG=/etc/rancher/k3s/k3s.yaml")
    if out, err := cmd.CombinedOutput(); err != nil {
        return fmt.Errorf("helm install myapp: %w: %s", err, out)
    }

    // Post-install: patch any annotations the chart's extraAnnotations layer dropped
    exec.Command("k3s", "kubectl", "patch", "ingress", "myapp",
        "-n", "myapp", "--type=json",
        `-p=[{"op":"add","path":"/metadata/annotations/nginx.ingress.kubernetes.io~1affinity","value":"cookie"}]`,
    ).Run()

    return nil
}
```

Note: in JSON patch, `/` in annotation keys is encoded as `~1`.

---

## Domain / Ingress Decision Tree

```
Has CLOUDFLARE_DOMAIN configured?
├── YES (domain mode)
│   ├── Use subdomain ingress: host: myapp.{domain}
│   ├── ssl-redirect: "false"       (Cloudflare terminates TLS)
│   ├── force-ssl-redirect: "false" (prevents empty-host redirect loop)
│   └── cloudflared routes *.{domain} → localhost:80 → ingress-nginx
│
└── NO (nip.io mode)
    ├── Use NodePort for direct IP access (ports 30000-32767)
    ├── OR use path-based ingress (no host:) — requires rewrite-target
    └── Host: {tailscale-ip}.nip.io  (changes if leader re-elects — avoid for prod)
```

---

## Subdomain vs Path-Based Ingress

| | Subdomain (`myapp.example.com`) | Path (`example.com/myapp`) |
|---|---|---|
| Requires domain | Yes | No |
| App base URL | `/` (works out of the box) | `/myapp` (many apps break) |
| Session cookies | Work naturally | Need `cookie-path` annotation |
| Cloudflare | One wildcard tunnel rule covers all | Separate path rules |
| Recommended | All apps when domain is available | LAN-only / legacy apps |

Path-based ingress requires these extra annotations:

```yaml
nginx.ingress.kubernetes.io/use-regex: "true"
nginx.ingress.kubernetes.io/rewrite-target: /$2
# path: /myapp(/|$)(.*)
```

---

## Session Affinity

Add this to every ingress for any app with login sessions:

```yaml
annotations:
  nginx.ingress.kubernetes.io/affinity: "cookie"
  nginx.ingress.kubernetes.io/session-cookie-name: "MYAPP_INGRESSCOOKIE"
  nginx.ingress.kubernetes.io/session-cookie-expires: "172800"
  nginx.ingress.kubernetes.io/session-cookie-max-age: "172800"
  nginx.ingress.kubernetes.io/session-cookie-samesite: "None"
  nginx.ingress.kubernetes.io/session-cookie-secure: "true"
```

Without affinity, nginx round-robins across pods and users get logged out when they
land on a different pod than the one that issued their session token. This is the root
cause of the intermittent "Invalid username or password" / "first time visit" prompt
in Rancher.

---

## Annotation Quick Reference

| Annotation | Value | When to use |
|---|---|---|
| `nginx.ingress.kubernetes.io/ssl-redirect` | `"false"` | Cloudflare or LAN (no cert on nginx) |
| `nginx.ingress.kubernetes.io/force-ssl-redirect` | `"false"` | Prevents empty-host redirect loop |
| `nginx.ingress.kubernetes.io/backend-protocol` | `"HTTPS"` | Backend speaks HTTPS (Rancher, etc.) |
| `nginx.ingress.kubernetes.io/proxy-ssl-verify` | `"off"` | Backend uses self-signed cert |
| `nginx.ingress.kubernetes.io/proxy-body-size` | `"0"` | No upload size limit (Longhorn, files) |
| `nginx.ingress.kubernetes.io/proxy-read-timeout` | `"120"` | Slow backends (JupyterHub, API-heavy) |
| `nginx.ingress.kubernetes.io/use-regex` | `"true"` | Regex path with capture groups |
| `nginx.ingress.kubernetes.io/rewrite-target` | `/$2` | Strip path prefix |
| `nginx.ingress.kubernetes.io/affinity` | `"cookie"` | Session stickiness |
| `nginx.ingress.kubernetes.io/permanent-redirect` | URL | Nginx-level 301 redirect |

**Rule**: All annotation keys go in `values.yaml` or via `kubectl annotate`.
Never use `helm --set` or `--set-string` for annotation keys.

---

## Fleet GitRepo Deployment

Fleet deploys charts from git repos using `fleet.yaml` as the config layer.
Values in `fleet.yaml` are **pure YAML** — dotted annotation keys have no
escaping issue here because Fleet renders a values file, not `--set` flags.

### How values reach the chart

```
clusteros-helm-values (ConfigMap, fleet-default)
        │  fleet.yaml: helm.valuesFrom
        ▼
Fleet merges it as -f values into helm install
        │  merged with fleet.yaml: helm.values (chart-specific overrides)
        ▼
Chart templates: {{ .Values.global.clusterDomain }}
```

### `fleet.yaml`

Place this at the root of your git repo (or in the `path:` subdirectory
configured in `/etc/clusteros/gitops-repos.yaml`):

```yaml
# fleet.yaml
defaultNamespace: myapp

helm:
  # Pull cluster domain/mode from the ConfigMap node-agent publishes.
  # Fleet merges this first, then merges helm.values on top.
  valuesFrom:
  - configMapKeyRef:
      name: clusteros-helm-values
      namespace: fleet-local    # or fleet-default for remote-cluster GitRepos
      key: values.yaml

  # Chart-specific values — layered on top of valuesFrom.
  # Annotation keys with dots are fine here: this is YAML, not --set.
  values:
    ingress:
      enabled: true
      annotations:
        nginx.ingress.kubernetes.io/ssl-redirect: "false"
        nginx.ingress.kubernetes.io/affinity: "cookie"
        nginx.ingress.kubernetes.io/session-cookie-name: "MYAPP_STICKY"
        nginx.ingress.kubernetes.io/session-cookie-expires: "172800"
        nginx.ingress.kubernetes.io/session-cookie-max-age: "172800"
```

If your app has an HTTPS backend (like Rancher):

```yaml
  values:
    ingress:
      annotations:
        nginx.ingress.kubernetes.io/backend-protocol: "HTTPS"
        nginx.ingress.kubernetes.io/proxy-ssl-verify: "off"
        nginx.ingress.kubernetes.io/ssl-redirect: "false"
```

For an external chart (not bundled in the repo), add `helm.repo` and `helm.chart`:

```yaml
helm:
  repo: https://charts.example.com
  chart: myapp
  version: "1.2.3"
  releaseName: myapp
  valuesFrom:
  - configMapKeyRef:
      name: clusteros-helm-values
      namespace: fleet-local    # or fleet-default for remote-cluster GitRepos
      key: values.yaml
  values:
    ingress:
      enabled: true
```

### `values.yaml` (chart defaults)

Define defaults for everything Fleet or the user might override.
The `global` section mirrors what `clusteros-helm-values` provides:

```yaml
# values.yaml
global:
  clusterDomain: ""       # filled by clusteros-helm-values at deploy time
  clusterMode: "nip"      # "domain" | "nip"
  sslRedirect: "true"     # overridden to "false" when Cloudflare in front
  rancherHost: ""
  ingressAnnotations: {}  # overridden with ssl-redirect etc. by clusteros-helm-values

ingress:
  enabled: true
  annotations: {}         # chart-specific annotations from fleet.yaml helm.values
  host: ""                # fallback for manual deploy without Fleet
```

### Chart template `templates/ingress.yaml`

```yaml
{{- if .Values.ingress.enabled }}
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: {{ include "myapp.fullname" . }}
  namespace: {{ .Release.Namespace }}
  annotations:
    {{- /* global annotations from clusteros-helm-values (ssl-redirect, etc.) */}}
    {{- with .Values.global.ingressAnnotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
    {{- /* chart-specific annotations from fleet.yaml helm.values */}}
    {{- with .Values.ingress.annotations }}
    {{- toYaml . | nindent 4 }}
    {{- end }}
spec:
  ingressClassName: nginx
  rules:
  - host: {{ if .Values.global.clusterDomain -}}
      myapp.{{ .Values.global.clusterDomain }}
    {{- else if .Values.ingress.host -}}
      {{ .Values.ingress.host }}
    {{- else -}}
      myapp.cluster.local
    {{- end }}
    http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: {{ include "myapp.fullname" . }}
            port:
              number: 80
{{- end }}
```

The `toYaml | nindent 4` pattern renders any map of annotations cleanly with
no key-escaping needed — the template engine outputs them as-is.

### Reading domain in a pod at runtime

If the chart also needs the domain inside the container (e.g. for callback URLs),
inject it from `clusteros-config` rather than baking it in at Helm install time:

```yaml
# templates/deployment.yaml
env:
- name: CLUSTER_DOMAIN
  valueFrom:
    configMapKeyRef:
      name: clusteros-config
      namespace: kube-system
      key: domain
      optional: true    # pod still starts when no domain configured
- name: SSL_REDIRECT
  valueFrom:
    configMapKeyRef:
      name: clusteros-config
      namespace: kube-system
      key: ssl-redirect
      optional: true
```

### Namespace RBAC for cross-namespace ConfigMap reference

Fleet's `valuesFrom` only resolves ConfigMaps in the **same namespace as the
GitRepo**. Node-agent publishes `clusteros-helm-values` into both `fleet-local`
and `fleet-default`, so whichever namespace your GitRepo lives in, no extra RBAC
is needed — just reference the matching namespace.

The `clusteros-config` ConfigMap (in `kube-system`) is for pods and scripts only
— pods need a ServiceAccount with `get` on ConfigMaps in `kube-system` if they
read it via the API. Reading it as an env var via `valueFrom.configMapKeyRef`
works because kubelet resolves it during pod creation, not via the pod's SA.
