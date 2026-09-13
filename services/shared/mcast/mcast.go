// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mcast provides a long-lived per-interface UDP send-socket pool for
// mDNS traffic, shared by the mDNS responder (all platforms) and the discovery
// browser's Windows workaround re-send.
//
// It exists because the pre-consolidation code opened a fresh unicast-bound UDP
// socket for every single transmission — one per interface per send — and closed
// it again. On a host whose packet filter inspects each new flow (the macOS
// Application Firewall), that pattern makes the filter re-evaluate and log a brand-
// new flow for every mDNS packet instead of passing a small set of stable flows.
// A pool holds one socket per interface and reuses it, so the filter sees each
// interface once and then just forwards.
//
// The pool keys sockets by interface index and reports send outcomes by interface
// index. The discovery browser maps those indices to names for its SendFailures
// telemetry (which the netpick address ranker consumes); the mDNS responder uses
// the indices directly. It is a leaf: it depends only on the standard library and
// the ipv4 helpers, and takes its interface set from the caller (typically
// netmon.Enumerate) rather than enumerating on its own. It skips the AirDrop and
// internal low-latency links (see excludedIface), which never reach the LAN.
package mcast

import (
	"fmt"
	"net"
	"strings"
	"sync"

	"golang.org/x/net/ipv4"
)

// ttl is the multicast time-to-live every send socket is configured with. It
// matches the value the per-send code this replaces used, so a routed-VLAN or
// multi-hop-LAN deployment sees the same on-link range as before.
const ttl = 255

// excludedIface reports whether an interface is one we never send mDNS on: an
// Apple AirDrop / P2P link (awdl*) or an internal low-latency link (llw*). These
// never reach the LAN or a VPN, so a socket and a per-send flow for them is pure
// waste — and on a host whose packet filter inspects each new flow it is part of
// the very churn this pool exists to remove. They usually carry no routable
// address (so the caller's non-loopback-IPv4 filter already drops them), but a
// host that assigns one a link-local IPv4 would otherwise advertise and transmit
// on it. Real NICs (en*) and LAN/VPN interfaces (bridge*, utun*) are kept.
func excludedIface(name string) bool {
	n := strings.ToLower(name)
	return strings.HasPrefix(n, "awdl") || strings.HasPrefix(n, "llw")
}

// sendSocket is one interface's long-lived send socket plus the facts the pool
// needs to decide whether to reuse or rebuild it on a refresh.
type sendSocket struct {
	src  net.IP // the address the socket is bound to, for reuse comparison
	conn *net.UDPConn
}

// Sender owns the per-interface send-socket pool. Its methods are safe for
// concurrent use; the pool is rebuilt under a lock on Refresh.
type Sender struct {
	mu    sync.Mutex
	conns map[int]*sendSocket
}

// New builds a Sender whose pool spans ifaces (interface index -> its
// non-loopback IPv4 addresses, as netmon.Enumerate reports). An interface whose
// socket cannot be bound is skipped rather than aborting the whole pool, so one
// wedged adapter never darkens the rest.
func New(ifaces map[int][]net.IP) *Sender {
	s := &Sender{conns: make(map[int]*sendSocket, len(ifaces))}
	s.refresh(ifaces)
	return s
}

// Refresh replaces the pool with one spanning ifaces, reusing a socket whenever
// its interface's source address is unchanged and rebinding only what moved (a
// DHCP re-lease, an interface that appeared or went away). The responder drives
// this from its network monitor; the browser from its scan loop.
func (s *Sender) Refresh(ifaces map[int][]net.IP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refresh(ifaces)
}

// refresh is Refresh with the lock already held.
func (s *Sender) refresh(ifaces map[int][]net.IP) {
	next := make(map[int]*sendSocket, len(ifaces))
	for idx, addrs := range ifaces {
		if len(addrs) == 0 {
			continue
		}
		src := addrs[0]
		if old, ok := s.conns[idx]; ok && old.src.Equal(src) {
			next[idx] = old // reuse; validated (non-excluded) when first bound
			continue
		}
		ifi, err := net.InterfaceByIndex(idx)
		if err != nil || excludedIface(ifi.Name) {
			continue // gone, or an AirDrop/internal link we never send mDNS on
		}
		if old, ok := s.conns[idx]; ok {
			_ = old.conn.Close()
		}
		ss, err := dial(src, ifi)
		if err != nil {
			continue
		}
		next[idx] = ss
	}
	for idx, old := range s.conns {
		if _, ok := next[idx]; !ok {
			_ = old.conn.Close()
		}
	}
	s.conns = next
}

// dial opens a send socket bound to src on an ephemeral port with the multicast
// egress interface and TTL set — the same socket the per-send code used to open
// on every transmission, now held for reuse.
func dial(src net.IP, ifi *net.Interface) (*sendSocket, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: src, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("mcast: bind %s: %w", src, err)
	}
	ss := &sendSocket{src: src, conn: conn}
	pc := ipv4.NewPacketConn(conn)
	_ = pc.SetMulticastInterface(ifi)
	_ = pc.SetMulticastTTL(ttl)
	return ss, nil
}

// Ifaces returns the interface indices the pool currently holds a socket for.
func (s *Sender) Ifaces() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]int, 0, len(s.conns))
	for idx := range s.conns {
		out = append(out, idx)
	}
	return out
}

// SendMulticast writes buf to target from each of the given interfaces' pooled
// sockets and returns the per-interface outcome keyed by interface index (true =
// the write was accepted by the socket). An interface with no pooled socket is
// omitted, so a missing key means the send was skipped rather than failed. The
// value reflects only the socket write, not delivery.
func (s *Sender) SendMulticast(buf []byte, target *net.UDPAddr, ifaces []int) map[int]bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	outcomes := make(map[int]bool, len(ifaces))
	for _, idx := range ifaces {
		ss, ok := s.conns[idx]
		if !ok {
			continue
		}
		_, err := ss.conn.WriteToUDP(buf, target)
		outcomes[idx] = err == nil
	}
	return outcomes
}

// SendUnicast writes buf to target from the one interface's pooled socket, for a
// reply to a specific querier. It returns an error when the interface has no
// pooled socket so a caller bug surfaces instead of silently dropping the reply.
func (s *Sender) SendUnicast(buf []byte, target *net.UDPAddr, ifIndex int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss, ok := s.conns[ifIndex]
	if !ok {
		return fmt.Errorf("mcast: no send socket for interface %d", ifIndex)
	}
	_, err := ss.conn.WriteToUDP(buf, target)
	return err
}

// Close closes every pooled socket and empties the pool. It is safe to call more
// than once.
func (s *Sender) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for idx, ss := range s.conns {
		_ = ss.conn.Close()
		delete(s.conns, idx)
	}
}
