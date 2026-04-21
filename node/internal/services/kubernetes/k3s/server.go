package k3s

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/cluster-os/node/internal/roles"
	"github.com/sirupsen/logrus"
)

//go:embed manifests/slurm/slurmdbd.yaml
var slurmdbdManifest []byte

// K3sServer manages the k3s server process on the elected leader node.
// It implements roles.Role for health checking.
// Startup is triggered by the daemon phase machine — not by a leadership callback.
type K3sServer struct {
	*roles.BaseRole
	nodeIP              string
	serverURL           string // non-empty = joining mode (--server URL); empty = leader (--cluster-init)
	joinToken           string // join token for joining mode
	dataDir             string
	tokenPath           string
	k3sCmd              *exec.Cmd
	manifestsDir        string
	slurmdbdDeployed    bool
	slurmRestDeployed   bool
	servicesDeployed    bool
	startCount          int // number of times Start() has been called
}

// NewK3sServerRole creates a K3sServer for the cluster leader (--cluster-init).
// The nodeIP is the Tailscale/LAN IP to bind to.
func NewK3sServerRole(nodeIP string, logger *logrus.Logger) *K3sServer {
	return &K3sServer{
		BaseRole:     roles.NewBaseRole("k3s-server", logger),
		nodeIP:       nodeIP,
		dataDir:      "/var/lib/rancher/k3s",
		tokenPath:    "/var/lib/rancher/k3s/server/token",
		manifestsDir: "/var/lib/cluster-os/k8s-manifests",
	}
}

// NewK3sServerRoleJoining creates a K3sServer for a secondary control-plane node.
// It joins an existing k3s cluster (embedded etcd) instead of initialising one.
// The node runs both the k3s server AND kubelet/kube-proxy (--disable-agent=false
// is the k3s default), so it continues to schedule workloads alongside CP duties.
func NewK3sServerRoleJoining(nodeIP, serverURL, joinToken string, logger *logrus.Logger) *K3sServer {
	return &K3sServer{
		BaseRole:     roles.NewBaseRole("k3s-server", logger),
		nodeIP:       nodeIP,
		serverURL:    serverURL,
		joinToken:    joinToken,
		dataDir:      "/var/lib/rancher/k3s",
		tokenPath:    "/var/lib/rancher/k3s/server/token",
		manifestsDir: "/var/lib/cluster-os/k8s-manifests",
	}
}

// Start starts k3s server either as the cluster leader (--cluster-init) or as a
// secondary control-plane node (--server URL --token TOKEN), depending on whether
// serverURL is set. Called once by the phase machine per leadership/CP term.
func (ks *K3sServer) Start() error {
	if ks.serverURL != "" {
		ks.Logger().Infof("Starting k3s server (secondary CP, joining %s)", ks.serverURL)
	} else {
		ks.Logger().Info("Starting k3s server (leader, --cluster-init)")
	}

	if err := os.MkdirAll(ks.dataDir, 0755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	ks.startCount++
	ks.killExistingK3s(ks.startCount == 1)

	if ks.nodeIP != "" {
		ks.Logger().Infof("Waiting for node IP %s to be bound...", ks.nodeIP)
		if err := ks.waitForIPReady(); err != nil {
			ks.Logger().Warnf("IP readiness: %v (proceeding anyway)", err)
		}
	}

	if err := ks.resetEtcdIfStale(); err != nil {
		ks.Logger().Warnf("etcd reset: %v", err)
	}

	// Determine the address k3s will advertise in the kubeconfig.
	// If no Tailscale IP is available yet, fall back to 127.0.0.1 so the
	// kubeconfig always gets a connectable address instead of 0.0.0.0.
	advertiseAddr := ks.nodeIP
	if advertiseAddr == "" {
		advertiseAddr = "127.0.0.1"
	}

	args := []string{
		"server",
		"--data-dir", ks.dataDir,
		"--disable", "servicelb",
		"--disable", "traefik",
		"--snapshotter", "native",
		// Use the official k8s pause image from registry.k8s.io instead of the
		// default rancher/mirrored-pause from Docker Hub.  Docker Hub (registry-1.docker.io)
		// is frequently rate-limited or unreachable; registry.k8s.io is CDN-backed
		// and accessible from most networks.  Every pod sandbox requires this image,
		// so a pull failure here blocks ALL pod scheduling.
		"--pause-image", "registry.k8s.io/pause:3.6",
		// Write the kubeconfig world-readable so non-root users (e.g. the
		// 'clusteros' service account running 'cluster test') can call kubectl
		// without sudo.  All nodes are on a trusted Tailscale mesh so this is safe.
		"--write-kubeconfig-mode", "0644",
		// Always set an explicit advertise address so the kubeconfig server URL
		// is never written as 0.0.0.0:6443 (happens when nodeIP is empty and
		// --bind-address defaults to 0.0.0.0, making the kubeconfig useless).
		"--advertise-address", advertiseAddr,
		"--bind-address", "0.0.0.0",
		"--tls-san", advertiseAddr,
		"--tls-san", "127.0.0.1",
	}

	// Leader initialises the embedded etcd cluster; secondary CP servers join it.
	if ks.serverURL != "" {
		// Wipe stale server-side state from a previous leadership term.
		// k3s refuses to start in joining mode if local TLS/cred files are
		// newer than the remote datastore, producing a fatal "could cause a
		// cluster outage" error. Removing them here lets k3s re-fetch from
		// the existing etcd cluster, which is always safe on a CP join.
		for _, dir := range []string{
			filepath.Join(ks.dataDir, "server/tls"),
			filepath.Join(ks.dataDir, "server/cred"),
			filepath.Join(ks.dataDir, "server/db"),
		} {
			if _, err := os.Stat(dir); err == nil {
				ks.Logger().Infof("Removing stale server state before CP join: %s", dir)
				if err := os.RemoveAll(dir); err != nil {
					ks.Logger().Warnf("Failed to remove %s: %v", dir, err)
				}
			}
		}
		args = append(args, "--server", ks.serverURL, "--token", ks.joinToken)
	} else {
		args = append(args, "--cluster-init")
	}

	if ks.nodeIP != "" {
		args = append(args, "--node-ip", ks.nodeIP)
		if lanIP := detectLANIP(); lanIP != "" {
			args = append(args, "--tls-san", lanIP)
		}
	}
	// Note: --flannel-iface tailscale0 is set via /etc/rancher/k3s/config.yaml.
	// Do NOT add it here to avoid duplicate flags.

	// etcd-arg is server-only and must NOT be in config.yaml (shared with k3s agent).
	// Passing it to k3s agent causes a fatal "flag provided but not defined" crash-loop.
	// Inject it here for the server command line only.
	args = append(args, "--etcd-arg=--max-request-bytes=8388608")

	cmd := exec.Command("k3s", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("k3s start: %w", err)
	}

	ks.k3sCmd = cmd
	ks.SetRunning(true)
	ks.Logger().Infof("k3s server started (PID %d)", cmd.Process.Pid)

	// Reap the process when it exits so HealthCheck can detect death via nil check.
	// Without this, k3s becomes a zombie on exit: kill(pid,0) on a zombie returns 0
	// (success), so Signal(0) never errors and HealthCheck never triggers a restart.
	go func(c *exec.Cmd) {
		c.Wait()
		ks.Logger().Warnf("k3s server (PID %d) exited — HealthCheck will restart", c.Process.Pid)
		if ks.k3sCmd == c {
			ks.k3sCmd = nil
			ks.SetRunning(false)
		}
	}(cmd)

	// Pre-seed the pause image into k3s's containerd immediately after startup.
	// We wait for the containerd socket, pull registry.k8s.io/pause:3.6 (Google CDN —
	// never touches Docker Hub), and tag it as docker.io/rancher/mirrored-pause:3.6.
	// This runs in the background so Start() returns immediately; the image will be
	// present well before kubelet begins scheduling the first pod sandbox.
	go ensurePauseImageInContainerd(ks.Logger())

	// Patch the kubeconfig as soon as k3s writes it — independent of WaitForAPIReady.
	// WaitForAPIReady also calls patchKubeconfigForLocalAccess(), but if it times out
	// the kubeconfig would be left with the raw advertise-address (Tailscale IP) rather
	// than 127.0.0.1, making local kubectl calls fail when Tailscale self-routing breaks.
	// This goroutine polls for the file and patches it the moment it appears, so the
	// fix applies even when the API is slow to start.
	go ks.watchAndPatchKubeconfig()

	return nil
}

// watchAndPatchKubeconfig polls for the k3s kubeconfig and patches the server URL
// to 127.0.0.1:6443 as soon as the file appears.  This is a belt-and-suspenders
// complement to the patchKubeconfigForLocalAccess() call in WaitForAPIReady.
func (ks *K3sServer) watchAndPatchKubeconfig() {
	const kubeconfigPath = "/etc/rancher/k3s/k3s.yaml"
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(kubeconfigPath); err == nil {
			ks.patchKubeconfigForLocalAccess()
			return
		}
		time.Sleep(5 * time.Second)
	}
	ks.Logger().Warn("watchAndPatchKubeconfig: kubeconfig not found after 10 min")
}

// ensurePauseImageInContainerd waits for k3s's embedded containerd socket to appear,
// then pulls registry.k8s.io/pause:3.6 and tags it as docker.io/rancher/mirrored-pause:3.6.
//
// Why: k3s generates a containerd config.toml on each start, but the store is empty on
// a fresh boot.  Kubelet begins scheduling pods almost immediately; if the pause image
// isn't already in the store, containerd tries to pull rancher/mirrored-pause:3.6 from
// Docker Hub — which is frequently unreachable.  Pulling from registry.k8s.io (Google CDN)
// is always available.  The docker.io alias ensures pods whose specs still reference the
// old name also find the image locally without any network pull.
//
// Called as a goroutine from both K3sServer.Start() and K3sAgent.Start().
func ensurePauseImageInContainerd(log *logrus.Logger) {
	const (
		sock       = "/run/k3s/containerd/containerd.sock"
		pauseImg   = "registry.k8s.io/pause:3.6"
		pauseAlias = "docker.io/rancher/mirrored-pause:3.6"
	)

	// Wait up to 2 min for the containerd socket to appear.
	for i := 0; i < 60; i++ {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if _, err := os.Stat(sock); err != nil {
		log.Warn("ensurePauseImage: containerd socket not ready after 120s — skipping")
		return
	}

	// Fast path: image already in the local containerd store — just add the
	// docker.io alias and return, zero network required.
	if out, err := exec.Command("k3s", "ctr", "images", "tag", "--force",
		pauseImg, pauseAlias).CombinedOutput(); err == nil {
		log.Infof("Pause image alias set from local cache: %s → %s", pauseImg, pauseAlias)
		return
	} else {
		log.Debugf("ensurePauseImage: local tag failed (%s) — trying airgap tarball", strings.TrimSpace(string(out)))
	}

	// Airgap path: import from the tarball placed by apply-patch.sh.
	// k3s has its own preload loop that reads agent/images/*.tar at startup, but
	// that loop is asynchronous and may not have run by the time we get here.
	// Importing directly is instant and requires zero network access.
	const airgapTar = "/var/lib/rancher/k3s/agent/images/pause-3.6.tar"
	if _, err := os.Stat(airgapTar); err == nil {
		if out, err := exec.Command("k3s", "ctr", "images", "import", airgapTar).CombinedOutput(); err == nil {
			exec.Command("k3s", "ctr", "images", "tag", "--force", pauseImg, pauseAlias).Run() //nolint:errcheck
			log.Infof("Pause image imported from airgap tarball and tagged: %s → %s", pauseImg, pauseAlias)
			return
		} else {
			log.Debugf("ensurePauseImage: airgap import failed (%s) — falling back to network pull", strings.TrimSpace(string(out)))
		}
	}

	// Slow path: not cached and no airgap tarball.  Pull from registry.k8s.io
	// (Google CDN) — never touches Docker Hub which is frequently unavailable.
	if out, err := exec.Command("k3s", "ctr", "images", "pull", pauseImg).CombinedOutput(); err != nil {
		log.Warnf("ensurePauseImage: pull %s failed: %v: %s", pauseImg, err, strings.TrimSpace(string(out)))
		return
	}

	// Tag as the docker.io alias so pods referencing the old rancher name find it locally.
	exec.Command("k3s", "ctr", "images", "tag", "--force", pauseImg, pauseAlias).Run() //nolint:errcheck
	log.Infof("Pause image pulled and cached: %s → %s", pauseImg, pauseAlias)
}

// WaitForAPIReady blocks until the K3s API server responds to kubectl, or timeout.
func (ks *K3sServer) WaitForAPIReady(timeout time.Duration) error {
	ks.Logger().Infof("Waiting up to %s for K3s API server to be ready...", timeout)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if exec.Command("k3s", "kubectl", "get", "nodes").Run() == nil {
			ks.Logger().Info("K3s API server is ready")
			ks.patchKubeconfigForLocalAccess()
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("K3s API server not ready after %s", timeout)
}

// patchKubeconfigForLocalAccess rewrites the k3s kubeconfig server URL to use
// 127.0.0.1 instead of the Tailscale IP. When --node-ip is passed, k3s writes
// the kubeconfig with the node IP as the server address. Connecting to the own
// Tailscale IP from the same node may fail if local self-routing is broken, but
// 127.0.0.1:6443 always works because --bind-address is set to 0.0.0.0.
func (ks *K3sServer) patchKubeconfigForLocalAccess() {
	const kubeconfigPath = "/etc/rancher/k3s/k3s.yaml"
	data, err := os.ReadFile(kubeconfigPath)
	if err != nil {
		ks.Logger().Debugf("kubeconfig patch: read %s: %v", kubeconfigPath, err)
		return
	}
	lines := strings.Split(string(data), "\n")
	changed := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "server: https://") && strings.Contains(trimmed, ":6443") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			newLine := indent + "server: https://127.0.0.1:6443"
			if lines[i] != newLine {
				lines[i] = newLine
				changed = true
			}
		}
	}
	if !changed {
		return
	}
	if err := os.WriteFile(kubeconfigPath, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		ks.Logger().Warnf("kubeconfig patch: write: %v", err)
		return
	}
	ks.Logger().Info("Patched k3s kubeconfig server URL → https://127.0.0.1:6443")
}

// ReadToken reads the cluster join token from disk, retrying up to 60s.
func (ks *K3sServer) ReadToken() (string, error) {
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(ks.tokenPath); err == nil && len(data) > 0 {
			token := strings.TrimSpace(string(data))
			ks.Logger().Infof("K3s token read (%d chars)", len(token))
			return token, nil
		}
		time.Sleep(2 * time.Second)
	}
	return "", fmt.Errorf("K3s token not available after 60s at %s", ks.tokenPath)
}

// ReadCACert returns the k3s cluster CA certificate PEM that the leader's server is using.
// Workers install this before starting the k3s agent so they trust the leader's TLS cert
// without needing the patch/k3s-ca.crt bundle (which may differ across installations).
func (ks *K3sServer) ReadCACert() ([]byte, error) {
	caPath := filepath.Join(ks.dataDir, "server/tls/server-ca.crt")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(caPath); err == nil && len(data) > 0 {
			ks.Logger().Infof("Read cluster CA cert (%d bytes) from %s", len(data), caPath)
			return data, nil
		}
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("cluster CA cert not available after 30s at %s", caPath)
}

// waitForAPIStable waits until the k3s API is consistently reachable (3 successes
// with 2s gaps) before proceeding with deployment. This guards against a transient
// "connection refused" that occurs when k3s restarts its API server internally ~5s
// after startup (TLS handshake warm-up, etcd compaction, etc.).
func (ks *K3sServer) waitForAPIStable() {
	consec := 0
	for consec < 3 {
		if exec.Command("k3s", "kubectl", "get", "nodes", "--request-timeout=5s").Run() == nil {
			consec++
		} else {
			consec = 0
		}
		time.Sleep(2 * time.Second)
	}
	ks.Logger().Info("k3s API stable — proceeding with service deployment")
}

// DeployClusterServices installs Longhorn, nginx-ingress, cert-manager, Rancher, slurmdbd,
// and the SLURM REST API. Called as a goroutine from the phase machine after K3s API is ready.
func (ks *K3sServer) DeployClusterServices(mungeKey []byte) {
	ks.Logger().Info("Starting cluster services deployment...")
	// Wait for the API to be consistently reachable before deploying. k3s sometimes
	// drops connections for ~5s after the first "ready" probe due to internal TLS
	// handshake warm-up and etcd initialisation.
	ks.waitForAPIStable()

	if err := ks.deploySlurmdbd(mungeKey); err != nil {
		ks.Logger().Warnf("Failed to deploy slurmdbd: %v", err)
	}

	if err := ks.deployMetalLB(); err != nil {
		ks.Logger().Warnf("MetalLB unavailable (%v) — services remain on NodePort", err)
	}

	// Deploy nginx-ingress as a DaemonSet with hostNetwork so every node binds
	// port 80/443 and NodePorts 30080/30443.  Rancher, Longhorn, and SLURM REST
	// are all routed via this ingress controller.
	if err := ks.deployIngressNginx(); err != nil {
		ks.Logger().Warnf("nginx-ingress unavailable (%v) — Rancher/Longhorn ingress won't work", err)
	}

	// Deploy a ClusterOS landing page as the nginx default backend so that any
	// unmatched request (e.g. http://IP:30080/) shows a useful page with service links
	// instead of the bare nginx 404.
	ks.deployClusterOSLandingPage()

	// Wait for nginx-ingress controller to be ready before deploying ingress resources.
	ks.Logger().Info("Waiting 30s for nginx-ingress to be ready before deploying ingress resources...")
	time.Sleep(30 * time.Second)

	if err := ks.deployLonghorn(); err != nil {
		ks.Logger().Warnf("Failed to deploy Longhorn: %v", err)
	} else {
		// Register each node's extra disks with Longhorn now that the CRDs are installed.
		// We wait briefly for the Longhorn node controller to create Node resources.
		go ks.registerAllNodesExtraDisks()
	}
	// Always ensure the Longhorn ingress exists — deployLonghorn() returns early when
	// Longhorn is already installed, so createLonghornIngress() would be skipped on
	// restarts without this unconditional call.
	ks.createLonghornIngress()

	if err := ks.deployCertManager(); err != nil {
		ks.Logger().Warnf("Failed to deploy cert-manager: %v (skipping Rancher)", err)
		// Still deploy SLURM REST even if Rancher fails.
		ks.deploySLURMRestAPI(mungeKey)
		ks.servicesDeployed = true
		return
	}

	if err := ks.deployRancher(); err != nil {
		ks.Logger().Warnf("Failed to deploy Rancher: %v", err)
	} else {
		// Apply declarative Helm repos — runs every startup so repos survive full rebuilds.
		// Blocks briefly (waits for CRD) but runs after deployRancher so Rancher is up.
		go ks.applyRancherCatalogs()

		// Install rancher-backup operator and configure scheduled snapshots.
		// Runs in background; also triggers auto-restore if a backup exists in the PVC.
		go ks.deployRancherBackup()
	}

	// NOTE: we deliberately do NOT deploy a path:/ catchall Rancher ingress here.
	// The landing page is already wired as the nginx --default-backend-service, so
	// unmatched routes return the auto-discovery page.  A path:/ Rancher catchall
	// would swallow every request (including /longhorn, /slurm) and return 504
	// whenever Rancher is slow or unhealthy.  Rancher stays accessible via /rancher
	// (redirect → $host:30444) and directly at https://NODE_IP:30444.
	//
	// Delete the catchall if it was deployed by a previous version of node-agent.
	exec.Command("k3s", "kubectl", "-n", "cattle-system", "delete", "ingress",
		"rancher-catchall", "--ignore-not-found=true").Run()

	ks.deploySLURMRestAPI(mungeKey)

	ks.servicesDeployed = true
	ks.Logger().Info("Cluster services deployment complete")

	// Re-apply firewall rules now that kube-proxy, MetalLB, and ingress-nginx have
	// all added their own iptables rules. Without this, the NodePort range and other
	// ports may be blocked on the leader's LAN interface because kube-proxy's rule
	// ordering interleaves with iptables rules applied at daemon startup.
	ks.refreshFirewallRules()
}

// HealthCheck checks if k3s server process is alive, restarting if needed.
func (ks *K3sServer) HealthCheck() error {
	if ks.k3sCmd == nil || ks.k3sCmd.Process == nil {
		ks.Logger().Warn("k3s server not running — attempting restart")
		return ks.Start()
	}
	if err := ks.k3sCmd.Process.Signal(syscall.Signal(0)); err != nil {
		ks.Logger().Warnf("k3s server process died: %v — restarting", err)
		ks.k3sCmd.Wait()
		ks.k3sCmd = nil
		ks.SetRunning(false)
		return ks.Start()
	}
	return nil
}

// Stop terminates the k3s server process.
func (ks *K3sServer) Stop(ctx context.Context) error {
	ks.Logger().Info("Stopping k3s server")
	cmd := ks.k3sCmd // capture before reaper goroutine can nil it
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		cmd.Process.Kill()
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		<-done
	}
	ks.k3sCmd = nil
	ks.SetRunning(false)
	return nil
}

// resetEtcdIfStale wipes etcd data if the stored IP marker differs from nodeIP.
// This prevents "not a member of cluster" errors on IP changes.
func (ks *K3sServer) resetEtcdIfStale() error {
	if ks.nodeIP == "" {
		return nil
	}
	etcdDir := filepath.Join(ks.dataDir, "server/db/etcd")
	markerFile := filepath.Join(etcdDir, ".cluster-os-ip")

	if data, err := os.ReadFile(markerFile); err == nil {
		storedIP := strings.TrimSpace(string(data))
		if storedIP != ks.nodeIP {
			ks.Logger().Infof("Etcd IP changed (%s → %s), resetting etcd data", storedIP, ks.nodeIP)
			if err := os.RemoveAll(etcdDir); err != nil {
				return fmt.Errorf("remove etcd dir: %w", err)
			}
		} else {
			return nil // IPs match, no reset needed
		}
	}

	// Write marker for next restart
	_ = os.MkdirAll(filepath.Dir(markerFile), 0755)
	return os.WriteFile(markerFile, []byte(ks.nodeIP), 0644)
}

// killExistingK3s kills any existing K3s/etcd processes and optionally orphaned containerd.
// killContainerd should be true only on the first start — on HealthCheck restarts we must
// NOT kill containerd because k3s will reuse the already-running instance and avoid the
// slow re-initialization.  If containerd is killed on restart, k3s starts it fresh and
// needs >75 s for it to become ready — longer than k3s's internal CRD registration timeout
// — causing a fatal crash loop.
func (ks *K3sServer) killExistingK3s(killContainerd bool) {
	// Kill any stale k3s agent processes first.
	//
	// When a node transitions from worker→leader (e.g. after re-election), the
	// old k3s agent process may still be running and holding port 6444 (the k3s
	// internal load balancer).  k3s server also binds 6444 — if the old agent
	// still owns it, the new server fails immediately with "bind: address already
	// in use" before we even get a chance to check port 2380.
	// Kill all k3s agent/server processes unconditionally and wait for 6444 to
	// be released before proceeding.
	ks.Logger().Info("Killing any stale k3s agent/server processes before server start")
	exec.Command("pkill", "-TERM", "-f", "k3s agent").Run()  //nolint:errcheck
	exec.Command("pkill", "-TERM", "-f", "k3s server").Run() //nolint:errcheck
	// Give them 3s to exit gracefully, then force-kill.
	for i := 0; i < 3; i++ {
		time.Sleep(1 * time.Second)
		agentGone, _ := exec.Command("pgrep", "-f", "k3s agent").Output()
		serverGone, _ := exec.Command("pgrep", "-f", "k3s server").Output()
		if len(agentGone) == 0 && len(serverGone) == 0 {
			break
		}
	}
	exec.Command("pkill", "-KILL", "-f", "k3s agent").Run()  //nolint:errcheck
	exec.Command("pkill", "-KILL", "-f", "k3s server").Run() //nolint:errcheck

	// Wait for port 6444 (internal load balancer) to be released.
	ks.Logger().Info("Waiting for port 6444 to be released...")
	for i := 0; i < 10; i++ {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:6444", 500*time.Millisecond)
		if err != nil {
			break // port free
		}
		conn.Close()
		time.Sleep(1 * time.Second)
	}

	// Only kill orphaned containerd on the FIRST start (killContainerd==true).
	//
	// On first start, an orphaned containerd from a prior k3s run may have a stale
	// in-memory config (wrong sandbox_image, old pause alias).  Killing it forces k3s
	// to start a fresh containerd that reads the current config.
	//
	// On HealthCheck restarts (killContainerd==false) we must NOT kill containerd.
	// When k3s exits with a fatal error it orphans its containerd child.  If we kill
	// that containerd too, k3s must re-initialize it from scratch on restart — a
	// process that takes >75 s (image store indexing etc.).  k3s's internal CRD
	// registration timeout is ~75 s, so the registration always fails → crash loop.
	// Leaving the already-warm containerd running lets k3s reconnect instantly and
	// complete CRD registration well within the timeout.
	if killContainerd {
		ks.Logger().Info("Killing any orphaned k3s containerd before first start")
		exec.Command("pkill", "-TERM", "-f", "/run/k3s/containerd/containerd").Run() //nolint:errcheck
		time.Sleep(1 * time.Second)
		exec.Command("pkill", "-KILL", "-f", "/run/k3s/containerd/containerd").Run() //nolint:errcheck
		os.Remove("/run/k3s/containerd/containerd.sock")                              //nolint:errcheck
	} else {
		ks.Logger().Info("Skipping containerd kill on restart — reusing running containerd instance")
	}

	// Check port 2380 (etcd) — run k3s-killall.sh if still bound.
	addrs := []string{"127.0.0.1:2380"}
	if ks.nodeIP != "" && ks.nodeIP != "127.0.0.1" {
		addrs = append(addrs, ks.nodeIP+":2380")
	}

	inUse := false
	for _, addr := range addrs {
		conn, err := net.DialTimeout("tcp", addr, 1*time.Second)
		if err == nil {
			conn.Close()
			inUse = true
			break
		}
	}
	if !inUse {
		return
	}

	ks.Logger().Warn("Port 2380 still in use — running k3s-killall.sh")
	if _, err := os.Stat("/usr/local/bin/k3s-killall.sh"); err == nil {
		exec.Command("/usr/local/bin/k3s-killall.sh").Run()
	} else {
		exec.Command("pkill", "-9", "-f", "k3s server").Run()
		exec.Command("pkill", "-9", "-f", "etcd").Run()
	}

	// Wait for the port to actually be released rather than using a fixed sleep.
	ks.Logger().Info("Waiting for port 2380 to be released...")
	for i := 0; i < 15; i++ {
		time.Sleep(1 * time.Second)
		released := true
		for _, addr := range addrs {
			conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
			if err == nil {
				conn.Close()
				released = false
				break
			}
		}
		if released {
			ks.Logger().Info("Port 2380 released — proceeding")
			return
		}
	}
	ks.Logger().Warn("Port 2380 still in use after 15s — proceeding anyway")
}

// waitForIPReady waits until the node IP is bound on a local interface
func (ks *K3sServer) waitForIPReady() error {
	for i := 0; i < 60; i++ {
		addrs, err := net.InterfaceAddrs()
		if err == nil {
			for _, addr := range addrs {
				if ipNet, ok := addr.(*net.IPNet); ok && ipNet.IP.String() == ks.nodeIP {
					return nil
				}
			}
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("IP %s not bound after 120s", ks.nodeIP)
}

// refreshFirewallRules re-applies the critical iptables/UFW rules on the leader node
// after all services are deployed. This is necessary because kube-proxy, MetalLB, and
// k3s add their own iptables rules during startup which can interleave with rules
// applied at daemon startup, sometimes pushing our ACCEPT rules below UFW's REJECT rule.
//
// Both UFW and iptables are updated: UFW is the authoritative ruleset when it is
// active, but iptables -I (insert at position 1) acts as a belt-and-suspenders
// guarantee that the rule fires before any REJECT even when UFW ordering drifts.
func (ks *K3sServer) refreshFirewallRules() {
	ks.Logger().Info("Refreshing firewall rules on leader (post-deployment)")

	rules := [][2]string{
		{"22", "tcp"},
		{"80", "tcp"},
		{"443", "tcp"},
		{"6443", "tcp"},
		{"6444", "tcp"},
		{"6817", "tcp"},
		{"6818", "tcp"},
		{"6819", "tcp"},
		{"7476", "tcp"},
		{"7946", "tcp"},
		{"7946", "udp"},
		{"8472", "udp"},
		{"10250", "tcp"},
		{"2379:2380", "tcp"},
		{"30000:32767", "tcp"},
		{"30000:32767", "udp"},
	}

	for _, r := range rules {
		port, proto := r[0], r[1]
		exec.Command("ufw", "allow", port+"/"+proto).Run()
		check := exec.Command("iptables", "-C", "INPUT", "-p", proto, "--dport", port, "-j", "ACCEPT")
		if check.Run() != nil {
			exec.Command("iptables", "-I", "INPUT", "1", "-p", proto, "--dport", port, "-j", "ACCEPT").Run()
		}
	}

	// Trust Tailscale interface (covers all overlay traffic without per-port rules).
	for _, iface := range []string{"tailscale0", "ts0"} {
		exec.Command("ufw", "allow", "in", "on", iface).Run()
	}
	checkCGNAT := exec.Command("iptables", "-C", "INPUT", "-s", "100.64.0.0/10", "-j", "ACCEPT")
	if checkCGNAT.Run() != nil {
		exec.Command("iptables", "-I", "INPUT", "1", "-s", "100.64.0.0/10", "-j", "ACCEPT").Run()
	}

	// Remove stale NAT REDIRECT rules for port 80/443 if they were added by a
	// previous version. Some older deployments created PREROUTING or OUTPUT
	// REDIRECTs (80→30080, 443→30443) which caused outbound HTTPS to be
	// redirected locally. Ensure both PREROUTING and OUTPUT entries are removed.
	for _, pair := range [][2]string{{"80", "30080"}, {"443", "30443"}} {
		from, to := pair[0], pair[1]
		// remove PREROUTING REDIRECT instances
		for {
			if exec.Command("iptables", "-t", "nat", "-C", "PREROUTING",
				"-p", "tcp", "--dport", from, "-j", "REDIRECT", "--to-ports", to).Run() != nil {
				break
			}
			exec.Command("iptables", "-t", "nat", "-D", "PREROUTING",
				"-p", "tcp", "--dport", from, "-j", "REDIRECT", "--to-ports", to).Run()
		}
		// remove OUTPUT REDIRECT instances (local processes rewriting outbound packets)
		for {
			if exec.Command("iptables", "-t", "nat", "-C", "OUTPUT",
				"-p", "tcp", "--dport", from, "-j", "REDIRECT", "--to-ports", to).Run() != nil {
				break
			}
			exec.Command("iptables", "-t", "nat", "-D", "OUTPUT",
				"-p", "tcp", "--dport", from, "-j", "REDIRECT", "--to-ports", to).Run()
		}
	}

	// FORWARD chain: allow pod and service CIDRs so NodePort DNAT'd packets pass
	// through to pods even when UFW's default FORWARD policy is DROP.
	exec.Command("sysctl", "-w", "net.ipv4.ip_forward=1").Run()
	for _, cidr := range []string{"10.42.0.0/16", "10.43.0.0/16"} {
		if exec.Command("iptables", "-C", "FORWARD", "-d", cidr, "-j", "ACCEPT").Run() != nil {
			exec.Command("iptables", "-I", "FORWARD", "1", "-d", cidr, "-j", "ACCEPT").Run()
		}
		if exec.Command("iptables", "-C", "FORWARD", "-s", cidr, "-j", "ACCEPT").Run() != nil {
			exec.Command("iptables", "-I", "FORWARD", "1", "-s", cidr, "-j", "ACCEPT").Run()
		}
	}
	for _, iface := range []string{"flannel.1", "cni0"} {
		exec.Command("ufw", "allow", "in", "on", iface).Run()
		if exec.Command("iptables", "-C", "FORWARD", "-i", iface, "-j", "ACCEPT").Run() != nil {
			exec.Command("iptables", "-I", "FORWARD", "1", "-i", iface, "-j", "ACCEPT").Run()
		}
		if exec.Command("iptables", "-C", "FORWARD", "-o", iface, "-j", "ACCEPT").Run() != nil {
			exec.Command("iptables", "-I", "FORWARD", "1", "-o", iface, "-j", "ACCEPT").Run()
		}
	}

	// Ensure nginx-ingress has hostNetwork=true. Runs after pod rollout completes.
	// Belt-and-suspenders: deployIngressNginx already calls this, but refreshFirewallRules
	// is also called at end of DeployClusterServices so one of the two calls will win.
	ks.patchNginxHostNetwork()

	ks.Logger().Info("Firewall rules refreshed — NodePorts confirmed active (hostNetwork nginx, flannel VXLAN, pod CIDR FORWARD)")
}

// detectLANSubnet returns the LAN IP plus a MetalLB pool in the upper end of
// the detected /24 subnet (192.168.1.220-192.168.1.250).
// Returns empty strings if no suitable interface is found.
func detectLANSubnet() (lanIP, poolStart, poolEnd string) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", "", ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if strings.HasPrefix(iface.Name, "tailscale") || strings.HasPrefix(iface.Name, "ts") ||
			strings.HasPrefix(iface.Name, "wg") || strings.HasPrefix(iface.Name, "docker") ||
			strings.HasPrefix(iface.Name, "veth") || strings.HasPrefix(iface.Name, "br-") ||
			strings.HasPrefix(iface.Name, "cni") || strings.HasPrefix(iface.Name, "flannel") {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipNet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
				continue // Skip Tailscale CGNAT range
			}
			// Use the first three octets of the /24 (or wider subnet) for the pool.
			// Always target the last /24 block the IP sits in.
			pool := fmt.Sprintf("%d.%d.%d", ip4[0], ip4[1], ip4[2])
			return ip4.String(), pool + ".220", pool + ".250"
		}
	}
	return "", "", ""
}

// detectLANIP returns the primary non-Tailscale, non-loopback IPv4 address
func detectLANIP() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if strings.HasPrefix(iface.Name, "tailscale") || strings.HasPrefix(iface.Name, "ts") ||
			strings.HasPrefix(iface.Name, "wg") || strings.HasPrefix(iface.Name, "docker") ||
			strings.HasPrefix(iface.Name, "veth") || strings.HasPrefix(iface.Name, "br-") ||
			strings.HasPrefix(iface.Name, "cni") || strings.HasPrefix(iface.Name, "flannel") {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok {
				ip4 := ipNet.IP.To4()
				if ip4 != nil && !ip4.IsLoopback() {
					if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 {
						continue // Skip Tailscale CGNAT range
					}
					return ip4.String()
				}
			}
		}
	}
	return ""
}

// ── Deployment functions ─────────────────────────────────────────────────────

// deployMetalLB installs MetalLB in Layer2 mode and assigns an IP pool from the
// upper range of the detected LAN /24 subnet (e.g. 192.168.1.220-192.168.1.250).
// The MetalLB speaker memberlist port is moved from 7946 (conflicts with Serf)
// to 7476. On failure the caller falls back to NodePort-only access.
func (ks *K3sServer) deployMetalLB() error {
	alreadyInstalled := exec.Command("k3s", "kubectl", "-n", "metallb-system",
		"get", "deployment", "controller").Run() == nil

	if !alreadyInstalled {
		ks.Logger().Info("Installing MetalLB (Layer2 mode)...")

		const metallbURL = "https://raw.githubusercontent.com/metallb/metallb/v0.14.8/config/manifests/metallb-native.yaml"
		if out, err := exec.Command("k3s", "kubectl", "apply", "-f", metallbURL).CombinedOutput(); err != nil {
			return fmt.Errorf("apply metallb manifest: %w: %s", err, out)
		}

		// Create the memberlist secret that MetalLB speakers require to form their gossip mesh.
		// Without this, speakers stay in ContainerCreating indefinitely on fresh installs.
		if exec.Command("k3s", "kubectl", "-n", "metallb-system", "get", "secret", "memberlist").Run() != nil {
			keyBytes, err := exec.Command("openssl", "rand", "-base64", "128").Output()
			if err != nil {
				return fmt.Errorf("generate memberlist key: %w", err)
			}
			createCmd := exec.Command("k3s", "kubectl", "-n", "metallb-system", "create", "secret",
				"generic", "memberlist", "--from-literal=secretkey="+strings.TrimSpace(string(keyBytes)))
			if out, err := createCmd.CombinedOutput(); err != nil {
				ks.Logger().Warnf("MetalLB memberlist secret creation failed: %v: %s (continuing)", err, out)
			} else {
				ks.Logger().Info("MetalLB memberlist secret created")
			}
		}

		// Wait for controller Deployment to be ready (up to 3 min).
		ks.Logger().Info("Waiting up to 3m for MetalLB controller to be ready...")
		if out, err := exec.Command("k3s", "kubectl", "-n", "metallb-system",
			"rollout", "status", "deployment/controller", "--timeout=180s").CombinedOutput(); err != nil {
			return fmt.Errorf("metallb controller not ready: %w: %s", err, out)
		}

		// Detect LAN subnet and apply IP pool CRs.
		_, poolStart, poolEnd := detectLANSubnet()
		if poolStart == "" {
			return fmt.Errorf("no LAN interface detected — cannot configure MetalLB IP pool")
		}
		ks.Logger().Infof("MetalLB IP pool: %s-%s", poolStart, poolEnd)

		poolYAML := fmt.Sprintf(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata:
  name: lan-pool
  namespace: metallb-system
spec:
  addresses:
  - %s-%s
`, poolStart, poolEnd)
		cmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(poolYAML)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("apply IPAddressPool: %w: %s", err, out)
		}

		const l2AdvYAML = `apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata:
  name: lan-advertisement
  namespace: metallb-system
spec:
  ipAddressPools:
  - lan-pool
`
		cmd2 := exec.Command("k3s", "kubectl", "apply", "-f", "-")
		cmd2.Stdin = strings.NewReader(l2AdvYAML)
		if out, err := cmd2.CombinedOutput(); err != nil {
			return fmt.Errorf("apply L2Advertisement: %w: %s", err, out)
		}

		ks.Logger().Infof("MetalLB installed — pool %s-%s, L2 advertisement active", poolStart, poolEnd)
	} // end if !alreadyInstalled

	// Always ensure the speaker memberlist port is 7476 (not 7946 which conflicts with Serf).
	// Runs on every leader start so existing installs are also fixed.
	// kubectl set env is the reliable way to add/update env vars without touching other fields.
	if out, err := exec.Command("k3s", "kubectl", "set", "env",
		"daemonset/speaker", "-n", "metallb-system",
		"METALLB_ML_BIND_ADDR=0.0.0.0",
		"METALLB_ML_BIND_PORT=7476").CombinedOutput(); err != nil {
		ks.Logger().Warnf("MetalLB speaker port patch: %v: %s (continuing)", err, out)
	} else {
		ks.Logger().Info("MetalLB speaker memberlist port set to 7476 (Serf port 7946 freed)")
	}

	// Wait for speaker DaemonSet rollout.
	exec.Command("k3s", "kubectl", "-n", "metallb-system",
		"rollout", "status", "daemonset/speaker", "--timeout=120s").Run()

	return nil
}

// getIngressVIP returns the external IP assigned by MetalLB to ingress-nginx-controller,
// or an empty string if MetalLB is not deployed or no IP has been assigned yet.
func (ks *K3sServer) getIngressVIP() string {
	out, err := exec.Command("k3s", "kubectl", "get", "svc",
		"ingress-nginx-controller", "-n", "ingress-nginx",
		"-o", `jsonpath={.status.loadBalancer.ingress[0].ip}`).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (ks *K3sServer) deploySlurmdbd(mungeKey []byte) error {
	if ks.slurmdbdDeployed {
		return nil
	}
	ks.Logger().Info("Deploying slurmdbd to Kubernetes")

	if err := os.MkdirAll(ks.manifestsDir, 0755); err != nil {
		return fmt.Errorf("create manifests dir: %w", err)
	}

	manifestPath := filepath.Join(ks.manifestsDir, "slurmdbd.yaml")
	if err := os.WriteFile(manifestPath, slurmdbdManifest, 0644); err != nil {
		return fmt.Errorf("write slurmdbd manifest: %w", err)
	}

	if len(mungeKey) > 0 {
		if err := ks.createMungeKeySecret(mungeKey); err != nil {
			ks.Logger().Warnf("Munge key secret: %v (continuing)", err)
		}
	}

	cmd := exec.Command("k3s", "kubectl", "apply", "-f", manifestPath)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply slurmdbd: %w: %s", err, string(output))
	}

	ks.slurmdbdDeployed = true
	ks.Logger().Info("slurmdbd deployed successfully")
	return nil
}

func (ks *K3sServer) createMungeKeySecret(mungeKey []byte) error {
	// Create slurm namespace
	nsCmd := exec.Command("k3s", "kubectl", "create", "namespace", "slurm", "--dry-run=client", "-o", "yaml")
	nsYaml, _ := nsCmd.Output()
	applyNs := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	applyNs.Stdin = strings.NewReader(string(nsYaml))
	applyNs.Run()

	tmpFile, err := os.CreateTemp("", "munge-key-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.Write(mungeKey)
	tmpFile.Close()

	cmd := exec.Command("k3s", "kubectl", "create", "secret", "generic", "munge-key",
		"--namespace", "slurm",
		"--from-file=munge.key="+tmpFile.Name(),
		"--dry-run=client", "-o", "yaml")
	secretYaml, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("generate secret yaml: %w", err)
	}

	applyCmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	applyCmd.Stdin = strings.NewReader(string(secretYaml))
	if output, err := applyCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply munge secret: %w: %s", err, string(output))
	}
	return nil
}

// nginxDaemonSetYAML is applied after the upstream manifest installs RBAC/ConfigMap/webhook.
// We replace the single-replica Deployment with a DaemonSet so nginx runs on EVERY node
// (leader + all workers). With hostNetwork=true the pod binds directly to ports 80/443
// in the host network namespace — nmap and browsers see them as open on every node IP.
// tolerations:[{operator:Exists}] ensures scheduling on all nodes regardless of taints.
const nginxDaemonSetYAML = `
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: ingress-nginx-controller
  namespace: ingress-nginx
  labels:
    app.kubernetes.io/name: ingress-nginx
    app.kubernetes.io/part-of: ingress-nginx
    app.kubernetes.io/component: controller
spec:
  selector:
    matchLabels:
      app.kubernetes.io/name: ingress-nginx
      app.kubernetes.io/component: controller
  template:
    metadata:
      labels:
        app.kubernetes.io/name: ingress-nginx
        app.kubernetes.io/instance: ingress-nginx
        app.kubernetes.io/component: controller
    spec:
      serviceAccountName: ingress-nginx
      hostNetwork: true
      dnsPolicy: ClusterFirstWithHostNet
      tolerations:
      - operator: Exists
      terminationGracePeriodSeconds: 300
      containers:
      - name: controller
        image: %s
        args:
        - /nginx-ingress-controller
        - --election-id=ingress-nginx-leader
        - --controller-class=k8s.io/ingress-nginx
        - --ingress-class=nginx
        - --configmap=$(POD_NAMESPACE)/ingress-nginx-controller
        env:
        - name: POD_NAME
          valueFrom:
            fieldRef:
              fieldPath: metadata.name
        - name: POD_NAMESPACE
          valueFrom:
            fieldRef:
              fieldPath: metadata.namespace
        - name: LD_PRELOAD
          value: /usr/local/lib/libmimalloc.so
        ports:
        - name: http
          containerPort: 80
          protocol: TCP
        - name: https
          containerPort: 443
          protocol: TCP
        - name: metrics
          containerPort: 10254
          protocol: TCP
        - name: webhook
          containerPort: 8443
          protocol: TCP
        readinessProbe:
          httpGet:
            path: /healthz
            port: 10254
          initialDelaySeconds: 10
          periodSeconds: 10
          failureThreshold: 5
        livenessProbe:
          httpGet:
            path: /healthz
            port: 10254
          initialDelaySeconds: 10
          periodSeconds: 10
          failureThreshold: 5
        resources:
          requests:
            cpu: 100m
            memory: 90Mi
        securityContext:
          allowPrivilegeEscalation: true
          capabilities:
            add:
            - NET_BIND_SERVICE
            drop:
            - ALL
          runAsUser: 101
`

func (ks *K3sServer) deployIngressNginx() error {
	// If the DaemonSet is already deployed we are done — idempotent.
	dsExists := exec.Command("k3s", "kubectl", "-n", "ingress-nginx",
		"get", "daemonset", "ingress-nginx-controller").Run() == nil
	if dsExists {
		ks.Logger().Debug("nginx-ingress DaemonSet already installed")
		// Always delete the admission webhook — it frequently breaks (no endpoints,
		// 502 Bad Gateway on TLS) and blocks ALL ingress creation silently.
		// nginx-ingress works correctly without it; validation is nice-to-have only.
		exec.Command("k3s", "kubectl", "delete", "validatingwebhookconfiguration",
			"ingress-nginx-admission", "--ignore-not-found=true").Run()
		ks.patchIngressForMetalLB()
		return nil
	}

	ks.Logger().Info("Installing nginx ingress (RBAC, ConfigMap, webhook)...")
	nginxURL := "https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.12.0/deploy/static/provider/baremetal/deploy.yaml"
	if output, err := exec.Command("k3s", "kubectl", "apply", "-f", nginxURL).CombinedOutput(); err != nil {
		return fmt.Errorf("apply nginx-ingress manifest: %w: %s", err, string(output))
	}

	// Clean up any admission Jobs stuck in CreateContainerError (e.g. corrupt image layers).
	// The certgen Jobs are one-shot; deleting and re-applying the manifest recreates them.
	ks.fixStuckAdmissionJobs()

	// Wait for the upstream Deployment pod so we can read its exact image tag/digest.
	exec.Command("k3s", "kubectl", "-n", "ingress-nginx",
		"rollout", "status", "deployment/ingress-nginx-controller", "--timeout=3m").Run()

	imgOut, _ := exec.Command("k3s", "kubectl", "-n", "ingress-nginx", "get", "deploy",
		"ingress-nginx-controller", "-o", "jsonpath={.spec.template.spec.containers[0].image}").Output()
	nginxImage := strings.TrimSpace(string(imgOut))
	if nginxImage == "" {
		nginxImage = "registry.k8s.io/ingress-nginx/controller:v1.12.0"
	}
	ks.Logger().Infof("nginx-ingress image: %s", nginxImage)

	// Replace the Deployment with a DaemonSet.
	// Single-replica Deployment → only one node gets port 80/443.
	// DaemonSet → every node gets port 80/443 (hostNetwork binds directly).
	exec.Command("k3s", "kubectl", "-n", "ingress-nginx", "delete", "deploy",
		"ingress-nginx-controller", "--ignore-not-found=true").Run()

	ks.Logger().Infof("Creating nginx-ingress DaemonSet (hostNetwork=true, all nodes)...")
	cmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(fmt.Sprintf(nginxDaemonSetYAML, nginxImage))
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply nginx-ingress DaemonSet: %w: %s", err, out)
	}

	// Wait for the DaemonSet to roll out on the leader before continuing.
	exec.Command("k3s", "kubectl", "-n", "ingress-nginx",
		"rollout", "status", "daemonset/ingress-nginx-controller", "--timeout=5m").Run()

	// Pin stable NodePorts on the Service (kept for backwards-compat / worker DNAT fallback).
	patchJSON := `{"spec":{"ports":[{"name":"http","port":80,"targetPort":"http","nodePort":30080,"protocol":"TCP"},{"name":"https","port":443,"targetPort":"https","nodePort":30443,"protocol":"TCP"}]}}`
	exec.Command("k3s", "kubectl", "-n", "ingress-nginx", "patch", "svc", "ingress-nginx-controller",
		"--type=merge", "-p", patchJSON).Run()

	ks.Logger().Info("nginx-ingress DaemonSet ready — port 80/443 bound on every cluster node")

	// Delete the admission webhook — it uses a certgen Job that only runs once and
	// its TLS cert gets out of sync with rolling pod replacements.  Without this
	// deletion, kubectl apply for any Ingress resource will hit a 502 Bad Gateway
	// from the broken webhook and silently fail.
	exec.Command("k3s", "kubectl", "delete", "validatingwebhookconfiguration",
		"ingress-nginx-admission", "--ignore-not-found=true").Run()

	// Upgrade to LoadBalancer type if MetalLB is ready.
	ks.patchIngressForMetalLB()
	return nil
}

// fixStuckAdmissionJobs detects ingress-nginx admission Jobs whose pods are stuck in
// CreateContainerError (typically due to corrupt containerd image layers from a previous
// failed pull). For each stuck Job it:
//  1. Evicts the corrupt image from the containerd content store.
//  2. Deletes the Job (k8s will not automatically recreate one-shot Jobs, so the
//     manifest must be re-applied — the caller does that via the existing apply step).
func (ks *K3sServer) fixStuckAdmissionJobs() {
	// List all Jobs in ingress-nginx namespace.
	out, err := exec.Command("k3s", "kubectl", "-n", "ingress-nginx",
		"get", "jobs", "-o", "jsonpath={.items[*].metadata.name}").Output()
	if err != nil {
		return
	}
	for _, jobName := range strings.Fields(string(out)) {
		// Find pods belonging to this Job.
		podOut, _ := exec.Command("k3s", "kubectl", "-n", "ingress-nginx",
			"get", "pods", "-l", "job-name="+jobName,
			"-o", "jsonpath={range .items[*]}{.metadata.name}={.status.containerStatuses[0].state.waiting.reason}{\"\\n\"}{end}").Output()
		stuck := false
		for _, line := range strings.Split(string(podOut), "\n") {
			if strings.Contains(line, "CreateContainerError") || strings.Contains(line, "ErrImagePull") || strings.Contains(line, "ImagePullBackOff") {
				stuck = true
				// Try to extract image from pod and evict from containerd.
				parts := strings.SplitN(line, "=", 2)
				podName := parts[0]
				imgOut, _ := exec.Command("k3s", "kubectl", "-n", "ingress-nginx",
					"get", "pod", podName,
					"-o", "jsonpath={.spec.containers[0].image}").Output()
				image := strings.TrimSpace(string(imgOut))
				if image != "" {
					ks.Logger().Infof("Evicting corrupt image %s from containerd for Job %s", image, jobName)
					exec.Command("k3s", "ctr", "images", "rm", image).Run()
				}
			}
		}
		if stuck {
			ks.Logger().Infof("Deleting stuck admission Job %s (will be recreated on next apply)", jobName)
			exec.Command("k3s", "kubectl", "-n", "ingress-nginx", "delete", "job", jobName, "--ignore-not-found=true").Run()
		}
	}
}

// patchNginxHostNetwork ensures any existing nginx-ingress controller (DaemonSet or
// legacy Deployment) has hostNetwork=true. Called from refreshFirewallRules as a
// belt-and-suspenders check after all services deploy.
func (ks *K3sServer) patchNginxHostNetwork() {
	for _, kind := range []string{"daemonset", "deployment"} {
		if exec.Command("k3s", "kubectl", "-n", "ingress-nginx",
			"get", kind, "ingress-nginx-controller").Run() != nil {
			continue
		}
		out, _ := exec.Command("k3s", "kubectl", "-n", "ingress-nginx", "get", kind,
			"ingress-nginx-controller", "-o", "jsonpath={.spec.template.spec.hostNetwork}").Output()
		if strings.TrimSpace(string(out)) == "true" {
			ks.Logger().Debugf("nginx-ingress %s already has hostNetwork=true", kind)
			return
		}
		ks.Logger().Infof("Patching nginx-ingress %s: hostNetwork=true", kind)
		mergePatch := `{"spec":{"template":{"spec":{"hostNetwork":true,"dnsPolicy":"ClusterFirstWithHostNet"}}}}`
		exec.Command("k3s", "kubectl", "-n", "ingress-nginx", "patch", kind,
			"ingress-nginx-controller", "--type=merge", "-p", mergePatch).Run()
		rolloutTarget := kind + "/ingress-nginx-controller"
		exec.Command("k3s", "kubectl", "-n", "ingress-nginx",
			"rollout", "status", rolloutTarget, "--timeout=3m").Run()
		return
	}
}

// patchIngressForMetalLB upgrades the ingress-nginx-controller Service from NodePort
// to LoadBalancer if MetalLB is deployed. Idempotent — safe to call multiple times.
func (ks *K3sServer) patchIngressForMetalLB() {
	if exec.Command("k3s", "kubectl", "-n", "metallb-system",
		"get", "deployment", "controller").Run() != nil {
		return // MetalLB not deployed
	}

	// Check current service type — skip if already LoadBalancer.
	out, err := exec.Command("k3s", "kubectl", "get", "svc",
		"ingress-nginx-controller", "-n", "ingress-nginx",
		"-o", `jsonpath={.spec.type}`).Output()
	if err != nil {
		return
	}
	if strings.TrimSpace(string(out)) == "LoadBalancer" {
		// Already LoadBalancer; check if VIP is assigned.
		if vip := ks.getIngressVIP(); vip != "" {
			ks.Logger().Debugf("ingress-nginx already has MetalLB VIP: %s", vip)
		}
		return
	}

	ks.Logger().Info("MetalLB detected — patching ingress-nginx-controller to LoadBalancer type")
	lbPatch := `[{"op":"replace","path":"/spec/type","value":"LoadBalancer"}]`
	if out, err := exec.Command("k3s", "kubectl", "patch", "svc",
		"ingress-nginx-controller", "-n", "ingress-nginx",
		"--type=json", "-p", lbPatch).CombinedOutput(); err != nil {
		ks.Logger().Warnf("patch ingress-nginx-controller to LoadBalancer: %v: %s", err, out)
		return
	}

	// Poll up to 2 min for VIP assignment.
	ks.Logger().Info("Waiting up to 2m for MetalLB to assign VIP to ingress-nginx...")
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if vip := ks.getIngressVIP(); vip != "" {
			ks.Logger().Infof("MetalLB VIP assigned: %s — cluster accessible on http://%s and https://%s",
				vip, vip, vip)
			return
		}
		time.Sleep(5 * time.Second)
	}
	ks.Logger().Warn("MetalLB VIP not assigned after 2m — ingress-nginx remains on NodePort fallback")
}

func (ks *K3sServer) deployLonghorn() error {
	if exec.Command("k3s", "kubectl", "-n", "longhorn-system", "get", "deployment", "longhorn-driver-deployer").Run() == nil {
		return nil // already installed
	}
	ks.Logger().Info("Installing Longhorn distributed storage...")

	longhornURL := "https://raw.githubusercontent.com/longhorn/longhorn/v1.7.2/deploy/longhorn.yaml"
	cmd := exec.Command("k3s", "kubectl", "apply", "-f", longhornURL)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply longhorn: %w: %s", err, string(output))
	}

	exec.Command("k3s", "kubectl", "-n", "longhorn-system",
		"rollout", "status", "deployment/longhorn-driver-deployer", "--timeout=180s").Run()

	// Set as default StorageClass
	patch := `{"metadata":{"annotations":{"storageclass.kubernetes.io/is-default-class":"true"}}}`
	exec.Command("k3s", "kubectl", "patch", "storageclass", "longhorn", "-p", patch).Run()

	ks.exposeLonghornUI()
	ks.createLonghornIngress()
	ks.Logger().Info("Longhorn storage ready (NodePort 30900)")
	return nil
}

// registerAllNodesExtraDisks waits for Longhorn Node resources to appear (one per k8s
// node) then patches each one with the extra disk paths discovered by apply-patch.sh.
// Disk paths follow the standard pattern /mnt/clusteros/disk-N so we only need to store
// the count (ndisks Serf tag) to reconstruct them, keeping the Serf tag budget tight.
func (ks *K3sServer) registerAllNodesExtraDisks() {
	// Give Longhorn node controller time to create Node resources.
	time.Sleep(60 * time.Second)

	// Get all k8s nodes and their Longhorn counterparts.
	out, err := exec.Command("k3s", "kubectl", "get", "nodes", "-o",
		"jsonpath={range .items[*]}{.metadata.name}{'\\n'}{end}").Output()
	if err != nil {
		ks.Logger().Warnf("[longhorn] Could not list k8s nodes: %v", err)
		return
	}

	for _, nodeName := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		nodeName = strings.TrimSpace(nodeName)
		if nodeName == "" {
			continue
		}
		// Read the ndisks annotation set by node-agent via apply-patch.sh output.
		// We stored ndisks as a k8s node label so we don't need SSH.
		labelOut, err := exec.Command("k3s", "kubectl", "get", "node", nodeName,
			"-o", "jsonpath={.metadata.labels.clusteros-ndisks}").Output()
		if err != nil {
			continue
		}
		ndisksStr := strings.TrimSpace(string(labelOut))
		ndisks := 0
		if ndisksStr != "" {
			fmt.Sscanf(ndisksStr, "%d", &ndisks)
		}
		if ndisks == 0 {
			continue
		}
		ks.registerNodeDisksWithLonghorn(nodeName, ndisks)
	}
}

// registerNodeDisksWithLonghorn patches the Longhorn Node resource for nodeName
// to add extra disk paths /mnt/clusteros/disk-0 … disk-(n-1).
func (ks *K3sServer) registerNodeDisksWithLonghorn(nodeName string, n int) {
	// Check Longhorn Node resource exists.
	if exec.Command("k3s", "kubectl", "-n", "longhorn-system", "get",
		"node.longhorn.io", nodeName).Run() != nil {
		ks.Logger().Debugf("[longhorn] Node resource for %s not yet ready — skipping disk registration", nodeName)
		return
	}

	// Build the disk map JSON.
	diskEntries := ""
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("/mnt/clusteros/disk-%d", i)
		key := fmt.Sprintf("clusteros-disk-%d", i)
		if diskEntries != "" {
			diskEntries += ","
		}
		diskEntries += fmt.Sprintf(`"%s":{"path":"%s","allowScheduling":true,"storageReserved":1073741824,"tags":["extra"]}`,
			key, path)
	}
	patch := fmt.Sprintf(`{"spec":{"disks":{%s}}}`, diskEntries)

	out, err := exec.Command("k3s", "kubectl", "-n", "longhorn-system",
		"patch", "node.longhorn.io", nodeName,
		"--type=merge", "-p", patch).CombinedOutput()
	if err != nil {
		ks.Logger().Warnf("[longhorn] Failed to register disks for node %s: %v — %s", nodeName, err, string(out))
		return
	}
	ks.Logger().Infof("[longhorn] Registered %d extra disk(s) for node %s", n, nodeName)
}

func (ks *K3sServer) exposeLonghornUI() {
	if exec.Command("k3s", "kubectl", "-n", "longhorn-system", "get", "svc", "longhorn-frontend-nodeport").Run() == nil {
		return
	}
	svcYAML := `apiVersion: v1
kind: Service
metadata:
  name: longhorn-frontend-nodeport
  namespace: longhorn-system
spec:
  type: NodePort
  selector:
    app: longhorn-ui
  ports:
  - name: http
    port: 80
    targetPort: 8000
    nodePort: 30900
`
	cmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(svcYAML)
	cmd.Run()
}

func (ks *K3sServer) createLonghornIngress() {
	// rewrite-target strips the /longhorn prefix before forwarding to the Longhorn UI,
	// which serves its assets from /, not from /longhorn.
	ingressYAML := `apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: longhorn-ingress
  namespace: longhorn-system
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: "0"
    nginx.ingress.kubernetes.io/use-regex: "true"
    nginx.ingress.kubernetes.io/rewrite-target: /$2
spec:
  ingressClassName: nginx
  rules:
  - http:
      paths:
      - path: /longhorn(/|$)(.*)
        pathType: ImplementationSpecific
        backend:
          service:
            name: longhorn-frontend
            port:
              number: 80
`
	cmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(ingressYAML)
	cmd.Run()
}

// deployClusterOSLandingPage deploys a Python HTTP server that auto-discovers every
// Kubernetes NodePort service via the in-cluster API on each page load.  It requires
// a ClusterRole so it can list services across all namespaces.  The page auto-refreshes
// every 20 s, so new services appear automatically as the cluster converges.
func (ks *K3sServer) deployClusterOSLandingPage() {
	// Always re-apply the ConfigMap so script changes take effect on existing clusters.
	// The Deployment and RBAC are idempotent (kubectl apply is a no-op when unchanged).
	ks.Logger().Info("Deploying ClusterOS dynamic landing page (auto-discovers all NodePort services)...")

	// RBAC — ServiceAccount + ClusterRole + ClusterRoleBinding.
	const rbacYAML = `apiVersion: v1
kind: ServiceAccount
metadata:
  name: clusteros-landing
  namespace: ingress-nginx
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: clusteros-landing-reader
rules:
- apiGroups: [""]
  resources: ["services"]
  verbs: ["get", "list"]
- apiGroups: ["networking.k8s.io"]
  resources: ["ingresses"]
  verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: clusteros-landing-reader
subjects:
- kind: ServiceAccount
  name: clusteros-landing
  namespace: ingress-nginx
roleRef:
  kind: ClusterRole
  name: clusteros-landing-reader
  apiGroup: rbac.authorization.k8s.io`

	// Python HTTP server.  Written at natural Python indentation — the YAML block-scalar
	// indent (4 spaces) is added programmatically below so the source stays readable.
	// CSS {{/}} in the f-string are Python's way of writing literal { } inside an f-string.
	const pythonScript = `#!/usr/bin/env python3
"""ClusterOS dynamic landing page — auto-discovers every Kubernetes NodePort service."""
import http.server, json, ssl, urllib.request, os, socket

TOKEN_FILE  = "/var/run/secrets/kubernetes.io/serviceaccount/token"
CACERT_FILE = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
NODE_IP     = os.environ.get("NODE_IP", "localhost")  # fallback only

# Use KUBERNETES_SERVICE_HOST env var (always injected by k3s into every pod)
# so we bypass CoreDNS entirely.  This prevents readiness-probe timeouts after
# cluster resets when CoreDNS is still catching up.
_K8S_HOST = os.environ.get("KUBERNETES_SERVICE_HOST", "kubernetes.default.svc")
_K8S_PORT = os.environ.get("KUBERNETES_SERVICE_PORT_HTTPS",
            os.environ.get("KUBERNETES_SERVICE_PORT", "443"))
API_BASE   = f"https://{_K8S_HOST}:{_K8S_PORT}"

# Human-readable labels for known ingress paths.
INGRESS_LABELS = {
    "/longhorn":  ("Longhorn Storage",     "Storage UI"),
    "/rancher":   ("Rancher",              "K8s Management UI"),
    "/slurm":     ("SLURM Monitor",        "Job Queue & Nodes"),
    "/slurm/api": ("SLURM REST API",       "REST / JSON"),
    "/jupyter":   ("JupyterHub",           "Notebook UI"),
}

# NodePort to probe (TCP connect) for each ingress path.
# Used to show live up/down status on the landing page.
PROBE_PORT = {
    "/rancher":   30444,
    "/longhorn":  30900,
    "/slurm/api": 30819,
    "/jupyter":   30888,
}

# Rancher API endpoint — discover deployed workloads automatically
RANCHER_NAMESPACE = os.environ.get("RANCHER_NAMESPACE", "cattle-system")
RANCHER_API_URL = f"https://{_K8S_HOST}:{_K8S_PORT}/apis/management.cattle.io/v3"


def port_open(host, port, timeout=2):
    """TCP connect check — fast, no TLS handshake needed."""
    try:
        with socket.create_connection((host, int(port)), timeout=timeout):
            return True
    except OSError:
        return False


def k8s_get(path):
    token = open(TOKEN_FILE).read().strip()
    ctx   = ssl.create_default_context(cafile=CACERT_FILE)
    req   = urllib.request.Request(
        API_BASE + path,
        headers={"Authorization": "Bearer " + token},
    )
    with urllib.request.urlopen(req, context=ctx, timeout=5) as r:
        return json.loads(r.read())


def collect_ingresses():
    """Return list of (path, label, desc) for every ingress rule."""
    try:
        items = k8s_get("/apis/networking.k8s.io/v1/ingresses").get("items", [])
    except Exception:
        return []
    seen, rows = set(), []
    for ing in items:
        for rule in ing.get("spec", {}).get("rules", []):
            for po in rule.get("http", {}).get("paths", []):
                raw = po.get("path", "/")
                # strip regex suffixes like (/|$)(.*) to get the clean prefix
                clean = raw.split("(")[0].rstrip("/") or "/"
                if clean in seen:
                    continue
                seen.add(clean)
                label, desc = INGRESS_LABELS.get(clean, (clean, "Web service"))
                rows.append((clean, label, desc))
    rows.sort()
    return rows


def collect_rancher_workloads(req_host):
    """Discover all Rancher-deployed workloads via API.
    
    Returns list of (ns, name, type, np, url, up) tuples for services with NodePorts.
    Falls back to accessing via Rancher's proxy if no direct NodePort.
    """
    rows = []
    try:
        # Query Rancher for deployed workloads (Deployments, StatefulSets, DaemonSets)
        for wl_type in ["deployments", "statefulsets", "daemonsets"]:
            try:
                wls = k8s_get(f"/api/v1/{wl_type}").get("items", [])
            except:
                continue
            
            for wl in wls:
                ns = wl.get("metadata", {}).get("namespace", "")
                name = wl.get("metadata", {}).get("name", "")
                
                # Skip system namespaces and cluster-os services
                if ns.startswith("kube-") or ns.startswith("cattle-"):
                    continue
                if name.startswith("coredns") or name.startswith("flannel"):
                    continue
                
                # Look for associated service with NodePort
                try:
                    svc_list = k8s_get(f"/api/v1/namespaces/{ns}/services").get("items", [])
                    for svc in svc_list:
                        svc_name = svc.get("metadata", {}).get("name", "")
                        if svc.get("spec", {}).get("type") != "NodePort":
                            continue
                        
                        for p in svc.get("spec", {}).get("ports", []):
                            np = p.get("nodePort")
                            if not np:
                                continue
                            scheme = "https" if np in (30443, 30444) else "http"
                            up = port_open(req_host, np)
                            rows.append((ns, svc_name, wl_type.rstrip("s"), np, 
                                       f"{scheme}://{req_host}:{np}", up))
                except:
                    pass
    except:
        pass
    
    return rows


def collect_nodeports(req_host):
    try:
        items   = k8s_get("/api/v1/services").get("items", [])
        api_err = None
    except Exception as exc:
        items, api_err = [], str(exc)
    # Skip services that already have a dedicated ingress entry above.
    SKIP_NP = {30819}  # slurmrestd REST — exposed via /slurm/api ingress
    rows = []
    for svc in items:
        ns   = svc["metadata"]["namespace"]
        name = svc["metadata"]["name"]
        if svc.get("spec", {}).get("type") != "NodePort":
            continue
        for p in svc.get("spec", {}).get("ports", []):
            np = p.get("nodePort")
            if not np or np in SKIP_NP:
                continue
            scheme = "https" if np in (30443, 30444) else "http"
            up = port_open(req_host, np)
            rows.append((ns, name, np, p.get("name", ""), f"{scheme}://{req_host}:{np}", up))
    rows.sort()
    return rows, api_err


def status_dot(up):
    """Colored circle: green = up, red = down/starting."""
    if up is True:
        return "<span class='dot up' title='Service responding'>&#9679;</span>"
    if up is False:
        return "<span class='dot down' title='Not yet reachable — may still be deploying'>&#9679;</span>"
    return "<span class='dot unknown' title='Status unknown'>&#9679;</span>"


def build_page(req_host):
    ingresses         = collect_ingresses()
    nodeports, np_err = collect_nodeports(req_host)
    rancher_wls       = collect_rancher_workloads(req_host)

    # Web interfaces section (ingress-based) with live status dots.
    ing_rows = ""
    for path, label, desc in ingresses:
        probe = PROBE_PORT.get(path)
        up    = port_open(req_host, probe) if probe else None
        dot   = status_dot(up)
        dim   = " style='opacity:.45'" if up is False else ""
        ing_rows += (
            f"<tr>"
            f"<td>{dot} <b><a href='{path}' target='_blank'{dim}>{label}</a></b></td>"
            f"<td><span class='desc'>{desc}</span></td>"
            f"<td><a href='{path}' target='_blank'{dim}>{path}</a></td>"
            f"</tr>"
        )
    if not ing_rows:
        ing_rows = "<tr><td colspan='3' class='empty'>No ingress services yet — cluster may still be provisioning</td></tr>"

    # NodePort direct-access section.
    np_rows = ""
    for ns, name, np, pn, url, up in nodeports:
        label = f"{name} / {pn}" if pn else name
        dot   = status_dot(up)
        dim   = " style='opacity:.45'" if not up else ""
        np_rows += (
            f"<tr>"
            f"<td><span class='ns'>{ns}</span></td>"
            f"<td>{dot} <b>{label}</b></td>"
            f"<td><a href='{url}' target='_blank'{dim}>:{np}</a></td>"
            f"</tr>"
        )
    
    # Rancher-deployed workloads section.
    rancher_rows = ""
    for ns, name, wl_type, np, url, up in rancher_wls:
        label = f"{name} ({wl_type})"
        dot   = status_dot(up)
        dim   = " style='opacity:.45'" if not up else ""
        rancher_rows += (
            f"<tr>"
            f"<td><span class='ns'>{ns}</span></td>"
            f"<td>{dot} <b>{label}</b></td>"
            f"<td><a href='{url}' target='_blank'{dim}>:{np}</a></td>"
            f"</tr>"
        )
    
    if not np_rows and not rancher_rows:
        combined_np = "<tr><td colspan='3' class='empty'>No additional NodePort services</td></tr>"
    else:
        combined_np = np_rows + rancher_rows

    err = f"<div class='err'>Kubernetes API: {np_err}</div>" if np_err else ""
    return f"""<!DOCTYPE html>
<html lang='en'><head><meta charset='utf-8'>
<meta http-equiv='refresh' content='20'>
<title>ClusterOS</title><style>
*{{box-sizing:border-box}}
body{{font-family:monospace;background:#0f172a;color:#cbd5e1;max-width:960px;margin:48px auto;padding:0 20px}}
h1{{color:#34d399;margin:0 0 4px}}
h2{{color:#60a5fa;font-size:.9em;letter-spacing:.08em;text-transform:uppercase;margin:28px 0 8px;border-bottom:1px solid #1e293b;padding-bottom:6px}}
.sub{{color:#64748b;font-size:.85em;margin-bottom:28px}}
table{{width:100%;border-collapse:collapse;margin-bottom:8px}}
th{{text-align:left;padding:8px 14px;background:#1e293b;color:#60a5fa;
    font-size:.8em;letter-spacing:.05em;text-transform:uppercase;border-bottom:2px solid #334155}}
td{{padding:9px 14px;border-bottom:1px solid #1e293b;vertical-align:middle}}
tr:hover td{{background:#1e293b}}
a{{color:#34d399;text-decoration:none}}a:hover{{text-decoration:underline}}
.ns{{color:#64748b;font-size:.8em}}
.desc{{color:#94a3b8;font-size:.85em}}
.dot{{font-size:.7em;margin-right:5px}}
.dot.up{{color:#22c55e}}
.dot.down{{color:#ef4444}}
.dot.unknown{{color:#f59e0b}}
.err{{background:#450a0a;border:1px solid #7f1d1d;border-radius:6px;
      padding:12px 16px;margin:16px 0;color:#fca5a5;font-size:.9em}}
.empty{{color:#475569;font-style:italic;padding:16px 14px}}
</style></head>
<body>
<h1>ClusterOS</h1>
<div class='sub'>Node: <b>{req_host}</b> &mdash; auto-refreshes every 20&nbsp;s</div>
{err}
<h2>Web Interfaces</h2>
<table>
  <thead><tr><th>Service</th><th>Type</th><th>Path</th></tr></thead>
  <tbody>{ing_rows}</tbody>
</table>
<h2>Direct NodePort Access</h2>
<table>
  <thead><tr><th>Namespace</th><th>Service</th><th>Port</th></tr></thead>
  <tbody>{combined_np}</tbody>
</table>
</body></html>"""


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        # Use X-Forwarded-Host (set by nginx-ingress) or Host header to determine
        # which IP/hostname the browser used.  This ensures generated links work
        # whether the user accessed via LAN IP, Tailscale IP, or a hostname.
        req_host = (
            self.headers.get("X-Forwarded-Host")
            or self.headers.get("Host", NODE_IP)
        ).split(":")[0]
        path = self.path.split("?")[0].rstrip("/")
        # /rancher redirect — bounce the browser to Rancher's NodePort using
        # the same IP the client used to reach us, so the redirect works
        # regardless of whether the user is on LAN or Tailscale.
        if path in ("/rancher", "/rancher/"):
            location = f"https://{req_host}:30444"
            self.send_response(302)
            self.send_header("Location", location)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        
        # For unknown paths (not "/" or "/index.html"), forward to Rancher.
        # This allows services to be discoverable via Rancher's UI even if
        # they don't have explicit ingress rules — the landing page auto-discovers
        # them and shows links to their NodePorts.
        if path not in ("", "/", "/index.html", "/favicon.ico"):
            location = f"https://{req_host}:30444{self.path}"
            self.send_response(302)
            self.send_header("Location", location)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        
        try:
            body = build_page(req_host).encode("utf-8")
            code = 200
        except Exception as exc:
            body = f"<pre>{exc}</pre>".encode()
            code = 500
        self.send_response(code)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *a):
        pass


print("ClusterOS landing page listening on :8080", flush=True)
http.server.HTTPServer(("", 8080), Handler).serve_forever()`

	// Build ConfigMap YAML: indent every script line by 4 spaces into the block scalar.
	var indented strings.Builder
	for _, line := range strings.Split(pythonScript, "\n") {
		if line == "" {
			indented.WriteString("\n")
		} else {
			indented.WriteString("    " + line + "\n")
		}
	}
	scriptYAML := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n" +
		"  name: clusteros-landing-script\n  namespace: ingress-nginx\n" +
		"data:\n  server.py: |\n" + indented.String()

	// Service (ClusterIP :80 → :8080) + Deployment (python:3.12-alpine).
	const deployYAML = `apiVersion: v1
kind: Service
metadata:
  name: clusteros-landing
  namespace: ingress-nginx
spec:
  selector:
    app: clusteros-landing
  ports:
  - port: 80
    targetPort: 8080
---
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: clusteros-landing
  namespace: ingress-nginx
spec:
  selector:
    matchLabels:
      app: clusteros-landing
  template:
    metadata:
      labels:
        app: clusteros-landing
    spec:
      serviceAccountName: clusteros-landing
      tolerations:
      - operator: Exists
        effect: NoSchedule
      - operator: Exists
        effect: NoExecute
      containers:
      - name: landing
        image: python:3.12-alpine
        command: ["python3", "/app/server.py"]
        env:
        - name: NODE_IP
          valueFrom:
            fieldRef:
              fieldPath: status.hostIP
        ports:
        - containerPort: 8080
        readinessProbe:
          httpGet:
            path: /
            port: 8080
          initialDelaySeconds: 5
          periodSeconds: 15
          timeoutSeconds: 10
        volumeMounts:
        - name: script
          mountPath: /app
          readOnly: true
      volumes:
      - name: script
        configMap:
          name: clusteros-landing-script`

	for _, manifest := range []string{rbacYAML, scriptYAML, deployYAML} {
		cmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		if out, err := cmd.CombinedOutput(); err != nil {
			ks.Logger().Warnf("landing page manifest: %v: %s", err, out)
		}
	}

	// Restart the landing page pod so it picks up any ConfigMap script changes.
	exec.Command("k3s", "kubectl", "-n", "ingress-nginx", "rollout", "restart",
		"daemonset/clusteros-landing").Run()

	// Patch the nginx-ingress-controller DaemonSet to use the landing page as its
	// global default backend — requests not matched by any Ingress rule hit the landing
	// page instead of the built-in nginx 404. Only patched once; idempotency check
	// runs every call.
	checkOut, _ := exec.Command("k3s", "kubectl", "-n", "ingress-nginx",
		"get", "daemonset", "ingress-nginx-controller",
		"-o", `jsonpath={.spec.template.spec.containers[0].args}`).Output()
	if !strings.Contains(string(checkOut), "default-backend-service") {
		patch := `[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--default-backend-service=ingress-nginx/clusteros-landing"}]`
		if out, err := exec.Command("k3s", "kubectl", "-n", "ingress-nginx",
			"patch", "daemonset", "ingress-nginx-controller",
			"--type=json", "-p", patch).CombinedOutput(); err != nil {
			ks.Logger().Warnf("patch nginx-ingress default-backend-service: %v: %s", err, out)
		} else {
			ks.Logger().Info("nginx-ingress DaemonSet patched — ClusterOS landing page is now the default backend")
		}
	}

	ks.Logger().Info("ClusterOS dynamic landing page deployed — discovers all NodePort services in real time")
}

// deploySLURMRestAPI deploys slurmrestd as a Kubernetes pod + NodePort service.
// slurmrestd is bundled with SLURM 20.11+.  We run it as a host-networked pod on the
// leader so it can reach the local slurmctld socket.  Exposed on NodePort 30819.
func (ks *K3sServer) deploySLURMRestAPI(mungeKey []byte) {
	if ks.slurmRestDeployed {
		return
	}
	ks.Logger().Info("Deploying SLURM REST API (slurmrestd)...")

	// Create the munge secret in slurm namespace (may already exist from slurmdbd deploy).
	if len(mungeKey) > 0 {
		if err := ks.createMungeKeySecret(mungeKey); err != nil {
			ks.Logger().Debugf("Munge secret (already exists?): %v", err)
		}
	}

	// Label this node as the SLURM controller so slurmrestd is pinned here.
	// slurmrestd needs the FULL slurm.conf (with AccountingStorageType), which only
	// exists on the leader/controller node — worker nodes only have a minimal client config.
	hostname, _ := os.Hostname()
	if hostname != "" {
		labelCmd := exec.Command("k3s", "kubectl", "label", "node", hostname,
			"clusteros.io/slurm-controller=true", "--overwrite")
		if out, err := labelCmd.CombinedOutput(); err != nil {
			ks.Logger().Warnf("Failed to label SLURM controller node: %v: %s", err, out)
		}
	}

	// Deploy slurmrestd as a Deployment pinned to the SLURM controller node (hostNetwork
	// = can reach local slurmctld on 127.0.0.1:6817).  The host's munge socket and
	// slurm.conf are mounted directly so no separate munged is needed inside the container.
	const restYAML = `apiVersion: v1
kind: Service
metadata:
  name: slurmrestd
  namespace: slurm
spec:
  type: NodePort
  selector:
    app: slurmrestd
  ports:
  - name: http
    port: 6820
    targetPort: 6820
    nodePort: 30819
    protocol: TCP
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: slurmrestd
  namespace: slurm
spec:
  replicas: 1
  selector:
    matchLabels:
      app: slurmrestd
  template:
    metadata:
      labels:
        app: slurmrestd
    spec:
      hostNetwork: true
      hostPID: true
      tolerations:
      - operator: Exists
      nodeSelector:
        clusteros.io/slurm-controller: "true"
      volumes:
      - name: munge-socket
        hostPath:
          path: /var/run/munge
          type: DirectoryOrCreate
      - name: slurm-conf
        hostPath:
          path: /etc/slurm
          type: DirectoryOrCreate
      - name: host-usr
        hostPath:
          path: /usr
          type: Directory
      initContainers:
      - name: wait-munge
        image: busybox:latest
        command: ['sh', '-c', 'until [ -S /var/run/munge/munge.socket.2 ]; do sleep 2; done']
        volumeMounts:
        - name: munge-socket
          mountPath: /var/run/munge
      containers:
      - name: slurmrestd
        image: ubuntu:24.04
        command:
        - /bin/sh
        - -c
        - |
          # Create slurm user matching host UID — slurmrestd refuses to run as root
          groupadd -g 64030 slurm 2>/dev/null || true
          useradd -u 64030 -g 64030 -M -s /usr/sbin/nologin slurm 2>/dev/null || true
          # slurm.conf PluginDir uses the host path; symlink so it resolves inside the container
          mkdir -p /usr/lib/x86_64-linux-gnu
          ln -sfn /hostfs/usr/lib/x86_64-linux-gnu/slurm-wlm /usr/lib/x86_64-linux-gnu/slurm-wlm
          exec su -s /bin/sh slurm -c 'exec /hostfs/usr/sbin/slurmrestd -f /etc/slurm/slurm.conf -a rest_auth/local 0.0.0.0:6820'
        env:
        - name: LD_LIBRARY_PATH
          value: /hostfs/usr/lib/x86_64-linux-gnu/slurm-wlm:/hostfs/usr/lib/x86_64-linux-gnu:/hostfs/usr/lib/aarch64-linux-gnu/slurm-wlm:/hostfs/usr/lib/aarch64-linux-gnu
        volumeMounts:
        - name: munge-socket
          mountPath: /var/run/munge
        - name: slurm-conf
          mountPath: /etc/slurm
          readOnly: true
        - name: host-usr
          mountPath: /hostfs/usr
          readOnly: true
        ports:
        - containerPort: 6820
        readinessProbe:
          tcpSocket:
            port: 6820
          initialDelaySeconds: 15
          periodSeconds: 10
          failureThreshold: 30
`
	// Ensure slurm namespace exists.
	nsCmd := exec.Command("k3s", "kubectl", "create", "namespace", "slurm",
		"--dry-run=client", "-o", "yaml")
	if nsYaml, err := nsCmd.Output(); err == nil {
		applyNs := exec.Command("k3s", "kubectl", "apply", "-f", "-")
		applyNs.Stdin = strings.NewReader(string(nsYaml))
		applyNs.Run()
	}

	cmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(restYAML)
	if output, err := cmd.CombinedOutput(); err != nil {
		ks.Logger().Warnf("slurmrestd deploy: %v: %s", err, output)
		return
	}

	// Ingress for slurmrestd REST API at /slurm/api/ (browser-facing UI is at /slurm/).
	const slurmIngressYAML = `apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: slurmrestd-ingress
  namespace: slurm
  annotations:
    nginx.ingress.kubernetes.io/use-regex: "true"
    nginx.ingress.kubernetes.io/rewrite-target: /$2
    nginx.ingress.kubernetes.io/proxy-read-timeout: "120"
spec:
  ingressClassName: nginx
  rules:
  - http:
      paths:
      - path: /slurm/api(/|$)(.*)
        pathType: ImplementationSpecific
        backend:
          service:
            name: slurmrestd
            port:
              number: 6820
`
	cmd2 := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	cmd2.Stdin = strings.NewReader(slurmIngressYAML)
	if output, err := cmd2.CombinedOutput(); err != nil {
		ks.Logger().Warnf("slurmrestd ingress: %v: %s", err, output)
	}

	ks.deploySLURMWebDash(mungeKey)

	ks.slurmRestDeployed = true
	ks.Logger().Info("SLURM deployed — REST API NodePort 30819 + /slurm/api, web dashboard /slurm")
}

// deploySLURMWebDash deploys a lightweight Python web dashboard at /slurm/ that
// shows the live job queue (squeue) and node status (sinfo) as HTML.  It runs
// with hostNetwork so it can reach the local slurmctld, and mounts the host
// munge socket and slurm.conf just like slurmrestd does.
func (ks *K3sServer) deploySLURMWebDash(mungeKey []byte) {
	_ = mungeKey // reserved for future munge-token injection

	const script = `#!/usr/bin/env python3
"""ClusterOS SLURM web dashboard — job queue and node status."""
import http.server, json, os, subprocess

def run_json(cmd):
    r = subprocess.run(cmd, capture_output=True, text=True)
    if r.returncode != 0:
        return None, r.stderr.strip()
    try:
        return json.loads(r.stdout), None
    except Exception as e:
        return None, str(e)

def state_color(s):
    c = {"RUNNING":"#34d399","PENDING":"#fbbf24","FAILED":"#f87171",
         "CANCELLED":"#f87171","COMPLETED":"#60a5fa"}.get(s,"#94a3b8")
    return f"<span style='color:{c}'>{s}</span>"

def build_page():
    jobs_data, jobs_err = run_json(["squeue","--json"])
    nodes_data, nodes_err = run_json(["sinfo","--json"])

    jobs = (jobs_data or {}).get("jobs", [])
    job_rows = ""
    for j in jobs:
        jid   = j.get("job_id", "-")
        name  = j.get("name", "-")
        user  = j.get("user_name", "-")
        state = j.get("job_state", "-")
        state = " ".join(state) if isinstance(state, list) else str(state)
        nodes = j.get("nodes", "-")
        t     = j.get("run_time", {})
        secs  = t.get("number", 0) if isinstance(t, dict) else 0
        tstr  = f"{secs//3600}h{(secs%3600)//60}m{secs%60}s" if secs else "-"
        job_rows += f"<tr><td>{jid}</td><td>{name}</td><td>{user}</td><td>{state_color(state)}</td><td>{nodes}</td><td>{tstr}</td></tr>"
    if not job_rows:
        msg = jobs_err or "Queue is empty"
        job_rows = f"<tr><td colspan='6' class='empty'>{msg}</td></tr>"

    sinfo = (nodes_data or {}).get("sinfo", [])
    node_rows = ""
    for n in sinfo:
        nodes_obj = n.get("nodes", {})
        node_list = nodes_obj.get("nodes", []) if isinstance(nodes_obj, dict) else []
        nm = ", ".join(node_list) if node_list else "-"
        st = n.get("node", {}).get("state", ["-"])
        st = " ".join(st) if isinstance(st, list) else str(st)
        part_obj = n.get("partition", {})
        part = part_obj.get("name", "-") if isinstance(part_obj, dict) else str(part_obj)
        cpus = n.get("cpus", {})
        if isinstance(cpus, dict):
            cpus_s = f"{cpus.get('idle','-')}/{cpus.get('total','-')} idle"
        else:
            cpus_s = str(cpus)
        mem = n.get("memory", {})
        if isinstance(mem, dict):
            mem_min = mem.get("minimum", "-")
            mem_max = mem.get("maximum", "-")
            mem_s = f"{mem_min}-{mem_max} MiB"
        else:
            mem_s = str(mem)
        node_rows += f"<tr><td>{nm}</td><td>{part}</td><td>{state_color(st)}</td><td>{cpus_s}</td><td>{mem_s}</td></tr>"
    if not node_rows:
        msg = nodes_err or "No node data"
        node_rows = f"<tr><td colspan='5' class='empty'>{msg}</td></tr>"

    return f"""<!DOCTYPE html>
<html lang='en'><head><meta charset='utf-8'><meta http-equiv='refresh' content='15'>
<title>ClusterOS SLURM</title><style>
*{{box-sizing:border-box}}
body{{font-family:monospace;background:#0f172a;color:#cbd5e1;max-width:1100px;margin:48px auto;padding:0 20px}}
h1{{color:#34d399;margin:0 0 4px}}h2{{color:#60a5fa;font-size:.9em;letter-spacing:.08em;text-transform:uppercase;margin:28px 0 8px;border-bottom:1px solid #1e293b;padding-bottom:6px}}
.sub{{color:#64748b;font-size:.85em;margin-bottom:28px}}
table{{width:100%;border-collapse:collapse;margin-bottom:8px}}
th{{text-align:left;padding:8px 14px;background:#1e293b;color:#60a5fa;font-size:.8em;letter-spacing:.05em;text-transform:uppercase;border-bottom:2px solid #334155}}
td{{padding:9px 14px;border-bottom:1px solid #1e293b;vertical-align:middle}}
tr:hover td{{background:#1e293b}}a{{color:#34d399;text-decoration:none}}
.empty{{color:#475569;font-style:italic;padding:16px 14px}}
</style></head><body>
<h1>ClusterOS SLURM</h1>
<div class='sub'>auto-refreshes every 15&nbsp;s &mdash; <a href='/'>&#8592; home</a> &mdash; <a href='/slurm/api/'>REST API</a></div>
<h2>Job Queue</h2>
<table>
  <thead><tr><th>ID</th><th>Name</th><th>User</th><th>State</th><th>Nodes</th><th>Runtime</th></tr></thead>
  <tbody>{job_rows}</tbody>
</table>
<h2>Cluster Nodes</h2>
<table>
  <thead><tr><th>Node</th><th>Partition</th><th>State</th><th>CPUs</th><th>Memory</th></tr></thead>
  <tbody>{node_rows}</tbody>
</table>
</body></html>"""


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        try:
            body = build_page().encode("utf-8")
            code = 200
        except Exception as exc:
            body = f"<pre>Error: {exc}</pre>".encode()
            code = 500
        self.send_response(code)
        self.send_header("Content-Type", "text/html; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)
    def log_message(self, *a):
        pass

print("SLURM dashboard listening on :8090", flush=True)
http.server.HTTPServer(("", 8090), Handler).serve_forever()`

	// ConfigMap + Deployment + Service + Ingress.
	var indented strings.Builder
	for _, line := range strings.Split(script, "\n") {
		if line == "" {
			indented.WriteString("\n")
		} else {
			indented.WriteString("    " + line + "\n")
		}
	}
	cmYAML := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n" +
		"  name: slurmweb-script\n  namespace: slurm\n" +
		"data:\n  server.py: |\n" + indented.String()

	const slurmwebYAML = `apiVersion: v1
kind: Service
metadata:
  name: slurmweb
  namespace: slurm
spec:
  selector:
    app: slurmweb
  ports:
  - port: 80
    targetPort: 8090
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: slurmweb
  namespace: slurm
spec:
  replicas: 1
  selector:
    matchLabels:
      app: slurmweb
  template:
    metadata:
      labels:
        app: slurmweb
    spec:
      hostNetwork: true
      hostPID: true
      tolerations:
      - operator: Exists
      nodeSelector:
        clusteros.io/slurm-controller: "true"
      volumes:
      - name: munge-socket
        hostPath:
          path: /var/run/munge
          type: DirectoryOrCreate
      - name: slurm-conf
        hostPath:
          path: /etc/slurm
          type: DirectoryOrCreate
      - name: script
        configMap:
          name: slurmweb-script
      - name: host-usr
        hostPath:
          path: /usr
          type: Directory
      initContainers:
      - name: wait-munge
        image: busybox:latest
        command: ['sh', '-c', 'until [ -S /var/run/munge/munge.socket.2 ]; do sleep 2; done']
        volumeMounts:
        - name: munge-socket
          mountPath: /var/run/munge
      containers:
      - name: slurmweb
        image: ubuntu:24.04
        command:
        - /bin/sh
        - -c
        - |
          mkdir -p /usr/lib/x86_64-linux-gnu /usr/lib/aarch64-linux-gnu
          ln -sfn /hostfs/usr/lib/x86_64-linux-gnu/slurm-wlm /usr/lib/x86_64-linux-gnu/slurm-wlm 2>/dev/null || true
          ln -sfn /hostfs/usr/lib/aarch64-linux-gnu/slurm-wlm /usr/lib/aarch64-linux-gnu/slurm-wlm 2>/dev/null || true
          exec /hostfs/usr/bin/python3 /app/server.py
        env:
        - name: PATH
          value: /hostfs/usr/bin:/hostfs/usr/sbin:/usr/local/bin:/usr/bin:/bin
        - name: LD_LIBRARY_PATH
          value: /hostfs/usr/lib/x86_64-linux-gnu/slurm-wlm:/hostfs/usr/lib/x86_64-linux-gnu:/hostfs/usr/lib/aarch64-linux-gnu/slurm-wlm:/hostfs/usr/lib/aarch64-linux-gnu
        volumeMounts:
        - name: munge-socket
          mountPath: /var/run/munge
        - name: slurm-conf
          mountPath: /etc/slurm
          readOnly: true
        - name: script
          mountPath: /app
          readOnly: true
        - name: host-usr
          mountPath: /hostfs/usr
          readOnly: true
        ports:
        - containerPort: 8090
        readinessProbe:
          httpGet:
            path: /
            port: 8090
          initialDelaySeconds: 10
          periodSeconds: 15
          timeoutSeconds: 10
          failureThreshold: 12
`
	const slurmwebIngressYAML = `apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: slurmweb-ingress
  namespace: slurm
  annotations:
    nginx.ingress.kubernetes.io/use-regex: "true"
    nginx.ingress.kubernetes.io/rewrite-target: /$2
spec:
  ingressClassName: nginx
  rules:
  - http:
      paths:
      - path: /slurm(/|$)(.*)
        pathType: ImplementationSpecific
        backend:
          service:
            name: slurmweb
            port:
              number: 80
`
	for _, manifest := range []string{cmYAML, slurmwebYAML, slurmwebIngressYAML} {
		cmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(manifest)
		if out, err := cmd.CombinedOutput(); err != nil {
			ks.Logger().Warnf("slurmweb manifest: %v: %s", err, out)
		}
	}
	ks.Logger().Info("SLURM web dashboard deployed — /slurm/ (HTML) /slurm/api/ (REST)")
}

func (ks *K3sServer) deployCertManager() error {
	if exec.Command("k3s", "kubectl", "-n", "cert-manager", "get", "deployment", "cert-manager").Run() == nil {
		return nil
	}
	ks.Logger().Info("Installing cert-manager...")
	certManagerURL := "https://github.com/cert-manager/cert-manager/releases/download/v1.16.3/cert-manager.yaml"
	cmd := exec.Command("k3s", "kubectl", "apply", "-f", certManagerURL)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply cert-manager: %w: %s", err, string(output))
	}
	cmd = exec.Command("k3s", "kubectl", "-n", "cert-manager",
		"rollout", "status", "deployment/cert-manager-webhook", "--timeout=300s")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cert-manager webhook: %w: %s", err, string(output))
	}
	ks.Logger().Info("cert-manager ready")
	return nil
}

// ensureRancherDataPVC ensures the rancher-data PVC exists in cattle-system.
// On a normal deploy this is a no-op (PVC already Bound).
// On a fresh cluster after an etcd wipe the PV object is gone, but longhorn-ha
// has reclaimPolicy=Retain so the underlying Longhorn volume (with cattle.db) is
// still on disk.  We find any Released PV that previously backed rancher-data and
// clear its claimRef so it becomes Available again, then create a PVC that binds to it.
// This recovers Rancher's SQLite DB — repos, API keys, and all configuration — without
// reinstalling from scratch.
func (ks *K3sServer) ensureRancherDataPVC() error {
	const ns = "cattle-system"

	// Ensure namespace exists first.
	exec.Command("k3s", "kubectl", "create", "namespace", ns,
		"--dry-run=client", "-o", "yaml").Run()
	nsCmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	nsCmd.Stdin = strings.NewReader("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: " + ns + "\n")
	nsCmd.Run()

	// PVC already bound — nothing to do.
	out, _ := exec.Command("k3s", "kubectl", "get", "pvc", "rancher-data",
		"-n", ns, "-o", "jsonpath={.status.phase}").Output()
	if strings.TrimSpace(string(out)) == "Bound" {
		return nil
	}

	// Look for a Released PV backed by longhorn-ha that previously held rancher-data.
	// The PV's claimRef records the original namespace/name.
	pvList, _ := exec.Command("k3s", "kubectl", "get", "pv",
		"-o", `jsonpath={range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{.spec.claimRef.namespace}{"\t"}{.spec.claimRef.name}{"\t"}{.spec.storageClassName}{"\n"}{end}`).Output()

	var releasedPV string
	for _, line := range strings.Split(string(pvList), "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) < 5 {
			continue
		}
		pvName, phase, claimNS, claimName, sc := fields[0], fields[1], fields[2], fields[3], fields[4]
		if phase == "Released" && claimNS == ns && claimName == "rancher-data" && sc == "longhorn-ha" {
			releasedPV = pvName
			break
		}
	}

	if releasedPV != "" {
		ks.Logger().Infof("Found released rancher-data PV %s — re-binding to recover Rancher DB", releasedPV)
		// Clear the claimRef so the PV becomes Available and can be re-bound.
		patchCmd := exec.Command("k3s", "kubectl", "patch", "pv", releasedPV,
			"--type=json",
			`-p=[{"op":"remove","path":"/spec/claimRef"}]`)
		if out, err := patchCmd.CombinedOutput(); err != nil {
			ks.Logger().Warnf("patch PV claimRef: %v: %s", err, out)
		}
		// Create a PVC that binds specifically to this PV (volumeName selector).
		pvcYAML := fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: rancher-data
  namespace: %s
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: longhorn-ha
  volumeName: %s
  resources:
    requests:
      storage: 5Gi
`, ns, releasedPV)
		cmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(pvcYAML)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("re-bind PVC to released PV: %w: %s", err, out)
		}
		ks.Logger().Infof("rancher-data PVC re-bound to existing PV %s (Rancher DB recovered)", releasedPV)
		return nil
	}

	// No existing PV — create a fresh PVC (new cluster or first install).
	ks.Logger().Info("Creating new rancher-data PVC (fresh cluster)")
	pvcYAML := `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: rancher-data
  namespace: cattle-system
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: longhorn-ha
  resources:
    requests:
      storage: 5Gi
`
	cmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(pvcYAML)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create rancher-data PVC: %w: %s", err, out)
	}
	return nil
}

// ensureRancherDataMounted checks whether the Rancher deployment already declares
// the rancher-data volume.  If not it migrates existing /var/lib/rancher content
// from the running pod into the PVC, then patches the deployment so future
// restarts (including leader changes) resume from persisted state.
func (ks *K3sServer) ensureRancherDataMounted() {
	out, _ := exec.Command("k3s", "kubectl", "get", "deployment", "rancher",
		"-n", "cattle-system",
		"-o", `jsonpath={.spec.template.spec.volumes[?(@.name=="rancher-data")].name}`).Output()
	if strings.TrimSpace(string(out)) == "rancher-data" {
		return // already mounted — nothing to do
	}

	ks.Logger().Info("Rancher deployment has no persistent volume — migrating state and patching...")

	// Ensure PVC exists first (idempotent).
	if err := ks.ensureRancherDataPVC(); err != nil {
		ks.Logger().Warnf("rancher-data PVC: %v (skipping volume patch)", err)
		return
	}

	// Copy existing /var/lib/rancher content into the PVC before patching so that
	// cattle.db (settings, API keys, repos) survives the imminent pod restart.
	ks.migrateRancherDataToPVC()

	// Strategic-merge-patch the deployment to add the volume + volumeMount.
	// Strategic merge uses the "name" field as the merge key for volumes and
	// volumeMounts arrays, so this is additive — it won't remove existing entries.
	// Unlike JSON Patch op:add with "/-", strategic merge handles null arrays safely.
	mergePatch := `{"spec":{"template":{"spec":{` +
		`"volumes":[{"name":"rancher-data","persistentVolumeClaim":{"claimName":"rancher-data"}}],` +
		`"containers":[{"name":"rancher","volumeMounts":[{"name":"rancher-data","mountPath":"/var/lib/rancher"}]}]` +
		`}}}}`
	cmd := exec.Command("k3s", "kubectl", "patch", "deployment", "rancher",
		"-n", "cattle-system", "--type=strategic", "-p", mergePatch)
	if patchOut, err := cmd.CombinedOutput(); err != nil {
		ks.Logger().Warnf("patch rancher deployment volumes: %v: %s", err, patchOut)
		return
	}
	ks.Logger().Info("Rancher deployment patched — pod will restart with persistent /var/lib/rancher")
}

// migrateRancherDataToPVC copies /var/lib/rancher from the currently running Rancher
// pod into the rancher-data PVC so that the first restart with the mounted PVC
// picks up the existing cattle.db rather than starting empty.
//
// It is idempotent: if the PVC already contains cattle.db the copy is skipped.
func (ks *K3sServer) migrateRancherDataToPVC() {
	// Find a running Rancher pod to copy from.
	podOut, _ := exec.Command("k3s", "kubectl", "get", "pod",
		"-n", "cattle-system", "-l", "app=rancher",
		"--field-selector=status.phase=Running",
		"-o", "jsonpath={.items[0].metadata.name}").Output()
	srcPod := strings.TrimSpace(string(podOut))
	if srcPod == "" {
		ks.Logger().Info("No running Rancher pod found — PVC will be initialised fresh on first start")
		return
	}

	// Spin up a receiver pod that mounts the PVC; check if data is already there.
	const receiverName = "rancher-data-receiver"
	receiverYAML := `apiVersion: v1
kind: Pod
metadata:
  name: rancher-data-receiver
  namespace: cattle-system
spec:
  restartPolicy: Never
  containers:
  - name: receiver
    image: busybox:1.36
    command: ["sh", "-c", "sleep 120"]
    volumeMounts:
    - name: data
      mountPath: /var/lib/rancher
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: rancher-data
`
	exec.Command("k3s", "kubectl", "delete", "pod", receiverName,
		"-n", "cattle-system", "--ignore-not-found=true").Run()
	applyCmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	applyCmd.Stdin = strings.NewReader(receiverYAML)
	if applyCmd.Run() != nil {
		return
	}
	defer exec.Command("k3s", "kubectl", "delete", "pod", receiverName,
		"-n", "cattle-system", "--ignore-not-found=true").Run()

	// Wait up to 90s for the receiver pod to be Running.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		phase, _ := exec.Command("k3s", "kubectl", "get", "pod", receiverName,
			"-n", "cattle-system", "-o", "jsonpath={.status.phase}").Output()
		if strings.TrimSpace(string(phase)) == "Running" {
			break
		}
		time.Sleep(3 * time.Second)
	}
	phaseOut, _ := exec.Command("k3s", "kubectl", "get", "pod", receiverName,
		"-n", "cattle-system", "-o", "jsonpath={.status.phase}").Output()
	if strings.TrimSpace(string(phaseOut)) != "Running" {
		ks.Logger().Warn("rancher-data-receiver pod not ready — skipping data migration")
		return
	}

	// Skip if cattle.db already exists in the PVC (second run after a restart).
	checkOut, _ := exec.Command("k3s", "kubectl", "exec", receiverName,
		"-n", "cattle-system", "--",
		"sh", "-c", "test -f /var/lib/rancher/cattle.db && echo HAS_DATA").Output()
	if strings.Contains(string(checkOut), "HAS_DATA") {
		ks.Logger().Info("rancher-data PVC already contains cattle.db — skipping migration")
		return
	}

	// Tar the source pod's /var/lib/rancher into memory, then extract into the PVC via the receiver.
	ks.Logger().Infof("Copying /var/lib/rancher from %s into rancher-data PVC...", srcPod)
	tarOut, err := exec.Command("k3s", "kubectl", "exec", srcPod,
		"-n", "cattle-system", "--",
		"tar", "czf", "-", "-C", "/", "var/lib/rancher").Output()
	if err != nil || len(tarOut) == 0 {
		ks.Logger().Warnf("tar from rancher pod failed: %v — PVC will be initialised fresh", err)
		return
	}

	extractCmd := exec.Command("k3s", "kubectl", "exec", receiverName,
		"-n", "cattle-system", "-i", "--",
		"tar", "xzf", "-", "-C", "/")
	extractCmd.Stdin = bytes.NewReader(tarOut)
	if extractOut, err := extractCmd.CombinedOutput(); err != nil {
		ks.Logger().Warnf("extract into PVC failed: %v: %s", err, extractOut)
	} else {
		ks.Logger().Info("Rancher state migrated to persistent PVC — cattle.db will survive restarts and leader changes")
	}
}

func (ks *K3sServer) deployRancher() error {
	alreadyInstalled := exec.Command("k3s", "kubectl", "-n", "cattle-system", "get", "deployment", "rancher").Run() == nil

	if !alreadyInstalled {
	ks.Logger().Info("Installing Rancher management UI...")

	// Pre-create the rancher-data PVC backed by Longhorn (2 replicas, Retain reclaim).
	// On a fresh cluster after an etcd wipe the PV object is gone but the Longhorn
	// volume may still have data on disk.  We check for Released PVs first and
	// re-bind to any that were the rancher-data PVC — this recovers the SQLite DB
	// (including repos, API keys) without running helm install from scratch.
	if err := ks.ensureRancherDataPVC(); err != nil {
		ks.Logger().Warnf("rancher-data PVC: %v (continuing)", err)
	}

	// Use nip.io to turn the raw IP into a valid DNS hostname.
	// Kubernetes 1.28+ rejects raw IPs in Ingress spec.rules[0].host.
	// nip.io is a public wildcard DNS: 100-102-126-31.nip.io → 100.102.126.31.
	rancherHost := "rancher.cluster.local"
	if ks.nodeIP != "" {
		rancherHost = strings.ReplaceAll(ks.nodeIP, ".", "-") + ".nip.io"
	}

	helmPath := "/usr/local/bin/helm"
	if _, err := os.Stat(helmPath); os.IsNotExist(err) {
		ks.Logger().Info("Installing Helm...")
		installCmd := exec.Command("bash", "-c",
			"curl -fsSL https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash")
		if output, err := installCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("install helm: %w: %s", err, string(output))
		}
	}

	exec.Command("helm", "repo", "add", "rancher-stable",
		"https://releases.rancher.com/server-charts/stable").Run()
	exec.Command("helm", "repo", "update").Run()

	// Install Rancher WITHOUT ssl-passthrough. ssl-passthrough routes by TLS SNI hostname,
	// so accessing via a bare IP returns the nginx default page (no SNI match). Instead we
	// expose via a NodePort (HTTPS:30444) for direct IP access and let nginx proxy HTTP→HTTPS
	// for ingress-based access via the /rancher path.
	cmd := exec.Command("helm", "install", "rancher", "rancher-stable/rancher",
		"--namespace", "cattle-system", "--create-namespace",
		"--set", fmt.Sprintf("hostname=%s", rancherHost),
		"--set", "bootstrapPassword=admin",
		"--set", "ingress.tls.source=rancher",
		"--set", "ingress.ingressClassName=nginx",
		"--set", "replicas=1",
		"--set", "global.cattle.psp.enabled=false",
		"--set", fmt.Sprintf("extraEnv[0].name=CATTLE_SERVER_URL"),
		"--set", fmt.Sprintf("extraEnv[0].value=https://%s:30444", rancherHost),
		"--set", "extraEnv[1].name=CATTLE_FEATURES",
		"--set", "extraEnv[1].value=unsupported-storage-drivers=true",
		"--set-string", `ingress.extraAnnotations.nginx\.ingress\.kubernetes\.io/backend-protocol=HTTPS`,
		"--set-string", `ingress.extraAnnotations.nginx\.ingress\.kubernetes\.io/proxy-ssl-verify=off`,
		// Mount rancher-data PVC at /var/lib/rancher so the embedded SQLite DB
		// (cattle.db) survives pod rescheduling to a different node.
		"--set", "volumes[0].name=rancher-data",
		"--set", "volumes[0].persistentVolumeClaim.claimName=rancher-data",
		"--set", "volumeMounts[0].name=rancher-data",
		"--set", "volumeMounts[0].mountPath=/var/lib/rancher",
		"--wait", "--timeout", "5m0s",
		"--kubeconfig", "/etc/rancher/k3s/k3s.yaml",
	)
	cmd.Env = append(os.Environ(), "KUBECONFIG=/etc/rancher/k3s/k3s.yaml")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("helm install rancher: %w: %s", err, string(output))
	}
	} // end if !alreadyInstalled

	// Ensure the Rancher ingress uses HTTPS to talk to the backend.
	// The helm chart's extraAnnotations flag doesn't reliably apply due to
	// dot-in-key escaping; a post-install patch is the safe fallback.
	exec.Command("k3s", "kubectl", "patch", "ingress", "rancher",
		"-n", "cattle-system",
		"--type=json",
		`-p=[{"op":"add","path":"/metadata/annotations/nginx.ingress.kubernetes.io~1backend-protocol","value":"HTTPS"},`+
			`{"op":"replace","path":"/spec/rules/0/http/paths/0/backend/service/port/number","value":443}]`,
	).Run()

	// Always (re-)apply the NodePort service and redirect ingress so they survive
	// node-agent restarts where Rancher is already installed.

	// Expose Rancher via NodePort 30444 (HTTPS direct access by IP — no hostname needed).
	rancherNPYAML := `apiVersion: v1
kind: Service
metadata:
  name: rancher-nodeport
  namespace: cattle-system
spec:
  type: NodePort
  selector:
    app: rancher
  ports:
  - name: https
    port: 443
    targetPort: 443
    nodePort: 30444
`
	applyCmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	applyCmd.Stdin = strings.NewReader(rancherNPYAML)
	applyCmd.Run()

	// Delete any stale rancher-path-redirect ingress from previous deploys.
	exec.Command("k3s", "kubectl", "-n", "cattle-system", "delete", "ingress",
		"rancher-path-redirect", "--ignore-not-found=true").Run()

	// Create a /rancher ingress in the ingress-nginx namespace so that
	// collect_ingresses() on the landing page lists "Rancher" in the Web Interfaces
	// table on every node.  nginx routes /rancher to the clusteros-landing service,
	// which 302-redirects to https://<req_host>:30444 using the client's actual IP —
	// so the link works from both LAN and Tailscale.
	const rancherPathYAML = `apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: rancher-path-ingress
  namespace: ingress-nginx
spec:
  ingressClassName: nginx
  rules:
  - http:
      paths:
      - path: /rancher
        pathType: Prefix
        backend:
          service:
            name: clusteros-landing
            port:
              number: 80
`
	rancherPathCmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	rancherPathCmd.Stdin = strings.NewReader(rancherPathYAML)
	if out, err := rancherPathCmd.CombinedOutput(); err != nil {
		ks.Logger().Warnf("rancher-path-ingress: %v: %s", err, out)
	}

	// Ensure the rancher-data PVC is mounted on the Rancher deployment.
	// This is a no-op for fresh installs (already set via helm --set flags) but
	// patches existing deployments that were installed before this code was added.
	// Must run after the PVC is guaranteed to exist (ensureRancherDataPVC called above).
	ks.ensureRancherDataMounted()

	ks.Logger().Infof("Rancher NodePort 30444 applied (admin/admin)")
	return nil
}

// applyRancherCatalogs re-applies Helm repository definitions as ClusterRepo CRDs on every
// leader startup.  Because these are applied unconditionally (not gated on alreadyInstalled),
// the repos survive full cluster rebuilds — they are re-created from code without any manual
// intervention.  Add or remove repos here; the change takes effect on the next leader start.
func (ks *K3sServer) applyRancherCatalogs() {
	// Wait for Rancher's catalog CRD to exist (Rancher must be running first).
	ks.Logger().Info("Waiting for Rancher catalog CRD to be available...")
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		if exec.Command("k3s", "kubectl", "get", "crd",
			"clusterrepos.catalog.cattle.io").Run() == nil {
			break
		}
		time.Sleep(20 * time.Second)
	}
	if exec.Command("k3s", "kubectl", "get", "crd",
		"clusterrepos.catalog.cattle.io").Run() != nil {
		ks.Logger().Warn("Rancher catalog CRD not available — skipping catalog apply")
		return
	}

	// ── Declare your Helm repos here ────────────────────────────────────────────
	// Each ClusterRepo is applied with kubectl apply (idempotent).  Add a new
	// `---` separated block to persist an additional repo across rebuilds.
	const catalogsYAML = `apiVersion: catalog.cattle.io/v1
kind: ClusterRepo
metadata:
  name: rancher-stable
spec:
  url: https://releases.rancher.com/server-charts/stable
---
apiVersion: catalog.cattle.io/v1
kind: ClusterRepo
metadata:
  name: rancher-charts
spec:
  url: https://charts.rancher.io
---
apiVersion: catalog.cattle.io/v1
kind: ClusterRepo
metadata:
  name: bitnami
spec:
  url: https://charts.bitnami.com/bitnami
---
apiVersion: catalog.cattle.io/v1
kind: ClusterRepo
metadata:
  name: ingress-nginx
spec:
  url: https://kubernetes.github.io/ingress-nginx
`
	cmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(catalogsYAML)
	if out, err := cmd.CombinedOutput(); err != nil {
		ks.Logger().Warnf("rancher catalogs apply: %v: %s", err, out)
		return
	}
	ks.Logger().Info("Rancher catalogs applied (repos always in sync with code)")
}

// deployRancherBackup installs the rancher-backup-restore operator and configures
// a scheduled backup to a Longhorn-backed PVC (longhorn-ha = Retain reclaim policy).
//
// On a full cluster rebuild (etcd wiped), the restore flow is:
//   1. Fresh Rancher installed (empty DB)
//   2. rancher-backup operator installed (this function)
//   3. Operator detects existing backup files in the PVC (Longhorn volume survived)
//   4. A Restore CR is created — Rancher merges the backed-up config into its DB
//   5. applyRancherCatalogs() re-applies repos declaratively as a safety net
//
// Backup files are stored as <timestamp>.tar.gz in the PVC; the 5 most recent are kept.
func (ks *K3sServer) deployRancherBackup() {
	// Install rancher-backup CRDs + operator via Helm.
	exec.Command("helm", "repo", "add", "rancher-charts",
		"https://charts.rancher.io").Run()
	exec.Command("helm", "repo", "update").Run()

	alreadyInstalled := exec.Command("k3s", "kubectl", "-n", "cattle-resources-system",
		"get", "deployment", "rancher-backup").Run() == nil

	if !alreadyInstalled {
		ks.Logger().Info("Installing rancher-backup operator...")
		for _, chart := range []string{"rancher-backup-crd", "rancher-backup"} {
			cmd := exec.Command("helm", "upgrade", "--install", chart,
				"rancher-charts/"+chart,
				"--namespace", "cattle-resources-system",
				"--create-namespace",
				"--kubeconfig", "/etc/rancher/k3s/k3s.yaml")
			cmd.Env = append(os.Environ(), "KUBECONFIG=/etc/rancher/k3s/k3s.yaml")
			if out, err := cmd.CombinedOutput(); err != nil {
				ks.Logger().Warnf("helm install %s: %v: %s", chart, err, out)
				return
			}
		}
		ks.Logger().Info("rancher-backup operator installed")
	}

	// Ensure the backup storage PVC exists (longhorn-ha = 2 replicas, Retain reclaim).
	const backupPVCYAML = `apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: rancher-backup-storage
  namespace: cattle-resources-system
spec:
  accessModes: ["ReadWriteOnce"]
  storageClassName: longhorn-ha
  resources:
    requests:
      storage: 10Gi
`
	applyPVC := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	applyPVC.Stdin = strings.NewReader(backupPVCYAML)
	applyPVC.Run()

	// Create a scheduled Backup resource (every 6 hours, keep 5 most recent).
	// The operator writes <timestamp>.tar.gz files into the PVC.
	const backupCRYAML = `apiVersion: resources.cattle.io/v1
kind: Backup
metadata:
  name: clusteros-scheduled
  namespace: cattle-resources-system
spec:
  schedule: "0 */6 * * *"
  retentionCount: 5
  storageLocation:
    pvc:
      claimName: rancher-backup-storage
`
	applyBackup := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	applyBackup.Stdin = strings.NewReader(backupCRYAML)
	if out, err := applyBackup.CombinedOutput(); err != nil {
		ks.Logger().Warnf("rancher Backup CR: %v: %s", err, out)
	} else {
		ks.Logger().Info("Rancher backup schedule configured (every 6h → rancher-backup-storage PVC)")
	}

	// If this is a fresh Rancher install and backup files already exist in the PVC
	// (from a previous cluster lifetime), create a Restore CR so Rancher recovers
	// its repos, API keys, and other configuration automatically.
	ks.maybeRestoreRancherBackup()
}

// maybeRestoreRancherBackup creates a Restore CR if:
//   (a) no Restore has been attempted yet in this cluster lifetime, AND
//   (b) backup files exist in the rancher-backup-storage PVC
//
// It identifies the latest backup by querying a temporary pod that mounts the PVC.
func (ks *K3sServer) maybeRestoreRancherBackup() {
	// Skip if a Restore already exists — don't restore twice.
	out, _ := exec.Command("k3s", "kubectl", "get", "restore",
		"-n", "cattle-resources-system", "--no-headers").Output()
	if len(strings.TrimSpace(string(out))) > 0 {
		return
	}

	// Spawn a short-lived pod to list backup files in the PVC.
	const listerPodYAML = `apiVersion: v1
kind: Pod
metadata:
  name: rancher-backup-lister
  namespace: cattle-resources-system
spec:
  restartPolicy: Never
  containers:
  - name: lister
    image: busybox:1.36
    command: ["sh", "-c", "ls -1t /backup/*.tar.gz 2>/dev/null | head -1"]
    volumeMounts:
    - name: backup
      mountPath: /backup
  volumes:
  - name: backup
    persistentVolumeClaim:
      claimName: rancher-backup-storage
`
	// Clean up any previous lister pod.
	exec.Command("k3s", "kubectl", "delete", "pod", "rancher-backup-lister",
		"-n", "cattle-resources-system", "--ignore-not-found=true").Run()

	applyPod := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	applyPod.Stdin = strings.NewReader(listerPodYAML)
	if applyPod.Run() != nil {
		return
	}

	// Wait up to 60s for the pod to complete.
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		phase, _ := exec.Command("k3s", "kubectl", "get", "pod", "rancher-backup-lister",
			"-n", "cattle-resources-system",
			"-o", "jsonpath={.status.phase}").Output()
		if strings.TrimSpace(string(phase)) == "Succeeded" {
			break
		}
		time.Sleep(5 * time.Second)
	}

	logsOut, err := exec.Command("k3s", "kubectl", "logs", "rancher-backup-lister",
		"-n", "cattle-resources-system").Output()
	exec.Command("k3s", "kubectl", "delete", "pod", "rancher-backup-lister",
		"-n", "cattle-resources-system", "--ignore-not-found=true").Run()

	if err != nil {
		return
	}
	backupFile := strings.TrimSpace(string(logsOut))
	if backupFile == "" {
		ks.Logger().Info("No existing rancher backup found — starting fresh")
		return
	}
	// backupFile is the full path like /backup/2026-03-27T12:00:00Z.tar.gz;
	// the Restore CR only needs the filename portion.
	backupFilename := backupFile[strings.LastIndex(backupFile, "/")+1:]
	ks.Logger().Infof("Found existing rancher backup: %s — creating Restore CR", backupFilename)

	restoreCRYAML := fmt.Sprintf(`apiVersion: resources.cattle.io/v1
kind: Restore
metadata:
  name: clusteros-restore
  namespace: cattle-resources-system
spec:
  backupFilename: %s
  storageLocation:
    pvc:
      claimName: rancher-backup-storage
`, backupFilename)
	applyRestore := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	applyRestore.Stdin = strings.NewReader(restoreCRYAML)
	if out, err := applyRestore.CombinedOutput(); err != nil {
		ks.Logger().Warnf("rancher Restore CR: %v: %s", err, out)
	} else {
		ks.Logger().Infof("Rancher Restore CR created — waiting for operator to restore config...")
	}
}

// createRancherCatchallIngress adds a wildcard ingress rule (no Host filter) so that
// bare-IP access to nginx on :30080/:30443 routes to Rancher instead of the landing page.
// It waits up to 5 minutes for Rancher pods to be ready before creating the ingress —
// this prevents the catchall from immediately returning 502 due to unready backends.
func (ks *K3sServer) createRancherCatchallIngress(_ string) {
	if exec.Command("k3s", "kubectl", "-n", "cattle-system", "get", "ingress", "rancher-catchall").Run() == nil {
		return
	}

	// Wait for at least one Rancher pod to be ready before wiring up the catchall.
	// The landing page serves as the fallback during this window.
	ks.Logger().Info("Waiting up to 5m for Rancher pods to be ready before creating catchall ingress...")
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		out, err := exec.Command("k3s", "kubectl", "-n", "cattle-system",
			"get", "deployment", "rancher",
			"-o", "jsonpath={.status.readyReplicas}").Output()
		if err == nil {
			ready := strings.TrimSpace(string(out))
			if ready != "" && ready != "0" && ready != "<no value>" {
				ks.Logger().Infof("Rancher ready (%s replica(s)) — creating catchall ingress", ready)
				break
			}
		}
		time.Sleep(15 * time.Second)
	}
	// Verify before proceeding; if Rancher never became ready, skip the catchall
	// (the landing page already provides a fallback for unmatched routes).
	out, _ := exec.Command("k3s", "kubectl", "-n", "cattle-system",
		"get", "deployment", "rancher",
		"-o", "jsonpath={.status.readyReplicas}").Output()
	ready := strings.TrimSpace(string(out))
	if ready == "" || ready == "0" || ready == "<no value>" {
		ks.Logger().Warn("Rancher not ready after 5m — skipping catchall ingress (landing page provides fallback)")
		return
	}
	const ingressYAML = `apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: rancher-catchall
  namespace: cattle-system
  annotations:
    nginx.ingress.kubernetes.io/backend-protocol: "HTTPS"
    nginx.ingress.kubernetes.io/proxy-ssl-verify: "off"
    nginx.ingress.kubernetes.io/ssl-redirect: "false"
spec:
  ingressClassName: nginx
  defaultBackend:
    service:
      name: rancher
      port:
        number: 443
  rules:
  - http:
      paths:
      - path: /
        pathType: Prefix
        backend:
          service:
            name: rancher
            port:
              number: 443
`
	// Retry up to 5 times with 15s backoff — the ingress-nginx admission webhook
	// may not have endpoints ready immediately after nginx-ingress comes up.
	for attempt := 1; attempt <= 5; attempt++ {
		cmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
		cmd.Stdin = strings.NewReader(ingressYAML)
		if output, err := cmd.CombinedOutput(); err != nil {
			if attempt < 5 {
				ks.Logger().Warnf("rancher catchall ingress (attempt %d/5): %v — retrying in 15s", attempt, err)
				time.Sleep(15 * time.Second)
				continue
			}
			ks.Logger().Warnf("rancher catchall ingress: %v: %s", err, output)
		}
		break
	}
}
