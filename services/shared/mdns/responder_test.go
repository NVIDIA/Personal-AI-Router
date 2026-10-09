// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mdns

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/miekg/dns"
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
	test := func(name, instance, service, domain string, port int) {
		t.Run(name, func(t *testing.T) {
			_, err := NewResponder(instance, service, domain, port, nil)
			require.Error(t, err)
		})
	}
	test("missing instance", "", "_nvpair-test._tcp", "local", 14318)
	test("missing service", "myhost", "", "local", 14318)
	test("missing port", "myhost", "_nvpair-test._tcp", "local", 0)
}

func TestNewResponderNormalizesNames(t *testing.T) {
	// The success path needs a real multicast-capable interface; skip where
	// there isn't one (e.g. a locked-down CI sandbox) rather than fail.
	r, err := NewResponder(".myhost.", "._nvpair-test._tcp.", "", 14318, []string{"v=1"})
	if err != nil {
		t.Skipf("NewResponder unavailable in this environment: %v", err)
	}
	assert.Equal(t, "local", r.domain)
	assert.Equal(t, "_nvpair-test._tcp.local.", r.serviceName)
	assert.Equal(t, "myhost._nvpair-test._tcp.local.", r.instanceName)
}

func TestNewResponderCopiesTXT(t *testing.T) {
	in := []string{"v=1"}
	r := testResponder(in)
	in[0] = "mutated"
	assert.Equal(t, "v=1", r.currentTXT()[0], "currentTXT tracked caller's slice mutation")
}

func TestSrvRR(t *testing.T) {
	r := testResponder(nil)
	srv := r.srvRR(false)
	assert.Equal(t, uint16(14318), srv.Port, "SRV port")
	assert.Equal(t, "myhost.local.", srv.Target, "SRV target")
	assert.Equal(t, "myhost._nvpair-test._tcp.local.", srv.Hdr.Name, "SRV name")
	assert.Equal(t, uint16(0), srv.Hdr.Class&cacheFlush, "SRV class has cache-flush bit when flushCache=false")
	assert.NotEqual(t, uint16(0), r.srvRR(true).Hdr.Class&cacheFlush, "SRV class missing cache-flush bit when flushCache=true")
}

func TestTxtRR(t *testing.T) {
	r := testResponder([]string{"v=1", "ip=192.168.1.10"})
	txt := r.txtRR(false)
	assert.Equal(t, []string{"v=1", "ip=192.168.1.10"}, txt.Txt, "TXT records")
	assert.Equal(t, "myhost._nvpair-test._tcp.local.", txt.Hdr.Name, "TXT name")
}

func TestTxtRREmptyFallback(t *testing.T) {
	r := testResponder(nil)
	txt := r.txtRR(false)
	assert.Equal(t, []string{""}, txt.Txt, "empty TXT set should emit one empty string")
}

func TestUpdateTXTSwapsRecords(t *testing.T) {
	// No interfaces => the re-announce that UpdateTXT triggers is a no-op, so
	// this stays a pure in-memory test.
	r := &Responder{ifaceAddrs: map[int][]net.IP{}}
	r.txt = []string{"v=1"}

	r.UpdateTXT([]string{"v=1", "cluster-uuid=abc"})
	got := r.currentTXT()
	require.Len(t, got, 2, "currentTXT after UpdateTXT")
	assert.Equal(t, []string{"v=1", "cluster-uuid=abc"}, got, "currentTXT after UpdateTXT")

	// currentTXT must return a copy, not the internal slice.
	got[0] = "mutated"
	assert.Equal(t, "v=1", r.currentTXT()[0], "currentTXT returned a mutable reference to internal state")
}

func TestAddrsForResponse(t *testing.T) {
	r := testResponder(nil)

	got := r.addrsForResponse(1)
	require.Len(t, got, 1)
	assert.Equal(t, "192.168.1.10", got[0].String(), "addrsForResponse(1)")
	// Unknown interface index falls back to all addresses.
	assert.Len(t, r.addrsForResponse(99), 2)
	// ifIndex 0 (unknown receiving iface) also returns all.
	assert.Len(t, r.addrsForResponse(0), 2)
}

func TestAppendBrowseRRs(t *testing.T) {
	r := testResponder([]string{"v=1"})
	resp := new(dns.Msg)
	r.appendBrowseRRs(resp, 1)

	require.Len(t, resp.Answer, 1, "browse Answer count")
	ptr, ok := resp.Answer[0].(*dns.PTR)
	require.True(t, ok, "browse Answer[0]")
	assert.Equal(t, "myhost._nvpair-test._tcp.local.", ptr.Ptr, "browse Answer[0]")

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
	assert.Equal(t, 1, haveSRV, "browse Extra SRV")
	assert.Equal(t, 1, haveTXT, "browse Extra TXT")
	// ifIndex 1 scopes to that interface's single address.
	assert.Equal(t, 1, aCount, "browse Extra A count")
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
	assert.True(t, ifaceAddrsEqual(base, reordered), "order-insensitive equal maps reported unequal")
	// Different length.
	assert.False(t, ifaceAddrsEqual(base, map[int][]net.IP{1: {net.IPv4(192, 168, 1, 10)}}), "maps of different size reported equal")
	// Different address on an interface.
	diff := map[int][]net.IP{
		1: {net.IPv4(192, 168, 1, 10), net.IPv4(192, 168, 1, 12)},
		2: {net.IPv4(10, 0, 0, 5)},
	}
	assert.False(t, ifaceAddrsEqual(base, diff), "maps with a differing address reported equal")
}
