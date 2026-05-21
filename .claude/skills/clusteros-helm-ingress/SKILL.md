---
name: clusteros-helm-ingress
description: Deploy Helm charts on ClusterOS with correct domain resolution, nginx ingress annotations, and session affinity — without annotation escaping bugs.
origin: project
---

# ClusterOS Helm Ingress Skill

Guides deploying Helm charts or writing service deployers in Go on the ClusterOS
cluster, covering domain resolution, annotation safety, and session stickiness.

## When to Activate

- Adding a new Helm chart to the cluster services in `server.go`
- Writing Go code that calls `helm install` or `kubectl apply` for an Ingress
- Troubleshooting "Invalid username or password" / session drop after pod restart
- Any Ingress that needs nginx annotations (ssl-redirect, backend-protocol, affinity)
- Deploying an app that should live at `<subdomain>.<clusterdomain>`

## Domain Resolution

The cluster domain comes from `/etc/clusteros/cloudflare.env` → `CLOUDFLARE_DOMAIN=`.
Read it in Go with `readCloudflareDomain()`. From shell or CI:

```bash
# From the clusteros-config ConfigMap (accessible from any pod or kubectl client)
DOMAIN=$(k3s kubectl get cm clusteros-config -n kube-system \
           -o jsonpath='{.data.domain}' 2>/dev/null)
HOST="${DOMAIN:+myapp.${DOMAIN}}"
# nip.io fallback when no domain configured:
[ -z "$HOST" ] && HOST="$(k3s kubectl get nodes \
  -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}' \
  | tr '.' '-').nip.io"
```

In Go (follow node-agent pattern):

```go
host := "myapp." + domain
if domain == "" {
    host = strings.ReplaceAll(nodeIP, ".", "-") + ".nip.io"
}
```

## The Annotation Escaping Rule

**Never use `helm --set` or `--set-string` for annotation keys.**

`nginx.ingress.kubernetes.io/backend-protocol` contains dots that Helm parses as
nested key separators. `\.` escaping only works when the string reaches Helm's parser
unmodified — shell quoting and chart `extraAnnotations` layers silently break it.
The node-agent code itself gave up on this and always falls back to `kubectl patch`.

## Three Safe Patterns

### 1. Values file (preferred)

```bash
cat > /tmp/myapp-values.yaml <<EOF
ingress:
  enabled: true
  ingressClassName: nginx
  host: "${HOST}"
  annotations:
    nginx.ingress.kubernetes.io/ssl-redirect: "false"
    nginx.ingress.kubernetes.io/affinity: "cookie"
    nginx.ingress.kubernetes.io/session-cookie-name: "MYAPP_STICKY"
EOF
helm install myapp stable/myapp -n myapp --create-namespace \
  -f /tmp/myapp-values.yaml --wait --timeout 5m0s
```

### 2. Post-install kubectl annotate

```bash
helm install myapp stable/myapp -n myapp --create-namespace --wait
k3s kubectl annotate ingress myapp -n myapp \
  nginx.ingress.kubernetes.io/backend-protocol=HTTPS \
  nginx.ingress.kubernetes.io/proxy-ssl-verify=off \
  nginx.ingress.kubernetes.io/ssl-redirect=false \
  --overwrite
```

### 3. Disable chart ingress, apply your own

```bash
helm install myapp stable/myapp -n myapp --set ingress.enabled=false --wait
k3s kubectl apply -f ingress.yaml   # full YAML, no escaping needed
```

## Go Deployer Pattern (node-agent style)

```go
func deployMyApp(nodeIP, domain string) error {
    host := "myapp." + domain
    if domain == "" {
        host = strings.ReplaceAll(nodeIP, ".", "-") + ".nip.io"
    }
    values := fmt.Sprintf(`
ingress:
  enabled: true
  ingressClassName: nginx
  host: %q
  annotations:
    nginx.ingress.kubernetes.io/ssl-redirect: "false"
`, host)
    f, _ := os.CreateTemp("", "myapp-values-*.yaml")
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
    // Post-install patch for annotations the extraAnnotations layer dropped
    exec.Command("k3s", "kubectl", "patch", "ingress", "myapp",
        "-n", "myapp", "--type=json",
        `-p=[{"op":"add","path":"/metadata/annotations/nginx.ingress.kubernetes.io~1affinity","value":"cookie"}]`,
    ).Run()
    return nil
}
```

Note: JSON patch encodes `/` in annotation keys as `~1`.

## Session Affinity (always add for login-gated apps)

```yaml
annotations:
  nginx.ingress.kubernetes.io/affinity: "cookie"
  nginx.ingress.kubernetes.io/session-cookie-name: "MYAPP_INGRESSCOOKIE"
  nginx.ingress.kubernetes.io/session-cookie-expires: "172800"
  nginx.ingress.kubernetes.io/session-cookie-max-age: "172800"
  nginx.ingress.kubernetes.io/session-cookie-samesite: "None"
  nginx.ingress.kubernetes.io/session-cookie-secure: "true"
```

Without this, nginx round-robins pods and users get logged out mid-session.

## Annotation Cheatsheet

| Annotation (suffix) | Value | When |
|---|---|---|
| `ssl-redirect` | `"false"` | Cloudflare terminates TLS (always in domain mode) |
| `force-ssl-redirect` | `"false"` | Prevents empty-host redirect loop |
| `backend-protocol` | `"HTTPS"` | Backend speaks HTTPS (Rancher) |
| `proxy-ssl-verify` | `"off"` | Backend has self-signed cert |
| `proxy-body-size` | `"0"` | No upload limit (Longhorn, file apps) |
| `proxy-read-timeout` | `"120"` | Slow backends |
| `use-regex` | `"true"` | Regex path with capture groups |
| `rewrite-target` | `/$2` | Strip path prefix |
| `affinity` | `"cookie"` | Session stickiness |

All keys prefixed with `nginx.ingress.kubernetes.io/`.

## Full Reference

See `docs/HELM_INGRESS_DOMAIN_GUIDE.md` for the complete guide with decision trees,
subdomain vs path-based comparison, and all patterns with full examples.
