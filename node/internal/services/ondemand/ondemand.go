package ondemand

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"text/template"

	"github.com/cluster-os/node/internal/roles"
	"github.com/cluster-os/node/internal/state"
	"github.com/sirupsen/logrus"
)

// OpenOnDemand implements the Open OnDemand web portal role
type OpenOnDemand struct {
	*roles.BaseRole
	config       *Config
	clusterState *state.ClusterState
	apacheCmd    *exec.Cmd
	configPath   string
}

// Config contains configuration for Open OnDemand
type Config struct {
	ConfigPath     string
	ClusterName    string
	ServerName     string
	Port           int
	SlurmCluster   string
	AuthType       string // "pam", "ldap", "oidc"
	AllowedUsers   []string
	K3sEnabled     bool
	JupyterEnabled bool
}

// NewOpenOnDemand creates a new Open OnDemand role
func NewOpenOnDemand(roleConfig *roles.RoleConfig, logger *logrus.Logger) (roles.Role, error) {
	// Get hostname for server name
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "localhost"
	}

	config := &Config{
		ConfigPath:     "/etc/ood/config",
		ClusterName:    "cluster-os",
		ServerName:     hostname,
		Port:           8080,
		SlurmCluster:   "cluster-os",
		AuthType:       "pam",
		AllowedUsers:   []string{"clusteros"},
		K3sEnabled:     true,
		JupyterEnabled: true,
	}

	// Override from role config
	if val, ok := roleConfig.Config["config_path"].(string); ok {
		config.ConfigPath = val
	}
	if val, ok := roleConfig.Config["cluster_name"].(string); ok {
		config.ClusterName = val
	}
	if val, ok := roleConfig.Config["server_name"].(string); ok {
		config.ServerName = val
	}
	if val, ok := roleConfig.Config["port"].(int); ok {
		config.Port = val
	}
	if val, ok := roleConfig.Config["auth_type"].(string); ok {
		config.AuthType = val
	}

	return &OpenOnDemand{
		BaseRole:   roles.NewBaseRole("ondemand", logger),
		config:     config,
		configPath: config.ConfigPath,
	}, nil
}

// Start starts the Open OnDemand service
func (ood *OpenOnDemand) Start(ctx context.Context, clusterState *state.ClusterState) error {
	ood.Logger().Info("Starting Open OnDemand role")
	ood.clusterState = clusterState

	// Create necessary directories
	if err := ood.createDirectories(); err != nil {
		return fmt.Errorf("failed to create directories: %w", err)
	}

	// Don't start Apache yet - wait for leadership (runs on controller node)
	ood.SetRunning(true)
	ood.Logger().Info("Open OnDemand role started (waiting for leadership)")
	return nil
}

// Stop stops the Open OnDemand service
func (ood *OpenOnDemand) Stop(ctx context.Context) error {
	ood.Logger().Info("Stopping Open OnDemand role")

	if err := ood.stopApache(); err != nil {
		ood.Logger().Warnf("Error stopping Apache: %v", err)
	}

	ood.SetRunning(false)
	return nil
}

// Reconfigure regenerates configuration and restarts if leader
func (ood *OpenOnDemand) Reconfigure(clusterState *state.ClusterState) error {
	ood.Logger().Info("Reconfiguring Open OnDemand")
	ood.clusterState = clusterState

	if !ood.IsLeader() {
		return nil
	}

	// Regenerate all configurations
	if err := ood.generateAllConfigs(); err != nil {
		return fmt.Errorf("failed to generate configs: %w", err)
	}

	// Reload Apache
	exec.Command("systemctl", "reload", "apache2").Run()

	return nil
}

// HealthCheck checks if Open OnDemand is running
func (ood *OpenOnDemand) HealthCheck() error {
	if !ood.IsRunning() {
		return fmt.Errorf("Open OnDemand role is not running")
	}

	if ood.IsLeader() {
		// Check if Apache is running
		cmd := exec.Command("systemctl", "is-active", "apache2")
		if err := cmd.Run(); err != nil {
			// Try httpd (RHEL)
			cmd = exec.Command("systemctl", "is-active", "httpd")
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("Apache/httpd is not running")
			}
		}
	}

	return nil
}

// IsLeaderRequired returns true since OnDemand runs on the controller node
func (ood *OpenOnDemand) IsLeaderRequired() bool {
	return true
}

// OnLeadershipChange handles leadership changes
func (ood *OpenOnDemand) OnLeadershipChange(isLeader bool) error {
	ood.SetLeader(isLeader)

	if isLeader {
		ood.Logger().Info("Became Open OnDemand leader, starting service")
		return ood.startOnDemand()
	} else {
		ood.Logger().Info("Lost Open OnDemand leadership, stopping service")
		return ood.stopApache()
	}
}

// createDirectories creates necessary directories
func (ood *OpenOnDemand) createDirectories() error {
	dirs := []string{
		ood.configPath,
		filepath.Join(ood.configPath, "clusters.d"),
		filepath.Join(ood.configPath, "apps"),
		filepath.Join(ood.configPath, "apps", "sys"),
		filepath.Join(ood.configPath, "apps", "usr"),
		"/var/www/ood/apps/sys",
		"/var/www/ood/apps/sys/dashboard",
		"/var/www/ood/apps/sys/shell",
		"/var/www/ood/apps/sys/files",
		"/var/www/ood/apps/sys/activejobs",
		"/var/www/ood/apps/sys/myjobs",
		"/var/www/ood/apps/sys/bc_desktop",
		"/var/www/ood/public",
		"/var/log/ondemand-nginx",
		"/etc/ood/config/apps/bc_desktop",
		"/etc/ood/config/apps/jupyter",
		"/etc/ood/config/apps/vscode",
	}

	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}
	}

	return nil
}

// startOnDemand starts the Open OnDemand service
func (ood *OpenOnDemand) startOnDemand() error {
	ood.Logger().Info("Starting Open OnDemand service")

	// Generate all configurations
	if err := ood.generateAllConfigs(); err != nil {
		return fmt.Errorf("failed to generate configs: %w", err)
	}

	// Update OOD portal configuration
	cmd := exec.Command("sudo", "/opt/ood/ood-portal-generator/sbin/update_ood_portal")
	if out, err := cmd.CombinedOutput(); err != nil {
		ood.Logger().Warnf("Failed to update OOD portal: %v (%s)", err, string(out))
	}

	// Disable default Apache site and enable OOD site
	exec.Command("a2dissite", "000-default.conf").Run()
	exec.Command("a2ensite", "ood.conf").Run()

	// Start dashboard backend services (ttyd for terminal, filebrowser for files)
	ood.Logger().Info("Starting dashboard backend services...")

	// Start ttyd (web terminal)
	if out, err := exec.Command("systemctl", "start", "ttyd").CombinedOutput(); err != nil {
		ood.Logger().Warnf("Failed to start ttyd: %v (%s)", err, string(out))
	} else {
		ood.Logger().Info("ttyd (web terminal) started on port 8086")
	}

	// Start filebrowser
	if out, err := exec.Command("systemctl", "start", "filebrowser").CombinedOutput(); err != nil {
		ood.Logger().Warnf("Failed to start filebrowser: %v (%s)", err, string(out))
	} else {
		ood.Logger().Info("filebrowser started on port 8085")
	}

	// Start/restart Apache
	cmd = exec.Command("systemctl", "restart", "apache2")
	if out, err := cmd.CombinedOutput(); err != nil {
		// Try httpd (RHEL)
		cmd = exec.Command("systemctl", "restart", "httpd")
		if out2, err2 := cmd.CombinedOutput(); err2 != nil {
			return fmt.Errorf("failed to start Apache: apache2: %v (%s), httpd: %v (%s)",
				err, string(out), err2, string(out2))
		}
	}

	ood.Logger().Info("Open OnDemand started successfully")
	ood.Logger().Infof("Access dashboard at: http://%s:%d", ood.config.ServerName, ood.config.Port)
	return nil
}

// stopApache stops the Apache service and dashboard backends
func (ood *OpenOnDemand) stopApache() error {
	ood.Logger().Info("Stopping dashboard services")

	// Stop dashboard backend services
	exec.Command("systemctl", "stop", "ttyd").Run()
	exec.Command("systemctl", "stop", "filebrowser").Run()

	// Stop Apache
	exec.Command("systemctl", "stop", "apache2").Run()
	exec.Command("systemctl", "stop", "httpd").Run()

	if ood.apacheCmd != nil && ood.apacheCmd.Process != nil {
		if err := ood.apacheCmd.Process.Signal(syscall.SIGTERM); err != nil {
			ood.apacheCmd.Process.Kill()
		}
		ood.apacheCmd.Wait()
		ood.apacheCmd = nil
	}

	return nil
}

// generateAllConfigs generates all OnDemand configurations
func (ood *OpenOnDemand) generateAllConfigs() error {
	generators := []func() error{
		ood.generateSlurmClusterConfig,
		ood.generateK8sClusterConfig,
		ood.generateOODPortalConfig,
		ood.generateNginxStageConfig,
		ood.generateJupyterApp,
		ood.generateVSCodeApp,
		ood.generateDesktopApp,
		ood.generateShellApp,
		ood.generateFilesAppConfig,
		ood.generateJobTemplates,
		ood.generateDashboardConfig,
	}

	for _, gen := range generators {
		if err := gen(); err != nil {
			ood.Logger().Warnf("Config generation warning: %v", err)
			// Continue with other configs
		}
	}

	return nil
}

// generateSlurmClusterConfig generates the SLURM cluster configuration
func (ood *OpenOnDemand) generateSlurmClusterConfig() error {
	ood.Logger().Info("Generating SLURM cluster configuration")

	// Get SLURM controller address
	controllerAddr := "localhost"
	if leader, ok := ood.clusterState.GetLeaderNode("slurm-controller"); ok {
		controllerAddr = leader.Address
	}

	data := struct {
		ClusterName   string
		SlurmHost     string
		SlurmBinPath  string
		SlurmConfPath string
	}{
		ClusterName:   ood.config.SlurmCluster,
		SlurmHost:     controllerAddr,
		SlurmBinPath:  "/usr/bin",
		SlurmConfPath: "/etc/slurm",
	}

	content := fmt.Sprintf(`---
# %s SLURM cluster configuration for Open OnDemand
v2:
  metadata:
    title: "%s"
    url: null
    hidden: false
  login:
    host: "%s"
  job:
    adapter: "slurm"
    cluster: "%s"
    bin: "%s"
    conf: "%s/slurm.conf"
    copy_environment: true
  acls:
    - adapter: "group"
      groups:
        - "clusteros"
      type: "allowlist"
  batch_connect:
    basic:
      script_wrapper: |
        module purge 2>/dev/null || true
        %%s
      set_host: "host=$(hostname -s)"
    vnc:
      script_wrapper: |
        module purge 2>/dev/null || true
        export PATH="/opt/TurboVNC/bin:$PATH"
        export WEBSOCKIFY_CMD="/usr/bin/websockify"
        %%s
      set_host: "host=$(hostname -s)"
`, data.ClusterName, data.ClusterName, data.SlurmHost, data.ClusterName, data.SlurmBinPath, data.SlurmConfPath)

	clusterPath := filepath.Join(ood.configPath, "clusters.d", ood.config.SlurmCluster+".yml")
	if err := os.WriteFile(clusterPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write cluster config: %w", err)
	}

	ood.Logger().Infof("Generated SLURM cluster config: %s", clusterPath)
	return nil
}

// generateK8sClusterConfig generates the Kubernetes cluster configuration
func (ood *OpenOnDemand) generateK8sClusterConfig() error {
	if !ood.config.K3sEnabled {
		return nil
	}

	ood.Logger().Info("Generating Kubernetes cluster configuration")

	// Get K3s server address
	k3sHost := "localhost"
	if leader, ok := ood.clusterState.GetLeaderNode("k3s-server"); ok {
		k3sHost = leader.Address
	}

	content := fmt.Sprintf(`---
# Kubernetes (K3s) cluster configuration for Open OnDemand
v2:
  metadata:
    title: "ClusterOS Kubernetes"
    url: "https://%s:6443"
    hidden: false
  login:
    host: "%s"
  job:
    adapter: "kubernetes"
    cluster: "clusteros-k8s"
    bin: "/usr/local/bin/kubectl"
    config_file: "/etc/rancher/k3s/k3s.yaml"
    context: "default"
    namespace_prefix: "ood-user-"
    auto_supplemental_groups: true
    pod_security_policy: "privileged"
  batch_connect:
    basic:
      script_wrapper: |
        %%s
      set_host: "host=$POD_NAME"
`, k3sHost, k3sHost)

	clusterPath := filepath.Join(ood.configPath, "clusters.d", "k8s.yml")
	if err := os.WriteFile(clusterPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write k8s cluster config: %w", err)
	}

	ood.Logger().Infof("Generated K8s cluster config: %s", clusterPath)
	return nil
}

// generateOODPortalConfig generates the ood_portal.yml configuration
func (ood *OpenOnDemand) generateOODPortalConfig() error {
	ood.Logger().Info("Generating OOD portal configuration")

	content := fmt.Sprintf(`---
# ood_portal.yml - Open OnDemand portal configuration
# Generated by ClusterOS

# Listen settings
listen_addr_port:
  - "%d"

# Server settings
servername: %s
port: %d
ssl: null

# Authentication (PAM-based for ClusterOS)
auth:
  - "AuthType Basic"
  - "AuthName \"ClusterOS Login\""
  - "AuthBasicProvider PAM"
  - "AuthPAMService ood"
  - "Require valid-user"

# Security headers
security_csp_frame_ancestors: "https://%s http://%s"
security_strict_transport: false

# User mapping
user_map_match: ".*"
user_env: "REMOTE_USER"
map_fail_uri: "/register"

# Logout redirect
logout_redirect: "/pun/sys/dashboard/logout"

# Dashboard URL
pun_uri: "/pun"
node_uri: "/node"
rnode_uri: "/rnode"

# Public assets
public_uri: "/public"
public_root: "/var/www/ood/public"

# Analytics (disabled)
analytics: null

# Maintenance mode
use_maintenance: false

# Custom branding
navbar_type: "dark"
brand_bg_color: "#1a1a2e"
brand_link_active_bg_color: "#16213e"
`, ood.config.Port, ood.config.ServerName, ood.config.Port, ood.config.ServerName, ood.config.ServerName)

	portalPath := filepath.Join(ood.configPath, "ood_portal.yml")
	if err := os.WriteFile(portalPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write portal config: %w", err)
	}

	ood.Logger().Infof("Generated portal config: %s", portalPath)
	return nil
}

// generateNginxStageConfig generates the nginx_stage.yml configuration
func (ood *OpenOnDemand) generateNginxStageConfig() error {
	ood.Logger().Info("Generating nginx stage configuration")

	content := `---
# nginx_stage.yml - Open OnDemand nginx stage configuration

ondemand_version_path: "/opt/ood/VERSION"
ondemand_portal_path: "/etc/ood/config/ood_portal.yml"

# PUN (Per-User Nginx) configuration
pun_custom_env:
  OOD_DASHBOARD_TITLE: "ClusterOS Dashboard"
  OOD_BRAND_BG_COLOR: "#1a1a2e"
  OOD_BRAND_LINK_ACTIVE_BG_COLOR: "#16213e"
  OOD_DASHBOARD_SUPPORT_URL: "https://github.com/cluster-os/cluster-os"
  SLURM_CONF: "/etc/slurm/slurm.conf"
  KUBECONFIG: "/etc/rancher/k3s/k3s.yaml"

# Template paths
template_root: "/opt/ood/nginx_stage/templates"

# User validation
user_regex: '[\w@.\-]+'
min_uid: 1000
disabled_shell: "/access/denied"

# App configuration
app_config_path:
  sys: "/etc/ood/config/apps/sys/%<name>s"
  usr: "/etc/ood/config/apps/usr/%<owner>s/%<name>s"
  dev: "~/%<owner>s/%<name>s"

# Passenger settings
passenger_pool_idle_time: 300
passenger_options: {}
`

	configPath := filepath.Join(ood.configPath, "nginx_stage.yml")
	if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
		return fmt.Errorf("failed to write nginx stage config: %w", err)
	}

	ood.Logger().Infof("Generated nginx stage config: %s", configPath)
	return nil
}

// generateJupyterApp generates the Jupyter interactive app configuration
func (ood *OpenOnDemand) generateJupyterApp() error {
	if !ood.config.JupyterEnabled {
		return nil
	}

	ood.Logger().Info("Generating Jupyter app configuration")

	appDir := "/etc/ood/config/apps/jupyter"
	os.MkdirAll(appDir, 0755)

	// Form configuration
	formConfig := `---
# Jupyter Interactive App - Form Configuration
cluster: "cluster-os"
form:
  - bc_account
  - bc_num_hours
  - bc_num_slots
  - bc_queue
  - jupyterlab_switch
  - extra_args

attributes:
  bc_num_hours:
    value: 4
    min: 1
    max: 24
    step: 1
    label: "Hours"
    help: "Number of hours for the job"

  bc_num_slots:
    value: 1
    min: 1
    max: 16
    step: 1
    label: "CPU Cores"
    help: "Number of CPU cores"

  bc_queue:
    value: "all"
    label: "Partition"
    help: "SLURM partition to submit to"

  jupyterlab_switch:
    widget: "check_box"
    value: 1
    label: "Use JupyterLab"
    help: "Launch JupyterLab instead of classic Jupyter"

  extra_args:
    value: ""
    label: "Extra Jupyter Arguments"
    help: "Additional arguments to pass to Jupyter"
`
	if err := os.WriteFile(filepath.Join(appDir, "form.yml"), []byte(formConfig), 0644); err != nil {
		return err
	}

	// Submit configuration
	submitConfig := `---
# Jupyter Interactive App - Submit Configuration
batch_connect:
  template: "basic"

script:
  native:
    - "-N"
    - "1"
    - "-n"
    - "<%= bc_num_slots %>"
    - "-t"
    - "<%= bc_num_hours %>:00:00"
    - "-p"
    - "<%= bc_queue %>"
`
	if err := os.WriteFile(filepath.Join(appDir, "submit.yml.erb"), []byte(submitConfig), 0644); err != nil {
		return err
	}

	// Launch script
	launchScript := `#!/bin/bash
# Jupyter launch script for Open OnDemand

# Setup environment
export HOME="$HOME"
export XDG_RUNTIME_DIR=""

# Find Python/Jupyter
if command -v jupyter &> /dev/null; then
    JUPYTER_CMD="jupyter"
elif command -v jupyter-lab &> /dev/null; then
    JUPYTER_CMD="jupyter-lab"
elif [ -f /opt/conda/bin/jupyter ]; then
    JUPYTER_CMD="/opt/conda/bin/jupyter"
else
    echo "ERROR: Jupyter not found"
    exit 1
fi

# Determine mode (lab vs notebook)
<% if jupyterlab_switch == "1" %>
JUPYTER_MODE="lab"
<% else %>
JUPYTER_MODE="notebook"
<% end %>

# Generate token
export JUPYTER_TOKEN=$(openssl rand -hex 24)

# Get port from OnDemand
port=$(find_port)

# Create Jupyter config
mkdir -p ~/.jupyter
cat > ~/.jupyter/jupyter_config.py << JUPYTERCONF
c.NotebookApp.ip = '0.0.0.0'
c.NotebookApp.port = $port
c.NotebookApp.open_browser = False
c.NotebookApp.token = '$JUPYTER_TOKEN'
c.NotebookApp.allow_origin = '*'
c.NotebookApp.base_url = '${JUPYTER_BASE_URL:-/}'
c.NotebookApp.trust_xheaders = True
c.NotebookApp.disable_check_xsrf = False
JUPYTERCONF

# Start Jupyter
echo "Starting Jupyter $JUPYTER_MODE on port $port"
$JUPYTER_CMD $JUPYTER_MODE --config=~/.jupyter/jupyter_config.py <%= extra_args %>
`
	if err := os.WriteFile(filepath.Join(appDir, "template", "script.sh.erb"), []byte(launchScript), 0755); err != nil {
		os.MkdirAll(filepath.Join(appDir, "template"), 0755)
		os.WriteFile(filepath.Join(appDir, "template", "script.sh.erb"), []byte(launchScript), 0755)
	}

	// Manifest
	manifest := `---
name: Jupyter
category: Interactive Apps
subcategory: Servers
description: |
  Launch a Jupyter Notebook or JupyterLab session on a cluster compute node.
icon: fa://book
`
	os.WriteFile(filepath.Join(appDir, "manifest.yml"), []byte(manifest), 0644)

	ood.Logger().Infof("Generated Jupyter app config: %s", appDir)
	return nil
}

// generateVSCodeApp generates VS Code Server interactive app
func (ood *OpenOnDemand) generateVSCodeApp() error {
	ood.Logger().Info("Generating VS Code app configuration")

	appDir := "/etc/ood/config/apps/vscode"
	os.MkdirAll(appDir, 0755)

	formConfig := `---
# VS Code Server Interactive App
cluster: "cluster-os"
form:
  - bc_num_hours
  - bc_num_slots
  - bc_queue
  - working_dir

attributes:
  bc_num_hours:
    value: 4
    min: 1
    max: 24
  bc_num_slots:
    value: 1
    min: 1
    max: 8
  bc_queue:
    value: "all"
  working_dir:
    value: ""
    label: "Working Directory"
    help: "Directory to open (defaults to home)"
`
	os.WriteFile(filepath.Join(appDir, "form.yml"), []byte(formConfig), 0644)

	submitConfig := `---
batch_connect:
  template: "basic"
script:
  native:
    - "-N"
    - "1"
    - "-n"
    - "<%= bc_num_slots %>"
    - "-t"
    - "<%= bc_num_hours %>:00:00"
    - "-p"
    - "<%= bc_queue %>"
`
	os.WriteFile(filepath.Join(appDir, "submit.yml.erb"), []byte(submitConfig), 0644)

	manifest := `---
name: VS Code
category: Interactive Apps
subcategory: IDEs
description: |
  Launch a VS Code Server session for remote development.
icon: fa://code
`
	os.WriteFile(filepath.Join(appDir, "manifest.yml"), []byte(manifest), 0644)

	ood.Logger().Infof("Generated VS Code app config: %s", appDir)
	return nil
}

// generateDesktopApp generates remote desktop configuration
func (ood *OpenOnDemand) generateDesktopApp() error {
	ood.Logger().Info("Generating Desktop app configuration")

	appDir := "/etc/ood/config/apps/bc_desktop"
	os.MkdirAll(appDir, 0755)

	// Desktop form
	formConfig := `---
# Remote Desktop Interactive App
cluster: "cluster-os"
form:
  - bc_vnc_resolution
  - bc_num_hours
  - bc_num_slots
  - bc_queue
  - desktop

attributes:
  bc_vnc_resolution:
    required: true
    label: "Resolution"
    options:
      - ["1920x1080", "1920x1080"]
      - ["1280x720", "1280x720"]
      - ["1024x768", "1024x768"]
  bc_num_hours:
    value: 2
    min: 1
    max: 8
  bc_num_slots:
    value: 2
    min: 1
    max: 8
  bc_queue:
    value: "all"
  desktop:
    widget: "select"
    label: "Desktop Environment"
    options:
      - ["XFCE", "xfce"]
      - ["MATE", "mate"]
      - ["GNOME", "gnome"]
`
	os.WriteFile(filepath.Join(appDir, "form.yml"), []byte(formConfig), 0644)

	submitConfig := `---
batch_connect:
  template: "vnc"
script:
  native:
    - "-N"
    - "1"
    - "-n"
    - "<%= bc_num_slots %>"
    - "-t"
    - "<%= bc_num_hours %>:00:00"
    - "-p"
    - "<%= bc_queue %>"
`
	os.WriteFile(filepath.Join(appDir, "submit.yml.erb"), []byte(submitConfig), 0644)

	manifest := `---
name: Remote Desktop
category: Interactive Apps
subcategory: Desktops
description: |
  Launch a remote desktop session on a compute node.
icon: fa://desktop
`
	os.WriteFile(filepath.Join(appDir, "manifest.yml"), []byte(manifest), 0644)

	ood.Logger().Infof("Generated Desktop app config: %s", appDir)
	return nil
}

// generateShellApp generates shell access configuration
func (ood *OpenOnDemand) generateShellApp() error {
	ood.Logger().Info("Generating Shell app configuration")

	appDir := "/etc/ood/config/apps/shell"
	os.MkdirAll(appDir, 0755)

	// Shell app initializers
	initScript := `---
# Shell app configuration
ssh_hosts:
  - host: "localhost"
    title: "ClusterOS Login Node"
`
	os.WriteFile(filepath.Join(appDir, "initializers", "ssh_hosts.yml"), []byte(initScript), 0644)
	os.MkdirAll(filepath.Join(appDir, "initializers"), 0755)
	os.WriteFile(filepath.Join(appDir, "initializers", "ssh_hosts.yml"), []byte(initScript), 0644)

	ood.Logger().Infof("Generated Shell app config: %s", appDir)
	return nil
}

// generateFilesAppConfig generates file browser configuration
func (ood *OpenOnDemand) generateFilesAppConfig() error {
	ood.Logger().Info("Generating Files app configuration")

	appDir := "/etc/ood/config/apps/files"
	os.MkdirAll(appDir, 0755)

	// Files app env
	envConfig := `---
# File browser environment configuration
OOD_FILES_SSH_HOST: "localhost"
`
	os.WriteFile(filepath.Join(appDir, "env"), []byte(envConfig), 0644)

	ood.Logger().Infof("Generated Files app config: %s", appDir)
	return nil
}

// generateJobTemplates generates common SLURM job templates
func (ood *OpenOnDemand) generateJobTemplates() error {
	ood.Logger().Info("Generating job templates")

	templatesDir := "/var/www/ood/apps/sys/myjobs/templates"
	os.MkdirAll(templatesDir, 0755)

	// Basic job template
	basicJob := `#!/bin/bash
#SBATCH --job-name=my_job
#SBATCH --output=output_%j.log
#SBATCH --error=error_%j.log
#SBATCH --ntasks=1
#SBATCH --cpus-per-task=1
#SBATCH --time=01:00:00
#SBATCH --partition=all

# Your commands here
echo "Job started on $(hostname) at $(date)"
echo "Working directory: $(pwd)"

# Example: Run a Python script
# python my_script.py

# Example: Run an MPI job
# mpirun -np $SLURM_NTASKS ./my_mpi_program

echo "Job finished at $(date)"
`
	os.WriteFile(filepath.Join(templatesDir, "basic_job.sh"), []byte(basicJob), 0644)

	// MPI job template
	mpiJob := `#!/bin/bash
#SBATCH --job-name=mpi_job
#SBATCH --output=mpi_output_%j.log
#SBATCH --error=mpi_error_%j.log
#SBATCH --nodes=2
#SBATCH --ntasks-per-node=4
#SBATCH --time=02:00:00
#SBATCH --partition=all

# Load MPI module if needed
# module load openmpi

echo "MPI Job started at $(date)"
echo "Running on nodes: $SLURM_JOB_NODELIST"
echo "Number of tasks: $SLURM_NTASKS"

# Run MPI application
mpirun -np $SLURM_NTASKS ./my_mpi_program

echo "MPI Job finished at $(date)"
`
	os.WriteFile(filepath.Join(templatesDir, "mpi_job.sh"), []byte(mpiJob), 0644)

	// Python job template
	pythonJob := `#!/bin/bash
#SBATCH --job-name=python_job
#SBATCH --output=python_%j.log
#SBATCH --error=python_error_%j.log
#SBATCH --ntasks=1
#SBATCH --cpus-per-task=4
#SBATCH --mem=8G
#SBATCH --time=04:00:00
#SBATCH --partition=all

# Activate conda environment if using
# source /opt/conda/etc/profile.d/conda.sh
# conda activate myenv

echo "Python Job started at $(date)"
echo "Python version: $(python --version)"

# Run your Python script
python my_script.py

echo "Python Job finished at $(date)"
`
	os.WriteFile(filepath.Join(templatesDir, "python_job.sh"), []byte(pythonJob), 0644)

	// GPU job template
	gpuJob := `#!/bin/bash
#SBATCH --job-name=gpu_job
#SBATCH --output=gpu_%j.log
#SBATCH --error=gpu_error_%j.log
#SBATCH --ntasks=1
#SBATCH --cpus-per-task=4
#SBATCH --gres=gpu:1
#SBATCH --mem=16G
#SBATCH --time=04:00:00
#SBATCH --partition=all

# Load CUDA module if needed
# module load cuda

echo "GPU Job started at $(date)"
echo "CUDA devices: $CUDA_VISIBLE_DEVICES"

# Check GPU
nvidia-smi

# Run your GPU application
python train_model.py

echo "GPU Job finished at $(date)"
`
	os.WriteFile(filepath.Join(templatesDir, "gpu_job.sh"), []byte(gpuJob), 0644)

	// Array job template
	arrayJob := `#!/bin/bash
#SBATCH --job-name=array_job
#SBATCH --output=array_%A_%a.log
#SBATCH --error=array_error_%A_%a.log
#SBATCH --array=1-10
#SBATCH --ntasks=1
#SBATCH --cpus-per-task=1
#SBATCH --time=01:00:00
#SBATCH --partition=all

echo "Array Job started at $(date)"
echo "Array Task ID: $SLURM_ARRAY_TASK_ID"

# Process task based on array index
python process_data.py --task-id $SLURM_ARRAY_TASK_ID

echo "Array Task $SLURM_ARRAY_TASK_ID finished at $(date)"
`
	os.WriteFile(filepath.Join(templatesDir, "array_job.sh"), []byte(arrayJob), 0644)

	// Create template manifest
	manifest := `---
templates:
  - name: "Basic Job"
    path: "basic_job.sh"
    description: "Simple single-node job template"
  - name: "MPI Job"
    path: "mpi_job.sh"
    description: "Multi-node MPI job template"
  - name: "Python Job"
    path: "python_job.sh"
    description: "Python script job template"
  - name: "GPU Job"
    path: "gpu_job.sh"
    description: "GPU-enabled job template"
  - name: "Array Job"
    path: "array_job.sh"
    description: "Job array template for parallel tasks"
`
	os.WriteFile(filepath.Join(templatesDir, "manifest.yml"), []byte(manifest), 0644)

	ood.Logger().Infof("Generated job templates: %s", templatesDir)
	return nil
}

// generateDashboardConfig generates dashboard customization
func (ood *OpenOnDemand) generateDashboardConfig() error {
	ood.Logger().Info("Generating dashboard configuration")

	appDir := "/etc/ood/config/apps/dashboard"
	os.MkdirAll(appDir, 0755)

	// Dashboard environment
	envConfig := fmt.Sprintf(`# Dashboard environment
OOD_DASHBOARD_TITLE=ClusterOS Dashboard
OOD_BRAND_BG_COLOR=#1a1a2e
OOD_BRAND_LINK_ACTIVE_BG_COLOR=#16213e
SLURM_CONF=/etc/slurm/slurm.conf
KUBECONFIG=/etc/rancher/k3s/k3s.yaml
`)
	os.WriteFile(filepath.Join(appDir, "env"), []byte(envConfig), 0644)

	// Dashboard initializers
	initDir := filepath.Join(appDir, "initializers")
	os.MkdirAll(initDir, 0755)

	// Announcements/MOTD
	motd := `---
# Dashboard announcements
announcements:
  - type: info
    msg: |
      Welcome to ClusterOS! Access computing resources through the menus above.

      **Quick Links:**
      - [Submit a SLURM Job](/pun/sys/myjobs/new)
      - [Launch Jupyter](/pun/sys/jupyter/session/new)
      - [Browse Files](/pun/sys/files)
`
	os.WriteFile(filepath.Join(initDir, "announcements.yml"), []byte(motd), 0644)

	// Custom navigation
	navConfig := `---
# Custom navigation links
nav_categories:
  - title: "Interactive Apps"
    apps:
      - sys/jupyter
      - sys/vscode
      - sys/bc_desktop
  - title: "Jobs"
    apps:
      - sys/myjobs
      - sys/activejobs
  - title: "Files"
    apps:
      - sys/files
  - title: "Clusters"
    apps:
      - sys/shell

pinned_apps:
  - sys/jupyter
  - sys/myjobs
  - sys/files
  - sys/shell
`
	os.WriteFile(filepath.Join(initDir, "navigation.yml"), []byte(navConfig), 0644)

	// Support info
	supportConfig := `---
# Support configuration
support_ticket:
  email: "support@clusteros.local"

docs_url: "https://github.com/cluster-os/cluster-os"
`
	os.WriteFile(filepath.Join(initDir, "support.yml"), []byte(supportConfig), 0644)

	ood.Logger().Infof("Generated dashboard config: %s", appDir)
	return nil
}

// Helper template function
func writeTemplate(path string, tmpl *template.Template, data interface{}) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return tmpl.Execute(f, data)
}
