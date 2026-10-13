// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package netpick is the single source of truth for choosing which of a node's
// addresses to publish and which to dial, so every NVPAIR service agrees on the
// same answer instead of reducing a multi-homed host's address set with its own
// ad-hoc rule.
//
// "Address" here means anything a dialer can use: an IP literal or a DNS name.
// The local picker publishes IPv4 literals, because that is what a node can
// observe about itself; the remote side additionally accepts names, because a
// node reachable only over an encrypted overlay may have no address a peer can
// assume — a Tailscale node's MagicDNS name outlives every literal it holds.
//
// Selection is evidence-driven, not inferred from what a subnet number is
// assumed to mean. A private range says nothing about whether the fleet lives on
// it: a two-host direct-connect link between a pair of machines is as private as
// the office LAN, and ranking one RFC1918 block above another picks the wrong one
// as often as the right one. So the local picker (see local.go) decides from
// facts it can observe for free — prefix width, point-to-point flags, whether the
// interface can reach the mDNS group, the kernel's own default-route choice, and
// which addresses remote peers have demonstrably connected to — and scoreIP here
// only separates the classes that are genuinely unusable to a peer.
//
// Two contexts:
//
//   - Remote peer — we have the addresses and TXT keys the peer advertised.
//     Candidates() returns every address worth trying, best first, preserving the
//     peer's own "ips=" order because the peer ranked those with local evidence
//     no observer has. Primary() is its first entry.
//   - Local host — LocalCandidates() ranks our own addresses from Evidence and
//     produces what we publish as "ip=" and "ips=".
//
// Nothing here promises reachability. A published address is a best-evidence
// claim, and a consumer that must actually connect confirms it by connecting —
// see package reach, which walks a candidate list in this order.
package netpick

import (
	"net"
	"sort"
	"strings"

	"nvpair-shared/noderec"
)

// Address-class scores. These separate addresses a peer cannot use from ones it
// can; they deliberately do NOT rank the RFC1918 blocks against each other.
//
// Ranking 192.168/16 above 10/8 is what made a multi-homed host publish a
// direct-connect link instead of its LAN: both are private, and which one the
// fleet shares is not a property of the prefix. Interface- and route-level
// evidence answers that (local.go); the address alone cannot.
const (
	scoreUnparseable = -1000
	scoreLoopback    = -500 // a peer cannot reach us here
	scoreLinkLocal   = -100 // 169.254/16 and fe80::/10: no router forwards these
	scoreCGNAT       = -50  // 100.64/10: carrier NAT, not a LAN
	scoreDocker      = 20   // 172.17/16: Docker's default bridge, present on every host
	scoreIPv6Global  = 20
	scoreIPv6ULA     = 30
	scorePublic      = 40 // routable, but rarely how LAN peers reach each other
	// scoreHostname rates a DNS name. A name is not an address class at all: it
	// carries no prefix to judge, and it re-resolves on every dial, so it
	// survives a renumbering that strands every literal in the list. It sits
	// above the public and IPv6 classes and below the private blocks, which is
	// where a LAN peer's own literal still deserves to win.
	scoreHostname = 60
	scorePrivate  = 100
)

// maxHostnameLen and maxLabelLen are the DNS name limits (RFC 1035 2.3.4).
const (
	maxHostnameLen = 253
	maxLabelLen    = 63
)

// Hostname reports whether addr is a DNS name a dialer can hand to a resolver:
// one or more dot-separated labels of letters, digits, and interior hyphens,
// with an optional trailing root dot.
//
// It exists because an overlay network can give a node a name and no address a
// peer may assume. A Tailscale node's MagicDNS name (host.tailnet.ts.net) is the
// only identifier that stays correct across a re-authentication, and an mDNS peer
// can likewise be reachable only as host.local. Neither parses as an IP, so a
// candidate list that admits addresses alone would report such a node as having
// nowhere to be dialed.
//
// It deliberately rejects a "host:port" string: a colon is not a legal name
// character, and every consumer appends its own service port, so accepting one
// would produce "host:port:port".
func Hostname(addr string) bool {
	addr = strings.TrimSpace(addr)
	if addr == "" || len(addr) > maxHostnameLen || net.ParseIP(addr) != nil {
		return false
	}
	// One trailing root dot is legal and canonical; anything else empty is not.
	addr = strings.TrimSuffix(addr, ".")
	if addr == "" {
		return false
	}
	for _, label := range strings.Split(addr, ".") {
		if len(label) == 0 || len(label) > maxLabelLen {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			case c == '-' && i > 0 && i < len(label)-1:
			default:
				return false
			}
		}
	}
	return true
}

// dialable reports whether addr is something a consumer can hand to a dialer at
// all — an IP literal or a DNS name. It is the single admission test for every
// candidate list here, so "which addresses does this node have" has one answer
// rather than one per caller.
func dialable(addr string) bool {
	addr = strings.TrimSpace(addr)
	return net.ParseIP(addr) != nil || Hostname(addr)
}

// scoreAddr rates any dialable entry, name or literal. A name has no address
// class to judge, so it takes scoreHostname; everything else falls to scoreIP.
func scoreAddr(addr string) int {
	if ip := net.ParseIP(strings.TrimSpace(addr)); ip != nil {
		return scoreIP(ip)
	}
	if Hostname(addr) {
		return scoreHostname
	}
	return scoreUnparseable
}

// dockerDefaultBridge reports whether ip is in 172.17/16, Docker's default bridge
// subnet. It is the one private address that is not a host's own: every Docker
// host has 172.17.0.1, so it identifies no machine and must never be published as
// a way to reach one.
func dockerDefaultBridge(ip net.IP) bool {
	ip4 := ip.To4()
	return ip4 != nil && ip4[0] == 172 && ip4[1] == 17
}

// scoreIP rates an IP for LAN reachability from a peer; higher is better. It
// works from the IP alone — the only thing an observer has for a remote peer —
// so it can only demote what is unusable by construction. It cannot tell a LAN
// apart from a VPN or a point-to-point link, and does not try; the authoritative
// order for a multi-homed peer is the peer's own "ips=" list (see Candidates),
// which the peer derived from local evidence.
func scoreIP(ip net.IP) int {
	if ip == nil {
		return scoreUnparseable
	}
	if ip.IsLoopback() {
		return scoreLoopback
	}
	if ip4 := ip.To4(); ip4 != nil {
		switch {
		case ip4.IsLinkLocalUnicast(): // 169.254/16 (APIPA)
			return scoreLinkLocal
		case ip4[0] == 100 && ip4[1]&0xc0 == 64: // 100.64/10 (CGNAT)
			return scoreCGNAT
		case dockerDefaultBridge(ip4):
			return scoreDocker
		case ip4[0] == 10,
			ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31,
			ip4[0] == 192 && ip4[1] == 168:
			return scorePrivate
		default:
			return scorePublic
		}
	}
	// IPv6: usable but ranked below IPv4 for LAN dialing.
	if ip.IsLinkLocalUnicast() { // fe80::/10
		return scoreLinkLocal
	}
	if ip.IsPrivate() { // ULA fc00::/7
		return scoreIPv6ULA
	}
	return scoreIPv6Global
}

// RankRemote returns the dialable entries — IP literals and DNS names alike —
// sorted best-first for dialing a remote peer. The sort is stable and ties break
// on the address string, so a multi-homed peer's chosen address does not flap
// between equally-ranked candidates from scan to scan.
//
// With the RFC1918 blocks scored equally, most of a peer's addresses now tie and
// resolve by string order. That is intentional: this ranking is a last resort for
// a peer that published no "ips=" list, and a guess dressed up as a preference is
// worse than an arbitrary but stable order that a connection attempt corrects.
func RankRemote(addrs []string) []string {
	type scored struct {
		addr  string
		score int
	}
	ranked := make([]scored, 0, len(addrs))
	for _, a := range addrs {
		if dialable(a) {
			ranked = append(ranked, scored{addr: a, score: scoreAddr(a)})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score > ranked[j].score
		}
		return ranked[i].addr < ranked[j].addr
	})
	out := make([]string, len(ranked))
	for i, s := range ranked {
		out[i] = s.addr
	}
	return out
}

// IPFromTXT returns the value of the "ip=" mDNS TXT key — a node's own canonical
// address — or "" if absent.
func IPFromTXT(txt []string) string {
	for _, kv := range txt {
		if v, ok := strings.CutPrefix(kv, noderec.KeyIP+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// IPsFromTXT returns the ranked candidate list a node published in "ips=", in
// the node's own order, dropping entries nothing could dial. Empty when the key
// is absent — a node that publishes only "ip=" is not distinguishable here from
// one with a single address, and both are handled the same way.
func IPsFromTXT(txt []string) []string {
	for _, kv := range txt {
		v, ok := strings.CutPrefix(kv, noderec.KeyIPs+"=")
		if !ok {
			continue
		}
		var out []string
		for _, part := range strings.Split(v, noderec.IPsSeparator) {
			part = strings.TrimSpace(part)
			if dialable(part) {
				out = append(out, part)
			}
		}
		return out
	}
	return nil
}

// Candidates returns every address worth trying to reach a node, best first and
// deduplicated: the node's canonical "ip=", then the rest of its own "ips="
// order, then anything else it advertised that those two did not mention.
//
// The node's published order is preserved rather than re-sorted. A node ranks its
// own addresses from evidence only it can see — which of its interfaces carry
// live multicast, which the kernel routes off-link, which ones peers have
// actually connected to — and an observer re-sorting that list by address class
// would discard exactly the information that makes it correct.
//
// Advertised addresses the node did not rank are appended in RankRemote order:
// they are a fallback for a node whose published list is stale or truncated, not
// a competing opinion, so they never displace a ranked entry.
//
// An entry is kept when a dialer could use it: an IP literal, or a DNS name (see
// Hostname). Admitting names is what lets a node reachable only through an
// encrypted overlay — a Tailscale peer known by its MagicDNS name, or an mDNS
// peer known only as host.local — appear here at all rather than as a node with
// nowhere to be dialed.
//
// The result is capped at noderec.MaxAdvertisedIPs. Nothing here comes from a
// source this process controls — the TXT keys and the resolved address set both
// arrive from an unauthenticated mDNS record — and every entry costs a dialer one
// connect timeout, so the bound has to hold on the reading side too. A name is no
// more trusted than a literal from the same record: both are claims a consumer
// confirms by connecting, and a name resolves under the host's own resolver.
func Candidates(txt []string, addrs []string) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(a string) {
		a = strings.TrimSpace(a)
		if a == "" || seen[a] || len(out) >= noderec.MaxAdvertisedIPs || !dialable(a) {
			return
		}
		seen[a] = true
		out = append(out, a)
	}
	add(IPFromTXT(txt))
	for _, a := range IPsFromTXT(txt) {
		add(a)
	}
	for _, a := range RankRemote(addrs) {
		add(a)
	}
	return out
}

// Primary returns the single best address to reach a node, or "" when it
// published none. It is Candidates' first entry, so the node list, the settings
// page, and every dialer resolve the same address for a node.
//
// Callers that must actually connect should prefer Candidates and confirm by
// connecting (package reach): this answer is the best available claim, and on a
// multi-homed node the best claim is still only reachable from some vantage
// points — a direct-connect link is real for the machine on its other end and
// unreachable for everyone else.
func Primary(txt []string, addrs []string) string {
	if c := Candidates(txt, addrs); len(c) > 0 {
		return c[0]
	}
	return ""
}
