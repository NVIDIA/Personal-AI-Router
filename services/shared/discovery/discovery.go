// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package discovery provides a single mDNS service browser that six services
// (nvpair-node-scanner, nvpair-errors, ollama-proxy, lmstudio-proxy,
// nvpair-workload-manager, nvpair-cluster-manager) each carried a near-identical copy
// of (the mDNS dedup).
//
// The core is a scan-and-diff state machine: each scan transmits the PTR query
// from a long-lived per-interface send-socket pool (one unicast-bound socket per
// interface, reused every scan — this replaces the per-scan send socket, and the
// pool is the cross-platform stand-in for the grandcat/zeroconf resolver it
// supersedes) and reconciles the responses a single long-lived receive socket has
// gathered, against the known-node map. Both sockets open once for the life of the
// run and are reused every scan rather than opened fresh each time, which is what
// degraded the macOS network stack over time. Address/TXT comparison is
// order-insensitive so a multi-homed node whose records come back reordered doesn't
// churn a spurious "updated".
//
// The per-service variations are expressed as functional options rather than
// forks:
//   - WithInterval / WithScanTimeout — cluster-manager browses at 30s.
//   - WithMissThreshold — consecutive misses before eviction (default
//     missThresholdDefault).
//   - WithNoEviction — accumulate-only, never evict (cluster-manager).
//   - WithLivenessProbe — TCP-probe a threshold-missed node before evicting it;
//     if it still answers, keep it (the proxies' anti-flap guard).
//   - WithSelfFilter — drop our own advertisement from results (workload-manager).
//   - WithKeyFunc — key the node map by something other than the instance name
//     (cluster-manager keys by uuid= so two hosts sharing an instance name but
//     distinct UUIDs don't collide).
//
// Two consumption shapes are supported over the same core:
//   - Run(ctx, events) — push: a background loop that emits Discovered/Updated/
//     Removed events (scanner, errors, proxies). events may be nil for a
//     consumer that only wants the map maintained and queries it via Nodes()
//     (cluster-manager).
//   - Poll(ctx) — pull: one scan+reconcile that returns the current snapshot
//     (workload-manager's on-demand PeerSource).
package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"nvpair-shared/mcast"

	"github.com/miekg/dns"
)

// Event types emitted by Run.
const (
	Discovered = "discovered"
	Updated    = "updated"
	Removed    = "removed"
)

// probeConcurrency bounds how many liveness probes run at once. A probe against
// an address that silently drops SYNs (a firewalled or unroutable peer) burns
// its full dial timeout, so probing serially lets a few such peers push one
// reconcile past the scan interval and delay every other node's eviction or
// recovery.
const probeConcurrency = 8

// missThresholdDefault is how many consecutive scans a node must be absent from
// before it becomes eligible for eviction. At the default 5s interval that is a
// full minute of continuous silence.
//
// It was 15 seconds, which is not enough. A node saturated by its own inference
// load starves its control plane of CPU: mDNS responses stop going out and
// liveness probes stop being answered, for as long as the load lasts. Fifteen
// seconds of that was read as a departure, so a busy node was dropped from the
// cluster and its in-flight work failed with it — while it was still serving
// requests.
//
// The cost of waiting longer is that a genuinely departed node lingers for
// another three quarters of a minute. That is cheap: routing skips a node that
// will not answer, so the visible effect is one request that fails and retries.
// The cost of evicting too early is losing a working node and everything running
// on it, which is not.
//
// Waiting longer does not mean looking less often. The scan interval is
// unchanged, the node-info telemetry sweep still runs every couple of seconds,
// and probeAfterMisses starts the liveness probe a quarter of the way in — so by
// the time this threshold is reached the node has been asked repeatedly and has
// declined every time, rather than having been ignored for a minute and then
// judged on one dial.
//
// This is the coarse guard. The precise one is the node's own traffic — see
// nvpair-node-scanner's activityFreshness, which keeps a node that is currently
// streaming inference regardless of what its probes say.
const missThresholdDefault = 12

// probeAfterMisses is how many consecutive misses a node accumulates before the
// liveness probe starts asking about it — a quarter of the default threshold, so
// about fifteen seconds.
//
// Probing and giving up are separate decisions. The threshold above is when we
// stop believing a node is there; this is when we start checking, and it stays
// short so the minute is spent gathering evidence instead of waiting. A single
// answer resets the miss counter and the node is fully recovered, which is the
// common case for a peer whose multicast is being dropped while it is otherwise
// perfectly reachable. It also means eviction rests on a run of failed probes
// rather than the one that happens to land on the deadline.
//
// The added cost is one TCP connect per missing node per scan. It is independent
// of node-info telemetry's failure backoff because it answers a different
// question — whether an mDNS-missing node should be evicted — and it is never
// aimed at an inference port.
const probeAfterMisses = 3

// emptyScanGrace bounds how many consecutive scans that lose EVERY known node at
// once are treated as this browser's own failure rather than as evidence about
// the fleet.
//
// A scan returning nothing while several nodes are known says much less about
// those nodes than a scan returning some of them and not others. Independent
// machines do not leave a network in the same 5-second window; a process too
// starved to drain its multicast socket inside scanTimeout, however, hears
// silence from all of them at once. Counting that as a miss against every node
// is what let one machine saturated by its own inference load evict its whole
// cluster in a single reconcile — idle peers that were never sent any work
// included.
//
// The strength of the signal is what scales with the node count, so the
// suppression applies only when more than one node is known (see reconcile).
// With a single known node there is no "all at once" to be suspicious of: losing
// it looks the same whether it left or we stopped listening, and the ordinary
// miss threshold plus the liveness probe already cover that.
//
// It is a grace and not a veto because a whole fleet CAN genuinely go — a
// switch losing power, or the last peers shutting down together — and those
// records still have to age out. Past the bound, empty scans count normally.
const emptyScanGrace = 6

// Node is a single discovered service instance. ID is the mDNS instance name.
type Node struct {
	ID        string
	Host      string
	Port      int
	Addresses []string
	TXT       []string
}

// Event is a change to the discovered set.
type Event struct {
	Type string // Discovered, Updated, or Removed
	Node Node
}

type options struct {
	interval      time.Duration
	scanTimeout   time.Duration
	missThreshold int
	noEvict       bool
	liveness      func(Node) bool
	selfInstance  string
	keyFunc       func(Node) string
	warnCollision bool
}

// Option configures a Browser.
type Option func(*options)

// WithInterval sets the scan cadence for Run (default 5s).
func WithInterval(d time.Duration) Option { return func(o *options) { o.interval = d } }

// WithScanTimeout bounds how long each browse waits for responses (default 3s).
func WithScanTimeout(d time.Duration) Option { return func(o *options) { o.scanTimeout = d } }

// WithMissThreshold sets how many consecutive scans a node must be absent from
// before it is eligible for eviction (default missThresholdDefault).
func WithMissThreshold(n int) Option { return func(o *options) { o.missThreshold = n } }

// WithNoEviction makes the browser accumulate-only: a node is never removed once
// seen. For a consumer that wants a departed peer to keep resolving (e.g. an
// invite flow) rather than being evicted at the miss threshold.
func WithNoEviction() Option { return func(o *options) { o.noEvict = true } }

// WithLivenessProbe supplies a reachability check run against a threshold-missed
// node before it is evicted; returning true keeps the node (its miss counter is
// reset). This is the proxies' TCP-probe-before-evict guard: an mDNS miss is not
// proof a node is gone. Probes run outside the browser lock and up to
// probeConcurrency at a time, so fn must be safe for concurrent use.
func WithLivenessProbe(fn func(Node) bool) Option { return func(o *options) { o.liveness = fn } }

// WithSelfFilter excludes entries whose instance name matches selfInstance —
// our own advertisement looping back (workload-manager).
func WithSelfFilter(selfInstance string) Option {
	return func(o *options) { o.selfInstance = selfInstance }
}

// WithKeyFunc keys the node map by fn(node) instead of the instance name. Return
// "" to fall back to the instance name for that node. cluster-manager keys by
// uuid= so two hosts sharing an instance name but distinct UUIDs don't collide.
func WithKeyFunc(fn func(Node) string) Option { return func(o *options) { o.keyFunc = fn } }

// Browser maintains a reconciled set of discovered nodes for one service type.
type Browser struct {
	service string
	domain  string
	opt     options

	mu     sync.RWMutex
	nodes  map[string]Node
	misses map[string]int
	// emptyScans counts consecutive scans that returned nothing while nodes were
	// known, which emptyScanGrace bounds. Any scan that sees a single node clears
	// it: one answer proves the receive path works, so the next empty scan is
	// news rather than a continuation.
	emptyScans int
	// sendMisses counts each interface's consecutive failed mDNS sends, reset on
	// any success. Only a run of them is reported as a failure: sends happen
	// every scan, and treating an isolated blip as evidence would let address
	// selection move a host's canonical address for no reason.
	sendMisses map[string]int

	// sender is the long-lived per-interface send-socket pool the browse query is
	// transmitted from (see sendQuery). It holds one socket per interface
	// and reuses it for every transmission, so a host whose packet filter
	// inspects each new flow keeps a stable set of flows rather than a fresh one
	// per scan. It is created lazily on the first scan and refreshed only when
	// the host's interface set changes. Guarded by mu alongside the map it feeds.
	sender *mcast.Sender

	// recv is the long-lived receive socket (see receiver.go) that browses draw
	// their responses from, shared across all scans instead of a fresh socket per
	// scan. It is set up in Run and closed on shutdown; nil in tests that override
	// browseFunc. Guarded by mu.
	recv *receiver

	// browseFunc performs one browse cycle and returns the seen set keyed the
	// same way as the node map. Defaults to the real browse; overridden in tests
	// to exercise reconciliation deterministically.
	browseFunc func(context.Context) map[string]Node
}

// New builds a Browser for service (e.g. "_nvpair-node._tcp") in domain
// (default "local").
func New(service, domain string, opts ...Option) *Browser {
	if strings.Trim(domain, ".") == "" {
		domain = "local"
	}
	b := &Browser{
		service: service,
		domain:  domain,
		opt: options{
			interval:      5 * time.Second,
			scanTimeout:   3 * time.Second,
			missThreshold: missThresholdDefault,
			warnCollision: true,
		},
		nodes:      make(map[string]Node),
		misses:     make(map[string]int),
		sendMisses: make(map[string]int),
	}
	for _, o := range opts {
		o(&b.opt)
	}
	b.browseFunc = b.browse
	return b
}

// key returns the map key for a node — the configured key func, or the instance
// name when no func is set or it returns "".
func (b *Browser) key(n Node) string {
	if b.opt.keyFunc != nil {
		if k := b.opt.keyFunc(n); k != "" {
			return k
		}
	}
	return n.ID
}

// Seed inserts known nodes into the browser's map without a live scan — for
// priming from a cache (e.g. subscribe-cached peer locations) or in tests. An
// existing entry with the same key is replaced and its miss counter cleared.
func (b *Browser) Seed(nodes ...Node) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, n := range nodes {
		k := b.key(n)
		b.nodes[k] = n
		delete(b.misses, k)
	}
}

// sendFailureThreshold is how many consecutive failed sends an interface must
// accumulate before SendFailures reports it. At the default scan cadence that is
// roughly fifteen seconds of sustained failure: long enough that a transient
// error during a link renegotiation is ignored, short enough that a genuinely
// dead interface stops being advertised promptly.
const sendFailureThreshold = 3

// recordSendOutcomes folds one scan's per-interface send results into the
// consecutive-failure counters. An interface that succeeds is cleared outright,
// so recovery is immediate while failure has to persist.
func (b *Browser) recordSendOutcomes(outcomes map[string]bool) {
	if len(outcomes) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for iface, ok := range outcomes {
		if ok {
			delete(b.sendMisses, iface)
			continue
		}
		b.sendMisses[iface]++
	}
	// Drop counters for interfaces this scan did not attempt. A VPN adapter that
	// comes and goes, or a container's veth pair, would otherwise leave a run of
	// failures behind that keeps suppressing an address on an interface no longer
	// present — and the map would grow for the life of the process. Absence means
	// no evidence either way, which is what SendFailures documents.
	for iface := range b.sendMisses {
		if _, attempted := outcomes[iface]; !attempted {
			delete(b.sendMisses, iface)
		}
	}
}

// SendFailures reports which interfaces have failed to send for long enough to
// be treated as having no usable route out. It feeds address selection, which
// must not publish an address on an interface that cannot reach the network.
//
// Interfaces absent from the map are not asserted to work — an interface that has
// never been attempted simply has no evidence either way.
func (b *Browser) SendFailures() map[string]bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var out map[string]bool
	for iface, misses := range b.sendMisses {
		if misses < sendFailureThreshold {
			continue
		}
		if out == nil {
			out = make(map[string]bool)
		}
		out[iface] = true
	}
	return out
}

// Nodes returns a snapshot of the currently known nodes.
func (b *Browser) Nodes() []Node {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]Node, 0, len(b.nodes))
	for _, n := range b.nodes {
		out = append(out, n)
	}
	return out
}

// Run performs periodic scans until ctx is cancelled. When events is non-nil it
// receives Discovered/Updated/Removed events and is closed on return; pass nil
// to only maintain the map (query it via Nodes()).
//
// It opens the shared mDNS receive socket once for the lifetime of the run. That
// socket — not a fresh resolver per scan — is what the scans draw their responses
// from, which is what keeps a host whose kernel reaps socket state slowly from
// degrading over time.
func (b *Browser) Run(ctx context.Context, events chan<- Event) {
	if events != nil {
		defer close(events)
	}
	b.startReceive(ctx)
	b.scanEmit(ctx, events)

	ticker := time.NewTicker(b.opt.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.scanEmit(ctx, events)
		}
	}
}

// startReceive opens the browser's long-lived receive socket for the life of this
// run and starts the goroutines that feed it (the read loop that decodes arriving
// records, and the interface monitor that keeps the multicast memberships current).
// It is called once from Run — the only production consumer — and is idempotent.
// A browser driven purely through Poll never needs it, and a test that overrides
// browseFunc leaves recv nil, so no real socket is opened there.
//
// The socket is created outside the lock (a bind + group joins is a few system
// calls), then stored under it; a concurrent caller that got there first wins and
// the redundant socket is closed. If the socket cannot be bound (port in use, no
// multicast interface) it logs and leaves recv nil: browse then reports an empty
// scan, and the responder side is unaffected. On ctx cancellation the socket is
// closed and its multicast memberships left.
func (b *Browser) startReceive(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	ifaces, _ := enumerateIfaces()
	recv, err := NewReceiver(b.service, b.domain, ifaces)
	if err != nil {
		slog.Warn("mDNS browser could not open its receive socket; discovery will be empty",
			"service", b.service, "err", err)
		return
	}
	b.mu.Lock()
	if b.recv != nil {
		b.mu.Unlock()
		recv.Stop()
		return
	}
	b.recv = recv
	b.mu.Unlock()

	go recv.readLoop(ctx)
	go recv.monitorIfaces(ctx)
	go func() {
		<-ctx.Done()
		recv.Stop()
	}()
}

// Poll performs one scan+reconcile and returns the current snapshot. It's the
// pull-model entry point (workload-manager's PeerSource), an alternative to Run.
func (b *Browser) Poll(ctx context.Context) []Node {
	if ctx.Err() == nil {
		b.reconcile(b.browseFunc(ctx))
	}
	return b.Nodes()
}

// scanEmit runs one browse+reconcile and, when events is non-nil, forwards the
// resulting change events (stopping early if ctx is cancelled).
func (b *Browser) scanEmit(ctx context.Context, events chan<- Event) {
	if ctx.Err() != nil {
		return
	}
	pending := b.reconcile(b.browseFunc(ctx))
	if events == nil {
		return
	}
	for _, e := range pending {
		select {
		case events <- e:
		case <-ctx.Done():
			return
		}
	}
}

// reconcile folds a fresh scan's seen set into the known map and returns the
// resulting events. With a liveness probe configured, threshold-missed nodes are
// probed outside the lock before eviction.
func (b *Browser) reconcile(seen map[string]Node) []Event {
	b.mu.Lock()
	var pending []Event

	for k, node := range seen {
		existing, exists := b.nodes[k]
		if !exists {
			pending = append(pending, Event{Type: Discovered, Node: node})
		} else {
			if b.opt.warnCollision {
				if oldU, newU := uuidFromTXT(existing.TXT), uuidFromTXT(node.TXT); oldU != "" && newU != "" && oldU != newU {
					slog.Warn("mDNS node identity changed for the same key — possible hostname collision or host replacement",
						"service", b.service, "node_id", node.ID, "old_uuid", oldU, "new_uuid", newU)
				}
			}
			if !nodeEqual(existing, node) {
				pending = append(pending, Event{Type: Updated, Node: node})
			}
		}
		b.nodes[k] = node
		delete(b.misses, k)
	}

	if b.opt.noEvict {
		b.mu.Unlock()
		return pending
	}

	// Losing several independent nodes in one scan is treated as a failure of this
	// browser before it is treated as news about the fleet — see emptyScanGrace.
	// Bounded, so a fleet that really has gone still ages out.
	if len(seen) == 0 && len(b.nodes) > 1 {
		b.emptyScans++
		if b.emptyScans <= emptyScanGrace {
			if b.emptyScans == 1 {
				slog.Warn("mDNS scan returned no nodes at all; treating it as a local receive failure rather than a departure",
					"service", b.service, "known_nodes", len(b.nodes))
			}
			b.mu.Unlock()
			return pending
		}
		if b.emptyScans == emptyScanGrace+1 {
			slog.Warn("mDNS scans have returned nothing for too long to keep excusing; nodes will now age out",
				"service", b.service, "empty_scans", b.emptyScans, "known_nodes", len(b.nodes))
		}
	} else {
		b.emptyScans = 0
	}

	// Nodes absent from this scan: bump the miss count, then either queue a
	// liveness probe (from probeAfterMisses onward, well before the threshold) or,
	// with no probe configured, evict at the threshold.
	var probe []Node
	probeAt := b.probeAfter()
	for k, node := range b.nodes {
		if _, ok := seen[k]; ok {
			continue
		}
		b.misses[k]++
		if b.opt.liveness == nil {
			if b.misses[k] >= b.opt.missThreshold {
				pending = append(pending, Event{Type: Removed, Node: node})
				delete(b.nodes, k)
				delete(b.misses, k)
			}
			continue
		}
		if b.misses[k] >= probeAt {
			probe = append(probe, node)
		}
	}
	b.mu.Unlock()

	// Probe threshold-missed nodes outside the lock so a slow/dead peer can't
	// stall readers; the node stays in the map (routable) for the probe.
	for _, v := range b.probeLiveness(probe) {
		k := b.key(v.node)
		if v.alive {
			b.mu.Lock()
			if _, ok := b.nodes[k]; ok {
				b.misses[k] = 0
			}
			b.mu.Unlock()
			continue
		}
		b.mu.Lock()
		if cur, ok := b.nodes[k]; ok && b.misses[k] >= b.opt.missThreshold {
			pending = append(pending, Event{Type: Removed, Node: cur})
			delete(b.nodes, k)
			delete(b.misses, k)
		}
		b.mu.Unlock()
	}

	return pending
}

// probeAfter is the miss count from which a node is probed on every scan. It is
// probeAfterMisses, unless a browser configured a threshold shorter than that —
// a probe that first ran after the point of no return could never rescue
// anything, so it is clamped to the threshold instead.
func (b *Browser) probeAfter() int {
	if b.opt.missThreshold < probeAfterMisses {
		return b.opt.missThreshold
	}
	return probeAfterMisses
}

// verdict pairs a threshold-missed node with its liveness answer.
type verdict struct {
	node  Node
	alive bool
}

// probeLiveness runs the configured probe against every node, at most
// probeConcurrency at a time, and returns one verdict per node in input order so
// eviction stays deterministic regardless of which probe finishes first.
func (b *Browser) probeLiveness(nodes []Node) []verdict {
	verdicts := make([]verdict, len(nodes))
	slots := make(chan struct{}, probeConcurrency)
	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Add(1)
		slots <- struct{}{}
		go func(i int, n Node) {
			defer wg.Done()
			defer func() { <-slots }()
			verdicts[i] = verdict{node: n, alive: b.opt.liveness(n)}
		}(i, node)
	}
	wg.Wait()
	return verdicts
}

// browse performs one real mDNS browse cycle: it transmits the PTR query from the
// long-lived per-interface send pool and collects the responses the shared receive
// socket has gathered since the previous scan, returning the seen set keyed like
// the node map.
//
// It draws on the same two stable sockets for the whole run rather than opening a
// fresh grandcat/zeroconf resolver per scan — two UDP sockets and a JoinGroup on
// every interface each time — which is what degraded the macOS network stack over
// time. The receive socket is started in Run; a browser that never ran (or whose
// socket could not be bound) reports an empty scan.
func (b *Browser) browse(ctx context.Context) map[string]Node {
	seen := make(map[string]Node)

	b.mu.Lock()
	recv := b.recv
	b.mu.Unlock()
	if recv == nil {
		return seen
	}

	// Transmit the query from the per-interface send pool. This is the canonical
	// (and only) copy on every platform: the shared receive socket above collects
	// the responses. A send that fails at the socket is also the cheapest evidence
	// that the kernel has no usable route out of that interface, which address
	// selection consumes via recordSendOutcomes / SendFailures.
	b.recordSendOutcomes(b.sendQuery())

	scanCtx, cancel := context.WithTimeout(ctx, b.opt.scanTimeout)
	defer cancel()
	<-scanCtx.Done()

	for _, n := range recv.drain() {
		if b.opt.selfInstance != "" && n.ID == b.opt.selfInstance {
			continue // our own advertisement
		}
		seen[b.key(n)] = n
	}

	return seen
}

// enumerateIfaces returns the host's up, multicast-capable, non-loopback IPv4
// interfaces as an index->addresses map (the set mcast.Sender draws its pool
// from) alongside an index->name map (for reporting outcomes by the name that
// SendFailures and address selection key on). An interface the kernel reports no
// usable address for is omitted, exactly as the per-send path this replaces did.
func enumerateIfaces() (map[int][]net.IP, map[int]string) {
	v4 := make(map[int][]net.IP)
	names := make(map[int]string)
	ifaces, err := net.Interfaces()
	if err != nil {
		return v4, names
	}
	for _, ifi := range ifaces {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		var ips []net.IP
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ip4 := ipnet.IP.To4(); ip4 != nil && !ip4.IsLoopback() {
				ips = append(ips, ip4)
			}
		}
		if len(ips) == 0 {
			continue
		}
		v4[ifi.Index] = ips
		names[ifi.Index] = ifi.Name
	}
	return v4, names
}

// sendQuery transmits the browse PTR query from the browser's long-lived
// per-interface send-socket pool on every interface (see browse). It runs on every
// platform: the pool holds one unicast-bound socket per interface and reuses it,
// so an OS packet filter that inspects each new flow (the macOS Application
// Firewall, where this matters most) keeps a stable set of flows rather than a
// fresh one per scan, and the query is the single canonical copy — the receive
// socket collects the responses. It is created on first use and refreshed only
// when the host's interface set changes, so a steady-state scan causes no socket
// churn.
//
// It returns each attempted interface's outcome keyed by name for
// recordSendOutcomes / SendFailures. A send that fails at the socket is also the
// cheapest evidence available that the kernel has no usable route out of that
// interface, which is why address selection consumes it.
func (b *Browser) sendQuery() map[string]bool {
	v4, names := enumerateIfaces()

	b.mu.Lock()
	if b.sender == nil {
		b.sender = mcast.New(v4)
	} else {
		b.sender.Refresh(v4)
	}
	sender := b.sender
	b.mu.Unlock()

	msg := new(dns.Msg)
	qname := fmt.Sprintf("%s.%s.", strings.Trim(b.service, "."), strings.Trim(b.domain, "."))
	msg.SetQuestion(qname, dns.TypePTR)
	msg.RecursionDesired = false
	buf, err := msg.Pack()
	if err != nil {
		slog.Warn("mdns send: pack query failed", "service", b.service, "err", err)
		return nil
	}

	target := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
	outcomes := sender.SendMulticast(buf, target, sender.Ifaces())

	named := make(map[string]bool, len(outcomes))
	var sent int
	var dark []string
	for idx, ok := range outcomes {
		name, known := names[idx]
		if !known {
			continue
		}
		named[name] = ok
		if ok {
			sent++
		} else {
			dark = append(dark, name)
		}
	}

	if sent == 0 {
		// `attempted` 0 means no interface even qualified to be asked, which is a
		// different fault from interfaces present but whose sockets were refused;
		// `dark` lists the latter by name.
		slog.Warn("mdns send: query did not leave any interface",
			"service", b.service,
			"attempted", len(outcomes),
			"dark", strings.Join(dark, ","))
	}
	return named
}

// UUIDFromTXT returns the value of the "uuid=" TXT record, or "" if absent. It's
// the stable per-host identity carried on the node-scanner daemon's single
// _nvpair-node record, and was triplicated across the two proxies and the scanner
// before consolidation.
func UUIDFromTXT(txt []string) string { return uuidFromTXT(txt) }

// SameStringSet reports whether two slices hold the same elements regardless of
// order (a multiset compare). Exported so consumers (nvpair-node-scanner's
// model-inventory change detection) share this package's order-insensitive
// compare — the same one nodeEqual uses — instead of copying it.
func SameStringSet(a, b []string) bool { return sameStringSet(a, b) }

func uuidFromTXT(txt []string) string {
	for _, kv := range txt {
		if v, ok := strings.CutPrefix(kv, "uuid="); ok {
			return v
		}
	}
	return ""
}

// nodeEqual reports whether two nodes are equivalent. Addresses and TXT are
// compared order-insensitively: mDNS hands a multi-homed node's A/TXT records
// back in a non-deterministic order from scan to scan, and an order-sensitive
// compare emits a spurious "updated" (and its downstream churn) every scan even
// when nothing changed.
func nodeEqual(a, b Node) bool {
	if a.Host != b.Host || a.Port != b.Port {
		return false
	}
	return sameStringSet(a.Addresses, b.Addresses) && sameStringSet(a.TXT, b.TXT)
}

// sameStringSet reports whether two slices hold the same elements regardless of
// order.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, s := range a {
		counts[s]++
	}
	for _, s := range b {
		counts[s]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}

func isLinkLocal(ip net.IP) bool {
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}
