package discovery

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"sync"
	"time"
)

const (
	serfLANPort     = 7946
	lanScanInterval = 60 * time.Second
	probeTimeout    = 300 * time.Millisecond
	maxSubnetHosts  = 254 // only scan /24 or smaller
)

// StartLANDiscovery begins periodic LAN subnet scanning.
// It probes every host on local non-Tailscale /24-or-smaller subnets for the
// Serf gossip port (7946).  Any responding IP is fed into sd.Join so the node
// joins the Serf mesh over Ethernet even without Tailscale connectivity.
// If one node has WiFi+Tailscale and others only have Ethernet, they discover
// the WiFi node via LAN and then inherit the full Tailscale mesh from Serf tags.
func (sd *SerfDiscovery) StartLANDiscovery(ctx context.Context) {
	sd.lanMu.Lock()
	if sd.lanDiscoveryEnabled {
		sd.lanMu.Unlock()
		return
	}
	sd.lanDiscoveryEnabled = true
	sd.lanMu.Unlock()

	go func() {
		sd.logger.Info("[lan] LAN peer discovery enabled — scanning local subnets for Serf peers")
		// Run immediately so first-boot convergence is fast.
		sd.scanAndJoin()
		ticker := time.NewTicker(lanScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				sd.scanAndJoin()
			case <-ctx.Done():
				sd.logger.Info("[lan] LAN discovery stopped")
				return
			}
		}
	}()
}

// scanAndJoin discovers peer IPs on local subnets and Serf-joins them.
// It combines three discovery methods:
//  1. ARP neighbour table — immediate, zero-cost, works on direct patches
//  2. avahi-browse mDNS — works over link-local (169.254.x.x / fe80::), no DHCP needed
//  3. Subnet scan — TCP probe of every host in local /24-or-smaller subnets
func (sd *SerfDiscovery) scanAndJoin() {
	seen := make(map[string]bool)
	var candidates []string

	add := func(ips []string) {
		for _, ip := range ips {
			if !seen[ip] {
				seen[ip] = true
				candidates = append(candidates, ip)
			}
		}
	}

	add(arpNeighbours())
	add(mdnsPeers())
	add(scanLocalSubnets())

	if len(candidates) == 0 {
		return
	}
	peers := make([]string, 0, len(candidates))
	for _, ip := range candidates {
		peers = append(peers, fmt.Sprintf("%s:%d", ip, serfLANPort))
	}
	sd.logger.Debugf("[lan] Probing %d LAN candidate(s): %v", len(peers), peers)
	if n, err := sd.Join(peers); err != nil {
		sd.logger.Debugf("[lan] LAN join: %d joined, err: %v", n, err)
	} else if n > 0 {
		sd.logger.Infof("[lan] Joined %d new peer(s) via LAN", n)
	}
}

// arpNeighbours reads the kernel ARP/NDP table via `ip neigh show` and returns
// reachable neighbour IPs.  On a direct Ethernet patch the peer appears here
// within seconds of link-up, before any DHCP lease is issued — making this
// the fastest path for p2p discovery.
func arpNeighbours() []string {
	out, err := exec.Command("ip", "neigh", "show").Output()
	if err != nil {
		return nil
	}
	var ips []string
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		// Format: <ip> dev <iface> lladdr <mac> <state>
		// State can be REACHABLE, STALE, DELAY, PROBE — all usable.
		// Skip FAILED and INCOMPLETE (no MAC resolved yet).
		line := scanner.Text()
		if strings.Contains(line, "FAILED") || strings.Contains(line, "INCOMPLETE") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 1 {
			continue
		}
		ip := net.ParseIP(fields[0])
		if ip == nil || ip.IsLoopback() || ip.IsMulticast() {
			continue
		}
		ips = append(ips, fields[0])
	}
	return ips
}

// mdnsPeers runs avahi-browse to find nodes advertising _clusteros._tcp on the
// local link.  This works over link-local addresses (169.254.x.x, fe80::) so
// two directly-patched nodes with no DHCP server discover each other via mDNS
// multicast within a few seconds of link-up.
func mdnsPeers() []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "avahi-browse", "-rtp", "--no-db-lookup", "_clusteros._tcp").Output()
	if err != nil {
		return nil
	}
	var ips []string
	seen := make(map[string]bool)
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := scanner.Text()
		// Resolved records start with '=', unresolved with '+'.
		// Format: =;<iface>;<proto>;<name>;<type>;<domain>;<hostname>;<ip>;<port>;<txt>
		if !strings.HasPrefix(line, "=") {
			continue
		}
		parts := strings.SplitN(line, ";", 10)
		if len(parts) < 9 {
			continue
		}
		ip := parts[7]
		if net.ParseIP(ip) == nil || seen[ip] {
			continue
		}
		seen[ip] = true
		ips = append(ips, ip)
	}
	return ips
}

// scanLocalSubnets returns IPs that answer on serfLANPort within every local
// non-loopback, non-Tailscale /24-or-smaller subnet.
func scanLocalSubnets() []string {
	subnets := localPhysicalSubnets()
	if len(subnets) == 0 {
		return nil
	}
	var mu sync.Mutex
	var found []string
	var wg sync.WaitGroup

	for _, subnet := range subnets {
		hosts := expandSubnet(subnet)
		for _, ip := range hosts {
			wg.Add(1)
			go func(ip string) {
				defer wg.Done()
				if probeSerfPort(ip) {
					mu.Lock()
					found = append(found, ip)
					mu.Unlock()
				}
			}(ip)
		}
	}
	wg.Wait()
	return found
}

// localPhysicalSubnets returns IPv4 networks on local physical/virtual
// interfaces, skipping loopback, Tailscale, and WireGuard interfaces.
// Only subnets small enough to scan in full (/24 or smaller) are included.
func localPhysicalSubnets() []*net.IPNet {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var nets []*net.IPNet
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		// Skip overlay/tunnel interfaces — we want physical LAN only.
		name := iface.Name
		if strings.HasPrefix(name, "tailscale") || strings.HasPrefix(name, "wg") ||
			strings.HasPrefix(name, "utun") || strings.HasPrefix(name, "tun") {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			if ipNet.IP.To4() == nil || ipNet.IP.IsLoopback() {
				continue
			}
			ones, bits := ipNet.Mask.Size()
			if bits-ones <= 8 { // /24 or smaller (254 hosts or fewer)
				nets = append(nets, ipNet)
			}
		}
	}
	return nets
}

// expandSubnet returns all usable host IPs in the network
// (network address +1 … broadcast -1).
func expandSubnet(network *net.IPNet) []string {
	ones, bits := network.Mask.Size()
	numHosts := (1 << uint(bits-ones)) - 2
	if numHosts <= 0 || numHosts > maxSubnetHosts {
		return nil
	}
	ipv4 := network.IP.To4()
	if ipv4 == nil {
		return nil
	}
	base := binary.BigEndian.Uint32(ipv4)
	hosts := make([]string, 0, numHosts)
	for i := 1; i <= numHosts; i++ {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], base+uint32(i))
		hosts = append(hosts, net.IP(b[:]).String())
	}
	return hosts
}

// probeSerfPort returns true if ip is listening on serfLANPort via TCP.
func probeSerfPort(ip string) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", ip, serfLANPort), probeTimeout)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
