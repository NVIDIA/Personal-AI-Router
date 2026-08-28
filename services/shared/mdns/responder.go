// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package mdns provides a minimal, cross-platform mDNS responder that
// advertises a single service instance and answers queries for it on every
// multicast-capable IPv4 interface.
//
// It exists because grandcat/zeroconf's Server (and its Resolver, which the
// browser side in nvpair-shared/discovery works around too) binds a single UDP
// socket to the multicast group address 224.0.0.x:5353 for both sending and
// receiving. Windows refuses to send packets from a socket whose local address
// is a multicast group, so on Windows the library's outbound announcements,
// probes, and reactive responses never reach the wire — and the WriteTo errors
// are silently dropped inside zeroconf, so nothing logs a problem. On a
// multi-homed / VPN Windows host this is exactly why the machine stays
// invisible to LAN peers.
//
// We keep zeroconf's receive trick (join the group on each multicast interface,
// which works fine on Windows) but send every reply/announcement from a
// per-interface unicast-bound socket with SetMulticastInterface set explicitly.
// That path is well-supported on Windows.
//
// This is the single implementation consolidated (the mDNS dedup) from the five
// near-identical copies that lived in nvpair-advertiser,
// nvpair-node-info, nvpair-errors, nvpair-workload-manager, and nvpair-cluster-manager. The
// mechanism (Windows unicast send, netmon re-announce, RFC-6762 goodbye,
// cache-flush bit, per-interface reply scoping) comes from the advertiser's
// original responder; the mutable TXT support (UpdateTXT, used when the
// clusterId changes and by the future consolidated registry) comes from
// cluster-manager's already-shipped dynamic-TXT variant.
package mdns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"nvpair-shared/netmon"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

const (
	mdnsPort = 5353
	// recordTTL matches what zeroconf advertises for non-A records (3200s)
	// for service-level records, but RFC 6762 §10 says A records SHOULD use
	// a TTL of 120s to account for IP address changes. We use the shorter
	// value uniformly — the cost of a slightly more chatty re-announce is
	// minimal and it keeps caches fresh.
	recordTTL = 120
	// reannounceEvery is how often we send unsolicited announcements after
	// the initial pair. RFC 6762 doesn't strictly require periodic
	// announcements, but it makes us robust to clients that miss the
	// startup announcement and never send a query (e.g. they're already
	// caching a stale entry).
	reannounceEvery = 60 * time.Second
	// cacheFlush is the top bit of qclass marking a record as "this is the
	// authoritative current value, flush any cached value with a different
	// rdata".
	cacheFlush uint16 = 1 << 15
)

var (
	mdnsGroupV4  = net.IPv4(224, 0, 0, 251)
	mdnsTargetV4 = &net.UDPAddr{IP: mdnsGroupV4, Port: mdnsPort}
)

// Responder advertises a single service instance and answers mDNS queries
// for it on every multicast-capable IPv4 interface.
type Responder struct {
	instance string
	service  string // e.g. "_nvpair-ollama._tcp"
	domain   string // e.g. "local"
	port     int
	hostName string // e.g. "myhost.local."

	serviceName  string // "_nvpair-ollama._tcp.local."
	instanceName string // "myhost._nvpair-ollama._tcp.local."

	// txtMu guards txt, which UpdateTXT can swap while Run is serving.
	// Callers that never call UpdateTXT simply read the value set at
	// construction.
	txtMu sync.RWMutex
	txt   []string

	// addrMu guards ifaceAddrs, which is refreshed live by watchAddrs as
	// interfaces come and go. It is treated as copy-on-write: readers take a
	// reference under RLock and use it lock-free, and a refresh swaps in a
	// brand-new map rather than mutating the existing one.
	addrMu sync.RWMutex
	// ifaceAddrs maps interface index to its IPv4 unicast addresses.
	ifaceAddrs map[int][]net.IP
}

// NewResponder builds a Responder for the given service instance. domain
// defaults to "local" if empty. The hostname is taken from os.Hostname().
// Returns an error if no multicast-capable IPv4 interface with at least one
// non-loopback address can be found.
func NewResponder(instance, service, domain string, port int, txt []string) (*Responder, error) {
	domain = strings.Trim(domain, ".")
	if domain == "" {
		domain = "local"
	}
	instance = strings.Trim(instance, ".")
	service = strings.Trim(service, ".")

	if instance == "" {
		return nil, errors.New("missing instance name")
	}
	if service == "" {
		return nil, errors.New("missing service")
	}
	if port == 0 {
		return nil, errors.New("missing port")
	}

	hostBase, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("hostname: %w", err)
	}
	hostBase = strings.Trim(hostBase, ".")
	hostName := hostBase + "." + domain + "."

	r := &Responder{
		instance:     instance,
		service:      service,
		domain:       domain,
		port:         port,
		txt:          append([]string(nil), txt...),
		hostName:     hostName,
		serviceName:  fmt.Sprintf("%s.%s.", service, domain),
		instanceName: fmt.Sprintf("%s.%s.%s.", instance, service, domain),
		// netmon's IfaceV4 applies the same filter we used to build by hand
		// (up + multicast + non-loopback interface, non-loopback IPv4), and
		// is what watchAddrs feeds back on every change.
		ifaceAddrs: netmon.Enumerate().IfaceV4,
	}
	if len(r.ifaceAddrs) == 0 {
		return nil, errors.New("no multicast-capable IPv4 interfaces with non-loopback addresses")
	}
	return r, nil
}

// ifaces returns the current interface→addresses map. The returned map is
// immutable (refreshes swap in a new one), so callers may use it lock-free
// after this returns.
func (r *Responder) ifaces() map[int][]net.IP {
	r.addrMu.RLock()
	defer r.addrMu.RUnlock()
	return r.ifaceAddrs
}

// UpdateTXT swaps the advertised TXT records and re-announces immediately. It
// is safe to call before or during Run (announcements are sent on freshly
// opened per-interface sockets, independent of Run's receive socket). Callers
// with a static TXT never need it.
func (r *Responder) UpdateTXT(txt []string) {
	r.txtMu.Lock()
	r.txt = append([]string(nil), txt...)
	r.txtMu.Unlock()
	r.sendAnnouncement()
}

// currentTXT returns a copy of the live TXT set, safe against a concurrent UpdateTXT.
func (r *Responder) currentTXT() []string {
	r.txtMu.RLock()
	defer r.txtMu.RUnlock()
	return append([]string(nil), r.txt...)
}

// watchAddrs keeps ifaceAddrs in sync with the host's live interface set and
// re-announces (with the cache-flush bit) whenever it changes, so peers drop a
// stale A record promptly after a VPN/dock change or sleep/wake re-IP. Note we
// don't re-join the multicast group on a newly appeared interface here, so
// queries arriving on it aren't answered until restart; the periodic and
// on-change announcements still publish our current addresses on it.
func (r *Responder) watchAddrs(ctx context.Context) {
	mon, err := netmon.Watch(ctx)
	if err != nil {
		slog.Warn("mdns: network monitor unavailable; advertised addresses are static", "err", err)
		return
	}
	for range mon.Subscribe() {
		next := mon.Snapshot().IfaceV4
		if len(next) == 0 {
			continue // keep the last good set rather than going dark
		}
		r.addrMu.Lock()
		changed := !ifaceAddrsEqual(r.ifaceAddrs, next)
		if changed {
			r.ifaceAddrs = next
		}
		r.addrMu.Unlock()
		if changed {
			slog.Debug("mdns: interface addresses changed, re-announcing")
			r.sendAnnouncement()
		}
	}
}

// ifaceAddrsEqual reports whether two interface→addresses maps hold the same
// addresses per interface (order-insensitive).
func ifaceAddrsEqual(a, b map[int][]net.IP) bool {
	if len(a) != len(b) {
		return false
	}
	for idx, av := range a {
		bv, ok := b[idx]
		if !ok || len(av) != len(bv) {
			return false
		}
		seen := make(map[string]int, len(av))
		for _, ip := range av {
			seen[ip.String()]++
		}
		for _, ip := range bv {
			seen[ip.String()]--
		}
		for _, n := range seen {
			if n != 0 {
				return false
			}
		}
	}
	return true
}

// Run is a blocking responder loop. It handles incoming queries, periodically
// re-announces the service, and on context cancellation sends a "goodbye"
// (TTL=0) before returning.
//
// The receive socket binds the real mDNS group 224.0.0.251:5353 with
// SO_REUSEADDR (see setReuseAddr) so multiple responders — and the browser's
// zeroconf socket — coexist on 5353 on the same host.
func (r *Responder) Run(ctx context.Context) error {
	lc := net.ListenConfig{Control: setReuseAddr}
	pktConn, err := lc.ListenPacket(ctx, "udp4", mdnsGroupV4.String()+":"+fmt.Sprint(mdnsPort))
	if err != nil {
		return fmt.Errorf("listen mdns: %w", err)
	}
	udpConn := pktConn.(*net.UDPConn)
	defer udpConn.Close()

	pc := ipv4.NewPacketConn(udpConn)
	// SetControlMessage is "not implemented" on Windows in golang.org/x/net.
	// On platforms where it works it gives us the receiving interface index
	// for each packet, which we use to scope our reply to the same iface.
	// On Windows the cm passed to ReadFrom is nil, so ifIndex stays 0 and
	// we fall back to replying on every multicast iface — slightly chattier
	// but functionally correct. So we treat any error from this as advisory.
	if err := pc.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		slog.Debug("mdns: control message not available, will reply on all interfaces", "reason", err)
	}

	var joined int
	for ifIdx := range r.ifaces() {
		ifi, err := net.InterfaceByIndex(ifIdx)
		if err != nil {
			continue
		}
		if err := pc.JoinGroup(ifi, &net.UDPAddr{IP: mdnsGroupV4}); err != nil {
			slog.Debug("mdns: JoinGroup failed", "iface", ifi.Name, "err", err)
			continue
		}
		joined++
	}
	if joined == 0 {
		return errors.New("failed to join mDNS group on any interface")
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r.announceLoop(ctx)
	}()

	go r.watchAddrs(ctx)

	go func() {
		<-ctx.Done()
		_ = udpConn.Close()
	}()

	buf := make([]byte, 65536)
	for {
		if ctx.Err() != nil {
			break
		}
		_ = udpConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, cm, from, err := pc.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) {
				break
			}
			slog.Debug("mdns: read error", "err", err)
			continue
		}
		var ifIndex int
		if cm != nil {
			ifIndex = cm.IfIndex
		}
		var msg dns.Msg
		if err := msg.Unpack(buf[:n]); err != nil {
			continue
		}
		r.handleQuery(&msg, ifIndex, from)
	}

	wg.Wait()
	r.sendGoodbye()
	return nil
}

// handleQuery answers an incoming query for our service, instance, or host name,
// replying unicast when a question set the QU (unicast-response) bit and
// multicast otherwise. It stays silent when nothing we own matched.
func (r *Responder) handleQuery(query *dns.Msg, ifIndex int, from net.Addr) {
	if query.Response || len(query.Question) == 0 {
		return
	}

	resp := new(dns.Msg)
	resp.MsgHdr.Response = true
	resp.MsgHdr.Authoritative = true
	resp.Compress = true

	// RFC 6762 §18.6: if any question has the QU bit set, the responder MAY
	// reply unicast. We track it across all questions and send the same way.
	unicast := false
	matched := false
	for _, q := range query.Question {
		if q.Qclass&cacheFlush != 0 {
			unicast = true
		}
		switch q.Name {
		case r.serviceName:
			if q.Qtype == dns.TypePTR || q.Qtype == dns.TypeANY {
				r.appendBrowseRRs(resp, ifIndex)
				matched = true
			}
		case r.instanceName:
			if q.Qtype == dns.TypeSRV || q.Qtype == dns.TypeTXT || q.Qtype == dns.TypeANY {
				r.appendLookupRRs(resp, ifIndex, false)
				matched = true
			}
		case r.hostName:
			if q.Qtype == dns.TypeA || q.Qtype == dns.TypeANY {
				resp.Answer = r.appendAddrRecords(resp.Answer, ifIndex, false)
				matched = true
			}
		}
	}
	if !matched {
		return
	}

	buf, err := resp.Pack()
	if err != nil {
		slog.Debug("mdns: pack response failed", "err", err)
		return
	}

	if unicast {
		r.sendUnicast(buf, ifIndex, from)
	} else {
		r.sendMulticast(buf, ifIndex)
	}
}

// appendBrowseRRs fills resp for a service browse (a PTR question): the PTR is
// the Answer, with SRV/TXT/A added as Extra so a browser can resolve us in one
// round-trip.
func (r *Responder) appendBrowseRRs(resp *dns.Msg, ifIndex int) {
	resp.Answer = append(resp.Answer, &dns.PTR{
		Hdr: dns.RR_Header{Name: r.serviceName, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: recordTTL},
		Ptr: r.instanceName,
	})
	resp.Extra = append(resp.Extra, r.srvRR(false), r.txtRR(false))
	resp.Extra = r.appendAddrRecords(resp.Extra, ifIndex, false)
}

// appendLookupRRs fills resp for a direct instance lookup (an SRV/TXT question):
// SRV+TXT as the Answer, A records as Extra.
func (r *Responder) appendLookupRRs(resp *dns.Msg, ifIndex int, flushCache bool) {
	resp.Answer = append(resp.Answer, r.srvRR(flushCache), r.txtRR(flushCache))
	resp.Extra = r.appendAddrRecords(resp.Extra, ifIndex, flushCache)
}

// srvRR builds the instance's SRV record. flushCache sets the cache-flush bit
// (RFC 6762 §10.2) — set on unsolicited announcements so peers replace a stale
// cached value, clear on reactive replies. The same convention applies to
// txtRR and appendAddrRecords.
func (r *Responder) srvRR(flushCache bool) *dns.SRV {
	class := uint16(dns.ClassINET)
	if flushCache {
		class |= cacheFlush
	}
	return &dns.SRV{
		Hdr:    dns.RR_Header{Name: r.instanceName, Rrtype: dns.TypeSRV, Class: class, Ttl: recordTTL},
		Port:   uint16(r.port),
		Target: r.hostName,
	}
}

// txtRR builds the instance's TXT record from the live set (which UpdateTXT may
// have swapped). See srvRR for flushCache.
func (r *Responder) txtRR(flushCache bool) *dns.TXT {
	class := uint16(dns.ClassINET)
	if flushCache {
		class |= cacheFlush
	}
	txt := r.currentTXT()
	if len(txt) == 0 {
		// An empty TXT record set is valid; emit a single empty string so the
		// record exists (some resolvers expect a TXT RR alongside SRV).
		txt = []string{""}
	}
	return &dns.TXT{
		Hdr: dns.RR_Header{Name: r.instanceName, Rrtype: dns.TypeTXT, Class: class, Ttl: recordTTL},
		Txt: txt,
	}
}

// appendAddrRecords appends A records for the addresses we advertise on ifIndex
// (see addrsForResponse). See srvRR for flushCache.
func (r *Responder) appendAddrRecords(list []dns.RR, ifIndex int, flushCache bool) []dns.RR {
	class := uint16(dns.ClassINET)
	if flushCache {
		class |= cacheFlush
	}
	for _, ip := range r.addrsForResponse(ifIndex) {
		list = append(list, &dns.A{
			Hdr: dns.RR_Header{Name: r.hostName, Rrtype: dns.TypeA, Class: class, Ttl: recordTTL},
			A:   ip,
		})
	}
	return list
}

// addrsForResponse returns the IPv4 addresses to advertise. When we know
// which interface received the query, advertise only addresses on that
// interface so we don't tell a peer about IPs they can't reach. When we
// don't know, fall back to advertising all of them.
func (r *Responder) addrsForResponse(ifIndex int) []net.IP {
	m := r.ifaces()
	if ifIndex != 0 {
		if a, ok := m[ifIndex]; ok {
			return a
		}
	}
	var all []net.IP
	for _, v := range m {
		all = append(all, v...)
	}
	return all
}

// announceLoop sends the two startup announcements RFC 6762 §8.3 recommends
// (~1s apart, so a listener that missed the first still hears us), then
// re-announces every reannounceEvery until ctx is cancelled.
func (r *Responder) announceLoop(ctx context.Context) {
	r.sendAnnouncement()
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Second):
	}
	r.sendAnnouncement()

	ticker := time.NewTicker(reannounceEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.sendAnnouncement()
		}
	}
}

// sendAnnouncement multicasts an unsolicited PTR+SRV+TXT+A (with the cache-flush
// bit) on every interface, so peers learn or refresh our record without querying.
func (r *Responder) sendAnnouncement() {
	for ifIdx := range r.ifaces() {
		resp := new(dns.Msg)
		resp.MsgHdr.Response = true
		resp.MsgHdr.Authoritative = true
		resp.Compress = true
		resp.Answer = append(resp.Answer,
			&dns.PTR{
				Hdr: dns.RR_Header{Name: r.serviceName, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: recordTTL},
				Ptr: r.instanceName,
			},
			r.srvRR(true),
			r.txtRR(true),
		)
		resp.Answer = r.appendAddrRecords(resp.Answer, ifIdx, true)
		buf, err := resp.Pack()
		if err != nil {
			continue
		}
		r.sendOnInterface(buf, ifIdx, mdnsTargetV4)
	}
}

// sendGoodbye multicasts a TTL=0 PTR on every interface so peers evict us at
// once on shutdown instead of waiting out the record TTL.
func (r *Responder) sendGoodbye() {
	for ifIdx := range r.ifaces() {
		resp := new(dns.Msg)
		resp.MsgHdr.Response = true
		resp.MsgHdr.Authoritative = true
		resp.Answer = append(resp.Answer, &dns.PTR{
			Hdr: dns.RR_Header{Name: r.serviceName, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 0},
			Ptr: r.instanceName,
		})
		buf, err := resp.Pack()
		if err != nil {
			continue
		}
		r.sendOnInterface(buf, ifIdx, mdnsTargetV4)
	}
}

// sendMulticast sends buf to the mDNS group — scoped to the interface the query
// arrived on when known (ifIndex != 0), otherwise on every interface.
func (r *Responder) sendMulticast(buf []byte, ifIndex int) {
	m := r.ifaces()
	if ifIndex != 0 {
		if _, ok := m[ifIndex]; ok {
			r.sendOnInterface(buf, ifIndex, mdnsTargetV4)
			return
		}
	}
	for idx := range m {
		r.sendOnInterface(buf, idx, mdnsTargetV4)
	}
}

// sendUnicast sends buf straight back to the querier, preferring the receiving
// interface and otherwise stopping at the first interface that accepts it.
func (r *Responder) sendUnicast(buf []byte, ifIndex int, to net.Addr) {
	udpAddr, ok := to.(*net.UDPAddr)
	if !ok {
		return
	}
	m := r.ifaces()
	if ifIndex != 0 {
		if _, ok := m[ifIndex]; ok {
			r.sendOnInterface(buf, ifIndex, udpAddr)
			return
		}
	}
	for idx := range m {
		if r.sendOnInterface(buf, idx, udpAddr) == nil {
			return
		}
	}
}

// sendOnInterface is the core of the Windows send workaround: it transmits buf
// from a fresh unicast-bound socket on the given interface (setting the
// multicast interface + TTL for group targets), never from the multicast-bound
// receive socket that Windows refuses to send from.
func (r *Responder) sendOnInterface(buf []byte, ifIndex int, target *net.UDPAddr) error {
	addrs, ok := r.ifaces()[ifIndex]
	if !ok || len(addrs) == 0 {
		return errors.New("no addresses on interface")
	}
	src := addrs[0]
	ifi, err := net.InterfaceByIndex(ifIndex)
	if err != nil {
		return err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: src, Port: 0})
	if err != nil {
		slog.Debug("mdns: bind failed", "iface", ifi.Name, "ip", src.String(), "err", err)
		return err
	}
	defer conn.Close()
	if target.IP.IsMulticast() {
		pc := ipv4.NewPacketConn(conn)
		_ = pc.SetMulticastInterface(ifi)
		_ = pc.SetMulticastTTL(255)
	}
	if _, err := conn.WriteToUDP(buf, target); err != nil {
		slog.Debug("mdns: write failed", "iface", ifi.Name, "ip", src.String(), "target", target.String(), "err", err)
		return err
	}
	return nil
}
