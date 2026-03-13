package controller

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/cluster-os/node/internal/roles"
	"github.com/cluster-os/node/internal/services/slurm/auth"
	"github.com/sirupsen/logrus"
)

// WorkerInfo describes a SLURM worker node for config generation.
type WorkerInfo struct {
	Name    string
	Addr    string // Tailscale IP or LAN IP
	CPUs    int
	MemMB   int // RAM in MB (0 = fall back to 4096)
	GPUs    int // GPU count (0 = no GPU, omit Gres line)
	TmpDisk int // scratch disk space in MB (0 = omit from NodeName line)
}

// SLURMController manages the slurmctld daemon on the elected leader node.
// Startup is driven by the phase machine — no leadership callbacks needed.
type SLURMController struct {
	*roles.BaseRole
	controllerIP    string
	mungeKey        []byte
	workers         []WorkerInfo
	slurmdbdHost    string
	slurmctldCmd    *exec.Cmd
	mungeKeyManager *auth.MungeKeyManager
	slurmConfPath   string
	configPath      string
	statePath       string
}

// NewSLURMControllerRole creates a controller with all values known at construction time.
// mungeKey and workers come directly from the phase machine — no polling, no retries.
func NewSLURMControllerRole(controllerIP string, mungeKey []byte, workers []WorkerInfo, slurmdbdHost string, logger *logrus.Logger) *SLURMController {
	return &SLURMController{
		BaseRole:        roles.NewBaseRole("slurm-controller", logger),
		controllerIP:    controllerIP,
		mungeKey:        mungeKey,
		workers:         workers,
		slurmdbdHost:    slurmdbdHost,
		slurmConfPath:   "/etc/slurm/slurm.conf",
		configPath:      "/etc/slurm",
		statePath:       "/var/lib/slurm",
		mungeKeyManager: auth.NewMungeKeyManager(logger),
	}
}

// Start writes config, sets up munge, and starts slurmctld.
// Called once by the phase machine after the munge key is published.
func (sc *SLURMController) Start() error {
	sc.Logger().Info("Starting SLURM controller")

	if err := sc.createDirectories(); err != nil {
		return fmt.Errorf("create directories: %w", err)
	}

	if err := sc.generateConfig(); err != nil {
		return fmt.Errorf("generate slurm.conf: %w", err)
	}

	if err := sc.mungeKeyManager.WriteMungeKey(sc.mungeKey); err != nil {
		return fmt.Errorf("write munge key: %w", err)
	}

	if err := sc.mungeKeyManager.StartMungeDaemon(); err != nil {
		return fmt.Errorf("start munge: %w", err)
	}

	if err := sc.startSlurmctld(); err != nil {
		return fmt.Errorf("start slurmctld: %w", err)
	}

	sc.SetRunning(true)
	return nil
}

// Stop stops slurmctld
func (sc *SLURMController) Stop(ctx context.Context) error {
	sc.Logger().Info("Stopping SLURM controller")
	return sc.stopSlurmctld()
}

// HealthCheck verifies slurmctld is alive, restarting if needed
func (sc *SLURMController) HealthCheck() error {
	if sc.slurmctldCmd == nil || sc.slurmctldCmd.Process == nil {
		sc.Logger().Warn("slurmctld not running — attempting restart")
		return sc.startSlurmctld()
	}
	if err := sc.slurmctldCmd.Process.Signal(syscall.Signal(0)); err != nil {
		sc.Logger().Warnf("slurmctld process died: %v — restarting", err)
		sc.slurmctldCmd.Wait()
		sc.slurmctldCmd = nil
		return sc.startSlurmctld()
	}
	return nil
}

// Reconfigure updates slurm.conf and hot-reloads slurmctld without a full restart.
// SIGHUP causes slurmctld to re-read the config; scontrol reconfigure applies it to
// currently running jobs/nodes and picks up newly added NodeName entries.
func (sc *SLURMController) Reconfigure(workers []WorkerInfo) error {
	sc.workers = workers
	if err := sc.generateConfig(); err != nil {
		return err
	}
	if sc.slurmctldCmd != nil && sc.slurmctldCmd.Process != nil {
		sc.slurmctldCmd.Process.Signal(syscall.SIGHUP)
	}
	// Give slurmctld time to re-read the file, then apply the reconfiguration.
	// This is the equivalent of 'scontrol reconfigure' which propagates the new
	// NodeName entries to slurmd processes without restarting slurmctld.
	go func() {
		time.Sleep(2 * time.Second)
		exec.Command("scontrol", "reconfigure").Run()
		sc.Logger().Info("SLURM reconfigure applied")
	}()
	return nil
}

func (sc *SLURMController) createDirectories() error {
	for _, dir := range []string{sc.configPath, sc.statePath, filepath.Join(sc.statePath, "slurmctld")} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	// Log directory
	os.MkdirAll("/var/log/slurm", 0755)
	return nil
}

func (sc *SLURMController) startSlurmctld() error {
	cmd := exec.Command("slurmctld", "-D", "-f", sc.slurmConfPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start slurmctld: %w", err)
	}
	sc.slurmctldCmd = cmd
	sc.Logger().Infof("slurmctld started (PID %d)", cmd.Process.Pid)

	// Add an iptables REDIRECT rule so that client tools (squeue, sbatch, sinfo)
	// running on this same node can reach slurmctld without going through the
	// Tailscale overlay.  When squeue connects to the Tailscale IP on port 6817,
	// the OUTPUT chain redirects the packet to 127.0.0.1:6817 instead.
	// This avoids failures when Tailscale is in a degraded state (DERP relay,
	// NAT traversal in progress, etc.) while still keeping the Tailscale IP as
	// the SlurmctldHost for remote workers.
	if sc.controllerIP != "" && sc.controllerIP != "127.0.0.1" {
		exec.Command("iptables", "-t", "nat", "-I", "OUTPUT",
			"-d", sc.controllerIP, "-p", "tcp", "--dport", "6817",
			"-j", "REDIRECT", "--to-ports", "6817").Run()
		sc.Logger().Debugf("Added iptables REDIRECT for slurmctld local access (%s:6817 → 127.0.0.1:6817)", sc.controllerIP)
	}

	// Reap the process when it exits so HealthCheck can detect death via nil check.
	// Without this, slurmctld becomes a zombie on exit: kill(pid,0) on a zombie
	// returns 0 (success), so Signal(0) never errors and HealthCheck never restarts.
	go func(c *exec.Cmd) {
		c.Wait()
		sc.Logger().Warnf("slurmctld (PID %d) exited — HealthCheck will restart", c.Process.Pid)
		if sc.slurmctldCmd == c {
			sc.slurmctldCmd = nil
			sc.SetRunning(false)
		}
	}(cmd)
	return nil
}

func (sc *SLURMController) stopSlurmctld() error {
	cmd := sc.slurmctldCmd // capture before reaper goroutine can nil it
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	// Remove the local REDIRECT rule added at startup.
	if sc.controllerIP != "" && sc.controllerIP != "127.0.0.1" {
		exec.Command("iptables", "-t", "nat", "-D", "OUTPUT",
			"-d", sc.controllerIP, "-p", "tcp", "--dport", "6817",
			"-j", "REDIRECT", "--to-ports", "6817").Run()
	}
	cmd.Process.Signal(os.Interrupt)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		<-done
	}
	sc.slurmctldCmd = nil
	sc.SetRunning(false)
	return nil
}

func (sc *SLURMController) generateConfig() error {
	// Determine accounting storage settings.
	// slurmdbd listens on port 6819 (not the REST NodePort 30819).
	acctHost := ""
	acctPort := 6819
	if sc.slurmdbdHost != "" && isTCPReachable(sc.slurmdbdHost, acctPort) {
		acctHost = sc.slurmdbdHost
		sc.Logger().Infof("SlurmDBD reachable at %s:%d — enabling accounting", acctHost, acctPort)
	} else {
		sc.Logger().Info("SlurmDBD not reachable — starting without accounting")
	}

	// Compute controller memory in MB (cap at 256 GB, floor at 1024 MB).
	controllerMemMB := 1024
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					kb, _ := strconv.ParseInt(fields[1], 10, 64)
					mb := int(kb / 1024)
					if mb > 1024 {
						controllerMemMB = mb
					}
				}
				break
			}
		}
	}

	// slurmctld calls gethostname() to decide if it is the controller.
	// SlurmctldHost must match that hostname exactly; using a bare IP causes
	// slurmctld to exit immediately with "Not running on SlurmctldHost" when
	// the daemon has renamed the host to "node-X-Y" via setUniqueHostname.
	// The "hostname(addr)" syntax lets SLURM clients resolve the connection
	// address from the IP while slurmctld still matches via the hostname.
	controllerHostname, _ := os.Hostname()
	if controllerHostname == "" {
		controllerHostname = sc.controllerIP
	}

	controllerGPUs := localGPUCount()

	// HasGPUs is true if any node in the cluster has at least one GPU.
	// When true, slurm.conf must declare GresTypes=gpu and each GPU node
	// gets a Gres=gpu:N entry on its NodeName line.
	hasGPUs := controllerGPUs > 0
	for _, w := range sc.workers {
		if w.GPUs > 0 {
			hasGPUs = true
			break
		}
	}

	data := struct {
		ControllerNode        string
		ControllerHostname    string
		ControllerCPUs        int
		ControllerMemMB       int
		ControllerGPUs        int
		HasGPUs               bool
		Workers               []WorkerInfo
		AccountingStorageHost string
		AccountingStoragePort int
	}{
		ControllerNode:        sc.controllerIP,
		ControllerHostname:    controllerHostname,
		ControllerCPUs:        runtime.NumCPU(),
		ControllerMemMB:       controllerMemMB,
		ControllerGPUs:        controllerGPUs,
		HasGPUs:               hasGPUs,
		Workers:               sc.workers,
		AccountingStorageHost: acctHost,
		AccountingStoragePort: acctPort,
	}

	tmpl, err := template.New("slurm.conf").Parse(slurmConfTemplate)
	if err != nil {
		return fmt.Errorf("parse template: %w", err)
	}

	tmpPath := sc.slurmConfPath + ".tmp"
	f, err := os.Create(tmpPath)
	if err != nil {
		return fmt.Errorf("create temp conf: %w", err)
	}
	defer f.Close()

	if err := tmpl.Execute(f, data); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("execute template: %w", err)
	}
	f.Close()

	if err := os.Rename(tmpPath, sc.slurmConfPath); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("rename conf: %w", err)
	}
	sc.Logger().Infof("Generated slurm.conf (controller=%s, workers=%d)", sc.controllerIP, len(sc.workers))
	return nil
}

// localGPUCount returns the number of GPUs visible on this node.
// Detects NVIDIA GPUs via /dev/nvidia[0-9]* and AMD GPUs via
// /sys/class/drm/renderD* with vendor ID 0x1002.
func localGPUCount() int {
	count := 0
	// NVIDIA: each GPU exposes /dev/nvidia0, /dev/nvidia1, …
	if entries, err := filepath.Glob("/dev/nvidia[0-9]*"); err == nil {
		count += len(entries)
	}
	// AMD: renderD128, renderD129, … — filter by vendor ID
	if renders, err := filepath.Glob("/sys/class/drm/renderD*"); err == nil {
		for _, r := range renders {
			vendor, _ := os.ReadFile(filepath.Join(r, "device", "vendor"))
			if strings.TrimSpace(string(vendor)) == "0x1002" {
				count++
			}
		}
	}
	return count
}

func isTCPReachable(host string, port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 3*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

const slurmConfTemplate = `# SLURM Configuration — auto-generated by Cluster-OS
ClusterName=cluster-os
SlurmctldHost={{.ControllerHostname}}({{.ControllerNode}})
SlurmctldPort=6817
SlurmctldParameters=enable_configless
AuthType=auth/munge
CryptoType=crypto/munge

{{if .AccountingStorageHost}}
AccountingStorageType=accounting_storage/slurmdbd
AccountingStorageHost={{.AccountingStorageHost}}
AccountingStoragePort={{.AccountingStoragePort}}
AccountingStorageEnforce=associations,limits,qos
JobAcctGatherType=jobacct_gather/linux
JobAcctGatherFrequency=30
{{else}}
AccountingStorageType=accounting_storage/none
{{end}}

SchedulerType=sched/backfill
SelectType=select/cons_tres
SelectTypeParameters=CR_Core_Memory
{{if .HasGPUs}}GresTypes=gpu{{end}}

SlurmctldLogFile=/var/log/slurm/slurmctld.log
SlurmdLogFile=/var/log/slurm/slurmd.log
SlurmctldDebug=info
SlurmdDebug=info

StateSaveLocation=/var/lib/slurm/slurmctld
SlurmdSpoolDir=/var/lib/slurm/slurmd

ProctrackType=proctrack/linuxproc
TaskPlugin=task/none

MpiDefault=pmix
MpiParams=ports=12000-12999
PrologFlags=Alloc

# Controller node — also participates as a compute node so jobs can run on it.
# slurmd on the controller uses -N {{.ControllerNode}} to register with this exact NodeName.
NodeName={{.ControllerNode}} NodeAddr={{.ControllerNode}} CPUs={{if le .ControllerCPUs 1}}1{{else}}{{.ControllerCPUs}}{{end}} RealMemory={{.ControllerMemMB}}{{if gt .ControllerGPUs 0}} Gres=gpu:{{.ControllerGPUs}}{{end}} State=UNKNOWN
{{range .Workers}}
NodeName={{.Name}} NodeAddr={{.Addr}} CPUs={{if le .CPUs 1}}1{{else}}{{.CPUs}}{{end}} RealMemory={{if gt .MemMB 0}}{{.MemMB}}{{else}}4096{{end}}{{if gt .GPUs 0}} Gres=gpu:{{.GPUs}}{{end}}{{if gt .TmpDisk 0}} TmpDisk={{.TmpDisk}}{{end}} State=UNKNOWN
{{end}}

PartitionName=all Nodes=ALL Default=YES MaxTime=INFINITE State=UP
`
