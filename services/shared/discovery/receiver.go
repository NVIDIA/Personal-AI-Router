// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"net"
	"strings"
	"sync"
	"time"

	"nvpair-shared/netmon"

	"github.com/miekg/dns"
	"golang.org/x/net/ipv4"
)

const (
	// mdnsPort is the mDNS UDP port.
	mdnsPort = 5353
	// readDeadline bounds one receive read so the loop can observe shutdown
	// between packets; it matches the responder's read cadence.
	readDeadline = 500 * time.Millisecond
)

// mdnsGroupV4 is the mDNS IPv4 multicast group the browser joins to receive.
var mdnsGroupV4 = net.IPv4(224, 0, 0, 251)

// receiver is a single long-lived mDNS receive socket: bound to the multicast
// wildcard on 5353, joined to the mDNS group on every multicast interface, and
// continuously decoding incoming DNS-SD records into Node values that the browser
// drains on each scan.
//
// It exists because the browser previously built a fresh grandcat/zeroconf
// resolver — two UDP sockets plus a JoinGroup on every interface — on every scan
// for the life of the process. That create/join/teardown churn degrades the macOS
// network stack over time. One stable socket, joined once per interface and
// re-joined only when the interface set changes, removes the churn while
// receiving exactly the packets the per-scan socket did.
//
// It binds the wildcard (224.0.0.0:5353) rather than the group address so it
// shares 5353 with the system mDNS responder and the PAIR responder without a
// bind race; joining the group on each interface is what actually receives.
//
// It is deliberately IPv4-only. The PAIR responder (shared/mdns) is IPv4-only,
// so every node it advertises is reachable over IPv4; the grandcat/zeroconf
// resolver it replaced opened a second IPv6 socket that only ever carried
// non-PAIR traffic. One IPv4 socket therefore receives exactly the PAIR records
// the two-socket resolver did, at half the socket churn.
type receiver struct {
	service string
	domain  string

	mu     sync.Mutex
	conn   *net.UDPConn
	pconn  *ipv4.PacketConn
	joined map[int]bool
	buf    []Node
	stop   chan struct{}
}

// mdnsWildcardV4 is the address the receive socket binds to: the multicast
// wildcard 224.0.0.0 (not 0.0.0.0). Binding the multicast network rather than the
// unicast wildcard is what lets it coexist with the system mDNS responder and the
// PAIR responder on 5353 without SO_REUSEPORT — exactly the bind the grandcat/
// zeroconf resolver it replaces used, which is why the per-scan socket received
// the responses it did.
var mdnsWildcardV4 = net.IPv4(224, 0, 0, 0)

// NewReceiver binds the long-lived receive socket and joins the mDNS group on
// every interface in ifaces. It returns an error only if the socket cannot be
// bound; interfaces that fail to join are skipped (a wedged adapter must not
// darken the rest).
func NewReceiver(service, domain string, ifaces map[int][]net.IP) (*receiver, error) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: mdnsWildcardV4, Port: mdnsPort})
	if err != nil {
		return nil, err
	}
	r := &receiver{
		service: service,
		domain:  domain,
		conn:    conn,
		pconn:   ipv4.NewPacketConn(conn),
		joined:  make(map[int]bool),
		stop:    make(chan struct{}),
	}
	r.joinedOn(ifaces)
	return r, nil
}

// serviceName returns the fully-qualified service type (e.g. "_nvpair-node._tcp.local.").
func (r *receiver) serviceName() string {
	domain := r.domain
	if domain == "" {
		domain = "local"
	}
	return strings.Trim(r.service, ".") + "." + strings.Trim(domain, ".") + "."
}

// joinedOn reconciles the set of interfaces the socket is a member of the mDNS
// group on with ifaces: it joins each new one and leaves each gone one, without
// recreating the socket. JoinGroup/LeaveGroup are quick syscalls; holding mu for
// the refresh only briefly delays a concurrent drain, which is acceptable.
func (r *receiver) joinedOn(ifaces map[int][]net.IP) {
	group := &net.UDPAddr{IP: mdnsGroupV4}
	r.mu.Lock()
	defer r.mu.Unlock()
	for idx := range r.joined {
		if _, ok := ifaces[idx]; !ok {
			leaveGroup(r.pconn, idx, group)
			delete(r.joined, idx)
		}
	}
	for idx := range ifaces {
		if r.joined[idx] {
			continue
		}
		if joinGroup(r.pconn, idx, group) {
			r.joined[idx] = true
		}
	}
}

// refresh reconciles the set of joined interfaces with ifaces, joining new ones
// and leaving gone ones, without recreating the socket.
func (r *receiver) refresh(ifaces map[int][]net.IP) {
	r.joinedOn(ifaces)
}

// drain returns and clears the nodes decoded since the last drain.
func (r *receiver) drain() []Node {
	r.mu.Lock()
	out := r.buf
	r.buf = nil
	r.mu.Unlock()
	return out
}

// readLoop decodes received DNS-SD messages into Node values and accumulates them
// until drained. It runs until Stop.
func (r *receiver) readLoop(ctx context.Context) {
	// Capture the socket handles under the lock, once. Stop() writes r.conn under
	// the same lock, so this read is synchronized with it; holding locals keeps the
	// loop reading a stable handle, and a closed socket surfaces as a read error
	// (observed below) rather than a nil dereference if the deadline and the close
	// land on the same read.
	r.mu.Lock()
	conn := r.conn
	pc := r.pconn
	r.mu.Unlock()
	buf := make([]byte, 65536)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(readDeadline))
		n, _, _, err := pc.ReadFrom(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				// Idle: no packet within the deadline; loop back and re-read. The
				// deadline is what keeps this loop able to observe shutdown.
				continue
			}
			// A non-timeout read error means the socket closed (Stop) or faulted.
			// Stop closes r.stop before the socket, so a closed socket means
			// shutdown; any other fault is tolerated and retried rather than
			// darkening the whole browser.
			select {
			case <-r.stop:
				return
			case <-ctx.Done():
				return
			default:
				continue
			}
		}
		if n <= 0 {
			continue
		}
		var msg dns.Msg
		if err := msg.Unpack(buf[:n]); err != nil {
			continue
		}
		nodes := r.decode(&msg)
		if len(nodes) == 0 {
			continue
		}
		r.mu.Lock()
		r.buf = append(r.buf, nodes...)
		r.mu.Unlock()
	}
}

// Stop closes the socket (unblocking the read loop) and leaves every group. It is
// safe to call more than once.
func (r *receiver) Stop() {
	r.mu.Lock()
	select {
	case <-r.stop:
		r.mu.Unlock()
		return
	default:
		close(r.stop)
	}
	conn := r.conn
	r.conn = nil
	idxs := make([]int, 0, len(r.joined))
	for idx := range r.joined {
		idxs = append(idxs, idx)
	}
	r.joined = make(map[int]bool)
	r.mu.Unlock()
	group := &net.UDPAddr{IP: mdnsGroupV4}
	for _, idx := range idxs {
		leaveGroup(r.pconn, idx, group)
	}
	if conn != nil {
		_ = conn.Close()
	}
}

// joinGroup joins the mDNS group on the interface idx and reports success. A
// missing or wedged interface is skipped rather than failing the whole refresh.
func joinGroup(pc *ipv4.PacketConn, idx int, group *net.UDPAddr) bool {
	ifi, err := net.InterfaceByIndex(idx)
	if err != nil {
		return false
	}
	return pc.JoinGroup(ifi, group) == nil
}

// leaveGroup leaves the mDNS group on the interface idx; a missing interface is a
// no-op.
func leaveGroup(pc *ipv4.PacketConn, idx int, group *net.UDPAddr) {
	ifi, err := net.InterfaceByIndex(idx)
	if err != nil {
		return
	}
	_ = pc.LeaveGroup(ifi, group)
}

// decode turns a single DNS-SD message into the Node values it describes for this
// browser's service type, following the same association rules as the
// grandcat/zeroconf client it replaces: a PTR names the instance, an SRV gives the
// instance's host and port, a TXT gives its records, and A/AAAA (keyed by the
// instance's host) give its addresses. A record with a TTL of 0 is a goodbye and
// is dropped. A node is emitted once it has both an SRV and at least one address.
func (r *receiver) decode(msg *dns.Msg) []Node {
	type inst struct {
		host   string
		port   int
		txt    []string
		addrs  []string
		ttl    uint32
		hasSRV bool
	}
	byName := make(map[string]*inst)
	get := func(name string) *inst {
		if v, ok := byName[name]; ok {
			return v
		}
		v := &inst{}
		byName[name] = v
		return v
	}
	service := r.serviceName()
	for _, rr := range append(append(msg.Answer, msg.Ns...), msg.Extra...) {
		switch v := rr.(type) {
		case *dns.PTR:
			if v.Hdr.Name == service {
				get(v.Ptr)
			}
		case *dns.SRV:
			if strings.HasSuffix(v.Hdr.Name, service) {
				i := get(v.Hdr.Name)
				i.host = v.Target
				i.port = int(v.Port)
				i.ttl = v.Hdr.Ttl
				i.hasSRV = true
			}
		case *dns.TXT:
			if strings.HasSuffix(v.Hdr.Name, service) {
				i := get(v.Hdr.Name)
				i.txt = v.Txt
				i.ttl = v.Hdr.Ttl
			}
		case *dns.A:
			for _, i := range byName {
				if i.host != "" && i.host == v.Hdr.Name {
					i.addrs = append(i.addrs, v.A.String())
				}
			}
		case *dns.AAAA:
			if isLinkLocal(v.AAAA) {
				continue
			}
			for _, i := range byName {
				if i.host != "" && i.host == v.Hdr.Name {
					i.addrs = append(i.addrs, v.AAAA.String())
				}
			}
		}
	}

	var out []Node
	for name, i := range byName {
		if !i.hasSRV || i.ttl == 0 || len(i.addrs) == 0 {
			continue
		}
		instance := strings.TrimSuffix(name, service)
		node := Node{
			ID:        strings.Trim(instance, "."),
			Host:      i.host,
			Port:      i.port,
			Addresses: i.addrs,
			TXT:       i.txt,
		}
		out = append(out, node)
	}
	return out
}

// monitorIfaces refreshes the receiver's joined interface set whenever the host's
// addressing changes, driven by netmon. It returns on ctx cancellation.
func (r *receiver) monitorIfaces(ctx context.Context) {
	mon, err := netmon.Watch(ctx)
	if err != nil {
		// Without a live monitor the set is static; the socket still receives on
		// whatever it joined at startup.
		<-ctx.Done()
		return
	}
	for range mon.Subscribe() {
		r.refresh(mon.Snapshot().IfaceV4)
	}
}
