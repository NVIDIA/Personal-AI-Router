// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package netpick

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRankRemote_DemotesOnlyUnusableClasses(t *testing.T) {
	// Docker's default bridge, CGNAT and link-local rank below any real private
	// or public address. The private blocks are NOT ranked against each other:
	// which one the fleet shares is not a property of the prefix.
	assert.Equal(t, []string{"10.5.5.5", "192.168.0.10", "172.17.0.2", "100.64.0.1", "169.254.3.3"},
		RankRemote([]string{"169.254.3.3", "172.17.0.2", "10.5.5.5", "100.64.0.1", "192.168.0.10"}),
		"unusable classes must rank after usable addresses")
}

// TestRankRemote_PrivateBlocksTie is the deliberate reversal of the old policy: a
// 192.168 address no longer outranks a 10.x one. Preferring one private block
// made a multi-homed host publish a two-host direct-connect link over its LAN,
// and no address-only rule can tell those apart. Equal scores resolve by string
// order, which is arbitrary but stable, and a connection attempt corrects it.
func TestRankRemote_PrivateBlocksTie(t *testing.T) {
	assert.Equal(t, scoreIP(net.ParseIP("10.5.5.5")), scoreIP(net.ParseIP("192.168.1.20")), "scoreIP 192.168")
	assert.Equal(t, scoreIP(net.ParseIP("10.5.5.5")), scoreIP(net.ParseIP("172.20.0.5")), "scoreIP 172.16/12")
	// Docker's default bridge is the one 172.16/12 address that stays demoted.
	assert.Less(t, scoreIP(net.ParseIP("172.17.0.1")), scoreIP(net.ParseIP("172.20.0.5")), "scoreIP 172.17 docker")
}

func TestRankRemote_PrivateBeatsPublic(t *testing.T) {
	got := RankRemote([]string{"8.8.8.8", "10.5.5.5"})
	require.Len(t, got, 2)
	assert.Equal(t, "10.5.5.5", got[0])
}

func TestRankRemote_DropsUnparseableAndStableTie(t *testing.T) {
	// Equal score -> string order, junk dropped.
	assert.Equal(t, []string{"192.168.0.3", "192.168.0.9"}, RankRemote([]string{"not-an-ip", "192.168.0.9", "192.168.0.3", ""}))
}

func TestPrimary_TXTWins(t *testing.T) {
	// The node's own ip= TXT is authoritative even if the address list leads
	// with something else.
	assert.Equal(t, "192.168.1.5", Primary([]string{"ip=192.168.1.5"}, []string{"10.0.0.1", "192.168.1.5"}), "Primary with ip= TXT")
}

func TestPrimary_InvalidTXTFallsBackToRanked(t *testing.T) {
	assert.Equal(t, "10.0.0.1", Primary([]string{"ip=garbage", "other=x"}, []string{"10.0.0.1", "192.168.1.5"}), "Primary fallback")
}

func TestPrimary_Empty(t *testing.T) {
	assert.Equal(t, "", Primary(nil, nil))
	assert.Equal(t, "", Primary(nil, []string{"junk"}), "Primary with only junk")
}

func TestIPFromTXT(t *testing.T) {
	assert.Equal(t, "192.168.0.2", IPFromTXT([]string{"uuid=abc", "ip=192.168.0.2", "v=1"}))
	assert.Equal(t, "", IPFromTXT([]string{"uuid=abc"}), "IPFromTXT without ip")
}

func TestIPsFromTXT(t *testing.T) {
	assert.Equal(t, []string{"10.172.54.70", "192.168.240.2", "192.168.240.6"},
		IPsFromTXT([]string{"uuid=abc", "ips=10.172.54.70,192.168.240.2, 192.168.240.6 ,junk"}))
	assert.Empty(t, IPsFromTXT([]string{"uuid=abc"}), "IPsFromTXT without ips")
}

// TestCandidates_PreservesPublishedOrder is the core of the multi-homed fix: the
// publisher ranked its own addresses from evidence no observer has, so an
// observer must not re-sort them. Re-sorting by address class here is exactly what
// used to promote a direct-connect link over the LAN.
func TestCandidates_PreservesPublishedOrder(t *testing.T) {
	txt := []string{"uuid=abc", "ip=10.172.54.70", "ips=10.172.54.70,192.168.240.2"}
	assert.Equal(t, []string{"10.172.54.70", "192.168.240.2"}, Candidates(txt, []string{"192.168.240.2", "10.172.54.70"}))
}

func TestCandidates_AppendsUnrankedAdvertisedAddresses(t *testing.T) {
	// An address the node did not rank is a fallback, not a competing opinion:
	// it is appended, never promoted above a ranked entry.
	assert.Equal(t, []string{"10.0.0.5", "172.20.0.5", "192.168.9.9"}, Candidates(
		[]string{"ip=10.0.0.5", "ips=10.0.0.5,172.20.0.5"},
		[]string{"192.168.9.9", "10.0.0.5"},
	))
}

func TestCandidates_Deduplicates(t *testing.T) {
	assert.Equal(t, []string{"10.0.0.5"}, Candidates(
		[]string{"ip=10.0.0.5", "ips=10.0.0.5,10.0.0.5"},
		[]string{"10.0.0.5"},
	))
}

func TestVirtualIface(t *testing.T) {
	assert.True(t, virtualIface("vEthernet (Default Switch)"))
	assert.True(t, virtualIface("docker0"))
	assert.True(t, virtualIface("br-1a2b"))
	assert.True(t, virtualIface("tailscale0"))
	assert.True(t, virtualIface("utun3"))
	assert.True(t, virtualIface("VirtualBox Host-Only"))
	assert.True(t, virtualIface("vEthernet (WSL)"))
	assert.False(t, virtualIface("Ethernet"))
	assert.False(t, virtualIface("Wi-Fi"))
	assert.False(t, virtualIface("eth0"))
	assert.False(t, virtualIface("en0"))
	assert.False(t, virtualIface("wlan0"))
	assert.False(t, virtualIface("enP7s7"))
	assert.False(t, virtualIface("enp1s0f0np0"))
}

// TestPhysicalBonus_VEthernetIsCallerGated pins the contract physicalBonus
// documents: it does not itself recognize a virtual adapter, so a Windows
// "vEthernet (...)" name would collect the Ethernet bonus. Callers must check
// virtualIface first, and rankLocal does (see TestRankLocal_ExcludesVirtual).
func TestPhysicalBonus_VEthernetIsCallerGated(t *testing.T) {
	assert.Equal(t, 15, physicalBonus("vEthernet (Default Switch)"))
	assert.Greater(t, physicalBonus("Wi-Fi"), physicalBonus("Ethernet"), "physicalBonus Wi-Fi")
	assert.Equal(t, 0, physicalBonus("someswitch0"))
}

// sparkHost describes the multi-homed host from the reported failure: a real LAN
// NIC, two cabled direct-connect ports on their own /30 links, and two container
// bridges. Every interface name starts with a physical-looking prefix, so name
// heuristics alone cannot separate them.
func sparkHost() []localIface {
	return []localIface{
		{name: "enP7s7", addrs: []localAddr{{ip: "10.172.54.70", prefixLen: 22}}},
		{name: "enp1s0f0np0", addrs: []localAddr{{ip: "192.168.240.2", prefixLen: 30}}},
		{name: "enP2p1s0f0np0", addrs: []localAddr{{ip: "192.168.240.6", prefixLen: 30}}},
		{name: "docker0", addrs: []localAddr{{ip: "172.17.0.1", prefixLen: 16}}},
		{name: "br-66ec6b8cad2f", addrs: []localAddr{{ip: "172.18.0.1", prefixLen: 16}}},
	}
}

// TestRankLocal_PrefersLANOverDirectConnect is the reported defect. The old policy
// scored 192.168/16 above 10/8 and added an "Ethernet" bonus for every en-prefixed
// name, so both /30 direct-connect ports outranked the real LAN and the host
// published an address no other machine could reach.
func TestRankLocal_PrefersLANOverDirectConnect(t *testing.T) {
	got := rankLocal(sparkHost(), Evidence{}, "")
	require.NotEmpty(t, got, "rankLocal returned no candidates")
	assert.Equal(t, "10.172.54.70", got[0], "canonical address")
}

// TestRankLocal_ExcludesVirtual: container bridge addresses are never published
// by a host that has a physical address. Every Docker host has 172.17.0.1, so a
// peer told to dial it reaches its own bridge rather than this node.
func TestRankLocal_ExcludesVirtual(t *testing.T) {
	got := rankLocal(sparkHost(), Evidence{}, "")
	assert.NotContains(t, got, "172.17.0.1", "rankLocal published container bridge")
	assert.NotContains(t, got, "172.18.0.1", "rankLocal published container bridge")
}

// TestRankLocal_VirtualIsTheAnswerWhenItIsTheOnlyOne: a Windows host on a Hyper-V
// external switch holds its LAN address on "vEthernet (...)" while the physical
// NIC bound to that switch has none. Demoting overlay adapters must not become
// publishing no address at all, which would make the host unreachable.
func TestRankLocal_VirtualIsTheAnswerWhenItIsTheOnlyOne(t *testing.T) {
	ifaces := []localIface{
		{name: "vEthernet (External Switch)", addrs: []localAddr{{ip: "192.168.1.40", prefixLen: 24}}},
		{name: "docker0", addrs: []localAddr{{ip: "172.17.0.1", prefixLen: 16}}},
	}
	assert.Equal(t, []string{"192.168.1.40"}, rankLocal(ifaces, Evidence{}, "192.168.1.40"))
}

// TestRankLocal_VirtualNeverOutranksPhysical: the fallback is a last resort, not a
// tier. One qualified physical address is enough to keep an overlay address off
// the front of the list, whatever else favours it.
func TestRankLocal_VirtualNeverOutranksPhysical(t *testing.T) {
	ifaces := []localIface{
		{name: "wg0", addrs: []localAddr{{ip: "10.99.0.2", prefixLen: 24}}},
		{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: 24}}},
	}
	assert.Equal(t, []string{"10.0.0.5"}, rankLocal(ifaces, Evidence{}, "10.99.0.2"))
}

// TestRankLocal_PeerProvenOverlayIsPublishedLast: qualification is judged from
// this host's vantage point, so a LAN address that qualifies here is still
// unreachable from a peer that shares only a tunnel with us. A completed inbound
// connection is proof from the other side, and the address that carried it is
// published — behind the LAN, never as the canonical answer.
func TestRankLocal_PeerProvenOverlayIsPublishedLast(t *testing.T) {
	ifaces := []localIface{
		{name: "wg0", addrs: []localAddr{{ip: "10.99.0.2", prefixLen: 24}}},
		{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: 24}}},
	}
	ev := Evidence{PeerObserved: map[string]bool{"10.99.0.2": true}}
	require.Equal(t, []string{"10.0.0.5", "10.99.0.2"}, rankLocal(ifaces, ev, "10.99.0.2"))

	// Without that proof the tunnel stays unpublished: an overlay address no peer
	// has used is an ambiguous entry that costs every dialer a confirmation.
	ifaces[0].addrs[0].ip = "10.99.0.3"
	require.Equal(t, []string{"10.0.0.5"}, rankLocal(ifaces, Evidence{}, ""))
}

// TestRankLocal_OverlayLANSurvivesADirectConnectNIC is the headline defect in the
// configuration that first survived it. A Windows host on a Hyper-V external
// switch holds its LAN address on "vEthernet (...)" while the bound NIC has none;
// add a Thunderbolt direct-connect port on a real NIC and the physical pass
// produces a candidate — an unqualified one, the very kind this ranking demotes.
// Gating the overlay pass on any physical candidate published the /30 and dropped
// the LAN address entirely.
func TestRankLocal_OverlayLANSurvivesADirectConnectNIC(t *testing.T) {
	ifaces := []localIface{
		{name: "vEthernet (External Switch)", addrs: []localAddr{{ip: "192.168.1.40", prefixLen: 24}}},
		{name: "Ethernet 5", addrs: []localAddr{{ip: "192.168.240.2", prefixLen: 30}}},
		{name: "docker0", addrs: []localAddr{{ip: "172.17.0.1", prefixLen: 16}}},
	}
	// The /30 keeps its place at the back: it is the fast path for the machine
	// cabled to it, and only the canonical answer was ever in dispute.
	require.Equal(t, []string{"192.168.1.40", "192.168.240.2"}, rankLocal(ifaces, Evidence{}, "192.168.1.40"))
}

// TestRankLocal_OverlayRunsWhenNoPhysicalAddressQualifies covers the other
// unqualified reason with no direct-connect link involved: the only physical NIC
// cannot send at all, so the tunnel is the only address a peer can use.
func TestRankLocal_OverlayRunsWhenNoPhysicalAddressQualifies(t *testing.T) {
	ifaces := []localIface{
		{name: "tailscale0", addrs: []localAddr{{ip: "100.101.102.103", prefixLen: 32}}},
		{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: 24}}},
	}
	ev := Evidence{SendFailed: map[string]bool{"eth0": true}}
	require.Equal(t, []string{"100.101.102.103", "10.0.0.5"}, rankLocal(ifaces, ev, ""))
}

// TestRankLocal_KeepsDirectConnectAsLastResort: a /30 link must not be canonical,
// but it is still real for the machine on its far end, so it stays in the list for
// that pair to use.
func TestRankLocal_KeepsDirectConnectAsLastResort(t *testing.T) {
	assert.Equal(t, []string{"10.172.54.70", "192.168.240.2", "192.168.240.6"}, rankLocal(sparkHost(), Evidence{}, ""))
}

// TestRankLocal_NarrowPrefixesAndPointToPoint pins both hard disqualifiers
// independently of each other, and confirms an unknown prefix does not disqualify
// (a platform that reports an address without a mask must not lose its LAN).
func TestRankLocal_NarrowPrefixesAndPointToPoint(t *testing.T) {
	test := func(name string, prefixLen int) {
		t.Run(name, func(t *testing.T) {
			ifaces := []localIface{
				{name: "eth1", addrs: []localAddr{{ip: "10.9.9.1", prefixLen: prefixLen}}},
				{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: 24}}},
			}
			assert.Equal(t, "10.0.0.5", rankLocal(ifaces, Evidence{}, "")[0])
		})
	}
	test("/30 prefix", 30)
	test("/31 prefix", 31)
	test("/32 prefix", 32)

	ptp := []localIface{
		{name: "eth1", pointToPoint: true, addrs: []localAddr{{ip: "10.9.9.1", prefixLen: 24}}},
		{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: 24}}},
	}
	assert.Equal(t, "10.0.0.5", rankLocal(ptp, Evidence{}, "")[0], "with a point-to-point interface present")

	unknown := []localIface{{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: -1}}}}
	assert.Equal(t, []string{"10.0.0.5"}, rankLocal(unknown, Evidence{}, ""), "unknown prefix length")
}

// TestRankLocal_SendFailureDisqualifies: a send that fails at the socket reports
// the kernel has no route out of that interface, which no peer's unicast
// connection can work around. Hearing no replies is NOT recorded as failure —
// a network that filters multicast still carries unicast.
func TestRankLocal_SendFailureDisqualifies(t *testing.T) {
	ifaces := []localIface{
		{name: "eth1", addrs: []localAddr{{ip: "10.9.9.1", prefixLen: 24}}},
		{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: 24}}},
	}
	ev := Evidence{SendFailed: map[string]bool{"eth1": true}}
	assert.Equal(t, "10.0.0.5", rankLocal(ifaces, ev, "")[0], "canonical")
}

// TestRankLocal_EvidencePrecedence pins the tier order: proof from a peer beats a
// peer merely being on-link, which beats the kernel's default route, which beats
// every name or address heuristic.
func TestRankLocal_EvidencePrecedence(t *testing.T) {
	ifaces := []localIface{
		{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: 24}}},
		{name: "eth1", addrs: []localAddr{{ip: "10.0.1.5", prefixLen: 24}}},
		{name: "eth2", addrs: []localAddr{{ip: "10.0.2.5", prefixLen: 24}}},
	}

	// Route source beats nothing else.
	assert.Equal(t, "10.0.2.5", rankLocal(ifaces, Evidence{}, "10.0.2.5")[0], "route source")
	// A peer on-link beats the route source.
	ev := Evidence{PeerOnLink: map[string]bool{"eth1": true}}
	assert.Equal(t, "10.0.1.5", rankLocal(ifaces, ev, "10.0.2.5")[0], "peer-on-link")
	// A peer's completed connection beats both.
	ev.PeerObserved = map[string]bool{"10.0.0.5": true}
	assert.Equal(t, "10.0.0.5", rankLocal(ifaces, ev, "10.0.2.5")[0], "peer-observed")
}

func TestFacingPeers(t *testing.T) {
	ifaces := sparkHost()
	// The LAN peer is on-link for the LAN NIC; the direct-connect partner is
	// on-link for its own /30.
	got := facingPeers(ifaces, []string{"10.172.54.52", "192.168.240.1"})
	assert.True(t, got["enP7s7"], "facingPeers missing enP7s7 for a peer in its /22")
	assert.True(t, got["enp1s0f0np0"], "facingPeers missing enp1s0f0np0 for its /30 partner")
	assert.False(t, got["enP2p1s0f0np0"], "facingPeers marked enP2p1s0f0np0, whose /30 holds no peer")
	// Container bridges are excluded from candidates, so they are not consulted.
	assert.False(t, got["docker0"], "facingPeers marked a container bridge")
}

// TestFacingPeers_IgnoresSelf: our own advertisement loops back through mDNS, and
// seeing our own address is not evidence that any other machine is out there.
func TestFacingPeers_IgnoresSelf(t *testing.T) {
	ifaces := []localIface{{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: 24}}}}
	require.Empty(t, facingPeers(ifaces, []string{"10.0.0.5"}), "facingPeers with only our own address")
	assert.True(t, facingPeers(ifaces, []string{"10.0.0.9"})["eth0"], "facingPeers with a real peer")
}

func TestFacingPeers_NoPeersOrUnknownPrefix(t *testing.T) {
	ifaces := []localIface{{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: -1}}}}
	assert.Empty(t, facingPeers(ifaces, []string{"10.0.0.9"}), "facingPeers with an unknown prefix")
	assert.Empty(t, facingPeers(sparkHost(), nil), "facingPeers with no peers")
}

// TestRankLocal_PeerProofRescuesUnqualified: hard disqualifiers are evidence, but
// a peer actually connecting is stronger evidence. A LAN that reports a narrow
// prefix must still win once a peer proves it works.
func TestRankLocal_PeerProofOutranksUnqualified(t *testing.T) {
	ifaces := []localIface{
		{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: 30}}},
		{name: "eth1", addrs: []localAddr{{ip: "10.0.1.5", prefixLen: 30}}},
	}
	ev := Evidence{PeerObserved: map[string]bool{"10.0.1.5": true}}
	assert.Equal(t, "10.0.1.5", rankLocal(ifaces, ev, "")[0], "canonical")
}

// TestRankLocal_DeterministicTieBreak is the flap fix. Two equally-scored
// addresses previously resolved by interface enumeration order, so a host
// republished a different canonical address across restarts and network changes
// and churned every consumer. Order must depend only on the described host.
func TestRankLocal_DeterministicTieBreak(t *testing.T) {
	forward := rankLocal(sparkHost(), Evidence{}, "")
	reversed := sparkHost()
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	assert.Equal(t, forward, rankLocal(reversed, Evidence{}, ""), "enumeration order changed the result")
}

func TestRankLocal_ExcludesLoopbackAndLinkLocal(t *testing.T) {
	ifaces := []localIface{{name: "eth0", addrs: []localAddr{
		{ip: "127.0.0.1", prefixLen: 8},
		{ip: "169.254.7.7", prefixLen: 16},
		{ip: "fe80::1", prefixLen: 64},
		{ip: "10.0.0.5", prefixLen: 24},
	}}}
	assert.Equal(t, []string{"10.0.0.5"}, rankLocal(ifaces, Evidence{}, ""))
}

func TestRankLocal_NoAddresses(t *testing.T) {
	require.Empty(t, rankLocal(nil, Evidence{}, ""))
}

// TestLocalCandidates_Smoke is environment-dependent: it must not panic, and every
// address it returns must be a publishable non-loopback IPv4.
func TestLocalCandidates_Smoke(t *testing.T) {
	got := LocalCandidates(Evidence{})
	for _, a := range got {
		ip := net.ParseIP(a)
		assert.NotEmpty(t, ip, "LocalCandidates returned")
		assert.NotEmpty(t, ip.To4(), "LocalCandidates returned")
		assert.False(t, ip.IsLoopback(), "LocalCandidates returned")
		assert.False(t, ip.IsLinkLocalUnicast(), "LocalCandidates returned")
	}
	best := BestLocalIP(Evidence{})
	if len(got) == 0 {
		assert.Equal(t, "", best)
		return
	}
	assert.Equal(t, got[0], best)
}

// TestRouteSourceIP_Smoke: the route lookup must never panic and must return
// either "" (no default route) or a non-loopback IPv4.
func TestRouteSourceIP_Smoke(t *testing.T) {
	got := routeSourceIP()
	if got == "" {
		return
	}
	ip := net.ParseIP(got)
	require.NotEmpty(t, ip)
	require.NotEmpty(t, ip.To4())
	assert.False(t, ip.IsLoopback(), "routeSourceIP")
}
