package config

import (
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
	Name       string `mapstructure:"name"`
	Region     string `mapstructure:"region"`
	Datacenter string `mapstructure:"datacenter"`
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
	v.SetDefault("networking.wifi.enabled", true)
	v.SetDefault("networking.wifi.ssid", "TALKTALK665317")
	v.SetDefault("networking.wifi.key", "NXJP7U39")

	// Roles defaults
	v.SetDefault("roles.enabled", []string{"wireguard"})

	// Logging defaults
	v.SetDefault("logging.level", "info")
	v.SetDefault("logging.format", "json")
	v.SetDefault("logging.output", "stdout")

	// Cluster defaults
	v.SetDefault("cluster.name", "cluster-os")
	v.SetDefault("cluster.region", "default")
	v.SetDefault("cluster.datacenter", "default")
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

// IsBootstrap returns true if this node should bootstrap a new cluster
func (c *Config) IsBootstrap() bool {
	return len(c.Discovery.BootstrapPeers) == 0
}
