// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mdns

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

// testResponder builds a Responder with fixed fields so the record-building
// tests don't depend on the host's real interfaces or hostname.
func testResponder(txt []string) *Responder {
	r := &Responder{
		instance:     "myhost",
		service:      "_nvpair-test._tcp",
		domain:       "local",
		port:         14318,
		hostName:     "myhost.local.",
		serviceName:  "_nvpair-test._tcp.local.",
		instanceName: "myhost._nvpair-test._tcp.local.",
		ifaceAddrs: map[int][]net.IP{
			1: {net.IPv4(192, 168, 1, 10)},
			2: {net.IPv4(10, 0, 0, 5)},
		},
	}
	r.txt = append([]string(nil), txt...)
	return r
}

func TestNewResponderValidation(t *testing.T) {
	// These cases return before any interface enumeration, so they're
	// environment-independent.
	cases := []struct {
		name                      string
		instance, service, domain string
		port                      int
	}{
		{"missing instance", "", "_nvpair-test._tcp", "local", 14318},
		{"missing service", "myhost", "", "local", 14318},
		{"missing port", "myhost", "_nvpair-test._tcp", "local", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewResponder(tc.instance, tc.service, tc.domain, tc.port, nil); err == nil {
				t.Fatalf("NewResponder(%q,%q,%q,%d) = nil error, want error", tc.instance, tc.service, tc.domain, tc.port)
			}
		})
	}
}

func TestNewResponderNormalizesNames(t *testing.T) {
	// The success path needs a real multicast-capable interface; skip where
	// there isn't one (e.g. a locked-down CI sandbox) rather than fail.
	r, err := NewResponder(".myhost.", "._nvpair-test._tcp.", "", 14318, []string{"v=1"})
	if err != nil {
		t.Skipf("NewResponder unavailable in this environment: %v", err)
	}
	if r.domain != "local" {
		t.Errorf("domain = %q, want local (default)", r.domain)
	}
	if want := "_nvpair-test._tcp.local."; r.serviceName != want {
		t.Errorf("serviceName = %q, want %q", r.serviceName, want)
	}
	if want := "myhost._nvpair-test._tcp.local."; r.instanceName != want {
		t.Errorf("instanceName = %q, want %q", r.instanceName, want)
	}
}

func TestNewResponderCopiesTXT(t *testing.T) {
	in := []string{"v=1"}
	r := testResponder(in)
	in[0] = "mutated"
	if got := r.currentTXT(); got[0] != "v=1" {
		t.Errorf("currentTXT tracked caller's slice mutation: got %q", got[0])
	}
}

func TestSrvRR(t *testing.T) {
	r := testResponder(nil)
	srv := r.srvRR(false)
	if srv.Port != 14318 {
		t.Errorf("SRV port = %d, want 14318", srv.Port)
	}
	if srv.Target != "myhost.local." {
		t.Errorf("SRV target = %q, want myhost.local.", srv.Target)
	}
	if srv.Hdr.Name != "myhost._nvpair-test._tcp.local." {
		t.Errorf("SRV name = %q", srv.Hdr.Name)
	}
	if srv.Hdr.Class&cacheFlush != 0 {
		t.Error("SRV class has cache-flush bit when flushCache=false")
	}
	if r.srvRR(true).Hdr.Class&cacheFlush == 0 {
		t.Error("SRV class missing cache-flush bit when flushCache=true")
	}
}

func TestTxtRR(t *testing.T) {
	r := testResponder([]string{"v=1", "ip=192.168.1.10"})
	txt := r.txtRR(false)
	if len(txt.Txt) != 2 || txt.Txt[0] != "v=1" || txt.Txt[1] != "ip=192.168.1.10" {
		t.Errorf("TXT records = %v", txt.Txt)
	}
	if txt.Hdr.Name != "myhost._nvpair-test._tcp.local." {
		t.Errorf("TXT name = %q", txt.Hdr.Name)
	}
}

func TestTxtRREmptyFallback(t *testing.T) {
	r := testResponder(nil)
	txt := r.txtRR(false)
	if len(txt.Txt) != 1 || txt.Txt[0] != "" {
		t.Errorf("empty TXT set should emit one empty string, got %v", txt.Txt)
	}
}

func TestUpdateTXTSwapsRecords(t *testing.T) {
	// No interfaces => the re-announce that UpdateTXT triggers is a no-op, so
	// this stays a pure in-memory test.
	r := &Responder{ifaceAddrs: map[int][]net.IP{}}
	r.txt = []string{"v=1"}

	r.UpdateTXT([]string{"v=1", "cluster-uuid=abc"})
	got := r.currentTXT()
	if len(got) != 2 || got[0] != "v=1" || got[1] != "cluster-uuid=abc" {
		t.Fatalf("currentTXT after UpdateTXT = %v", got)
	}

	// currentTXT must return a copy, not the internal slice.
	got[0] = "mutated"
	if again := r.currentTXT(); again[0] != "v=1" {
		t.Errorf("currentTXT returned a mutable reference to internal state")
	}
}

func TestAddrsForResponse(t *testing.T) {
	r := testResponder(nil)

	if got := r.addrsForResponse(1); len(got) != 1 || !got[0].Equal(net.IPv4(192, 168, 1, 10)) {
		t.Errorf("addrsForResponse(1) = %v, want [192.168.1.10]", got)
	}
	// Unknown interface index falls back to all addresses.
	if got := r.addrsForResponse(99); len(got) != 2 {
		t.Errorf("addrsForResponse(99) = %v, want all (2) addresses", got)
	}
	// ifIndex 0 (unknown receiving iface) also returns all.
	if got := r.addrsForResponse(0); len(got) != 2 {
		t.Errorf("addrsForResponse(0) = %v, want all (2) addresses", got)
	}
}

func TestAppendBrowseRRs(t *testing.T) {
	r := testResponder([]string{"v=1"})
	resp := new(dns.Msg)
	r.appendBrowseRRs(resp, 1)

	if len(resp.Answer) != 1 {
		t.Fatalf("browse Answer count = %d, want 1 (PTR)", len(resp.Answer))
	}
	ptr, ok := resp.Answer[0].(*dns.PTR)
	if !ok || ptr.Ptr != "myhost._nvpair-test._tcp.local." {
		t.Fatalf("browse Answer[0] = %#v, want PTR to instance", resp.Answer[0])
	}

	var haveSRV, haveTXT, aCount int
	for _, rr := range resp.Extra {
		switch rr.(type) {
		case *dns.SRV:
			haveSRV++
		case *dns.TXT:
			haveTXT++
		case *dns.A:
			aCount++
		}
	}
	if haveSRV != 1 || haveTXT != 1 {
		t.Errorf("browse Extra SRV=%d TXT=%d, want 1 each", haveSRV, haveTXT)
	}
	// ifIndex 1 scopes to that interface's single address.
	if aCount != 1 {
		t.Errorf("browse Extra A count = %d, want 1 (iface-scoped)", aCount)
	}
}

// TestOpenSendConnUsesEphemeralPort guards the unicast-steal fix: the fallback
// send socket must not bind 5353, because a unicast-bound 5353 socket is more
// specific than Run's receive socket and would capture its unicast traffic.
func TestOpenSendConnUsesEphemeralPort(t *testing.T) {
	conn, err := openSendConn(net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatalf("openSendConn: %v", err)
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		t.Fatalf("LocalAddr = %T, want *net.UDPAddr", conn.LocalAddr())
	}
	if addr.Port == mdnsPort {
		t.Fatalf("fallback send socket bound %d; it would capture unicast", mdnsPort)
	}
}

// TestOpenSendConnCoexistsWithGroupSocket guards the bind path Run relies on:
// the fallback send socket must open while the multicast-group receive socket
// is already bound to 5353 in the same process.
func TestOpenSendConnCoexistsWithGroupSocket(t *testing.T) {
	lc := net.ListenConfig{Control: setReuseAddr}
	group, err := lc.ListenPacket(context.Background(), "udp4", net.JoinHostPort(mdnsGroupV4.String(), fmt.Sprint(mdnsPort)))
	if err != nil {
		t.Skipf("cannot bind group receive socket: %v", err)
	}
	defer group.Close()

	conn, err := openSendConn(net.IPv4(127, 0, 0, 1))
	if err != nil {
		t.Fatalf("openSendConn alongside group socket: %v", err)
	}
	defer conn.Close()
}

// TestSendOnInterfaceFreshConnGuard covers the fallback path when no shared
// socket is installed: an interface with no addresses must error rather than
// panic.
func TestSendOnInterfaceFreshConnGuard(t *testing.T) {
	r := &Responder{ifaceAddrs: map[int][]net.IP{}}
	if err := r.sendOnInterface([]byte("x"), 42, mdnsTargetV4); err == nil {
		t.Fatal("sendOnInterface with no addresses on the interface = nil error, want error")
	}
}

// TestSendOnInterfaceUsesSharedSocket verifies the Unix send path: once Run
// installs its receive socket, sends go out that socket, so the datagram
// source port is the shared socket's port (5353 in production) instead of a
// fresh per-send socket's.
func TestSendOnInterfaceUsesSharedSocket(t *testing.T) {
	rcv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen receiver: %v", err)
	}
	defer rcv.Close()

	shared, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("listen shared: %v", err)
	}
	defer shared.Close()
	sharedPort := shared.LocalAddr().(*net.UDPAddr).Port

	r := &Responder{ifaceAddrs: map[int][]net.IP{1: {net.IPv4(127, 0, 0, 1)}}}
	r.setSendPC(ipv4.NewPacketConn(shared))

	if err := r.sendOnInterface([]byte("hi"), 1, rcv.LocalAddr().(*net.UDPAddr)); err != nil {
		t.Fatalf("sendOnInterface: %v", err)
	}
	_ = rcv.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 16)
	_, from, err := rcv.ReadFromUDP(buf)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if from.Port != sharedPort {
		t.Fatalf("datagram source port = %d, want shared socket port %d", from.Port, sharedPort)
	}
}

func TestIfaceAddrsEqual(t *testing.T) {
	base := map[int][]net.IP{
		1: {net.IPv4(192, 168, 1, 10), net.IPv4(192, 168, 1, 11)},
		2: {net.IPv4(10, 0, 0, 5)},
	}
	// Same addresses, different order within an interface => still equal.
	reordered := map[int][]net.IP{
		1: {net.IPv4(192, 168, 1, 11), net.IPv4(192, 168, 1, 10)},
		2: {net.IPv4(10, 0, 0, 5)},
	}
	if !ifaceAddrsEqual(base, reordered) {
		t.Error("order-insensitive equal maps reported unequal")
	}
	// Different length.
	if ifaceAddrsEqual(base, map[int][]net.IP{1: {net.IPv4(192, 168, 1, 10)}}) {
		t.Error("maps of different size reported equal")
	}
	// Different address on an interface.
	diff := map[int][]net.IP{
		1: {net.IPv4(192, 168, 1, 10), net.IPv4(192, 168, 1, 12)},
		2: {net.IPv4(10, 0, 0, 5)},
	}
	if ifaceAddrsEqual(base, diff) {
		t.Error("maps with a differing address reported equal")
	}
}
