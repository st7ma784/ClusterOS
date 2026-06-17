package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

const (
	tailscaleScanInterval = 30 * time.Second
	// Bootstrap burst — aggressive probing right after node-agent starts so a
	// cold-boot cluster meshes before the election window closes. See
	// StartTailscalePeerDiscovery.
	bootstrapProbeWindow   = 60 * time.Second
	bootstrapProbeInterval = 5 * time.Second
)

// StartTailscalePeerDiscovery probes all online Tailscale peers for Serf
// every 30 seconds and joins any that are listening on port 7946.
//
// This is the primary join mechanism for nodes that share a Tailscale network
// but have no hardcoded bootstrap_peers in node.yaml.  Without this, new nodes
// never find each other even though the IP-level connectivity already exists.
func (sd *SerfDiscovery) StartTailscalePeerDiscovery(ctx context.Context) {
	go func() {
		sd.logger.Info("[ts] Tailscale peer discovery enabled — will probe online peers for Serf")
		// Run immediately so fresh-boot join is fast.
		sd.probeAndJoinTailscalePeers()

		// Bootstrap burst: on a simultaneous cold boot, peers' tailscaled often
		// isn't "Online" yet when the first probe runs, so it finds nothing. The
		// election window can close (15–30s) before the next steady-state probe
		// (every 30s) ever runs. Probe aggressively for the first ~60s so a peer
		// that finishes booting a few seconds later is discovered well within the
		// election window.
		bootstrapDeadline := time.Now().Add(bootstrapProbeWindow)
		fast := time.NewTicker(bootstrapProbeInterval)
		for time.Now().Before(bootstrapDeadline) {
			select {
			case <-fast.C:
				sd.probeAndJoinTailscalePeers()
			case <-ctx.Done():
				fast.Stop()
				sd.logger.Info("[ts] Tailscale peer discovery stopped")
				return
			}
		}
		fast.Stop()

		ticker := time.NewTicker(tailscaleScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sd.probeAndJoinTailscalePeers()
			case <-ctx.Done():
				sd.logger.Info("[ts] Tailscale peer discovery stopped")
				return
			}
		}
	}()
}

func (sd *SerfDiscovery) probeAndJoinTailscalePeers() {
	peers := onlineTailscalePeerIPs()
	if len(peers) == 0 {
		return
	}

	addrs := make([]string, 0, len(peers))
	for _, ip := range peers {
		addrs = append(addrs, fmt.Sprintf("%s:%d", ip, serfLANPort))
	}

	sd.logger.Debugf("[ts] Probing %d Tailscale peer(s) for Serf: %v", len(addrs), addrs)
	if n, err := sd.Join(addrs); err != nil {
		sd.logger.Debugf("[ts] Tailscale join: %d joined, err: %v", n, err)
	} else if n > 0 {
		sd.logger.Infof("[ts] Joined %d peer(s) via Tailscale", n)
	}
}

// tailscaleStatus is the minimal subset of `tailscale status --json` we need.
type tailscaleStatus struct {
	Peer map[string]struct {
		TailscaleIPs []string `json:"TailscaleIPs"`
		Online       bool     `json:"Online"`
	} `json:"Peer"`
}

// onlineTailscalePeerIPs returns the first Tailscale IP of every online peer.
func onlineTailscalePeerIPs() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "tailscale", "status", "--json").Output()
	if err != nil {
		return nil
	}

	var status tailscaleStatus
	if err := json.Unmarshal(out, &status); err != nil {
		return nil
	}

	var ips []string
	for _, peer := range status.Peer {
		if peer.Online && len(peer.TailscaleIPs) > 0 {
			ips = append(ips, peer.TailscaleIPs[0])
		}
	}
	return ips
}
