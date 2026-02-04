package config

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/spf13/viper"
)

// Config represents the node configuration
type Config struct {
	Identity   IdentityConfig   `mapstructure:"identity"`
	Discovery  DiscoveryConfig  `mapstructure:"discovery"`
	Networking NetworkingConfig `mapstructure:"networking"`
	Tailscale  TailscaleConfig  `mapstructure:"tailscale"`
	Roles      RolesConfig      `mapstructure:"roles"`
	Logging    LoggingConfig    `mapstructure:"logging"`
	Cluster    ClusterConfig    `mapstructure:"cluster"`
}

// IdentityConfig contains identity-related settings
type IdentityConfig struct {
	Path string `mapstructure:"path"`
}

// DiscoveryConfig contains discovery-related settings
type DiscoveryConfig struct {
	BindAddr       string   `mapstructure:"bind_addr"`
	BindPort       int      `mapstructure:"bind_port"`
	BootstrapPeers []string `mapstructure:"bootstrap_peers"`
	NodeName       string   `mapstructure:"node_name"`
	EncryptKey     string   `mapstructure:"encrypt_key"`
}

// NetworkingConfig contains networking-related settings
type NetworkingConfig struct {
	Interface  string     `mapstructure:"interface"`
	ListenPort int        `mapstructure:"listen_port"`
	Subnet     string     `mapstructure:"subnet"`
	IPv6       bool       `mapstructure:"ipv6"`
	WiFi       WiFiConfig `mapstructure:"wifi"`
}

// WiFiConfig contains WiFi settings
type WiFiConfig struct {
	Enabled bool   `mapstructure:"enabled"`
	SSID    string `mapstructure:"ssid"`
	Key     string `mapstructure:"key"`
}

// TailscaleConfig contains Tailscale API settings
type TailscaleConfig struct {
	OAuthClientID     string `mapstructure:"oauth_client_id"`
	OAuthClientSecret string `mapstructure:"oauth_client_secret"`
	Tailnet           string `mapstructure:"tailnet"`
	APIDiscovery      bool   `mapstructure:"api_discovery"`
}

// RolesConfig contains role-related settings
type RolesConfig struct {
	Enabled      []string         `mapstructure:"enabled"`
	Capabilities CapabilitiesInfo `mapstructure:"capabilities"`
}

// CapabilitiesInfo describes node hardware capabilities
type CapabilitiesInfo struct {
	CPU  int    `mapstructure:"cpu"`
	RAM  string `mapstructure:"ram"`
	GPU  bool   `mapstructure:"gpu"`
	Arch string `mapstructure:"arch"`
}

// LoggingConfig contains logging settings
type LoggingConfig struct {
	Level  string `mapstructure:"level"`
	Format string `mapstructure:"format"`
	Output string `mapstructure:"output"`
}

// ClusterConfig contains cluster-wide settings
type ClusterConfig struct {
	Name         string `mapstructure:"name"`
	Region       string `mapstructure:"region"`
	Datacenter   string `mapstructure:"datacenter"`
	AuthKey      string `mapstructure:"auth_key"`      // Cluster authentication key (base64)
	ElectionMode string `mapstructure:"election_mode"` // "serf" (stateless) or "raft" (persistent)
}

// DefaultConfigPaths are the default paths to search for configuration files
var DefaultConfigPaths = []string{
	"/etc/cluster-os",
	"$HOME/.cluster-os",
	"./config",
	".",
}

// Load loads configuration from file and environment variables
func Load(configPath string) (*Config, error) {
	v := viper.New()

	// Set config file path if provided
	if configPath != "" {
		v.SetConfigFile(configPath)
	} else {
		// Search for config in default paths
		v.SetConfigName("node")
		v.SetConfigType("yaml")
		for _, path := range DefaultConfigPaths {
			v.AddConfigPath(path)
		}
	}

	// Set defaults
	setDefaults(v)

	// Read config file
	if err := v.ReadInConfig(); err != nil {
		if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
			return nil, fmt.Errorf("failed to read config file: %w", err)
		}
		// Config file not found is okay, we'll use defaults
		logrus.Warn("No config file found, using defaults")
	}

	// Allow environment variable overrides
	v.SetEnvPrefix("CLUSTEROS")
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()

	// Unmarshal configuration
	var config Config
	if err := v.Unmarshal(&config); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	// Load Tailscale OAuth credentials from file if not set in config
	if err := loadTailscaleOAuthConfig(&config); err != nil {
		logrus.Warnf("Failed to load Tailscale OAuth config: %v", err)
	}

	// Auto-detect capabilities if not set
	if err := autoDetectCapabilities(&config); err != nil {
		logrus.Warnf("Failed to auto-detect capabilities: %v", err)
	}

	// Set default node name if not provided
	if config.Discovery.NodeName == "" {
		hostname, err := os.Hostname()
		if err != nil {
			hostname = "unknown"
		}
		config.Discovery.NodeName = hostname
	}

	// Validate configuration
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("configuration validation failed: %w", err)
	}

	return &config, nil
}

// setDefaults sets default configuration values
func setDefaults(v *viper.Viper) {
	// Identity defaults
	v.SetDefault("identity.path", "/var/lib/cluster-os/identity.json")

	// Discovery defaults
	v.SetDefault("discovery.bind_addr", "0.0.0.0")
	v.SetDefault("discovery.bind_port", 7946)
	v.SetDefault("discovery.bootstrap_peers", []string{})

	// Networking defaults
	v.SetDefault("networking.interface", "wg0")
	v.SetDefault("networking.listen_port", 51820)
	v.SetDefault("networking.subnet", "10.42.0.0/16")
	v.SetDefault("networking.ipv6", false)
	v.SetDefault("networking.wifi.enabled", false)
	v.SetDefault("networking.wifi.ssid", "")
	v.SetDefault("networking.wifi.key", "")

	// Tailscale defaults
	v.SetDefault("tailscale.api_discovery", true)

	// Roles defaults
	v.SetDefault("roles.enabled", []string{"tailscale"})

	// Logging defaults
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")
	v.SetDefault("logging.output", "stdout")

	// Cluster defaults
	v.SetDefault("cluster.name", "cluster-os")
	v.SetDefault("cluster.region", "default")
	v.SetDefault("cluster.datacenter", "default")
	v.SetDefault("cluster.auth_key", "")          // Must be set by user
	v.SetDefault("cluster.election_mode", "serf") // "serf" (stateless) or "raft" (persistent)
}

// autoDetectCapabilities auto-detects hardware capabilities
func autoDetectCapabilities(config *Config) error {
	// Auto-detect CPU count
	if config.Roles.Capabilities.CPU == 0 {
		config.Roles.Capabilities.CPU = runtime.NumCPU()
	}

	// Auto-detect architecture
	if config.Roles.Capabilities.Arch == "" {
		config.Roles.Capabilities.Arch = runtime.GOARCH
	}

	// TODO: Implement RAM and GPU detection
	// For now, these remain as configured

	return nil
}

// Validate validates the configuration
func (c *Config) Validate() error {
	// Validate cluster authentication key
	if c.Cluster.AuthKey == "" {
		return fmt.Errorf("cluster.auth_key must be set - run scripts/generate-cluster-key.sh to create one")
	}

	// Validate discovery settings
	if c.Discovery.BindPort < 1 || c.Discovery.BindPort > 65535 {
		return fmt.Errorf("invalid discovery bind port: %d", c.Discovery.BindPort)
	}

	// Validate networking settings
	if c.Networking.ListenPort < 1 || c.Networking.ListenPort > 65535 {
		return fmt.Errorf("invalid networking listen port: %d", c.Networking.ListenPort)
	}

	// Validate logging level
	validLogLevels := map[string]bool{
		"debug": true,
		"info":  true,
		"warn":  true,
		"error": true,
	}
	if !validLogLevels[strings.ToLower(c.Logging.Level)] {
		return fmt.Errorf("invalid logging level: %s", c.Logging.Level)
	}

	// Validate logging format
	if c.Logging.Format != "json" && c.Logging.Format != "text" {
		return fmt.Errorf("invalid logging format: %s (must be 'json' or 'text')", c.Logging.Format)
	}

	return nil
}

// GetLogLevel returns the logrus log level from configuration
func (c *Config) GetLogLevel() logrus.Level {
	level, err := logrus.ParseLevel(c.Logging.Level)
	if err != nil {
		return logrus.InfoLevel
	}
	return level
}

// Save writes the configuration to the specified file
func Save(config *Config, configPath string) error {
	v := viper.New()

	// Set config file path
	v.SetConfigFile(configPath)
	v.SetConfigType("yaml")

	// Marshal configuration back to viper
	// We need to convert the struct back to a map that viper can write
	configMap := map[string]interface{}{
		"identity": map[string]interface{}{
			"path": config.Identity.Path,
		},
		"discovery": map[string]interface{}{
			"bind_addr":       config.Discovery.BindAddr,
			"bind_port":       config.Discovery.BindPort,
			"bootstrap_peers": config.Discovery.BootstrapPeers,
			"node_name":       config.Discovery.NodeName,
			"encrypt_key":     config.Discovery.EncryptKey,
		},
		"networking": map[string]interface{}{
			"interface":  config.Networking.Interface,
			"listen_port": config.Networking.ListenPort,
			"subnet":     config.Networking.Subnet,
			"ipv6":       config.Networking.IPv6,
			"wifi": map[string]interface{}{
				"enabled": config.Networking.WiFi.Enabled,
				"ssid":    config.Networking.WiFi.SSID,
				"key":     config.Networking.WiFi.Key,
			},
		},
		"roles": map[string]interface{}{
			"enabled": config.Roles.Enabled,
			"capabilities": map[string]interface{}{
				"cpu":  config.Roles.Capabilities.CPU,
				"ram":  config.Roles.Capabilities.RAM,
				"gpu":  config.Roles.Capabilities.GPU,
				"arch": config.Roles.Capabilities.Arch,
			},
		},
		"logging": map[string]interface{}{
			"level":  config.Logging.Level,
			"format": config.Logging.Format,
		},
		"cluster": map[string]interface{}{
			"name":          config.Cluster.Name,
			"region":        config.Cluster.Region,
			"datacenter":    config.Cluster.Datacenter,
			"auth_key":      config.Cluster.AuthKey,
			"election_mode": config.Cluster.ElectionMode,
		},
	}

	// Set all values in viper
	for section, values := range configMap {
		v.Set(section, values)
	}

	// Write config file
	if err := v.WriteConfig(); err != nil {
		return fmt.Errorf("failed to write config file: %w", err)
	}

	return nil
}

// loadTailscaleOAuthConfig loads Tailscale OAuth credentials from /etc/cluster-os/tailscale-oauth.conf
func loadTailscaleOAuthConfig(config *Config) error {
	file, err := os.Open("/etc/cluster-os/tailscale-oauth.conf")
	if err != nil {
		if os.IsNotExist(err) || os.IsPermission(err) {
			return nil // File doesn't exist or permission denied, not an error since config is optional
		}
		return err
	}
	defer file.Close()

	// Source the file as shell environment variables
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		// Parse KEY="value" format
		if parts := strings.SplitN(line, "=", 2); len(parts) == 2 {
			key := strings.TrimSpace(parts[0])
			value := strings.Trim(strings.TrimSpace(parts[1]), `"`)
			
			switch key {
			case "TAILSCALE_OAUTH_CLIENT_ID":
				if config.Tailscale.OAuthClientID == "" {
					config.Tailscale.OAuthClientID = value
				}
			case "TAILSCALE_OAUTH_CLIENT_SECRET":
				if config.Tailscale.OAuthClientSecret == "" {
					config.Tailscale.OAuthClientSecret = value
				}
			case "TAILSCALE_TAILNET":
				if config.Tailscale.Tailnet == "" {
					config.Tailscale.Tailnet = value
				}
			}
		}
	}

	return scanner.Err()
}

// IsBootstrap returns true if this node should bootstrap a new cluster
func (c *Config) IsBootstrap() bool {
	return len(c.Discovery.BootstrapPeers) == 0
}
