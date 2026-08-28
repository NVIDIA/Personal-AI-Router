// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package netpick

import (
	"net"
	"strings"
	"testing"
)

func TestRankRemote_DemotesOnlyUnusableClasses(t *testing.T) {
	// Docker's default bridge, CGNAT and link-local rank below any real private
	// or public address. The private blocks are NOT ranked against each other:
	// which one the fleet shares is not a property of the prefix.
	got := RankRemote([]string{"169.254.3.3", "172.17.0.2", "10.5.5.5", "100.64.0.1", "192.168.0.10"})
	if len(got) != 5 {
		t.Fatalf("RankRemote = %v, want 5 entries", got)
	}
	if got[0] != "10.5.5.5" || got[1] != "192.168.0.10" {
		t.Errorf("RankRemote leading entries = %v, want the two private addresses first in string order", got)
	}
	want := []string{"172.17.0.2", "100.64.0.1", "169.254.3.3"}
	for i, w := range want {
		if got[2+i] != w {
			t.Fatalf("RankRemote = %v, want %v in the trailing positions", got, want)
		}
	}
}

// TestRankRemote_PrivateBlocksTie is the deliberate reversal of the old policy: a
// 192.168 address no longer outranks a 10.x one. Preferring one private block
// made a multi-homed host publish a two-host direct-connect link over its LAN,
// and no address-only rule can tell those apart. Equal scores resolve by string
// order, which is arbitrary but stable, and a connection attempt corrects it.
func TestRankRemote_PrivateBlocksTie(t *testing.T) {
	if a, b := scoreIP(net.ParseIP("192.168.1.20")), scoreIP(net.ParseIP("10.5.5.5")); a != b {
		t.Fatalf("scoreIP 192.168(%d) and 10.x(%d) must tie", a, b)
	}
	if a, b := scoreIP(net.ParseIP("172.20.0.5")), scoreIP(net.ParseIP("10.5.5.5")); a != b {
		t.Fatalf("scoreIP 172.16/12(%d) and 10.x(%d) must tie", a, b)
	}
	// Docker's default bridge is the one 172.16/12 address that stays demoted.
	if a, b := scoreIP(net.ParseIP("172.17.0.1")), scoreIP(net.ParseIP("172.20.0.5")); a >= b {
		t.Fatalf("scoreIP 172.17 docker(%d) must rank below other 172.16/12(%d)", a, b)
	}
}

func TestRankRemote_PrivateBeatsPublic(t *testing.T) {
	got := RankRemote([]string{"8.8.8.8", "10.5.5.5"})
	if len(got) != 2 || got[0] != "10.5.5.5" {
		t.Fatalf("RankRemote = %v, want the private address first", got)
	}
}

func TestRankRemote_DropsUnparseableAndStableTie(t *testing.T) {
	got := RankRemote([]string{"not-an-ip", "192.168.0.9", "192.168.0.3", ""})
	want := []string{"192.168.0.3", "192.168.0.9"} // equal score -> string order, junk dropped
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("RankRemote = %v, want %v", got, want)
	}
}

func TestPrimary_TXTWins(t *testing.T) {
	// The node's own ip= TXT is authoritative even if the address list leads
	// with something else.
	if got := Primary([]string{"ip=192.168.1.5"}, []string{"10.0.0.1", "192.168.1.5"}); got != "192.168.1.5" {
		t.Fatalf("Primary with ip= TXT = %q, want 192.168.1.5", got)
	}
}

func TestPrimary_InvalidTXTFallsBackToRanked(t *testing.T) {
	if got := Primary([]string{"ip=garbage", "other=x"}, []string{"10.0.0.1", "192.168.1.5"}); got != "10.0.0.1" {
		t.Fatalf("Primary fallback = %q, want the top-ranked advertised address", got)
	}
}

func TestPrimary_Empty(t *testing.T) {
	if got := Primary(nil, nil); got != "" {
		t.Fatalf("Primary(nil,nil) = %q, want empty", got)
	}
	if got := Primary(nil, []string{"junk"}); got != "" {
		t.Fatalf("Primary with only junk = %q, want empty", got)
	}
}

func TestIPFromTXT(t *testing.T) {
	if got := IPFromTXT([]string{"uuid=abc", "ip=192.168.0.2", "v=1"}); got != "192.168.0.2" {
		t.Fatalf("IPFromTXT = %q, want 192.168.0.2", got)
	}
	if got := IPFromTXT([]string{"uuid=abc"}); got != "" {
		t.Fatalf("IPFromTXT without ip= = %q, want empty", got)
	}
}

func TestIPsFromTXT(t *testing.T) {
	got := IPsFromTXT([]string{"uuid=abc", "ips=10.172.54.70,192.168.240.2, 192.168.240.6 ,junk"})
	want := []string{"10.172.54.70", "192.168.240.2", "192.168.240.6"}
	if len(got) != len(want) {
		t.Fatalf("IPsFromTXT = %v, want %v (junk dropped, whitespace trimmed)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("IPsFromTXT = %v, want %v", got, want)
		}
	}
	if got := IPsFromTXT([]string{"uuid=abc"}); got != nil {
		t.Fatalf("IPsFromTXT without ips= = %v, want nil", got)
	}
}

// TestCandidates_PreservesPublishedOrder is the core of the multi-homed fix: the
// publisher ranked its own addresses from evidence no observer has, so an
// observer must not re-sort them. Re-sorting by address class here is exactly what
// used to promote a direct-connect link over the LAN.
func TestCandidates_PreservesPublishedOrder(t *testing.T) {
	txt := []string{"uuid=abc", "ip=10.172.54.70", "ips=10.172.54.70,192.168.240.2"}
	got := Candidates(txt, []string{"192.168.240.2", "10.172.54.70"})
	want := []string{"10.172.54.70", "192.168.240.2"}
	if len(got) != len(want) {
		t.Fatalf("Candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Candidates = %v, want %v (published order preserved)", got, want)
		}
	}
}

func TestCandidates_AppendsUnrankedAdvertisedAddresses(t *testing.T) {
	// An address the node did not rank is a fallback, not a competing opinion:
	// it is appended, never promoted above a ranked entry.
	got := Candidates(
		[]string{"ip=10.0.0.5", "ips=10.0.0.5,172.20.0.5"},
		[]string{"192.168.9.9", "10.0.0.5"},
	)
	want := []string{"10.0.0.5", "172.20.0.5", "192.168.9.9"}
	if len(got) != len(want) {
		t.Fatalf("Candidates = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Candidates = %v, want %v", got, want)
		}
	}
}

func TestCandidates_Deduplicates(t *testing.T) {
	got := Candidates(
		[]string{"ip=10.0.0.5", "ips=10.0.0.5,10.0.0.5"},
		[]string{"10.0.0.5"},
	)
	if len(got) != 1 || got[0] != "10.0.0.5" {
		t.Fatalf("Candidates = %v, want a single 10.0.0.5", got)
	}
}

func TestVirtualIface(t *testing.T) {
	for _, n := range []string{"vEthernet (Default Switch)", "docker0", "br-1a2b", "tailscale0", "utun3", "VirtualBox Host-Only", "vEthernet (WSL)"} {
		if !virtualIface(n) {
			t.Errorf("virtualIface(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"Ethernet", "Wi-Fi", "eth0", "en0", "wlan0", "enP7s7", "enp1s0f0np0"} {
		if virtualIface(n) {
			t.Errorf("virtualIface(%q) = true, want false", n)
		}
	}
}

// TestPhysicalBonus_VEthernetIsCallerGated pins the contract physicalBonus
// documents: it does not itself recognize a virtual adapter, so a Windows
// "vEthernet (...)" name would collect the Ethernet bonus. Callers must check
// virtualIface first, and rankLocal does (see TestRankLocal_ExcludesVirtual).
func TestPhysicalBonus_VEthernetIsCallerGated(t *testing.T) {
	if b := physicalBonus("vEthernet (Default Switch)"); b != 15 {
		t.Fatalf("physicalBonus(vEthernet) = %d, want 15 — gating is the caller's job", b)
	}
	if wifi, eth := physicalBonus("Wi-Fi"), physicalBonus("Ethernet"); wifi <= eth {
		t.Fatalf("physicalBonus Wi-Fi(%d) should rank above Ethernet(%d)", wifi, eth)
	}
	if b := physicalBonus("someswitch0"); b != 0 {
		t.Fatalf("physicalBonus(someswitch0) = %d, want 0", b)
	}
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
	if len(got) == 0 {
		t.Fatal("rankLocal returned no candidates")
	}
	if got[0] != "10.172.54.70" {
		t.Fatalf("canonical address = %q, want 10.172.54.70 (the LAN, not a /30 link); full order %v", got[0], got)
	}
}

// TestRankLocal_ExcludesVirtual: container bridge addresses are never published
// by a host that has a physical address. Every Docker host has 172.17.0.1, so a
// peer told to dial it reaches its own bridge rather than this node.
func TestRankLocal_ExcludesVirtual(t *testing.T) {
	for _, ip := range rankLocal(sparkHost(), Evidence{}, "") {
		if ip == "172.17.0.1" || ip == "172.18.0.1" {
			t.Errorf("rankLocal published container bridge %s", ip)
		}
	}
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
	got := rankLocal(ifaces, Evidence{}, "192.168.1.40")
	if len(got) != 1 || got[0] != "192.168.1.40" {
		t.Fatalf("rankLocal = %v, want [192.168.1.40]: the external switch is this host's only address, and Docker's bridge is nobody's", got)
	}
}

// TestRankLocal_VirtualNeverOutranksPhysical: the fallback is a last resort, not a
// tier. One qualified physical address is enough to keep an overlay address off
// the front of the list, whatever else favours it.
func TestRankLocal_VirtualNeverOutranksPhysical(t *testing.T) {
	ifaces := []localIface{
		{name: "wg0", addrs: []localAddr{{ip: "10.99.0.2", prefixLen: 24}}},
		{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: 24}}},
	}
	got := rankLocal(ifaces, Evidence{}, "10.99.0.2")
	if len(got) != 1 || got[0] != "10.0.0.5" {
		t.Fatalf("rankLocal = %v, want [10.0.0.5] only, even with the kernel routing off-link through the tunnel", got)
	}
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
	assertRanked(t, rankLocal(ifaces, ev, "10.99.0.2"), []string{"10.0.0.5", "10.99.0.2"})

	// Without that proof the tunnel stays unpublished: an overlay address no peer
	// has used is an ambiguous entry that costs every dialer a confirmation.
	ifaces[0].addrs[0].ip = "10.99.0.3"
	assertRanked(t, rankLocal(ifaces, Evidence{}, ""), []string{"10.0.0.5"})
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
	assertRanked(t, rankLocal(ifaces, Evidence{}, "192.168.1.40"),
		[]string{"192.168.1.40", "192.168.240.2"})
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
	assertRanked(t, rankLocal(ifaces, ev, ""), []string{"100.101.102.103", "10.0.0.5"})
}

// assertRanked compares a ranking to the exact list expected, in order.
func assertRanked(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rankLocal = %v, want %v", got, want)
	}
}

// TestRankLocal_KeepsDirectConnectAsLastResort: a /30 link must not be canonical,
// but it is still real for the machine on its far end, so it stays in the list for
// that pair to use.
func TestRankLocal_KeepsDirectConnectAsLastResort(t *testing.T) {
	got := rankLocal(sparkHost(), Evidence{}, "")
	want := []string{"10.172.54.70", "192.168.240.2", "192.168.240.6"}
	if len(got) != len(want) {
		t.Fatalf("rankLocal = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rankLocal = %v, want %v", got, want)
		}
	}
}

// TestRankLocal_NarrowPrefixesAndPointToPoint pins both hard disqualifiers
// independently of each other, and confirms an unknown prefix does not disqualify
// (a platform that reports an address without a mask must not lose its LAN).
func TestRankLocal_NarrowPrefixesAndPointToPoint(t *testing.T) {
	for _, prefixLen := range []int{30, 31, 32} {
		ifaces := []localIface{
			{name: "eth1", addrs: []localAddr{{ip: "10.9.9.1", prefixLen: prefixLen}}},
			{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: 24}}},
		}
		if got := rankLocal(ifaces, Evidence{}, ""); got[0] != "10.0.0.5" {
			t.Errorf("with a /%d present, canonical = %q, want 10.0.0.5", prefixLen, got[0])
		}
	}

	ptp := []localIface{
		{name: "eth1", pointToPoint: true, addrs: []localAddr{{ip: "10.9.9.1", prefixLen: 24}}},
		{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: 24}}},
	}
	if got := rankLocal(ptp, Evidence{}, ""); got[0] != "10.0.0.5" {
		t.Errorf("with a point-to-point interface present, canonical = %q, want 10.0.0.5", got[0])
	}

	unknown := []localIface{{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: -1}}}}
	if got := rankLocal(unknown, Evidence{}, ""); len(got) != 1 || got[0] != "10.0.0.5" {
		t.Errorf("unknown prefix length = %v, want 10.0.0.5 kept", got)
	}
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
	if got := rankLocal(ifaces, ev, ""); got[0] != "10.0.0.5" {
		t.Fatalf("canonical = %q, want 10.0.0.5 (eth1 cannot send)", got[0])
	}
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
	if got := rankLocal(ifaces, Evidence{}, "10.0.2.5"); got[0] != "10.0.2.5" {
		t.Errorf("route source = %q, want 10.0.2.5", got[0])
	}
	// A peer on-link beats the route source.
	ev := Evidence{PeerOnLink: map[string]bool{"eth1": true}}
	if got := rankLocal(ifaces, ev, "10.0.2.5"); got[0] != "10.0.1.5" {
		t.Errorf("peer-on-link = %q, want 10.0.1.5 to beat the route source", got[0])
	}
	// A peer's completed connection beats both.
	ev.PeerObserved = map[string]bool{"10.0.0.5": true}
	if got := rankLocal(ifaces, ev, "10.0.2.5"); got[0] != "10.0.0.5" {
		t.Errorf("peer-observed = %q, want 10.0.0.5 to beat every other signal", got[0])
	}
}

func TestFacingPeers(t *testing.T) {
	ifaces := sparkHost()
	// The LAN peer is on-link for the LAN NIC; the direct-connect partner is
	// on-link for its own /30.
	got := facingPeers(ifaces, []string{"10.172.54.52", "192.168.240.1"})
	if !got["enP7s7"] {
		t.Errorf("facingPeers missing enP7s7 for a peer in its /22: %v", got)
	}
	if !got["enp1s0f0np0"] {
		t.Errorf("facingPeers missing enp1s0f0np0 for its /30 partner: %v", got)
	}
	if got["enP2p1s0f0np0"] {
		t.Errorf("facingPeers marked enP2p1s0f0np0, whose /30 holds no peer: %v", got)
	}
	// Container bridges are excluded from candidates, so they are not consulted.
	if got["docker0"] {
		t.Errorf("facingPeers marked a container bridge: %v", got)
	}
}

// TestFacingPeers_IgnoresSelf: our own advertisement loops back through mDNS, and
// seeing our own address is not evidence that any other machine is out there.
func TestFacingPeers_IgnoresSelf(t *testing.T) {
	ifaces := []localIface{{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: 24}}}}
	if got := facingPeers(ifaces, []string{"10.0.0.5"}); len(got) != 0 {
		t.Fatalf("facingPeers with only our own address = %v, want empty", got)
	}
	if got := facingPeers(ifaces, []string{"10.0.0.9"}); !got["eth0"] {
		t.Fatalf("facingPeers with a real peer = %v, want eth0", got)
	}
}

func TestFacingPeers_NoPeersOrUnknownPrefix(t *testing.T) {
	ifaces := []localIface{{name: "eth0", addrs: []localAddr{{ip: "10.0.0.5", prefixLen: -1}}}}
	if got := facingPeers(ifaces, []string{"10.0.0.9"}); len(got) != 0 {
		t.Errorf("facingPeers with an unknown prefix = %v, want empty (containment unknowable)", got)
	}
	if got := facingPeers(sparkHost(), nil); len(got) != 0 {
		t.Errorf("facingPeers with no peers = %v, want empty", got)
	}
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
	if got := rankLocal(ifaces, ev, ""); got[0] != "10.0.1.5" {
		t.Fatalf("canonical = %q, want the peer-proven address among equally unqualified ones", got[0])
	}
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
	got := rankLocal(reversed, Evidence{}, "")
	if strings.Join(got, ",") != strings.Join(forward, ",") {
		t.Fatalf("enumeration order changed the result: %v vs %v", forward, got)
	}
}

func TestRankLocal_ExcludesLoopbackAndLinkLocal(t *testing.T) {
	ifaces := []localIface{{name: "eth0", addrs: []localAddr{
		{ip: "127.0.0.1", prefixLen: 8},
		{ip: "169.254.7.7", prefixLen: 16},
		{ip: "fe80::1", prefixLen: 64},
		{ip: "10.0.0.5", prefixLen: 24},
	}}}
	got := rankLocal(ifaces, Evidence{}, "")
	if len(got) != 1 || got[0] != "10.0.0.5" {
		t.Fatalf("rankLocal = %v, want only 10.0.0.5", got)
	}
}

func TestRankLocal_NoAddresses(t *testing.T) {
	if got := rankLocal(nil, Evidence{}, ""); len(got) != 0 {
		t.Fatalf("rankLocal(nil) = %v, want empty", got)
	}
}

// TestLocalCandidates_Smoke is environment-dependent: it must not panic, and every
// address it returns must be a publishable non-loopback IPv4.
func TestLocalCandidates_Smoke(t *testing.T) {
	got := LocalCandidates(Evidence{})
	for _, a := range got {
		ip := net.ParseIP(a)
		if ip == nil || ip.To4() == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			t.Errorf("LocalCandidates returned %q, want a non-loopback, non-link-local IPv4", a)
		}
	}
	best := BestLocalIP(Evidence{})
	if len(got) == 0 {
		if best != "" {
			t.Errorf("BestLocalIP = %q with no candidates, want empty", best)
		}
		return
	}
	if best != got[0] {
		t.Errorf("BestLocalIP = %q, want the first candidate %q", best, got[0])
	}
}

// TestRouteSourceIP_Smoke: the route lookup must never panic and must return
// either "" (no default route) or a non-loopback IPv4.
func TestRouteSourceIP_Smoke(t *testing.T) {
	got := routeSourceIP()
	if got == "" {
		return
	}
	ip := net.ParseIP(got)
	if ip == nil || ip.To4() == nil || ip.IsLoopback() {
		t.Fatalf("routeSourceIP = %q, want empty or a non-loopback IPv4", got)
	}
}
