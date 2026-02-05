package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cluster-os/node/internal/config"
	"github.com/cluster-os/node/internal/daemon"
	"github.com/cluster-os/node/internal/identity"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

var (
	Version   = "dev"
	Commit    = "unknown"
	BuildTime = "unknown"
)

func main() {
	app := &cli.App{
		Name:    "node-agent",
		Usage:   "ClusterOS Node Agent",
		Version: fmt.Sprintf("%s (commit: %s, built: %s)", Version, Commit, BuildTime),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Value:   "/etc/clusteros/node.yaml",
				Usage:   "Path to configuration file",
				EnvVars: []string{"NODE_CONFIG_PATH"},
			},
			&cli.StringFlag{
				Name:    "log-level",
				Aliases: []string{"l"},
				Value:   "info",
				Usage:   "Log level (debug, info, warn, error)",
			},
		},
		Commands: []*cli.Command{
			{
				Name:  "start",
				Usage: "Start the node agent",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:    "foreground",
						Aliases: []string{"f"},
						Usage:   "Run in foreground (don't daemonize)",
						Value:   false,
					},
				},
				Action: startCommand,
			},
			{
				Name:   "init",
				Usage:  "Initialize node identity",
				Action: initCommand,
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "identity-path",
						Aliases: []string{"i"},
						Value:   "/var/lib/cluster-os/identity.json",
						Usage:   "Path to store identity file",
					},
				},
			},
			{
				Name:   "info",
				Usage:  "Show node information",
				Action: infoCommand,
			},
			{
				Name:   "status",
				Usage:  "Show node status",
				Action: statusCommand,
			},
			{
				Name:    "version",
				Aliases: []string{"v"},
				Usage:   "Show version information",
				Action: func(c *cli.Context) error {
					fmt.Printf("node-agent %s\n", Version)
					fmt.Printf("Commit: %s\n", Commit)
					fmt.Printf("Built: %s\n", BuildTime)
					return nil
				},
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}

func startCommand(c *cli.Context) error {
	// Setup logger
	logger := logrus.New()
	level, err := logrus.ParseLevel(c.String("log-level"))
	if err != nil {
		logger.Warnf("Invalid log level %s, using info", c.String("log-level"))
		level = logrus.InfoLevel
	}
	logger.SetLevel(level)
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp: true,
	})

	// Load configuration
	configPath := c.String("config")
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config from %s: %w", configPath, err)
	}

	// Override log level from config if set
	if cfg.Logging.Level != "" {
		if configLevel, err := logrus.ParseLevel(cfg.Logging.Level); err == nil {
			logger.SetLevel(configLevel)
		}
	}

	logger.Infof("Starting ClusterOS Node Agent %s", Version)
	logger.Infof("Config: %s", configPath)

	// Load or create identity
	identityPath := cfg.Identity.Path
	if identityPath == "" {
		identityPath = "/var/lib/cluster-os/identity.json"
	}

	// Ensure identity directory exists
	if err := os.MkdirAll(filepath.Dir(identityPath), 0700); err != nil {
		return fmt.Errorf("failed to create identity directory: %w", err)
	}

	id, _, err := identity.LoadOrGenerate(identityPath)
	if err != nil {
		return fmt.Errorf("failed to load or create identity: %w", err)
	}

	logger.Infof("Node ID: %s", id.NodeID)

	// Create and start daemon
	d, err := daemon.New(&daemon.Config{
		Config:   cfg,
		Identity: id,
		Logger:   logger,
	})
	if err != nil {
		return fmt.Errorf("failed to create daemon: %w", err)
	}

	logger.Info("Starting daemon...")
	if err := d.Start(); err != nil {
		return fmt.Errorf("failed to start daemon: %w", err)
	}

	logger.Info("Node agent started successfully")
	return nil
}

func initCommand(c *cli.Context) error {
	identityPath := c.String("identity-path")

	// Ensure directory exists
	if err := os.MkdirAll(filepath.Dir(identityPath), 0700); err != nil {
		return fmt.Errorf("failed to create identity directory: %w", err)
	}

	// Check if identity already exists
	if _, err := os.Stat(identityPath); err == nil {
		fmt.Printf("Identity already exists at %s\n", identityPath)
		
		// Load and display existing identity
		id, err := identity.Load(identityPath)
		if err != nil {
			return fmt.Errorf("failed to load existing identity: %w", err)
		}
		
		fmt.Printf("Node ID: %s\n", id.NodeID)
		return nil
	}

	// Create new identity
	fmt.Printf("Creating new identity at %s...\n", identityPath)
	id, err := identity.Generate()
	if err != nil {
		return fmt.Errorf("failed to generate identity: %w", err)
	}

	if err := id.Save(identityPath); err != nil {
		return fmt.Errorf("failed to save identity: %w", err)
	}

	fmt.Printf("Identity created successfully\n")
	fmt.Printf("Node ID: %s\n", id.NodeID)
	return nil
}

func infoCommand(c *cli.Context) error {
	// Load configuration
	configPath := c.String("config")
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Load identity
	identityPath := cfg.Identity.Path
	if identityPath == "" {
		identityPath = "/var/lib/cluster-os/identity.json"
	}

	id, err := identity.Load(identityPath)
	if err != nil {
		return fmt.Errorf("failed to load identity: %w", err)
	}

	fmt.Println("Node Information")
	fmt.Println("================")
	fmt.Printf("Node ID:       %s\n", id.NodeID)
	fmt.Printf("Cluster:       %s\n", cfg.Cluster.Name)
	fmt.Printf("Region:        %s\n", cfg.Cluster.Region)
	fmt.Printf("Datacenter:    %s\n", cfg.Cluster.Datacenter)
	fmt.Printf("Enabled Roles: %v\n", cfg.Roles.Enabled)
	fmt.Println()

	return nil
}

func statusCommand(c *cli.Context) error {
	// This would connect to the running daemon to get status
	// For now, just show basic info
	fmt.Println("Node Status")
	fmt.Println("===========")
	fmt.Println("Status: Check systemctl status node-agent")
	fmt.Println()
	
	return nil
}
