package main

import (
	"fmt"
	"github.com/cluster-os/node/internal/config"
)

func main() {
	cfg, err := config.Load("config/node.yaml")
	if err != nil {
		fmt.Println("Error:", err)
	} else {
		fmt.Println("Loaded successfully")
		fmt.Printf("Roles enabled: %v\n", cfg.Roles.Enabled)
	}
}