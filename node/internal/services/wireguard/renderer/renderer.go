package renderer

import (
	"bytes"
	"fmt"
	"text/template"
	"time"
)

// PeerConfig represents a WireGuard peer configuration
type PeerConfig struct {
	NodeName            string
	NodeID              string
	PublicKey           string
	Endpoint            string
	AllowedIPs          string
	PersistentKeepalive int
}

// InterfaceConfig represents the WireGuard interface configuration
type InterfaceConfig struct {
	PrivateKey  string
	Address     string
	ListenPort  int
	MTU         int
	PostUp      string
	PreDown     string
	Peers       []PeerConfig
	GeneratedAt string
}

// Render renders the WireGuard configuration from a template
func Render(templatePath string, config *InterfaceConfig) (string, error) {
	// Set generation timestamp
	config.GeneratedAt = time.Now().UTC().Format(time.RFC3339)

	// Set default MTU if not specified
	if config.MTU == 0 {
		config.MTU = 1420
	}

	// Read template
	tmpl, err := template.ParseFiles(templatePath)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	// Execute template
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, config); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return buf.String(), nil
}

// RenderFromString renders the WireGuard configuration from a template string
func RenderFromString(templateStr string, config *InterfaceConfig) (string, error) {
	// Set generation timestamp
	config.GeneratedAt = time.Now().UTC().Format(time.RFC3339)

	// Set default MTU if not specified
	if config.MTU == 0 {
		config.MTU = 1420
	}

	// Parse template
	tmpl, err := template.New("wireguard").Parse(templateStr)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	// Execute template
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, config); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return buf.String(), nil
}
