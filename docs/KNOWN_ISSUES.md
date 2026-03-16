# ClusterOS — Known Failing Issues

Tracked issues with root-cause analysis and workarounds.
This document is the authoritative list; cross-reference git history for fix status.

---

## 1. Rancher Helm install fails — IP address as ingress hostname

**Symptom**
```
Failed to deploy Rancher: helm install rancher: exit status 1:
  Ingress.networking.k8s.io "rancher" is invalid:
  spec.rules[0].host: Invalid value: "100.102.126.31":
  must be a DNS name, not an IP address
```

**Root cause**
`server.go:deployRancher()` passes `--set hostname=<TAILSCALE_IP>` to `helm install rancher`.
Rancher generates a Kubernetes Ingress resource with `spec.rules[0].host` set to the IP.
Kubernetes 1.28+ rejects raw IP addresses in `spec.rules[0].host` — it must be a DNS name.

**Impact**
Rancher is not deployed. The Rancher UI (`https://NODE:30444`) is inaccessible.
All other services (SLURM, Longhorn, ingress) are unaffected.

**Fix needed** (`server.go:2154`)
Replace the raw IP with an `nip.io` hostname that resolves back to the IP:
```go
// Instead of: rancherHost = ks.nodeIP
rancherHost = strings.ReplaceAll(ks.nodeIP, ".", "-") + ".nip.io"
```
`nip.io` is a public wildcard DNS service: `100-102-126-31.nip.io` → `100.102.126.31`.
No DNS infrastructure required, works on any machine with internet access.

**Workaround (manual)**
```bash
# On the leader node:
NODE_IP=$(tailscale ip -4)
RANCHER_HOST="${NODE_IP//./-}.nip.io"

helm install rancher rancher-stable/rancher \
  --namespace cattle-system --create-namespace \
  --set hostname=$RANCHER_HOST \
  --set bootstrapPassword=admin \
  --set ingress.tls.source=rancher \
  --set ingress.ingressClassName=nginx \
  --set replicas=1 \
  --set global.cattle.psp.enabled=false \
  --wait --timeout 5m

# Access Rancher at:
echo "https://$NODE_IP:30444"
```

---

## 2. MetalLB speaker CrashLoopBackOff — port 7946 conflict with Serf

**Symptom**
```
Error: Could not set up network transport: failed to obtain an address:
  Failed to start TCP listener on "100.102.126.31" port 7946:
  listen tcp 100.102.126.31:7946: bind: address already in use
```
All `metallb-system/speaker-*` pods loop in CrashLoopBackOff on every node.

**Root cause**
MetalLB's speaker uses [memberlist](https://github.com/hashicorp/memberlist) for gossip,
binding `NODE_IP:7946` (TCP+UDP). ClusterOS Serf also uses memberlist and is already
bound to `0.0.0.0:7946`. MetalLB's bind attempt fails on startup.

**Impact**
MetalLB LoadBalancer services are non-functional. **Not blocking** — ClusterOS uses
`NodePort` services exclusively, not `LoadBalancer`. MetalLB is not required.

**Fix options**

*Option A — Remove MetalLB entirely (recommended)*
MetalLB adds no value when all services use NodePort.
Remove `deployMetalLB()` call from `DeployClusterServices()` in `server.go`.

*Option B — Change Serf memberlist port*
Set Serf `BindPort=7947` in `serf.go` and update firewall rules. Requires coordinated
redeploy of all nodes.

*Option C — Configure MetalLB to use a different port*
In the MetalLB manifest, set `METALLB_ML_BIND_PORT` environment variable for the speaker:
```yaml
env:
- name: METALLB_ML_BIND_PORT
  value: "7947"
```

**Current state**: MetalLB is non-functional on all nodes. Workaround: ignore.

---

## 3. slurmdbd pod stuck in Init:0/1 — wait-for-db DNS failure

**Symptom**
`slurm/slurmdbd-*` pod stays in `Init:0/1` indefinitely.
The init container runs:
```sh
until nc -z slurmdbd-mysql 3306; do echo waiting for db; sleep 2; done
```

**Root cause**
`slurmdbd` Deployment has `dnsPolicy: None` with nameservers `8.8.8.8`/`1.1.1.1` only —
added to ensure `apt-get` works when CoreDNS isn't ready at pod startup. However this means
the init container (`busybox:1.36`) also uses only public DNS, which cannot resolve
`slurmdbd-mysql.slurm.svc.cluster.local` (a cluster-internal name). Result: `nc: bad
address` → init container loops forever.

**Fix** (`manifests/slurm/slurmdbd.yaml`)
Add k3s CoreDNS IP `10.43.0.10` as first nameserver (before public DNS). CoreDNS resolves
`.svc.cluster.local` names; public DNS remains as fallback for `apt-get` / external hosts:
```yaml
dnsConfig:
  nameservers:
  - 10.43.0.10   # k3s CoreDNS
  - 8.8.8.8
  - 1.1.1.1
```

**Workaround**
Manually restart the pod after cluster is fully stable (CoreDNS running):
```bash
sudo k3s kubectl -n slurm delete pod -l app=slurmdbd
```

---

## 4. slurmrestd and slurmweb pods — apt-get failure at startup (fixed in c0eabe9)

**Symptom** (pre-fix)
slurmrestd and slurmweb pods exit with code 127 (command not found) or get stuck trying
to run `apt-get install` which fails because CoreDNS isn't ready at pod startup.

**Root cause**
Both pods used `ubuntu:22.04` / `ubuntu:24.04` base images and ran `apt-get install`
as their startup command. Network/DNS failures inside pods (especially on first boot
when CoreDNS is still starting) caused the install to fail and the pods to crash.

**Fix (deployed in c0eabe9-dirty)**
- **slurmrestd**: Changed to `debian:bookworm-slim` + host `/usr` volume mount at
  `/hostfs/usr`. Binary at `/hostfs/usr/sbin/slurmrestd` is invoked directly.
  No network needed at startup.
- **slurmweb**: Changed to `python:3.12-slim` + host `/usr` volume mount.
  Python is pre-installed; only SLURM CLI tools come from the host mount.

**Verification** (run after cluster reaches phase=ready):
```bash
sudo k3s kubectl -n slurm get pods
# slurmrestd-* and slurmweb-* should be Running with 1/1 READY
```

**Note**: Both pods mount `/usr` from the host as read-only. They will break if
SLURM binaries are not installed on the host at `/usr/sbin/slurmrestd` etc.
`apply-patch.sh` installs `slurm-wlm` on every node to ensure this.

---

## 5. Rancher landing page link shows "placeholder" (fixed in c0eabe9)

**Symptom** (pre-fix)
Clicking the `/rancher` link on the ClusterOS landing page navigated to a literal
"PLACEHOLDER" URL or got a 301 redirect loop to `https://$host:30444`
(where `$host` was the literal string, not interpolated).

**Root cause**
A previous code iteration created a `cattle-system/rancher-path-redirect` Ingress with:
```yaml
nginx.ingress.kubernetes.io/temporal-redirect: "https://$host:30444"
```
nginx-ingress v1.12 does not interpolate `$host` as a variable in annotation values —
it treats it as a literal string. This created a redirect to `https://$host:30444`.
A debugging session also left a `permanent-redirect: "PLACEHOLDER:30444"` annotation
that got browser-cached as a 301.

**Fix (deployed in c0eabe9-dirty)**
Removed `rancherRedirectYAML` / `rancherNPYAML` ingress creation from `deployRancher()`.
Added cleanup to delete the stale ingress on every deploy:
```go
exec.Command("k3s", "kubectl", "-n", "cattle-system", "delete", "ingress",
    "rancher-path-redirect", "--ignore-not-found=true").Run()
```
The `/rancher` path is now handled by the Python default backend which redirects the
browser to `https://<actual-host>:30444` using `window.location.href` (reads from
`X-Forwarded-Host` header so the correct IP is always used).

---

## 6. make deploy copies files to ~/patch/patch/ instead of ~/patch/ (fixed in c0eabe9)

**Symptom**
After `make deploy`, nodes show old `Bundle: 19c4628-dirty` despite local staging having
`c0eabe9-dirty`. The new files land at `~/patch/patch/` (nested) instead of `~/patch/`.

**Root cause**
`scp -r patch/ user@node:~/patch/` — when `~/patch/` already exists on the remote,
SCP copies the directory *inside* the existing one: `~/patch/ → ~/patch/patch/`.
`apply-patch.sh` reads `$SCRIPT_DIR/VERSION` where `SCRIPT_DIR=~/patch` — sees old file.

**Fix (deployed in c0eabe9-dirty, Makefile)**
Deploy step now clears the remote directory before uploading:
```makefile
$(_SSH_AUTH) $(SSH_USER)@$$node 'rm -rf ~/patch && mkdir -p ~/patch' 2>/dev/null || true; \
$(_SCP_AUTH) -r patch/ $(SSH_USER)@$$node:~/ \
```
Destination changed from `:~/patch/` to `:~/` so `patch/` is created at the correct level.

---

## 7. node 100.106.90.86 — consistently unreachable

**Symptom**
SSH connection times out on every deploy attempt. Node never appears in cluster.

**Likely cause**
Node is powered off, or Tailscale has dropped its session and it has no LAN-reachable IP.

**Action needed**
- Check physical power state
- If on Tailscale: `tailscale status` — is it listed as online?
- If online on Tailscale but SSH fails: node may have firewall blocking SSH or
  node-agent may have crashed before firewall rules were installed
- Last resort: physical console access or USB re-flash

---

## 8. slurmrestd/slurmweb glibc mismatch — host binary in wrong container OS (fixed)

**Symptom**
```
sh: symbol lookup error: /hostfs/usr/lib/x86_64-linux-gnu/libc.so.6:
  undefined symbol: __tunable_is_initialized, version GLIBC_PRIVATE
python3: symbol lookup error: /hostfs/usr/lib/x86_64-linux-gnu/libc.so.6:
  undefined symbol: __nptl_change_stack_perm, version GLIBC_PRIVATE
```

**Root cause**
`debian:bookworm-slim` (glibc 2.36) was used as the container base, but the host runs
Ubuntu 24.04 (glibc 2.39). `LD_LIBRARY_PATH=/hostfs/usr/lib/x86_64-linux-gnu` caused
the container's dynamic linker (2.36) to load the host's glibc (2.39). glibc uses
`GLIBC_PRIVATE` internal symbols that changed between 2.36 and 2.39 — the 2.36 linker
can't load 2.39's libc.

**Fix**
Changed base image to `ubuntu:24.04` (same glibc as host). Container linker and host
libraries now match (both 2.39) — `LD_LIBRARY_PATH` works correctly.

---

## 9. nginx-ingress admission webhook 502 — blocks all Ingress creation (fixed)

**Symptom**
```
error when creating Ingress: Internal error occurred: failed calling webhook
  "validate.nginx.ingress.kubernetes.io":
  failed to call webhook: Post ".../networking/v1/ingresses?timeout=10s":
  proxy error ... code 502: 502 Bad Gateway
```
All ingress resources fail to create. Landing page shows only services with NodePort
(Longhorn, slurm), not services exposed only via Ingress (slurmweb, Rancher redirect).

**Root cause**
The ingress-nginx DaemonSet pod template was missing the `app.kubernetes.io/instance: ingress-nginx`
label. The `ingress-nginx-controller-admission` ClusterIP Service requires this label in
its selector, so it had 0 endpoints. Even after patching the service, the TLS certificate
from the certgen Job had expired/mismatched, causing 502.

**Fix**
1. Added `app.kubernetes.io/instance: ingress-nginx` to pod template labels in the DaemonSet.
2. `deployIngressNginx()` now unconditionally deletes the `ingress-nginx-admission`
   ValidatingWebhookConfiguration on every run (both first install and restarts).
   The validation webhook is not required for correct operation; its only value is
   catching misconfigured ingress YAML before creation, which isn't worth the
   operational complexity on a private cluster.

---

## 10. slurmrestd pod CrashLoopBackOff — binary not found (fixed in apply-patch.sh)

**Symptom**
```
/bin/sh: 1: exec: /hostfs/usr/sbin/slurmrestd: not found
```
`slurmrestd` pod exits immediately. The pod mounts the host's `/usr` at `/hostfs/usr` and
tries to exec `/hostfs/usr/sbin/slurmrestd`.

**Root cause**
The `slurmrestd` binary ships in a **separate** Ubuntu 24.04 package called `slurmrestd`
(in the `universe` repo). `slurm-wlm` does NOT include it. The binary is absent from the
host, so the pod's exec fails.

**Fix**
Add `slurmrestd` to the package install list in `apply-patch.sh`:
```bash
command -v slurmrestd &>/dev/null || PKGS="$PKGS slurmrestd"
```
After re-running `apply-patch.sh`, `/usr/sbin/slurmrestd` exists on the host. The pod
picks it up via the `/hostfs/usr` volume mount on the next restart.

**Workaround (manual)**
```bash
sudo apt-get install -y slurmrestd
sudo k3s kubectl -n slurm delete pod -l app=slurmrestd
```

---

## Status Summary

| # | Issue | Status | Blocking? |
|---|-------|--------|-----------|
| 1 | Rancher Helm install — IP as ingress host | **Fixed** — nip.io in latest build | — |
| 2 | MetalLB speaker port 7946 conflict | **Open** — remove MetalLB or change port | No (NodePort not LoadBalancer) |
| 3 | slurmdbd Init:0/1 — DNS/FQDN in wait-for-db | **Fixed** — full FQDN in latest build | — |
| 4 | slurmrestd/slurmweb apt-get at startup | **Fixed** in c0eabe9 | — |
| 5 | Landing page /rancher placeholder redirect | **Fixed** in c0eabe9 | — |
| 6 | make deploy copies to ~/patch/patch/ | **Fixed** in c0eabe9 | — |
| 7 | node 100.106.90.86 unreachable | **Unknown** — needs physical check | No |
| 8 | slurmrestd/slurmweb glibc mismatch (debian vs ubuntu host) | **Fixed** — ubuntu:24.04 base | — |
| 9 | nginx-ingress admission webhook 502 blocks ingress creation | **Fixed** — webhook deleted on deploy | — |
| 10 | slurmrestd pod — binary not found (`slurm-wlm` doesn't include it) | **Fixed** — `slurmrestd` pkg added to apply-patch.sh | — |
