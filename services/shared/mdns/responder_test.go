// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mdns

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/miekg/dns"

	"nvpair-shared/netmon"
)

// noMulticastErrFragment is the substring of NewResponder's failure that names
// its one environmental precondition (see responder.go's
// "no multicast-capable IPv4 interfaces" error).
const noMulticastErrFragment = "no multicast-capable IPv4 interface"

// requireMulticastOrFail lets a constructor-path test skip only on the
// one environmental precondition NewResponder documents — no multicast-capable
// IPv4 interface — and fail on any other error. An arbitrary error is not an
// environment, it's a bug; letting it skip would mean a responder broken for
// every host could pass with the host-record invariant unpinned.
func requireMulticastOrFail(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if !strings.Contains(err.Error(), noMulticastErrFragment) {
		t.Fatalf("NewResponder failed for a reason other than the environment precondition: %v", err)
	}
	t.Skipf("no multicast-capable IPv4 interface with a non-loopback address on this host: %v", err)
}

// testResponder builds a Responder with fixed fields so the record-building
// tests don't depend on the host's real interfaces or hostname. hostName is
// the synthetic host record PAIR claims, never a machine name; the instance
// name "myhost" stays a display name and owns no address record of its own.
func testResponder(txt []string) *Responder {
	r := &Responder{
		instance:     "myhost",
		service:      "_nvpair-test._tcp",
		domain:       "local",
		port:         14318,
		hostName:     "nvpair-0123456789ab.local.",
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

func TestNewResponder_RejectsInvalidRegistration(t *testing.T) {
	// These cases return before any interface enumeration, so they're
	// environment-independent.
	test := func(name, instance, service, domain string, port int) {
		t.Run(name, func(t *testing.T) {
			_, err := NewResponder(instance, "nvpair-0123456789ab", service, domain, port, nil)
			require.Error(t, err)
		})
	}
	test("missing instance", "", "_nvpair-test._tcp", "local", 14318)
	test("missing service", "myhost", "", "local", 14318)
	test("missing port", "myhost", "_nvpair-test._tcp", "local", 0)
}

func TestNewResponder_RejectsHostLabelThatIsNotASingleDNSLabel(t *testing.T) {
	// Like TestNewResponder_RejectsInvalidRegistration, these return before any
	// interface enumeration, so they're environment-independent: on a machine
	// without multicast the error must still be the label rejection, which is
	// why the assertion checks the message names the host label rather than
	// settling for any error.
	//
	// "two labels" is the shape develop produced BY ITSELF: NewResponder built
	// the host name as hostname + "." + domain + "." with the hostname from
	// os.Hostname(), so a machine whose BSD hostname carries a DNS search
	// domain ("indigo.lab") advertised the two-label SRV target
	// "indigo.lab.local." — a name macOS does not author and will not
	// resolve. Refusing multi-label input fixes that second defect; it is not
	// a style point.
	test := func(name, hostLabel string) {
		t.Run(name, func(t *testing.T) {
			_, err := NewResponder("myhost", hostLabel, "._nvpair-test._tcp.", "local", 14318, nil)
			require.Error(t, err, "host label %q must not be advertised", hostLabel)
			assert.ErrorContains(t, err, "host label",
				"the error must name the offending host label, proving the rejection is the label check and not the interface enumeration below it")
		})
	}
	test("two labels", "indigo.local")
	test("two labels with trailing dot", "nvpair-0123456789ab.local.")
	test("contains a space", "nvpair test")
	test("empty", "")
	test("64 bytes", strings.Repeat("a", 64))
	test("contains a slash", "nvpair/test")
}

func TestNewResponder_NormalizesNames(t *testing.T) {
	// The success path needs a real multicast-capable interface; skip only on
	// that named precondition (a locked-down CI sandbox), never on some other
	// error NewResponder could return.
	r, err := NewResponder(".myhost.", "nvpair-0123456789ab.", "._nvpair-test._tcp.", "", 14318, []string{"v=1"})
	requireMulticastOrFail(t, err)
	assert.Equal(t, "local", r.domain)
	assert.Equal(t, "_nvpair-test._tcp.local.", r.serviceName)
	assert.Equal(t, "myhost._nvpair-test._tcp.local.", r.instanceName)
	// A trailing dot on the host label is decoration, trimmed like the one on
	// the instance, so the advertised record stays a single label under the
	// domain rather than a name ending in an empty label.
	assert.Equal(t, "nvpair-0123456789ab.local.", r.AdvertisedHost(), "trailing dot on the host label")
}

// TestNewResponder_DoesNotAdvertiseTheOperatingSystemHostname is the red for
// the real defect: NewResponder used to derive its host record — the SRV target
// and the name its A records answer under — from os.Hostname(), so PAIR
// claimed the machine's name and macOS answered the contention by renaming the
// computer's Local hostname (issue #229). It runs the constructor for real,
// which needs a multicast-capable interface, so it skips on that one named
// precondition — on such hosts the invariant stays pinned by
// TestNewResponder_DerivesTheHostRecordFromItsParameterWithoutMulticast, which
// fakes only the interface source and never skips.
func TestNewResponder_DoesNotAdvertiseTheOperatingSystemHostname(t *testing.T) {
	r, err := NewResponder("myhost", "nvpair-0123456789ab", "._nvpair-test._tcp.", "local", 14318, nil)
	requireMulticastOrFail(t, err)
	assert.Equal(t, "nvpair-0123456789ab.local.", r.AdvertisedHost(),
		"the advertised host record must be the caller's single label plus the domain")

	hostname, err := os.Hostname()
	require.NoError(t, err)
	if hostname == "" || hostname == "nvpair-0123456789ab" {
		t.Skipf("this machine's os.Hostname() is %q, which cannot be told apart from the fixture label", hostname)
	}
	require.NotEqual(t, hostname+".local.", r.AdvertisedHost(),
		"the responder must not derive its host record from os.Hostname() (%q)", hostname)

	buf, err := r.announcementMsg(1).Pack()
	require.NoError(t, err, "pack announcement")
	var msg dns.Msg
	require.NoError(t, msg.Unpack(buf), "unpack announcement")
	for _, rr := range append(msg.Answer, msg.Extra...) {
		assert.NotEqual(t, hostname+".local.", rr.Header().Name,
			"no record on the wire may be named for the machine hostname %q", hostname)
	}
}

// TestNewResponder_DerivesTheHostRecordFromItsParameterWithoutMulticast pins
// the central invariant of the fix — the advertised host record is built from
// the hostLabel parameter, never from the machine — on EVERY host: it swaps
// only the interface source (the enumerateSnapshot seam) for a fake snapshot
// and still calls the real NewResponder, so there is no skip path and no
// environment it can hide in. A machine whose own hostname collides with the
// usual fixture label gets a different label so the "a hostname-derived value
// would be wrong" half stays meaningful here too.
//
// Tests that touch enumerateSnapshot must not use t.Parallel: it is package
// state, and a parallel sibling would read the fake or lose its own.
func TestNewResponder_DerivesTheHostRecordFromItsParameterWithoutMulticast(t *testing.T) {
	realEnumerate := enumerateSnapshot
	t.Cleanup(func() { enumerateSnapshot = realEnumerate })
	enumerateSnapshot = func() netmon.Snapshot {
		return netmon.Snapshot{
			LocalIPs: map[string]bool{"203.0.113.7": true},
			IfaceV4:  map[int][]net.IP{2: {net.IPv4(203, 0, 113, 7)}},
		}
	}

	hostname, err := os.Hostname()
	require.NoError(t, err)
	hostLabel := "nvpair-0123456789ab"
	if hostname == hostLabel {
		// Even on a machine named for this suite's fixture label, a
		// hostname-derived host record must be visibly wrong: any other
		// single label will do.
		hostLabel = "nvpair-not-the-hostname"
	}

	r, err := NewResponder("myhost", hostLabel, "._nvpair-test._tcp.", "local", 14318, nil)
	require.NoError(t, err, "the fake snapshot must satisfy the interface requirement")

	assert.Equal(t, hostLabel+".local.", r.AdvertisedHost(),
		"the host record must be the caller's label under the domain")
	require.NotEqual(t, hostname+".local.", r.AdvertisedHost(),
		"the host record must not be the machine hostname %q", hostname)

	buf, err := r.announcementMsg(2).Pack()
	require.NoError(t, err, "pack announcement")
	var msg dns.Msg
	require.NoError(t, msg.Unpack(buf), "unpack announcement")
	var srv *dns.SRV
	var addrs []*dns.A
	for _, rr := range append(msg.Answer, msg.Extra...) {
		switch rec := rr.(type) {
		case *dns.SRV:
			srv = rec
		case *dns.A:
			addrs = append(addrs, rec)
		}
		require.NotEqual(t, hostname+".local.", rr.Header().Name,
			"no record on the wire may be named for the machine hostname %q", hostname)
	}
	require.NotNil(t, srv, "announcement carries an SRV")
	require.NotEmpty(t, addrs, "announcement carries A records from the fake interface")
	assert.Equal(t, r.AdvertisedHost(), srv.Target, "SRV target is the advertised host record")
	for _, a := range addrs {
		assert.Equal(t, r.AdvertisedHost(), a.Header().Name, "A record owner name is the advertised host record")
	}
}

func TestNewResponder_CopiesTXT(t *testing.T) {
	in := []string{"v=1"}
	r := testResponder(in)
	in[0] = "mutated"
	assert.Equal(t, "v=1", r.currentTXT()[0], "currentTXT tracked caller's slice mutation")
}

func TestSrvRR_EncodesServiceTargetAndPort(t *testing.T) {
	r := testResponder(nil)
	srv := r.srvRR(false, recordTTL)
	assert.Equal(t, uint16(14318), srv.Port, "SRV port")
	assert.Equal(t, "nvpair-0123456789ab.local.", srv.Target, "SRV target")
	assert.Equal(t, "myhost._nvpair-test._tcp.local.", srv.Hdr.Name, "SRV name")
	assert.Equal(t, uint16(0), srv.Hdr.Class&cacheFlush, "SRV class has cache-flush bit when flushCache=false")
	assert.NotEqual(t, uint16(0), r.srvRR(true, recordTTL).Hdr.Class&cacheFlush, "SRV class missing cache-flush bit when flushCache=true")
}

func TestTxtRR_EncodesConfiguredTXTValues(t *testing.T) {
	r := testResponder([]string{"v=1", "ip=192.168.1.10"})
	txt := r.txtRR(false, recordTTL)
	assert.Equal(t, []string{"v=1", "ip=192.168.1.10"}, txt.Txt, "TXT records")
	assert.Equal(t, "myhost._nvpair-test._tcp.local.", txt.Hdr.Name, "TXT name")
}

func TestTxtRR_UsesFallbackForEmptyTXT(t *testing.T) {
	r := testResponder(nil)
	txt := r.txtRR(false, recordTTL)
	assert.Equal(t, []string{""}, txt.Txt, "empty TXT set should emit one empty string")
}

func TestUpdateTXT_SwapsRecords(t *testing.T) {
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

func TestAddrsForResponse_PrefersReceivingInterfaceAddresses(t *testing.T) {
	r := testResponder(nil)

	got := r.addrsForResponse(1)
	require.Len(t, got, 1)
	assert.Equal(t, "192.168.1.10", got[0].String(), "addrsForResponse(1)")
	// Unknown interface index falls back to all addresses.
	assert.Len(t, r.addrsForResponse(99), 2)
	// ifIndex 0 (unknown receiving iface) also returns all.
	assert.Len(t, r.addrsForResponse(0), 2)
}

func TestAppendBrowseRRs_IncludesServiceRecords(t *testing.T) {
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

func TestAnnouncementMsg_EveryAddressRecordIsOwnedByTheSRVTarget(t *testing.T) {
	// GUARD, not the red: testResponder injects hostName directly, so this is
	// green against develop too. It pins the wire consequence of the design
	// rule that the instance name is only a display name: an A record answering
	// under "myhost.local." would contend with the host's own mDNS responder
	// over the machine name even if the SRV pointed somewhere else. It runs with
	// no socket and no constructor, so the rule stays checked on machines that
	// have no multicast interface and must skip the constructor-path tests.
	r := testResponder(nil)
	buf, err := r.announcementMsg(1).Pack()
	require.NoError(t, err, "pack announcement")
	var msg dns.Msg
	require.NoError(t, msg.Unpack(buf), "unpack announcement")

	var srv *dns.SRV
	var addrs []*dns.A
	for _, rr := range append(msg.Answer, msg.Extra...) {
		switch rec := rr.(type) {
		case *dns.SRV:
			srv = rec
		case *dns.A:
			addrs = append(addrs, rec)
		}
		require.NotEqual(t, "myhost.local.", rr.Header().Name,
			"the instance name %q is a display name and must never own a record", r.instance)
	}
	require.NotNil(t, srv, "announcement carries an SRV")
	require.NotEmpty(t, addrs, "announcement carries A records")
	assert.Equal(t, "nvpair-0123456789ab.local.", srv.Target, "SRV target")
	for _, a := range addrs {
		assert.Equal(t, srv.Target, a.Header().Name, "A record owner name must match the SRV target")
	}
}

// packUnpack round-trips msg through the wire format so the goodbye tests
// assert what a peer parses, not an in-memory shortcut a later refactor could
// take before Pack is ever reached.
func packUnpack(t *testing.T, msg *dns.Msg, label string) *dns.Msg {
	t.Helper()
	buf, err := msg.Pack()
	require.NoError(t, err, "pack %s", label)
	var got dns.Msg
	require.NoError(t, got.Unpack(buf), "unpack %s", label)
	return &got
}

// sortedRecordStrings renders rrs as wire-notation strings, sorted so two
// messages carrying the same records compare equal regardless of append order.
// Each record's TTL is zeroed before rendering because the caller asserts TTL
// separately and RFC 6762 §10.1 makes TTL the only field allowed to differ
// between an announcement and its retraction — so a zeroed render pins that
// name, rrtype, class and rdata are exactly the announced ones. The zeroing
// happens on a deep copy: callers routinely assert TTL on the very records
// they render, and zeroing in place would turn each later assertion into a
// check against this helper's own side effect.
func sortedRecordStrings(rrs []dns.RR) []string {
	out := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		rendered := dns.Copy(rr)
		rendered.Header().Ttl = 0
		out = append(out, rendered.String())
	}
	sort.Strings(out)
	return out
}

// TestGoodbye_RetractsEveryRecordTheResponderClaimed is the D4 red: on
// shutdown PAIR must retract the SRV, TXT and A records it claimed at TTL 0,
// not just the PTR. Peers handed only a PTR retraction keep a live SRV target,
// TXT and A cached for up to the 120s record TTL, resolving and connecting to
// a node that is already gone for that whole window. It drives goodbyeMsg and
// announcementMsg through the real wire format with a fixture Responder, so
// unlike the constructor-path tests it cannot skip on a host without a
// multicast-capable interface.
func TestGoodbye_RetractsEveryRecordTheResponderClaimed(t *testing.T) {
	r := testResponder([]string{"v=1"})

	goodbye := packUnpack(t, r.goodbyeMsg(1), "goodbye on interface 1")
	announced := packUnpack(t, r.announcementMsg(1), "announcement on interface 1")
	require.NotEmpty(t, announced.Answer, "the fixture announcement carries records to retract")

	// A receiver that walks only Answer would ignore a retraction parked in
	// Ns or Extra, so every retraction record has to sit in the Answer section.
	require.Empty(t, goodbye.Ns, "goodbye parks records in Ns, where a receiver that walks only Answer would miss them")
	require.Empty(t, goodbye.Extra, "goodbye parks records in Extra, where a receiver that walks only Answer would miss them")

	var ptrs []*dns.PTR
	var srvs []*dns.SRV
	var txts []*dns.TXT
	var addrs []*dns.A
	for _, rr := range goodbye.Answer {
		switch rec := rr.(type) {
		case *dns.PTR:
			ptrs = append(ptrs, rec)
		case *dns.SRV:
			srvs = append(srvs, rec)
		case *dns.TXT:
			txts = append(txts, rec)
		case *dns.A:
			addrs = append(addrs, rec)
		}
	}
	require.Len(t, ptrs, 1, "goodbye answers with exactly one PTR")
	require.Len(t, srvs, 1, "goodbye answers with exactly one SRV: a PTR-only goodbye leaves the SRV target cached live")
	require.Len(t, txts, 1, "goodbye answers with exactly one TXT: a PTR-only goodbye leaves the TXT record cached")
	require.NotEmpty(t, addrs, "goodbye answers with at least one A: a PTR-only goodbye leaves the host address cached")

	// RFC 6762 §10.1: a retraction is the announced record with an RR TTL of
	// zero, while the announcement itself must keep the real record TTL.
	for _, rr := range goodbye.Answer {
		assert.Zero(t, rr.Header().Ttl, "goodbye record %s must retract at TTL 0", rr.Header().Name)
	}
	for _, rr := range announced.Answer {
		assert.Equal(t, uint32(recordTTL), rr.Header().Ttl, "announcement record %s must still carry the real record TTL", rr.Header().Name)
	}

	// Cache-flush bits mirror the announcement: §10.2, "The cache-flush bit
	// MUST NOT ever be set in any shared resource record" — so not on the
	// shared PTR — and "The host MUST set the cache-flush bit on all members
	// of the unique RRSet" — so on the unique SRV, TXT and A. (§8.3 is where
	// those records earn the bit by being verified-unique announcements;
	// §10.2 is where the rule itself lives, for every response.) Asserting
	// them against fixed expectations (not just against the announcement)
	// means a regression that drops the bit from both sides still fails here.
	assert.Zero(t, ptrs[0].Hdr.Class&cacheFlush, "the goodbye PTR is a shared record and must not carry the cache-flush bit, as announced")
	assert.NotZero(t, srvs[0].Hdr.Class&cacheFlush, "the goodbye SRV is a unique record and must carry the cache-flush bit, as announced")
	assert.NotZero(t, txts[0].Hdr.Class&cacheFlush, "the goodbye TXT is a unique record and must carry the cache-flush bit, as announced")
	for _, a := range addrs {
		assert.NotZero(t, a.Hdr.Class&cacheFlush, "the goodbye A for %s is a unique record and must carry the cache-flush bit, as announced", a.A)
	}

	// Same name, rrtype, class and rdata as the announcement — under §10.1
	// only the TTL may differ. A retraction record differing in any other
	// field does not match what the peer cached, so it retracts nothing.
	// Comparing the announced set against the goodbye set also pins A rdata:
	// goodbyeMsg(1) must retract exactly the addresses announcementMsg(1)
	// published on that same interface.
	assert.Equal(t, sortedRecordStrings(announced.Answer), sortedRecordStrings(goodbye.Answer),
		"the goodbye must retract the very records the announcement published")

	for _, a := range addrs {
		assert.Equal(t, srvs[0].Target, a.Header().Name, "A record owner name must be the SRV target it retracts")
	}
}

// TestGoodbye_ScopesAddressRetractionsToTheInterfaceTheyAreSentOn makes
// goodbyeMsg's ifIdx parameter load-bearing. The fixture's interfaces 1 and 2
// hold different addresses, so each goodbye must retract exactly its own
// interface's announced addresses and the two goodbye sets must differ: if a
// refactor hard-codes one interface, the goodbye on the other interface
// retracts the wrong address and this test fails — leaving peers to cache a
// reachable-looking address on an interface this node no longer serves.
func TestGoodbye_ScopesAddressRetractionsToTheInterfaceTheyAreSentOn(t *testing.T) {
	r := testResponder([]string{"v=1"})

	scopedAddrs := map[int][]string{}
	for _, ifIdx := range []int{1, 2} {
		goodbye := packUnpack(t, r.goodbyeMsg(ifIdx), fmt.Sprintf("goodbye on interface %d", ifIdx))
		announced := packUnpack(t, r.announcementMsg(ifIdx), fmt.Sprintf("announcement on interface %d", ifIdx))

		var goodbyeAddrs, announcedAddrs []string
		for _, rr := range goodbye.Answer {
			if a, ok := rr.(*dns.A); ok {
				goodbyeAddrs = append(goodbyeAddrs, a.A.String())
			}
		}
		for _, rr := range announced.Answer {
			if a, ok := rr.(*dns.A); ok {
				announcedAddrs = append(announcedAddrs, a.A.String())
			}
		}
		sort.Strings(goodbyeAddrs)
		sort.Strings(announcedAddrs)
		require.NotEmpty(t, goodbyeAddrs, "goodbye on interface %d carries A records", ifIdx)
		require.Equal(t, announcedAddrs, goodbyeAddrs,
			"the goodbye sent on interface %d must retract exactly the addresses that interface's announcement published", ifIdx)
		scopedAddrs[ifIdx] = goodbyeAddrs
	}

	// The fixture guarantees interfaces 1 and 2 hold different addresses, so
	// equal goodbye sets would prove ifIdx never reached the A-record builder.
	require.NotEqual(t, scopedAddrs[1], scopedAddrs[2],
		"interfaces 1 and 2 hold different addresses, so goodbyeMsg must retract different addresses on each")
}

// goodbyeAttempt is one send recorded by the goodbyeSend seam: the interface
// index it was sent on and the records parsed back out of the exact bytes
// handed to the send, so assertions run on what would hit the wire.
type goodbyeAttempt struct {
	ifIndex int
	records []string // sortedRecordStrings over the sent bytes' Answer
}

// captureGoodbyeSends replaces the goodbyeSend seam for one test and records
// every send attempt. sendGoodbye runs synchronously on the test goroutine,
// so the recorder needs no locking, and require.* inside the seam stays legal.
func captureGoodbyeSends(t *testing.T, onSend func(ifIndex int) error) *[]goodbyeAttempt {
	t.Helper()
	attempts := &[]goodbyeAttempt{}
	old := goodbyeSend
	t.Cleanup(func() { goodbyeSend = old })
	goodbyeSend = func(_ *Responder, buf []byte, ifIndex int) error {
		var msg dns.Msg
		require.NoError(t, msg.Unpack(buf), "goodbye bytes offered for interface %d do not parse", ifIndex)
		for _, rr := range msg.Answer {
			require.Zero(t, rr.Header().Ttl, "goodbye record %s offered on interface %d carries TTL %d, not the 0 that retracts it",
				rr.Header().Name, ifIndex, rr.Header().Ttl)
		}
		*attempts = append(*attempts, goodbyeAttempt{ifIndex: ifIndex, records: sortedRecordStrings(msg.Answer)})
		return onSend(ifIndex)
	}
	return attempts
}

// TestSendGoodbye_SendsOneGoodbyePerInterfaceScopedToItsOwnAddresses is the
// send-layer half of D4: on shutdown sendGoodbye must hand the send exactly
// one packed goodbye per interface the responder knows, and each packet must
// be that interface's own full retraction. The per-interface address scoping
// is proven at the send layer — from the bytes offered — not only at message
// construction, because a shutdown that packs iface 1's addresses into every
// interface's packet retracts the wrong A records wherever it goes out.
func TestSendGoodbye_SendsOneGoodbyePerInterfaceScopedToItsOwnAddresses(t *testing.T) {
	r := testResponder([]string{"v=1"})
	attempts := captureGoodbyeSends(t, func(int) error { return nil })

	r.sendGoodbye()

	require.Len(t, *attempts, 2, "the fixture responder knows exactly two interfaces, so shutdown must offer exactly two goodbye sends")
	byIface := map[int][]goodbyeAttempt{}
	for _, a := range *attempts {
		byIface[a.ifIndex] = append(byIface[a.ifIndex], a)
	}
	assert.Len(t, byIface[1], 1, "interface 1 must receive exactly one goodbye send")
	assert.Len(t, byIface[2], 1, "interface 2 must receive exactly one goodbye send")

	for _, ifIdx := range []int{1, 2} {
		require.Len(t, byIface[ifIdx], 1)
		want := sortedRecordStrings(packUnpack(t, r.goodbyeMsg(ifIdx), fmt.Sprintf("locally built goodbye for interface %d", ifIdx)).Answer)
		assert.Equal(t, want, byIface[ifIdx][0].records,
			"the packet offered for interface %d must be that interface's own full retraction", ifIdx)
	}
	assert.NotEqual(t, byIface[1][0].records, byIface[2][0].records,
		"the fixture interfaces hold different addresses, so the two offered packets must differ — identical packets would mean ifIdx was flattened away before the A records reached the wire")
}

// TestSendGoodbye_LogsWarnAndKeepsOtherInterfacesWhenOneSendFails pins I1
// and the loop rule together: the desktop runs services at warn by default,
// so a goodbye failure logged any quieter is invisible, and a retraction
// that never left the machine costs every peer the full record TTL of
// resolving a dead node — that is operational impact, not debug noise. One
// dead interface must still not suppress the other retractions.
func TestSendGoodbye_LogsWarnAndKeepsOtherInterfacesWhenOneSendFails(t *testing.T) {
	r := testResponder([]string{"v=1"})
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	attempts := captureGoodbyeSends(t, func(ifIndex int) error {
		if ifIndex == 1 {
			return errors.New("synthetic send failure on interface 1")
		}
		return nil
	})

	r.sendGoodbye()

	var attempted []int
	for _, a := range *attempts {
		attempted = append(attempted, a.ifIndex)
	}
	sort.Ints(attempted)
	assert.Equal(t, []int{1, 2}, attempted,
		"a failed send on interface 1 must not suppress interface 2's retraction")

	var entry struct {
		Level   string `json:"level"`
		Iface   string `json:"iface"`
		IfIndex int    `json:"ifIndex"`
	}
	require.NoError(t, json.Unmarshal(logs.Bytes(), &entry))
	assert.Equal(t, "WARN", entry.Level,
		"a goodbye send failure must be visible at the warn level the desktop default configures, which is where a silent 120s stale record becomes discoverable")
	assert.Equal(t, 1, entry.IfIndex,
		"the goodbye failure line must name which interface index failed")
	// The interface name half is exactly as available to this test as it is
	// to production: net.InterfaceByIndex is a fact about the host, and a
	// fixture index that resolves nowhere gets logged by index only. The
	// ifIndex and WARN assertions above hold either way.
	if ifi, err := net.InterfaceByIndex(1); err == nil {
		assert.Equal(t, ifi.Name, entry.Iface,
			"the goodbye failure line must name the interface, not only its index")
	}
}

// TestSendGoodbye_LogsWarnAndKeepsOtherInterfacesWhenOneGoodbyeCannotBePacked
// covers the other way one interface's retraction can be lost: a malformed
// address makes dns refuse to serialize that interface's packet. The failure
// must be logged at Warn like a send failure, and the loop must still reach
// the interfaces whose packets do build.
func TestSendGoodbye_LogsWarnAndKeepsOtherInterfacesWhenOneGoodbyeCannotBePacked(t *testing.T) {
	r := testResponder([]string{"v=1"})
	// A five-byte net.IP is not valid A rdata: it builds into the message in
	// memory, but dns.Msg.Pack refuses it — the hazard a silent `continue`
	// would swallow.
	r.ifaceAddrs = map[int][]net.IP{
		1: {net.IP{192, 0, 2, 100, 200}},
		2: {net.IPv4(10, 0, 0, 5)},
	}
	logs := captureDebugLogs(t)
	attempts := captureGoodbyeSends(t, func(int) error { return nil })

	r.sendGoodbye()

	var attempted []int
	for _, a := range *attempts {
		attempted = append(attempted, a.ifIndex)
	}
	assert.Equal(t, []int{2}, attempted,
		"interface 1's packet cannot be built, so only interface 2's retraction may be offered — and it may not be skipped")
	gotLogs := logs.String()
	assert.Contains(t, gotLogs, "level=WARN",
		"a goodbye that could not even be serialized is just as invisible a retraction loss as a failed send")
	assert.Contains(t, gotLogs, "ifIndex=1",
		"the unbuildable-goodbye line must name which interface index it happened on")
}

// TestSortedRecordStrings_DoesNotMutateTheRecordsItRenders pins the helper's
// contract: it exists to render records for comparison, and TTL is asserted
// by callers elsewhere in the very same tests that call it. A helper that
// zeroes TTL in place turns every TTL assertion that runs after it into a
// check against its own side effect.
func TestSortedRecordStrings_DoesNotMutateTheRecordsItRenders(t *testing.T) {
	rrs := []dns.RR{&dns.A{
		Hdr: dns.RR_Header{Name: "x.local.", Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: recordTTL},
		A:   net.IPv4(192, 0, 2, 1),
	}}
	rendered := sortedRecordStrings(rrs)
	assert.Equal(t, uint32(recordTTL), rrs[0].Header().Ttl,
		"sortedRecordStrings rewrote the caller's record TTL to 0 while rendering")
	require.Len(t, rendered, 1)
	assert.Contains(t, rendered[0], "\t0\t", "rendered form must still zero the TTL for order-free comparison")
}

func TestIfaceAddrsEqual_IgnoresAddressOrdering(t *testing.T) {
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
