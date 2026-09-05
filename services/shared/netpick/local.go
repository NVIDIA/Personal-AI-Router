// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package netpick

import (
	"net"
	"sort"
	"strings"
)

// routeProbeTarget is an address outside every local prefix, used only to ask the
// kernel which source address it would pick for off-link traffic. It is in
// TEST-NET-1 (RFC 5737), reserved for documentation and routed nowhere, so the
// intent is unmistakable: this is a routing-table question, not a destination.
//
// A connectionless UDP "dial" performs the route lookup and binds a local
// address. It transmits nothing, so this costs one syscall and no packets.
const routeProbeTarget = "192.0.2.1:9"

// maxPrefixLen is the narrowest IPv4 prefix that can still describe a shared
// network. A /30 holds two usable addresses and a /31 holds two total, so both
// describe a link between exactly one pair of machines: whoever is on the other
// end can reach it and nobody else can. A /32 has no peer at all.
//
// This is the generic form of the direct-connect problem — a machine with a
// cabled link to one neighbour must not publish that link as the address the
// fleet should use — and it holds without naming any particular subnet.
const maxPrefixLen = 30

// Evidence is what this host has observed about its own addresses. Every field is
// free to collect: it is a byproduct of work the node already does, so ranking
// never costs a probe of its own.
//
// The zero value is valid and means "nothing observed yet" — selection then falls
// back to route and interface facts, which is the correct answer at startup
// before any peer has been heard from.
type Evidence struct {
	// SendFailed holds interface names whose mDNS sends fail persistently. A send
	// that fails at the socket (typically "network is unreachable") reports that
	// the kernel has no usable route out of that interface, which no peer's
	// unicast connection can work around.
	//
	// Only a sustained failure belongs here, never a single one: sends are
	// frequent and an isolated blip would otherwise move a host's canonical
	// address, which is the churn this ranking exists to stop. It is also
	// distinct from hearing no replies — a network that filters multicast still
	// carries unicast — so silence is not recorded.
	SendFailed map[string]bool

	// PeerOnLink holds interface names whose own subnet contains at least one
	// discovered peer. That peer answered a multicast query and its address is
	// on-link here, so this interface reaches other nodes.
	//
	// It is inferred from addresses rather than from which socket received the
	// reply because the receiving interface is not available on every platform,
	// and an inference that works everywhere beats a fact that works on two
	// platforms out of three.
	PeerOnLink map[string]bool

	// PeerObserved holds this host's own addresses that a remote peer has
	// successfully connected to. This is proof rather than inference — a
	// completed inbound connection means that address is reachable from at least
	// one other machine — so it outranks every other signal.
	PeerObserved map[string]bool
}

func (e Evidence) sendFailed(iface string) bool { return e.SendFailed[iface] }
func (e Evidence) peerOnLink(iface string) bool { return e.PeerOnLink[iface] }
func (e Evidence) peerObserved(ip string) bool  { return e.PeerObserved[ip] }

// virtualIface reports whether an interface name looks like a virtual / overlay
// adapter (Docker, Hyper-V vEthernet, VPN/tunnel, VM host-only, WSL, Kubernetes
// CNI). Such an address is almost never how a LAN peer reaches this host, so
// rankLocal ranks every qualified physical address ahead of all of them.
//
// Kubernetes hosts need the overlay pass to be complete rather than merely the
// physical pass to be right. On an OVN-Kubernetes node the physical NIC carries
// no address at all: the LAN address sits on the OVS uplink bridge "br-ex",
// which the "br-" rule already demotes, while the pod-network management port
// "ovn-k8s-mp0" holds a routable-looking /23 that nothing off the node can
// reach. If that port counts as physical it is the only physical candidate,
// qualifies, and is published as canonical -- the Docker-bridge failure again,
// with an address every peer is told to dial and none can. Naming the CNI
// interfaces virtual sends both to the overlay pass, where the default-route
// evidence picks the uplink.
//
// It is a demotion and not an exclusion because on some hosts it is the only
// answer there is: a Windows host on a Hyper-V external switch holds its LAN
// address on "vEthernet (...)" while the physical NIC bound to that switch has
// none, and a remote machine may be reachable only over its VPN adapter.
// Publishing nothing at all would make those hosts unreachable, which is worse
// than publishing a lower-confidence address that a dialer confirms anyway.
//
// Which is why the gate is a qualified physical address and not merely a
// physical one: those same hosts often do have a physical address that cannot be
// the fleet's — a direct-connect /30, or a NIC whose sends fail — and letting it
// suppress the overlay pass would publish only the address nobody can use. A
// host that does have a qualified physical address still publishes an overlay
// address a peer has actually connected to, because qualification is judged from
// here and that peer's connection is judged from there.
func virtualIface(name string) bool {
	n := strings.ToLower(name)
	for _, v := range []string{
		"veth", "docker", "br-", "vethernet", "vmnet", "vboxnet", "virbr",
		"tailscale", "zt", "utun", "wg", "tun", "tap", "ppp", "vpn",
		"hyper-v", "virtual", "wsl", "loopback", "isatap", "teredo",
		// Kubernetes CNI: OVN-Kubernetes / Open vSwitch, Geneve and VXLAN
		// tunnel endpoints, flannel, Calico, Cilium, Antrea, kube-proxy IPVS.
		"ovn-", "ovs-", "genev", "vxlan", "flannel", "cni", "cali", "cilium",
		"antrea", "kube",
	} {
		if strings.Contains(n, v) {
			return true
		}
	}
	return false
}

// physicalBonus rewards interface names that look like a real wired/wireless NIC.
// Callers MUST check virtualIface first and skip this when virtual — a Windows
// "vEthernet (...)" adapter contains both "vethernet" and "ethernet", and must
// not collect the physical bonus.
//
// This is the weakest tier in the ranking, consulted only when route and
// multicast evidence cannot separate two addresses. Wi-Fi ranking above Ethernet
// is an arbitrary but deliberate tie-break; on a host where both carry the LAN,
// either answer works and a stable one matters more than which.
func physicalBonus(name string) int {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "wi-fi"), strings.Contains(n, "wifi"),
		strings.Contains(n, "wlan"), strings.HasPrefix(n, "wl"):
		return 20
	case strings.Contains(n, "ethernet"), strings.HasPrefix(n, "eth"),
		strings.HasPrefix(n, "en"):
		return 15
	}
	return 0
}

// localAddr is one of this host's addresses reduced to the facts ranking needs.
// prefixLen is -1 when the platform reported an address without a mask.
type localAddr struct {
	ip        string
	prefixLen int
}

// localIface is one enumerated interface, reduced to the facts ranking needs.
type localIface struct {
	name         string
	pointToPoint bool
	addrs        []localAddr
}

// candidate is one rankable local address with every tier key resolved, so the
// sort is a pure comparison over already-gathered facts.
type candidate struct {
	ip string
	// qualified is false when hard evidence says this address cannot be the one
	// the fleet shares. Such an address is still published, last: a
	// direct-connect link is genuinely reachable by the machine on its far end,
	// and dropping it would deny that pair its fast path. It just must never be
	// the canonical answer while any qualified address exists.
	qualified   bool
	peerSeen    bool
	peerOnLink  bool
	routeSource bool
	physical    int
	score       int
}

// betterThan orders candidates by descending strength of evidence, ending in the
// address string so the result is total and stable. Without that final key, two
// equally-scored addresses on a multi-homed host resolve by interface enumeration
// order, which is not stable across restarts or network changes — a host would
// republish a different canonical address for no reason and churn every consumer.
func (c candidate) betterThan(o candidate) bool {
	if c.qualified != o.qualified {
		return c.qualified
	}
	if c.peerSeen != o.peerSeen {
		return c.peerSeen
	}
	if c.peerOnLink != o.peerOnLink {
		return c.peerOnLink
	}
	if c.routeSource != o.routeSource {
		return c.routeSource
	}
	if c.physical != o.physical {
		return c.physical > o.physical
	}
	if c.score != o.score {
		return c.score > o.score
	}
	return c.ip < o.ip
}

// rankLocal ranks this host's publishable IPv4 addresses best-first. It is pure
// over its inputs so the whole policy is testable against a described host rather
// than whatever machine happens to run the tests.
//
// Excluded outright: loopback, link-local, and Docker's default bridge. None is a
// way for a peer to reach this host, and publishing one is worse than publishing
// nothing.
//
// Marked unqualified but still published, last: point-to-point interfaces,
// prefixes narrower than a shared network can be, and interfaces whose sends
// persistently fail. Each is direct evidence that the address is not the fleet's
// network, but it may still be one specific peer's best path.
//
// Virtual adapters are held back further still, to a second pass that runs only
// when the first produced no address that could be the fleet's. On the
// overwhelmingly common host with a qualified physical address, an unproven
// overlay address is never published — the ambiguity is not worth the entry.
// Otherwise one of them may be the only address any peer can use, and the
// alternative is advertising nothing usable at all.
//
// The gate is a qualified physical candidate rather than any physical candidate,
// because the unqualified ones are exactly the addresses this ranking exists to
// demote: a host whose only real NIC holds a direct-connect /30 would otherwise
// publish that /30 and suppress the LAN address it holds on a Hyper-V external
// switch. When the overlay pass runs, the unqualified physical addresses are
// published behind it rather than dropped — a /30 link is still real for the
// machine on its far end.
func rankLocal(ifaces []localIface, ev Evidence, routeIP string) []string {
	physical := rankIfaces(ifaces, ev, routeIP, false)
	overlay := rankIfaces(ifaces, ev, routeIP, true)
	// qualified is betterThan's first key, so the leading candidate alone answers
	// whether this host has any qualified physical address.
	if len(physical) > 0 && physical[0].qualified {
		return addresses(append(physical, peerProven(overlay)...))
	}
	return addresses(append(overlay, physical...))
}

// peerProven keeps the candidates a remote peer has demonstrably connected to.
//
// It is how an overlay address survives a host that has a qualified physical
// address. Qualification is judged from this host's own vantage point, and a LAN
// address that qualifies is still unreachable from a peer that only shares a
// tunnel with us — so a completed inbound connection to a tunnel address is the
// one signal that keeps that peer working. Published last, never canonical, on
// exactly the same footing as an unqualified physical address: real for one
// specific peer, and not the fleet's network.
func peerProven(cands []candidate) []candidate {
	proven := make([]candidate, 0, len(cands))
	for _, c := range cands {
		if c.peerSeen {
			proven = append(proven, c)
		}
	}
	return proven
}

// addresses reduces ranked candidates to the address list to publish, keeping
// each address's best-ranked appearance: one address can be enumerated on more
// than one interface, and the two passes are concatenated.
func addresses(cands []candidate) []string {
	out := make([]string, 0, len(cands))
	seen := make(map[string]bool, len(cands))
	for _, c := range cands {
		if seen[c.ip] {
			continue
		}
		seen[c.ip] = true
		out = append(out, c.ip)
	}
	return out
}

// rankIfaces ranks the addresses of either the physical or the virtual half of
// the host's interfaces, per rankLocal's two passes.
func rankIfaces(ifaces []localIface, ev Evidence, routeIP string, virtual bool) []candidate {
	var cands []candidate
	for _, ifi := range ifaces {
		if virtualIface(ifi.name) != virtual {
			continue
		}
		// physicalBonus does not itself recognize an overlay adapter — a Windows
		// "vEthernet (...)" name contains "ethernet" — so the caller gates it,
		// and on the virtual pass there is nothing to reward.
		physical := 0
		if !virtual {
			physical = physicalBonus(ifi.name)
		}
		sendFailed := ev.sendFailed(ifi.name)
		peerOnLink := ev.peerOnLink(ifi.name)
		for _, a := range ifi.addrs {
			ip := net.ParseIP(a.ip)
			if ip == nil {
				continue
			}
			ip4 := ip.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			// Every Docker host has 172.17.0.1, so a peer told to dial it
			// reaches its own bridge — and the TCP confirmation in package reach
			// would succeed there, against the wrong machine.
			if dockerDefaultBridge(ip4) {
				continue
			}
			narrow := a.prefixLen >= maxPrefixLen // -1 (unknown) never disqualifies
			cands = append(cands, candidate{
				ip:          ip4.String(),
				qualified:   !ifi.pointToPoint && !narrow && !sendFailed,
				peerSeen:    ev.peerObserved(ip4.String()),
				peerOnLink:  peerOnLink,
				routeSource: routeIP != "" && ip4.String() == routeIP,
				physical:    physical,
				score:       scoreIP(ip4),
			})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].betterThan(cands[j]) })
	return cands
}

// enumerate reduces the host's live interfaces to what rankLocal needs, skipping
// interfaces that are down or loopback.
func enumerate() []localIface {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	out := make([]localIface, 0, len(ifaces))
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		li := localIface{
			name:         ifi.Name,
			pointToPoint: ifi.Flags&net.FlagPointToPoint != 0,
			addrs:        make([]localAddr, 0, len(addrs)),
		}
		for _, a := range addrs {
			switch v := a.(type) {
			case *net.IPNet:
				prefixLen := -1
				if ones, bits := v.Mask.Size(); bits > 0 {
					prefixLen = ones
				}
				li.addrs = append(li.addrs, localAddr{ip: v.IP.String(), prefixLen: prefixLen})
			case *net.IPAddr:
				li.addrs = append(li.addrs, localAddr{ip: v.IP.String(), prefixLen: -1})
			}
		}
		out = append(out, li)
	}
	return out
}

// routeSourceIP returns the source address the kernel would use to reach an
// off-link destination — this host's default-route address. It is the kernel's
// own answer to "which of my interfaces faces the wider network", which is
// exactly the question address selection is asking, and it costs no packets.
//
// Empty when the host has no default route (an isolated LAN segment), in which
// case ranking falls through to the remaining tiers.
func routeSourceIP() string {
	conn, err := net.Dial("udp4", routeProbeTarget)
	if err != nil {
		return ""
	}
	defer func() { _ = conn.Close() }()
	ua, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || ua.IP == nil {
		return ""
	}
	ip4 := ua.IP.To4()
	if ip4 == nil || ip4.IsLoopback() {
		return ""
	}
	return ip4.String()
}

// facingPeers returns the names of interfaces whose own subnet contains at least
// one of the given peer addresses, which reports that the interface reaches other
// nodes. Pure over its inputs for testability.
//
// Containment is computed from the interface's address and prefix rather than
// from which socket received a reply: the receiving interface is unavailable on
// platforms that supply no packet control message, and this inference works
// everywhere the addresses are known.
func facingPeers(ifaces []localIface, peerIPs []string) map[string]bool {
	parsed := make([]net.IP, 0, len(peerIPs))
	for _, p := range peerIPs {
		if ip := net.ParseIP(strings.TrimSpace(p)); ip != nil {
			if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() {
				parsed = append(parsed, ip4)
			}
		}
	}
	if len(parsed) == 0 {
		return nil
	}
	out := make(map[string]bool)
	for _, ifi := range ifaces {
		if virtualIface(ifi.name) {
			continue
		}
		for _, a := range ifi.addrs {
			if a.prefixLen < 0 {
				continue
			}
			ip := net.ParseIP(a.ip)
			if ip == nil {
				continue
			}
			ip4 := ip.To4()
			if ip4 == nil {
				continue
			}
			network := net.IPNet{IP: ip4.Mask(net.CIDRMask(a.prefixLen, 32)), Mask: net.CIDRMask(a.prefixLen, 32)}
			for _, peer := range parsed {
				// A peer at our own address is our advertisement looping back,
				// not evidence that anything else is out there.
				if peer.Equal(ip4) {
					continue
				}
				if network.Contains(peer) {
					out[ifi.name] = true
					break
				}
			}
			if out[ifi.name] {
				break
			}
		}
	}
	return out
}

// IfacesFacingPeers reports which of this host's interfaces have a discovered
// peer on their own subnet, for Evidence.PeerOnLink.
func IfacesFacingPeers(peerIPs []string) map[string]bool {
	return facingPeers(enumerate(), peerIPs)
}

// LocalCandidates returns this host's publishable IPv4 addresses, best first —
// the values published as "ip=" (the first) and "ips=" (the list). Empty when the
// host has no address a peer could use.
func LocalCandidates(ev Evidence) []string {
	return rankLocal(enumerate(), ev, routeSourceIP())
}

// BestLocalIP returns this host's canonical IPv4 address to publish to peers, or
// "" if none. It is LocalCandidates' first entry.
func BestLocalIP(ev Evidence) string {
	if c := LocalCandidates(ev); len(c) > 0 {
		return c[0]
	}
	return ""
}
