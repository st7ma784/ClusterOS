package k3s

import (
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
	dataDir             string
	tokenPath           string
	k3sCmd              *exec.Cmd
	manifestsDir        string
	slurmdbdDeployed    bool
	slurmRestDeployed   bool
	servicesDeployed    bool
}

// NewK3sServerRole creates a K3sServer for health monitoring (implements roles.Role).
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

// Start starts k3s server as the cluster leader with --cluster-init.
// This is called once by the phase machine, not by leadership callbacks.
func (ks *K3sServer) Start() error {
	ks.Logger().Info("Starting k3s server (leader)")

	if err := os.MkdirAll(ks.dataDir, 0755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	ks.killExistingK3s()

	if ks.nodeIP != "" {
		ks.Logger().Infof("Waiting for node IP %s to be bound...", ks.nodeIP)
		if err := ks.waitForIPReady(); err != nil {
			ks.Logger().Warnf("IP readiness: %v (proceeding anyway)", err)
		}
	}

	if err := ks.resetEtcdIfStale(); err != nil {
		ks.Logger().Warnf("etcd reset: %v", err)
	}

	args := []string{
		"server",
		"--data-dir", ks.dataDir,
		"--cluster-init",  // leader always initialises the cluster
		"--disable", "servicelb",
		"--disable", "traefik",
		"--snapshotter", "native",
	}

	if ks.nodeIP != "" {
		args = append(args,
			"--node-ip", ks.nodeIP,
			"--advertise-address", ks.nodeIP,
			"--bind-address", "0.0.0.0",
			"--tls-san", ks.nodeIP,
		)
		if lanIP := detectLANIP(); lanIP != "" {
			args = append(args, "--tls-san", lanIP)
		}
	}

	cmd := exec.Command("k3s", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("k3s start: %w", err)
	}

	ks.k3sCmd = cmd
	ks.SetRunning(true)
	ks.Logger().Infof("k3s server started (PID %d)", cmd.Process.Pid)
	return nil
}

// WaitForAPIReady blocks until the K3s API server responds to kubectl, or timeout.
func (ks *K3sServer) WaitForAPIReady(timeout time.Duration) error {
	ks.Logger().Infof("Waiting up to %s for K3s API server to be ready...", timeout)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if exec.Command("k3s", "kubectl", "get", "nodes").Run() == nil {
			ks.Logger().Info("K3s API server is ready")
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	return fmt.Errorf("K3s API server not ready after %s", timeout)
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

// DeployClusterServices installs Longhorn, nginx-ingress, cert-manager, Rancher, slurmdbd,
// and the SLURM REST API. Called as a goroutine from the phase machine after K3s API is ready.
func (ks *K3sServer) DeployClusterServices(mungeKey []byte) {
	ks.Logger().Info("Starting cluster services deployment...")

	if err := ks.deploySlurmdbd(mungeKey); err != nil {
		ks.Logger().Warnf("Failed to deploy slurmdbd: %v", err)
	}

	if err := ks.deployIngressNginx(); err != nil {
		ks.Logger().Warnf("Failed to deploy nginx-ingress: %v", err)
	}

	// Wait briefly for nginx-ingress controller to be ready before deploying ingress resources.
	time.Sleep(15 * time.Second)

	if err := ks.deployLonghorn(); err != nil {
		ks.Logger().Warnf("Failed to deploy Longhorn: %v", err)
	}

	if err := ks.deployCertManager(); err != nil {
		ks.Logger().Warnf("Failed to deploy cert-manager: %v (skipping Rancher)", err)
		// Still deploy SLURM REST even if Rancher fails.
		ks.deploySLURMRestAPI(mungeKey)
		ks.servicesDeployed = true
		return
	}

	if err := ks.deployRancher(); err != nil {
		ks.Logger().Warnf("Failed to deploy Rancher: %v", err)
	}

	ks.deploySLURMRestAPI(mungeKey)

	ks.servicesDeployed = true
	ks.Logger().Info("Cluster services deployment complete")
}

// HealthCheck checks if k3s server process is alive.
func (ks *K3sServer) HealthCheck() error {
	if ks.k3sCmd == nil || ks.k3sCmd.Process == nil {
		return fmt.Errorf("k3s server not started")
	}
	if err := ks.k3sCmd.Process.Signal(syscall.Signal(0)); err != nil {
		ks.SetRunning(false)
		return fmt.Errorf("k3s server process dead: %w", err)
	}
	return nil
}

// Stop terminates the k3s server process.
func (ks *K3sServer) Stop(ctx context.Context) error {
	ks.Logger().Info("Stopping k3s server")
	if ks.k3sCmd == nil || ks.k3sCmd.Process == nil {
		return nil
	}
	if err := ks.k3sCmd.Process.Signal(os.Interrupt); err != nil {
		ks.k3sCmd.Process.Kill()
	}
	ks.k3sCmd.Wait()
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

// killExistingK3s kills any existing K3s/etcd processes holding port 2380
func (ks *K3sServer) killExistingK3s() {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:2380", 1*time.Second)
	if err != nil {
		return
	}
	conn.Close()
	ks.Logger().Warn("Port 2380 in use — killing stale K3s/etcd processes")

	if _, err := os.Stat("/usr/local/bin/k3s-killall.sh"); err == nil {
		if exec.Command("/usr/local/bin/k3s-killall.sh").Run() == nil {
			time.Sleep(3 * time.Second)
			return
		}
	}
	exec.Command("pkill", "-9", "-f", "k3s server").Run()
	exec.Command("pkill", "-9", "-f", "etcd").Run()
	time.Sleep(3 * time.Second)
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

func (ks *K3sServer) deployIngressNginx() error {
	if exec.Command("k3s", "kubectl", "-n", "ingress-nginx", "get", "deployment", "ingress-nginx-controller").Run() == nil {
		return nil // already installed
	}
	ks.Logger().Info("Installing nginx ingress controller...")

	nginxURL := "https://raw.githubusercontent.com/kubernetes/ingress-nginx/controller-v1.12.0/deploy/static/provider/baremetal/deploy.yaml"
	cmd := exec.Command("k3s", "kubectl", "apply", "-f", nginxURL)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply nginx-ingress: %w: %s", err, string(output))
	}

	exec.Command("k3s", "kubectl", "-n", "ingress-nginx",
		"rollout", "status", "deployment/ingress-nginx-controller", "--timeout=180s").Run()

	patchJSON := `{"spec":{"ports":[{"name":"http","port":80,"targetPort":"http","nodePort":30080,"protocol":"TCP"},{"name":"https","port":443,"targetPort":"https","nodePort":30443,"protocol":"TCP"}]}}`
	exec.Command("k3s", "kubectl", "-n", "ingress-nginx", "patch", "svc", "ingress-nginx-controller",
		"--type=merge", "-p", patchJSON).Run()

	ks.Logger().Info("nginx-ingress installed (NodePort 30080/30443)")
	return nil
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
	if exec.Command("k3s", "kubectl", "-n", "longhorn-system", "get", "ingress", "longhorn-ingress").Run() == nil {
		return
	}
	// rewrite-target strips the /longhorn prefix before forwarding to the Longhorn UI,
	// which serves its assets from /, not from /longhorn.
	ingressYAML := `apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: longhorn-ingress
  namespace: longhorn-system
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: "0"
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

	// Deploy slurmrestd as a DaemonSet on the leader node only (host network = can reach
	// local slurmctld via 127.0.0.1:6817 or /var/run/slurm/slurmctld.socket).
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
        node-role.kubernetes.io/control-plane: "true"
      volumes:
      - name: munge-key
        secret:
          secretName: munge-key
          defaultMode: 0400
      - name: slurm-run
        hostPath:
          path: /var/run/munge
          type: DirectoryOrCreate
      initContainers:
      - name: wait-munge
        image: busybox:latest
        command: ['sh', '-c', 'until [ -S /var/run/munge/munge.socket.2 ]; do sleep 2; done']
        volumeMounts:
        - name: slurm-run
          mountPath: /var/run/munge
      containers:
      - name: slurmrestd
        image: ghcr.io/opencontainers/ubuntu:22.04
        command:
        - sh
        - -c
        - |
          apt-get update -qq && apt-get install -y -qq slurm-wlm slurmrestd munge 2>/dev/null
          cp /munge-secret/munge.key /etc/munge/munge.key
          chmod 400 /etc/munge/munge.key
          chown munge:munge /etc/munge/munge.key
          munged --force
          sleep 2
          exec slurmrestd -f /etc/slurm/slurm.conf -a rest_auth/munge 0.0.0.0:6820
        volumeMounts:
        - name: munge-key
          mountPath: /munge-secret
        - name: slurm-run
          mountPath: /var/run/munge
        ports:
        - containerPort: 6820
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

	// Add ingress rule for /slurm → slurmrestd.
	const slurmIngressYAML = `apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: slurmrestd-ingress
  namespace: slurm
  annotations:
    nginx.ingress.kubernetes.io/rewrite-target: /$2
    nginx.ingress.kubernetes.io/proxy-read-timeout: "120"
spec:
  ingressClassName: nginx
  rules:
  - http:
      paths:
      - path: /slurm(/|$)(.*)
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

	ks.slurmRestDeployed = true
	ks.Logger().Info("SLURM REST API deployed — NodePort 30819, ingress /slurm")
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
		"rollout", "status", "deployment/cert-manager-webhook", "--timeout=180s")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cert-manager webhook: %w: %s", err, string(output))
	}
	ks.Logger().Info("cert-manager ready")
	return nil
}

func (ks *K3sServer) deployRancher() error {
	if exec.Command("k3s", "kubectl", "-n", "cattle-system", "get", "deployment", "rancher").Run() == nil {
		return nil
	}
	ks.Logger().Info("Installing Rancher management UI...")

	// Use the node IP as the Rancher hostname so TLS SAN is correct.
	// We also expose via NodePort 30444 so users can reach it directly without
	// needing a DNS name (which ssl-passthrough requires for SNI routing).
	rancherHost := "rancher.cluster.local"
	if ks.nodeIP != "" {
		rancherHost = ks.nodeIP
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
		"--kubeconfig", "/etc/rancher/k3s/k3s.yaml",
	)
	cmd.Env = append(os.Environ(), "KUBECONFIG=/etc/rancher/k3s/k3s.yaml")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("helm install rancher: %w: %s", err, string(output))
	}

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

	// Add a wildcard ingress rule (no host = matches any Host header) so accessing the
	// nginx ingress on :30080 or :30443 routes to Rancher instead of the nginx default page.
	go func() {
		time.Sleep(30 * time.Second)
		ks.createRancherCatchallIngress(rancherHost)
	}()

	ks.Logger().Infof("Rancher installed — https://%s:30444 (admin/admin)", rancherHost)
	return nil
}

// createRancherCatchallIngress adds a wildcard ingress rule (no Host filter) so that
// bare-IP access to nginx on :30080/:30443 routes to Rancher instead of the nginx 404
// page.  This is separate from the Helm-managed ingress, which only matches the specific
// hostname used during install.
func (ks *K3sServer) createRancherCatchallIngress(_ string) {
	if exec.Command("k3s", "kubectl", "-n", "cattle-system", "get", "ingress", "rancher-catchall").Run() == nil {
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
	cmd := exec.Command("k3s", "kubectl", "apply", "-f", "-")
	cmd.Stdin = strings.NewReader(ingressYAML)
	if output, err := cmd.CombinedOutput(); err != nil {
		ks.Logger().Warnf("rancher catchall ingress: %v: %s", err, output)
	}
}
