package k3s

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/cluster-os/node/internal/roles"
	"github.com/sirupsen/logrus"
)

// K3sAgent manages the k3s agent process on worker nodes.
// All connection details (serverURL, token) are passed explicitly by the
// phase machine — no cluster-state lookups, no retry loops.
type K3sAgent struct {
	*roles.BaseRole
	serverURL string
	token     string
	nodeIP    string
	dataDir   string
	k3sCmd    *exec.Cmd

	// nodeNotFoundCount counts consecutive 10-second windows in which the
	// kubelet logged "node not found" errors.  When this reaches the threshold
	// the agent is restarted so it re-bootstraps against the current leader,
	// re-creating the node object in the API server's etcd.
	nodeNotFoundCount int
}

// NewK3sAgentRole creates a K3sAgent with explicit connection parameters.
func NewK3sAgentRole(serverURL, token, nodeIP string, logger *logrus.Logger) *K3sAgent {
	return &K3sAgent{
		BaseRole:  roles.NewBaseRole("k3s-agent", logger),
		serverURL: serverURL,
		token:     token,
		nodeIP:    nodeIP,
		dataDir:   "/var/lib/rancher/k3s",
	}
}

// Start starts the k3s agent connecting to the given server.
// If a local k3s server is already running, this is a no-op.
func (ka *K3sAgent) Start() error {
	ka.Logger().Infof("Starting k3s agent → %s", ka.serverURL)

	if err := os.MkdirAll(ka.dataDir, 0755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	// Don't start agent if a local k3s server is already running
	if ka.isLocalK3sServerRunning() {
		ka.Logger().Info("Local k3s server running — skipping agent start")
		ka.SetRunning(true)
		return nil
	}

	if ka.serverURL == "" {
		return fmt.Errorf("k3s server URL is empty")
	}
	if ka.token == "" {
		return fmt.Errorf("k3s token is empty")
	}

	// Pre-flight: verify the remote k3s server is FULLY READY before starting the agent.
	//
	// A plain TCP dial to port 6443 succeeds as soon as k3s starts its listener —
	// before CA certs are generated, before etcd bootstraps, before the API is ready.
	// If we start the agent at that point, its internal supervisor (port 6444) begins
	// fetching CA certs from the server, gets RSTs back (server not ready yet), and
	// logs "failed to get CA certs: connection reset by peer" every 2 seconds.
	//
	// Fix: probe the /cacerts endpoint directly over HTTPS (InsecureSkipVerify — we
	// don't have the CA yet, that's what we're checking for).  A 200 OK means the
	// server has finished CA cert generation and is ready to bootstrap agents.
	// Any error (connection refused, RST, non-200) → return error → caller retries
	// after 30s rather than flooding the log for 2.5 min before HealthCheck kills it.
	caCheckURL := ka.serverURL + "/cacerts"
	caClient := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		},
	}
	resp, err := caClient.Get(caCheckURL)
	if err != nil {
		return fmt.Errorf("k3s server %s not ready (CA certs unavailable, will retry): %w", ka.serverURL, err)
	}
	io.Copy(io.Discard, resp.Body) //nolint:errcheck
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("k3s server %s CA cert endpoint returned %d (not ready, will retry)", ka.serverURL, resp.StatusCode)
	}

	// Belt-and-suspenders: flush nat OUTPUT immediately before starting k3s.
	// setupFirewallRules() already did this at daemon startup, but if something
	// re-added redirect rules in the interim (e.g. a concurrent kube-proxy restart
	// writing a jump rule that carries over stale REDIRECT entries), clear them now.
	// This is the last line of defence before containerd begins pulling images.
	exec.Command("iptables", "-t", "nat", "-F", "OUTPUT").Run() //nolint:errcheck

	// Kill any stale k3s agent process from a previous run.
	// Without this, restarting runJoining() leaves an orphaned agent process
	// that conflicts with the new one (duplicate node registration, port conflicts).
	ka.killExistingAgent()

	// Clean up orphaned kubelet pod volume bind-mounts before k3s starts.
	// When k3s agent is killed (SIGKILL), Longhorn local-volumes and other CSI
	// drivers leave nested bind-mounts inside /var/lib/kubelet/pods/<uid>/volumes/.
	// On the next start kubelet GC tries rmdir() — which fails with "directory not
	// empty" because the bind-mount is still active.  Pre-unmounting here silences
	// the "orphaned pod found, but failed to rmdir() volume" error spam.
	ka.cleanupOrphanedPodVolumes()

	// Purge the cached agent server config.
	// k3s agent writes <dataDir>/agent/etc/k3s.yaml with the server URL it
	// bootstrapped against.  On restart, k3s reads this file and contacts the
	// CACHED server address rather than the --server CLI flag, so if this node
	// was previously an agent pointing at a different leader (or at localhost
	// when it was a server itself), the agent will keep connecting to the wrong
	// address.  Removing the file forces a clean bootstrap against ka.serverURL.
	ka.purgeAgentCache()

	args := []string{
		"agent",
		"--server", ka.serverURL,
		"--token", ka.token,
		"--data-dir", ka.dataDir,
		// Use the official k8s pause image — avoids Docker Hub which is frequently
		// unreachable (connection refused / rate-limited).  Must match the server's
		// --pause-image so every node uses the same sandbox image digest.
		"--pause-image", "registry.k8s.io/pause:3.6",
	}
	if ka.nodeIP != "" {
		args = append(args, "--node-ip", ka.nodeIP)
	}
	// Force Flannel to use the Tailscale interface as the VXLAN endpoint.
	// This ensures cross-node pod traffic uses the stable Tailscale overlay
	// rather than a potentially unreachable LAN IP.
	args = append(args, "--flannel-iface", "tailscale0")

	cmd := exec.Command("k3s", args...)

	// Strip JOURNAL_STREAM so k3s writes to fd 2 (our pipe) instead of the journal.
	//
	// systemd sets JOURNAL_STREAM=fd:offset in node-agent's environment; k3s
	// inherits it and opens /run/systemd/journal/socket directly to write log
	// entries, bypassing fd 1/2 entirely.  Our pipe filter never sees those writes
	// and "failed to get CA certs" appears unfiltered at level=error in the journal.
	// Removing the variable forces k3s to use its logrus TextFormatter → stderr.
	filteredEnv := make([]string, 0, len(os.Environ()))
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "JOURNAL_STREAM=") {
			filteredEnv = append(filteredEnv, e)
		}
	}
	cmd.Env = filteredEnv

	// Pipe both stdout and stderr through a filter that demotes expected bootstrap
	// noise to Debug so it doesn't appear as errors in the journal.
	//
	// "failed to get CA certs" — k3s agent internal LB (6444) RSTs connections
	//     during its backend health-probe warm-up.  Normal; resolves in ~30 s.
	filterLine := func(line string) {
		switch {
		case strings.Contains(line, "failed to get CA certs"):
			// Expected bootstrap noise during 6444 LB warm-up — demote to debug.
			ka.Logger().Debugf("[k3s-agent] %s", line)
		case strings.Contains(line, "node") && strings.Contains(line, "not found") &&
			(strings.Contains(line, "kubelet_node_status") || strings.Contains(line, "nodelease") || strings.Contains(line, "eviction_manager")):
			// Kubelet cannot find its own node object in the API server.
			// This happens when the k3s server's etcd was wiped (e.g. after a
			// leader re-provision) while this agent was already running.
			// Increment the stuck counter so HealthCheck can trigger a re-bootstrap.
			ka.nodeNotFoundCount++
			fmt.Fprintln(os.Stderr, line)
		default:
			fmt.Fprintln(os.Stderr, line)
		}
	}

	stdoutR, stdoutW, stdoutPipeErr := os.Pipe()
	stderrR, stderrW, stderrPipeErr := os.Pipe()

	if stdoutPipeErr != nil {
		cmd.Stdout = os.Stdout
	} else {
		cmd.Stdout = stdoutW
	}
	if stderrPipeErr != nil {
		cmd.Stderr = os.Stderr
	} else {
		cmd.Stderr = stderrW
	}

	if err := cmd.Start(); err != nil {
		for _, w := range []*os.File{stdoutW, stderrW} {
			if w != nil {
				w.Close()
			}
		}
		for _, r := range []*os.File{stdoutR, stderrR} {
			if r != nil {
				r.Close()
			}
		}
		return fmt.Errorf("k3s agent start: %w", err)
	}

	startFilter := func(r *os.File, w *os.File) {
		if w == nil {
			return
		}
		w.Close()
		go func() {
			defer r.Close()
			scanner := bufio.NewScanner(r)
			for scanner.Scan() {
				filterLine(scanner.Text())
			}
		}()
	}
	startFilter(stdoutR, stdoutW)
	startFilter(stderrR, stderrW)

	ka.k3sCmd = cmd
	ka.SetRunning(true)
	ka.Logger().Infof("k3s agent started (PID %d)", cmd.Process.Pid)

	// Reap the process when it exits so HealthCheck can detect death via nil check.
	// Without this, k3s agent becomes a zombie: kill(pid,0) returns 0 on zombies,
	// so Signal(0) never errors and HealthCheck never triggers a restart.
	go func(c *exec.Cmd) {
		c.Wait()
		ka.Logger().Warnf("k3s agent (PID %d) exited — HealthCheck will restart", c.Process.Pid)
		if ka.k3sCmd == c {
			ka.k3sCmd = nil
			ka.SetRunning(false)
		}
	}(cmd)

	// Pre-seed the pause image on this worker node too — same reason as the server.
	go ensurePauseImageInContainerd(ka.Logger())

	return nil
}

// consecutiveCAFailures tracks how many health-check cycles the agent's
// supervisor (6444) has been in a stuck-bootstrap state.
// Declared outside HealthCheck so it persists across calls.
var consecutiveCAFailures int

// HealthCheck verifies the k3s agent process is alive, restarting if needed.
//
// The "stuck bootstrap" state: the agent process is alive and port 6444 accepts
// TCP connections, but the internal load-balancer immediately resets TLS connections
// because the upstream k3s server (6443) is unreachable.  This manifests as:
//   "read tcp 127.0.0.1:XXXXX->127.0.0.1:6444: read: connection reset by peer"
//
// A plain net.DialTimeout on 6444 SUCCEEDS in this state (TCP handshake works),
// so we distinguish healthy from stuck by also attempting a Read with a short
// deadline:
//   healthy: server waits for TLS ClientHello → Read times out (not a reset)
//   stuck:   server immediately sends RST    → Read returns "connection reset by peer"
//
// After 5 consecutive stuck checks (~2.5 min) we force-restart the agent so that
// killExistingAgent() can clear any stale k3s server process holding port 6444.
func (ka *K3sAgent) HealthCheck() error {
	if ka.k3sCmd == nil || ka.k3sCmd.Process == nil {
		ka.Logger().Warn("k3s agent not running — attempting restart")
		consecutiveCAFailures = 0
		ka.nodeNotFoundCount = 0
		return ka.Start()
	}
	if err := ka.k3sCmd.Process.Signal(syscall.Signal(0)); err != nil {
		ka.Logger().Warnf("k3s agent process died: %v — restarting", err)
		ka.k3sCmd.Wait()
		ka.k3sCmd = nil
		ka.SetRunning(false)
		consecutiveCAFailures = 0
		ka.nodeNotFoundCount = 0
		return ka.Start()
	}

	// Probe port 6444: first check TCP open, then probe for immediate reset.
	conn, err := net.DialTimeout("tcp", "127.0.0.1:6444", 2*time.Second)
	if err != nil {
		// Port not listening at all — agent starting or dead.
		consecutiveCAFailures++
	} else {
		// TCP handshake succeeded.  Now attempt a Read with a short deadline.
		// A healthy k3s supervisor waits for the TLS ClientHello before responding,
		// so a Read will time out (not a reset).  A stuck supervisor immediately resets.
		conn.SetDeadline(time.Now().Add(300 * time.Millisecond)) //nolint:errcheck
		buf := make([]byte, 1)
		_, readErr := conn.Read(buf)
		conn.Close()

		if readErr != nil && strings.Contains(readErr.Error(), "connection reset by peer") {
			// Supervisor accepted TCP but immediately reset — stuck bootstrap state.
			consecutiveCAFailures++
			ka.Logger().Debugf("k3s agent supervisor (6444) accepted TCP but reset immediately (stuck, check %d/5)", consecutiveCAFailures)
		} else {
			// Timeout (healthy — server waiting for ClientHello) or any other error.
			consecutiveCAFailures = 0
		}
	}

	if consecutiveCAFailures >= 5 {
		ka.Logger().Warnf("k3s agent supervisor (6444) stuck for %d consecutive checks — restarting to clear stale process", consecutiveCAFailures)
		if ka.k3sCmd != nil && ka.k3sCmd.Process != nil {
			ka.k3sCmd.Process.Kill()
		}
		ka.k3sCmd = nil
		ka.SetRunning(false)
		consecutiveCAFailures = 0
		ka.nodeNotFoundCount = 0
		return ka.Start()
	}

	// Detect persistent "node not found" from the kubelet status sync loop.
	//
	// When the k3s server's etcd is wiped (leader re-provision) and then restarts
	// fresh, the node object for this worker is gone.  The agent process stays alive
	// and port 6444 is healthy, so the checks above never fire.  Meanwhile the
	// kubelet logs "node not found" every 10 s indefinitely.
	//
	// nodeNotFoundCount is incremented by filterLine each time the pattern appears.
	// The threshold of 18 matches (≈3 minutes of continuous errors, 6 errors per
	// 30-second HealthCheck window) avoids false-positives during normal startup where
	// 1–2 transient "not found" lines are expected before the node is registered.
	const nodeNotFoundThreshold = 18
	if ka.nodeNotFoundCount >= nodeNotFoundThreshold {
		ka.Logger().Warnf("kubelet has logged %d consecutive 'node not found' errors — "+
			"node object missing from API server (etcd was likely wiped on leader re-provision); "+
			"restarting k3s agent to re-bootstrap and re-register node", ka.nodeNotFoundCount)
		if ka.k3sCmd != nil && ka.k3sCmd.Process != nil {
			ka.k3sCmd.Process.Kill()
		}
		ka.k3sCmd = nil
		ka.SetRunning(false)
		consecutiveCAFailures = 0
		ka.nodeNotFoundCount = 0
		return ka.Start()
	}

	if ka.nodeNotFoundCount > 0 {
		ka.Logger().Debugf("k3s agent: %d 'node not found' log lines seen (threshold %d for restart)",
			ka.nodeNotFoundCount, nodeNotFoundThreshold)
	}

	return nil
}

// Stop terminates the k3s agent process.
func (ka *K3sAgent) Stop(ctx context.Context) error {
	ka.Logger().Info("Stopping k3s agent")
	cmd := ka.k3sCmd // capture before reaper goroutine can nil it
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
	ka.k3sCmd = nil
	ka.SetRunning(false)
	return nil
}

// killExistingAgent kills any stale k3s processes before starting a new agent.
// This includes both k3s agent AND k3s server — when a node demotes from leader to
// worker, the old server process must be killed before the agent can bind its ports.
// It also kills orphaned k3s-embedded containerd, which is a separate subprocess
// that survives SIGKILL and retains the old sandbox_image config in memory.
func (ka *K3sAgent) killExistingAgent() {
	killed := false
	for _, pattern := range []string{"k3s agent", "k3s server"} {
		out, err := exec.Command("pgrep", "-f", pattern).Output()
		if err != nil || len(out) == 0 {
			continue
		}
		ka.Logger().Infof("Killing stale '%s' process(es) before agent start", pattern)
		exec.Command("pkill", "-TERM", "-f", pattern).Run()
		killed = true
	}
	if !killed {
		return
	}
	// Wait for processes to exit (graceful first, then force).
	for i := 0; i < 8; i++ {
		time.Sleep(1 * time.Second)
		agentGone := func() bool {
			o, _ := exec.Command("pgrep", "-f", "k3s agent").Output(); return len(o) == 0
		}()
		serverGone := func() bool {
			o, _ := exec.Command("pgrep", "-f", "k3s server").Output(); return len(o) == 0
		}()
		if agentGone && serverGone {
			break
		}
	}
	// Force kill remaining processes.
	exec.Command("pkill", "-KILL", "-f", "k3s agent").Run()
	exec.Command("pkill", "-KILL", "-f", "k3s server").Run()
	time.Sleep(1 * time.Second)

	// Kill orphaned k3s-embedded containerd unconditionally.
	//
	// Previously we only killed containerd when the socket existed. But
	// apply-patch.sh removes the socket before node-agent starts, so the old
	// containerd process is alive without a socket. If we skip the kill, k3s
	// sees "no socket" and starts a NEW containerd alongside the still-running
	// old one. The old process may recreate the socket before it finally exits,
	// and k3s latches onto it — inheriting stale sandbox_image in memory.
	//
	// Killing unconditionally guarantees a clean-slate containerd start every
	// time, so k3s always generates a fresh config.toml with the correct image.
	ka.Logger().Info("Killing any orphaned k3s containerd before agent start")
	exec.Command("pkill", "-TERM", "-f", "/run/k3s/containerd/containerd").Run() //nolint:errcheck
	time.Sleep(500 * time.Millisecond)
	exec.Command("pkill", "-KILL", "-f", "/run/k3s/containerd/containerd").Run() //nolint:errcheck
	os.Remove("/run/k3s/containerd/containerd.sock")                              //nolint:errcheck
}

// cleanupOrphanedPodVolumes force-unmounts lingering kubelet pod volume bind-mounts
// before k3s agent starts.  This must run AFTER killExistingAgent() has stopped all
// k3s processes — any active pod mounts are now orphaned and safe to remove.
//
// Why this is needed:
//   Longhorn local-volumes (kubernetes.io~local-volume) use nested bind-mounts inside
//   /var/lib/kubelet/pods/<uid>/volumes/.  When k3s is SIGKILLed, those mounts are
//   left behind.  On restart, kubelet GC finds the orphaned pod directory, calls
//   os.Remove() on it, and fails with "directory not empty" — even though the pod
//   no longer exists in the API server.  This fills the journal with the error:
//     "orphaned pod found, but failed to rmdir() volume ... directory not empty"
//   every 2 seconds indefinitely.
//
// Fix: enumerate all mount points under /var/lib/kubelet/pods in reverse depth order
// (deepest first so child mounts are removed before parents), lazy-unmount each one,
// then do a belt-and-suspenders recursive force-unmount of the whole tree.
func (ka *K3sAgent) cleanupOrphanedPodVolumes() {
	const podsDir = "/var/lib/kubelet/pods"
	if _, err := os.Stat(podsDir); err != nil {
		return // no pods directory, nothing to do
	}

	// findmnt -R lists all mounts under podsDir recursively (one per line).
	// Output order is parent-before-child; we need to unmount deepest first.
	out, err := exec.Command("findmnt", "-R", "-n", "-o", "TARGET", podsDir).Output()
	if err == nil && len(out) > 0 {
		var mounts []string
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if mp := strings.TrimSpace(line); mp != "" && mp != podsDir {
				mounts = append(mounts, mp)
			}
		}
		// Sort by path length descending: longest path = deepest mount = unmount first.
		sort.Slice(mounts, func(i, j int) bool { return len(mounts[i]) > len(mounts[j]) })
		for _, mp := range mounts {
			exec.Command("umount", "-l", mp).Run() //nolint:errcheck
		}
		if len(mounts) > 0 {
			ka.Logger().Infof("Lazy-unmounted %d orphaned pod volume mount(s) under %s", len(mounts), podsDir)
		}
	}

	// Belt-and-suspenders: recursive force-unmount for anything findmnt missed.
	exec.Command("umount", "-R", "-f", podsDir).Run() //nolint:errcheck
}

// purgeAgentCache removes the cached agent connection config so k3s bootstraps
// against the current ka.serverURL rather than a stale cached address.
//
// Only the connection config is removed (not CA certs or client credentials):
//   - etc/k3s.yaml        — cached server URL; may point to old leader
//   - etc/k3s.yaml.d/     — cached load-balancer server list
//
// Keeping server-ca.crt and client certs in place is intentional: the k3s
// agent's internal load-balancer (port 6444) uses them for its backend health
// probe TLS handshake.  Removing them makes the probe fall back to InsecureSkipVerify
// which is slower to confirm and prolongs the "failed to get CA certs" bootstrap
// window.  apply-patch.sh step 5 already performs a full credential wipe on
// every patch deployment; runtime restarts only need the connection config cleared.
func (ka *K3sAgent) purgeAgentCache() {
	agentDir := filepath.Join(ka.dataDir, "agent")

	// Remove the load-balancer/supervisor config (caches old server URL + endpoints).
	// Without this, the agent reconnects to the previous leader's address
	// even when --server points to the new one.
	if err := os.RemoveAll(filepath.Join(agentDir, "etc")); err == nil {
		ka.Logger().Info("Purged stale k3s agent connection config (etc/)")
	}
}

// isLocalK3sServerRunning returns true if a k3s server is listening on port 6443
func (ka *K3sAgent) isLocalK3sServerRunning() bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:6443", 1*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
