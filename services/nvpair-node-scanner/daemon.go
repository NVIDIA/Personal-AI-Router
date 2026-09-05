// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/mdns"
	"nvpair-shared/netmon"
	"nvpair-shared/netpick"
	"nvpair-shared/nodeid"
	"nvpair-shared/noderec"
	"nvpair-shared/reach"
)

// daemon is the promoted node-scanner: it advertises this
// node as ONE _nvpair-node record built from a registry of relayed service
// registrations, browses _nvpair-node to build a directory of every LAN node, and
// serves the discovery:* RPC (register/unregister/update-txt/get-nodes/
// reload-identity) while emitting discovery:node-* events.
//
// Post-cutover this is the sole discovery path: the legacy _nvpair-node-info
// browse and node/* stream are gone, and every consumer registers/subscribes
// through the broker relay, which the broker builds from this daemon's
// discovery:node-* stream and fans out to consumers as discovery:nodes snapshots.
type daemon struct {
	codec     *Codec
	mesh      *clustertrust.Mesh
	reg       *registry
	dir       *directory
	responder *mdns.Responder
	browser   *Discovery
	// instance is this node's mDNS instance name (the hostname). It names the
	// registry-driven self directory entry (publishSelf), which is anchored to
	// our own identity rather than to hearing our own multicast.
	instance string
	http     *http.Client
	// modelsHTTP fetches a peer's /v1/models. It gets a longer timeout than http
	// (node-info): engine-manager's models sweep is bounded by its own, larger
	// deadline, so a peer with several engines can legitimately take longer than
	// a node-info probe. A shorter client would give up mid-sweep and silently
	// drop that peer's models.
	modelsHTTP *http.Client
	// modelsClients is the long-lived per-peer mTLS pool for LAN /v1/models
	// fetches. A throwaway Transport every 15s paid a full handshake each time.
	modelsClients *clustertrust.PeerClientPool
	// tlsHTTP, when non-nil (the operator passed --client-cert/--client-key/
	// --ca-bundle), fetches node-info over HTTPS on the ni port instead of plain
	// HTTP. Dormant by default (nil): node-info is plain per the transport policy,
	// so this is opt-in scaffolding for a future TLS node-info rollout, gated only
	// on the flags — never inferred from a node's cluster-uuid or a TXT hint.
	tlsHTTP *http.Client
	// nodeInfoOrigins queues same-origin requests before http.Client starts its
	// timeout, while nodeInfoTransport enforces the matching connection bound.
	nodeInfoOrigins nodeInfoOriginGate

	// baseDir is the config base nodeid.Resolve reads identity from. Derived
	// from --cluster-dir (its parent) so the scanner's uuid= tracks the same
	// identity.json cluster-manager writes, even under a non-default cluster dir;
	// empty selects nodeid's shared default (production's default layout).
	baseDir string

	// clusterDir is the cluster trust subtree (--cluster-dir) holding this node's
	// keypair, pins, and admission record. It gates whether we advertise a
	// cluster-uuid=: we advertise only while actually a member
	// (clustertrust.Clustered), never merely because a keypair from a past
	// membership is still on disk — a left/removed node keeps its keypair, and
	// advertising off keypair presence makes it falsely appear clustered so peers
	// can never invite it back.
	clusterDir string

	// selfMu serializes every mutation of this node's own directory entry
	// (publishSelf / reloadIdentity / dropSelf) so a burst of service
	// registrations and a concurrent network/identity change can't interleave
	// into a torn self entry. The self key is written nowhere else — onBrowse
	// ignores our own uuid — so this is the sole self-key writer lock.
	selfMu sync.Mutex

	// lastInfo / lastModels cache each node's last successful enrichment (keyed by
	// hostUuid) — GPU/CPU/memory from node-info and the model list from
	// engine-manager — so a transient fetch failure reuses the prior values
	// instead of blanking the node card. Same resilience the legacy scanner path
	// had via its own lastInfo cache.
	infoMu     sync.Mutex
	lastInfo   map[string]NodeInfoResponse
	lastInfoAt map[string]time.Time
	lastModels map[string][]string
	// lastModelsByEngine caches the per-engine attribution (engine name ->
	// models) alongside the flat lastModels union, so a transient /v1/models
	// miss reuses both rather than blanking the per-engine card. Keyed by
	// hostUuid like the other enrichment caches.
	lastModelsByEngine map[string]map[string][]string
	// lastLoadedByEngine caches the per-engine loaded (in-memory) set from the
	// same /v1/models fetch, so a transient miss reuses it rather than blanking
	// a remote node's loaded state. Keyed by hostUuid.
	lastLoadedByEngine map[string]map[string][]string
	// Last-successful model VRAM observations, following the inventory cache lifecycle.
	lastLoadedVRAMByEngine map[string]map[string]map[string]uint64
	// nodeInfoDown holds the nodes whose last node-info enrichment failed, so
	// the outage is reported when it starts and when it ends rather than once
	// per sweep. Guarded by infoMu with the caches it explains: the reason a
	// card keeps showing an old inventory is exactly this entry.
	nodeInfoDown map[string]bool

	// activityMu guards lastActivityAt, when each peer was last seen returning
	// inference response bytes to one of this node's proxies (keyed by hostUuid,
	// relayed by the broker from the proxies).
	//
	// It is separate from the enrichment caches above because it is evidence of a
	// different kind. Those record what we managed to ask a peer; this records
	// what a peer sent us unprompted, on the data plane, as a by-product of doing
	// real work. That makes it the one liveness signal that gets STRONGER as a
	// node gets busier, which is precisely when the control-plane probes fail.
	activityMu     sync.Mutex
	lastActivityAt map[string]time.Time

	// enrichHosts remembers which of a node's published addresses answered, per
	// service, so a sweep asks that one instead of walking the list from the top
	// every peerRefreshInterval. It carries its own lock (see hostMemory).
	enrichHosts hostMemory
	// telemetryRetries owns one active telemetry attempt per node and suppresses
	// repeated remote attempts after failures at the same node-info target.
	// Local results always finish successfully so loopback stays on the healthy
	// cadence.
	telemetryRetries telemetryRetryGate

	// observedMu guards observed, this host's own addresses that remote peers
	// have actually connected to. nvpair-node-info sees those connections and the
	// broker relays the set here, because this daemon decides what the node
	// publishes and a peer-completed connection is the only direct proof that an
	// address works from somewhere other than this machine.
	observedMu sync.Mutex
	observed   map[string]bool
}

// nodeInfoFetchTimeout bounds each node-info enrichment HTTP(S) GET, shared by
// the plain and (opt-in) TLS clients.
const nodeInfoFetchTimeout = 3 * time.Second
const nodeInfoResponseDrainLimit = 64 << 10

// nodeInfoDialTimeout bounds only the TCP connect of that GET.
//
// The two are separated because the failures they cover are not the same. A peer
// that answers slowly is doing work — reading GPU inventory — and deserves the
// whole request budget. A peer whose address silently drops packets, which is
// what a host firewall or an address only its own side can reach looks like from
// here, never gets as far as a request, and waiting the full budget for it costs
// that much on every sweep and never learns anything. Connecting is a round trip
// on any working link, so a connect that has not completed in this long is not
// going to.
//
// It is reach.DefaultTimeout because an address a reachability probe would have
// already written off is not worth waiting longer for here.
const nodeInfoDialTimeout = reach.DefaultTimeout

// nodeInfoTransport is the transport both node-info clients dial through: the
// plain one built here and, when the operator opts enrichment onto HTTPS, the
// TLS one built in tls.go. The TLS handshake gets the same budget as the
// connect, for the same reason.
func nodeInfoTransport(tlsConfig *tls.Config) *http.Transport {
	return &http.Transport{
		DialContext:         (&net.Dialer{Timeout: nodeInfoDialTimeout}).DialContext,
		TLSClientConfig:     tlsConfig,
		TLSHandshakeTimeout: nodeInfoDialTimeout,
		MaxConnsPerHost:     1,
		MaxIdleConnsPerHost: 1,
		IdleConnTimeout:     90 * time.Second,
	}
}

// modelsFetchTimeout bounds a peer /v1/models GET. It exceeds engine-manager's
// own models-sweep cap (modelsTimeout, 5s) so the fetch waits for a bounded
// sweep to finish instead of timing out partway through it.
const modelsFetchTimeout = 6 * time.Second

// peerRefreshInterval is the cadence of the mDNS-independent convergence loop
// (refreshPeersLoop). The event-driven enrichment still handles fast initial
// discovery; this is the background sweep that catches changes that never move
// the mDNS record (a model pull/delete, a late engine start) and announcements
// that did move it but were not received (a peer joining or leaving a cluster).
// It comfortably exceeds modelsFetchTimeout so a worst-case sweep finishes well
// before the next tick.
const peerRefreshInterval = 15 * time.Second

// telemetryRefreshInterval keeps utilization observations fresh enough for the
// scheduler while leaving node-info's one-second collector as the faster
// sampling layer. Slow requests remain in flight across ticks without delaying
// dispatch to healthy nodes.
const telemetryRefreshInterval = 2 * time.Second
const telemetryRefreshJitter = 250 * time.Millisecond
const telemetryRetryMax = 30 * time.Second

// modelsRefreshConcurrency caps how many peer /v1/models fetches a single sweep
// runs at once. It sits well above realistic LAN node counts, so it only bounds
// a pathological fan-out (an unexpectedly large network) rather than throttling
// normal operation — the per-node goroutines still run concurrently so one slow
// peer never consumes another's timeout budget.
const modelsRefreshConcurrency = 16

// telemetryRefreshConcurrency limits simultaneous node-info GETs across every
// telemetry tick.
const telemetryRefreshConcurrency = 16

type telemetryRetryState struct {
	consecutiveFailures int
	retryAt             time.Time
	retryTargetKey      string
	token               uint64
	inFlight            bool
	activeTargetKey     string
	discardActive       bool
}

// telemetryRetryGate is zero-value ready. Active-attempt ownership and retry
// timing are deliberately separate: a changed endpoint can discard another
// target's backoff, but it cannot release a request that has not finished.
// Removal clears retry timing and marks any active result stale while preserving
// the claim until its matching finish.
type telemetryRetryGate struct {
	mu        sync.Mutex
	nextToken uint64
	peers     map[string]telemetryRetryState
}

func (g *telemetryRetryGate) claim(hostUUID, targetKey string, now time.Time) (uint64, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	state, exists := g.peers[hostUUID]
	if exists && state.inFlight {
		return 0, false
	}
	if exists && state.retryTargetKey == targetKey && now.Before(state.retryAt) {
		return 0, false
	}
	if state.retryTargetKey != targetKey {
		state.consecutiveFailures = 0
		state.retryAt = time.Time{}
		state.retryTargetKey = ""
	}
	g.nextToken++
	state.token = g.nextToken
	state.inFlight = true
	state.activeTargetKey = targetKey
	state.discardActive = false
	if g.peers == nil {
		g.peers = make(map[string]telemetryRetryState)
	}
	g.peers[hostUUID] = state
	return state.token, true
}

func (g *telemetryRetryGate) finish(hostUUID string, token uint64, ok bool, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()

	state, exists := g.peers[hostUUID]
	if !exists || !state.inFlight || state.token != token {
		return
	}
	state.inFlight = false
	if state.discardActive {
		delete(g.peers, hostUUID)
		return
	}
	if ok {
		delete(g.peers, hostUUID)
		return
	}
	if state.retryTargetKey != state.activeTargetKey {
		state.consecutiveFailures = 0
	}
	state.consecutiveFailures++
	state.retryTargetKey = state.activeTargetKey
	state.retryAt = now.Add(telemetryRetryDelay(state.consecutiveFailures))
	state.activeTargetKey = ""
	g.peers[hostUUID] = state
}

func (g *telemetryRetryGate) forget(hostUUID string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	state, exists := g.peers[hostUUID]
	if !exists {
		return
	}
	if !state.inFlight {
		delete(g.peers, hostUUID)
		return
	}
	state.consecutiveFailures = 0
	state.retryAt = time.Time{}
	state.retryTargetKey = ""
	state.discardActive = true
	g.peers[hostUUID] = state
}

func telemetryTargetKey(hosts []string, port int) string {
	return strconv.Itoa(port) + "\x00" + strings.Join(hosts, "\x00")
}

func telemetryRetryDelay(consecutiveFailures int) time.Duration {
	delay := telemetryRefreshInterval
	for failure := 0; failure < consecutiveFailures; failure++ {
		if delay >= telemetryRetryMax/2 {
			return telemetryRetryMax
		}
		delay *= 2
	}
	return delay
}

// newDaemon builds the daemon. tlsHTTP is nil unless the operator opted node-info
// enrichment onto HTTPS via the --client-cert/--client-key/--ca-bundle flags; nil
// keeps the default plain-HTTP fetch.
func newDaemon(codec *Codec, mesh *clustertrust.Mesh, clusterDir string, tlsHTTP *http.Client) *daemon {
	// nodeid reads <base>/cluster/identity.json; --cluster-dir is that cluster
	// subdir, so its parent is the base. Empty -> nodeid's shared default.
	baseDir := ""
	if clusterDir != "" {
		baseDir = filepath.Dir(clusterDir)
	}
	// The daemon is the single stamper of node identity: hostUuid from
	// cluster-manager's persisted identity when present (nodeid.Resolve prefers
	// it), clusterUuid from the cluster trust dir, addresses from the local
	// ranker.
	hostUUID := nodeid.Resolve(baseDir)
	var cluUUID string
	if mesh.Clustered() {
		cluUUID = mesh.NodeUUID()
	}
	// Cold start has no peer or send evidence yet, so this is the route- and
	// interface-only answer. refreshAdvertisedAddressesOnce converges it as
	// evidence arrives.
	reg := newRegistry(hostUUID, cluUUID, netpick.LocalCandidates(netpick.Evidence{}))

	instance, _ := os.Hostname()
	d := &daemon{
		codec:              codec,
		mesh:               mesh,
		reg:                reg,
		instance:           instance,
		baseDir:            baseDir,
		clusterDir:         clusterDir,
		dir:                newDirectory(),
		http:               &http.Client{Timeout: nodeInfoFetchTimeout, Transport: nodeInfoTransport(nil)},
		modelsHTTP:         &http.Client{Timeout: modelsFetchTimeout},
		modelsClients:      clustertrust.NewPeerClientPool(mesh, modelsFetchTimeout),
		tlsHTTP:            tlsHTTP,
		lastInfo:           make(map[string]NodeInfoResponse),
		lastInfoAt:         make(map[string]time.Time),
		lastModels:         make(map[string][]string),
		lastModelsByEngine: make(map[string]map[string][]string),
		lastLoadedByEngine: make(map[string]map[string][]string),
		nodeInfoDown:       make(map[string]bool),
		lastActivityAt:     make(map[string]time.Time),
	}
	// Anti-flap: before evicting a node that's missed the mDNS threshold,
	// confirm it still answers on node-info (or engine-manager as fallback).
	// Inference ports (ol/lm) are never probed — they are the promoted proxies
	// and SYN spam there contributed to accept-queue exhaustion on macOS.
	//
	// Key by uuid= (not the mDNS instance name): two hosts sharing a hostname but
	// holding distinct UUIDs must not collapse into one directory entry (matching
	// cluster-manager's browser). Self re-stamps are handled separately in
	// reloadIdentity, which moves this node's own directory entry to the new uuid.
	d.browser = NewDiscovery(noderec.ServiceType, noderec.Domain,
		WithLivenessProbe(func(n RawNode) bool { return d.reachable(n) }),
		WithKeyFunc(func(n RawNode) string { return UUIDFromTXT(n.TXT) }),
	)
	if resp, err := mdns.NewResponder(instance, noderec.ServiceType, noderec.Domain, noderec.SRVPort, reg.txt()); err != nil {
		slog.Warn("mDNS advertising disabled for _nvpair-node", "err", err)
	} else {
		d.responder = resp
	}
	return d
}

// run starts the _nvpair-node responder and browser, folding browse events into
// the directory and emitting discovery:node-* until ctx is cancelled.
func (d *daemon) run(ctx context.Context) {
	if d.responder != nil {
		go func() {
			if err := d.responder.Run(ctx); err != nil {
				slog.Warn("_nvpair-node responder exited", "err", err)
			}
		}()
		// Keep the advertised ip= tracking the host's live network the same way
		// the responder keeps its A records live (both via netmon).
		go d.watchAddress(ctx)
	}
	// Publish our own directory entry from the local registry BEFORE the browse
	// loop runs. Self presence is registry-driven, not browse-driven: on Windows
	// the host frequently does not loop its own multicast back to the browsing
	// socket, so aging self like a peer evicts the local node from its own
	// directory. Anchoring self to the identity/services we already own
	// makes it present the instant we start and immune to self-multicast loss.
	d.publishSelf()

	events := make(chan DiscoveryEvent, 32)
	go d.browser.Run(ctx, events)
	// Converge peer model inventories independently of mDNS change events: the
	// browse-driven enrichment above only re-runs when the raw mDNS record
	// changes, but models live off the record (engine-manager's em /v1/models),
	// so a pull/delete, a late engine start, or a missed announcement would
	// otherwise leave a directory entry permanently stale. Stops with ctx.
	go d.refreshPeersLoop(ctx)
	go d.refreshTelemetryLoop(ctx)
	for {
		select {
		case <-ctx.Done():
			if d.modelsClients != nil {
				d.modelsClients.CloseIdle()
			}
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			d.onBrowse(ev)
		}
	}
}

// onBrowse folds one _nvpair-node browse event into the directory (annotating
// trust from the cluster pin set) and emits the matching discovery:node-* event.
func (d *daemon) onBrowse(ev DiscoveryEvent) {
	// Never let a browse event touch our own entry. Self is registry-driven
	// (publishSelf); folding self browse events in here is exactly what evicted
	// the local node on Windows when its own multicast stopped looping back.
	// Our record always carries uuid=, so this reliably matches.
	if uuid := UUIDFromTXT(ev.Node.TXT); uuid != "" && uuid == d.reg.record().HostUUID {
		return
	}

	trusted := false
	if cu := clustertrust.ClusterUUIDFromTXT(ev.Node.TXT); cu != "" {
		d.mesh.Refresh()
		trusted = d.mesh.HasPin(cu)
	}
	node, ok := toDirectoryNode(ev.Node, trusted)
	if !ok {
		// Record without a uuid= — toDirectoryNode already warned. Nothing to
		// store, and a removed event for it is a no-op since it was never added.
		return
	}

	var method string
	switch ev.Type {
	case "removed":
		d.dir.remove(node.HostUUID)
		d.forget(node.HostUUID)
		method = noderec.NotifyNodeRemoved
	case "updated":
		d.enrich(&node)
		d.supersedingUpsert(node)
		method = noderec.NotifyNodeUpdated
	default: // discovered
		d.enrich(&node)
		d.supersedingUpsert(node)
		method = noderec.NotifyNodeDiscovered
	}
	d.emit(method, node)
}

// supersedingUpsert folds a browsed peer into the directory, evicting any record
// it is PROVEN to have replaced and emitting the removals BEFORE the caller emits
// the node's own event — so no consumer ever observes the machine under two
// identities at once.
//
// The directory can only nominate (directory.supersedeCandidates). Its signals
// are the arriving record's instance name — an unauthenticated mDNS field
// anything on the LAN can claim — plus a LastSeen gap that a healthy peer's
// frozen timestamp satisfies exactly as readily as a ghost's. Deleting on that
// alone would make mDNS a removal primitive: a record claiming a live peer's
// hostname would evict it, the browser re-reports a node only when its record
// CHANGES so it might never come back, and the loss would carry through to the
// broker's onNodeLost and to workload routing.
//
// So each candidate is confirmed the way the eviction path confirms a
// threshold-missed node: ask the machines involved who they are, and delete only
// on a definite mismatch. A candidate that cannot be confirmed stays, and is not
// left uncovered — when it stops being advertised the ordinary miss-threshold
// path (reachable) puts the same question to it.
func (d *daemon) supersedingUpsert(node noderec.DirectoryNode) {
	var proven []noderec.DirectoryNode
	for _, old := range d.dir.supersedeCandidates(node, d.reg.record().HostUUID) {
		if d.identityReplaced(old, node) {
			proven = append(proven, old)
		}
	}
	for _, old := range d.dir.upsertEvicting(node, proven) {
		slog.Warn("dropping node record proven superseded by a newer identity on the same host",
			"ip", old.IP, "droppedUuid", old.HostUUID, "droppedName", old.Name,
			"keptUuid", node.HostUUID, "keptName", node.Name,
			"ageSeconds", node.LastSeen-old.LastSeen)
		d.forget(old.HostUUID)
		d.emit(noderec.NotifyNodeRemoved, old)
	}
}

// identityReplaced reports whether old is PROVEN to be a pre-wipe ghost of the
// machine now arriving as replacement — the confirmation supersedingUpsert
// requires before it deletes anything.
//
// It takes two answers, both from node-info, and both are required:
//
//   - old must not still be answering for itself. Every address old published is
//     asked, at old's own ni port, and any of them naming old keeps it. This is
//     what tells a wipe apart from two hosts that merely share a hostname, and it
//     is why a record with no ni port to ask is never evicted here: a candidate
//     that cannot answer cannot be distinguished from a live namesake, and the
//     miss-threshold path will reach it on its own when it stops advertising.
//   - the machine at replacement's address must name someone other than old.
//     That is the arriving record's own port, which is where the machine is
//     answering now.
//
// Anything short of both — no ni port, no answer within the probe budget, a
// response without a hostUuid, or the 403 a clustered peer returns to a plaintext
// caller — leaves the record in place.
func (d *daemon) identityReplaced(old, replacement noderec.DirectoryNode) bool {
	oldNI, ok := old.Services[noderec.ServiceNodeInfo]
	if !ok || oldNI.Port == 0 {
		return false
	}
	stillItself, _ := d.identityAlive(noderec.NodeRecord{
		HostUUID: old.HostUUID,
		IP:       old.IP,
		Services: map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: oldNI.Port},
	}, old.CandidateIPs())
	if stillItself {
		return false
	}
	ni, ok := replacement.Services[noderec.ServiceNodeInfo]
	if !ok || ni.Port == 0 {
		return false
	}
	// Ask the arriving machine at every address it ranked, not just its canonical
	// one. Any of them naming old keeps the record, so a wider question can only
	// spare a live node; asking one address that this host happens not to reach
	// yields no answer at all, and a record with only ips= has no canonical
	// address to ask.
	replacementIPs := replacement.CandidateIPs()
	rec := noderec.NodeRecord{
		HostUUID: old.HostUUID,
		IP:       replacement.IP,
		Services: map[noderec.ServiceKey]int{noderec.ServiceNodeInfo: ni.Port},
	}
	alive, conclusive := d.identityAlive(rec, replacementIPs)
	return conclusive && !alive
}

// reloadTrust re-derives everything this daemon caches from the cluster pin set,
// in response to nvpair-cluster-manager reporting that the set moved
// (discovery:reload-trust).
//
// It exists because the browse path is not a sufficient trigger. A peer's trusted
// annotation is computed when its mDNS record is folded in, and the Browser only
// reports a node whose record actually changed — so a peer that was already
// advertising its cluster principal when this node joined is annotated from
// before our own pins existed and never revisited. That left it unroutable for
// the life of the process while the roster and the pins on disk both said
// otherwise.
//
// The announcement comes from the process that writes the pins, and only after
// the write lands, so the directory read here is never older than the event that
// prompted it. Recomputing unconditionally is deliberate: gating on any
// "did it change?" helper is how the previous membership hook silently did
// nothing.
func (d *daemon) reloadTrust() {
	d.mesh.Refresh()
	// A pin can flip membership on its own (a cluster dir predating admission.json
	// is clustered on pins alone), which changes what we advertise, so settle
	// identity before annotating peers against it.
	d.reconcileIdentity()
	d.reconcileTrust()
}

// reconcileIdentity re-advertises this node when live membership no longer
// matches the cluster principal the registry is publishing — the join or leave
// that reloadIdentity exists to pick up. Peers key their pins on our
// cluster-uuid= TXT, so a node that became a member must start advertising it
// without a restart; nothing else tells the fleet.
//
// The comparison is against the registry's current value rather than a change
// signal, so it is safe to call on any event: it is an in-memory string compare,
// and reloadIdentity's file read and interface enumeration only happen when the
// answer actually differs.
func (d *daemon) reconcileIdentity() {
	want := ""
	if d.mesh.Clustered() {
		want = d.mesh.NodeUUID()
	}
	if d.reg.record().ClusterUUID == want {
		return
	}
	d.reloadIdentity()
}

// reconcileTrust re-evaluates every directory entry's trusted annotation against
// the pin set the mesh currently holds, emitting one discovery:node-updated per
// entry whose answer changed. Entries that agree are left untouched and silent,
// so the steady state is a map lookup per node and no traffic — a peer paired or
// removed since the last pass is the only thing that produces an event.
//
// Self is included: it sits in the directory like any other entry, and
// Mesh.HasPin answers for our own principal through the same self-trust path
// publishSelf uses, so both stay consistent.
//
// The caller refreshes the mesh; this reads whatever it currently holds. It is
// idempotent, so a duplicated or coalesced announcement costs one map lookup per
// known node and emits nothing.
func (d *daemon) reconcileTrust() {
	for _, n := range d.dir.snapshot("") {
		// nil principal: this pass knows the pin set, not what each peer
		// advertises, so it re-derives trust from whatever principal is stored
		// when the write lands rather than from the snapshot it is iterating.
		node, changed := d.dir.applyClusterIdentity(n.HostUUID, nil, d.mesh.HasPin)
		if !changed {
			continue
		}
		slog.Info("node trust changed", "host_uuid", n.HostUUID,
			"cluster_uuid", n.ClusterUUID, "trusted", node.Trusted)
		d.emit(noderec.NotifyNodeUpdated, node)
	}
}

// emit pushes a discovery:node-* event for a directory node. Nil-codec-safe so
// directory-only unit tests can drive the daemon without a wired codec.
func (d *daemon) emit(method string, node noderec.DirectoryNode) {
	if d.codec == nil {
		return
	}
	if err := d.codec.Notify(method, noderec.NodeEvent{Node: node}); err != nil {
		slog.Warn("failed to emit node event", "method", method, "err", err)
	}
}

func (d *daemon) emitTelemetry(telemetry noderec.NodeTelemetry) {
	if d.codec == nil {
		return
	}
	if err := d.codec.Notify(noderec.NotifyNodeTelemetry, telemetry); err != nil {
		slog.Warn("failed to emit node telemetry", "host_uuid", telemetry.HostUUID, "err", err)
	}
}

// reloadIdentity re-resolves this node's advertised identity (hostUuid,
// clusterUuid, ip) and re-advertises if any of it changed. It has three triggers:
//
//   - Membership change: the mesh watch calls this when this node joins or leaves
//     a cluster, so cluster-uuid= appears in (or disappears from) this node's mDNS
//     record within one refresh interval. This is what lets peers key their pins
//     on us: without the TXT key a peer treats us as unclustered, skips us in its
//     workload broadcast, and drops us as a routing target — while we remain in
//     its roster. It must not depend on a process restart.
//   - Fresh-host startup order: the scanner spawns before cluster-manager, so it
//     mints its own node-id.json and advertises that uuid= before cluster-manager
//     writes identity.json. Once cluster-manager is up (the broker triggers a
//     reload), the re-resolve picks up identity.json so uuid= converges on the
//     cluster principal.
//   - Network change: watchAddress calls this on a netmon event so the advertised
//     addresses track the host's live network instead of freezing at the startup
//     value — matching how the responder keeps its A records live.
//
// Address evidence that arrives later without a network change is handled by
// refreshAdvertisedAddressesOnce, which re-ranks without re-reading identity.
//
// Safe to call concurrently: selfMu serializes it against publishSelf, and
// setIdentity/UpdateTXT are each mutex-guarded and idempotent for unchanged
// fields.
func (d *daemon) reloadIdentity() {
	d.selfMu.Lock()
	defer d.selfMu.Unlock()

	oldHostUUID := d.reg.record().HostUUID
	hostUUID := nodeid.Resolve(d.baseDir)
	// Re-derive cluster-uuid= from live membership, not keypair presence: a
	// keypair persists across a leave/removal, so re-reading the admission record
	// here lets a join, a teardown, or a restore converge the advertised cluster
	// status without a worker restart. Refresh picks up freshly-written pins too.
	d.mesh.Refresh()
	var cluUUID string
	if d.mesh.Clustered() {
		cluUUID = d.mesh.NodeUUID()
	}
	if !d.reg.setIdentity(hostUUID, cluUUID, d.localAddresses()) {
		return
	}
	if d.responder != nil {
		d.responder.UpdateTXT(d.reg.txt())
	}
	// A changed hostUuid (fresh-host re-stamp onto the cluster principal) leaves
	// our old entry under the stale uuid. Drop it (carrying its enrichment cache
	// onto the new uuid) so no ghost is served, then republish self under the new
	// identity below.
	if oldHostUUID != "" && oldHostUUID != hostUUID {
		d.dropSelf(oldHostUUID, hostUUID)
	}
	d.publishSelfLocked()
}

// localAddresses ranks this host's own addresses from the evidence the daemon has
// already gathered, producing the ip= / ips= values it publishes.
//
// Every input is a byproduct of work already done, so this costs no probe of its
// own: the browser records which interfaces its per-scan multicast query failed
// to leave, and the directory already holds the peers that answered — an
// interface whose own subnet contains one of them demonstrably reaches other
// nodes. That is what replaces guessing from subnet numbers, which is what made a
// multi-homed host publish a two-host direct-connect link as its address.
func (d *daemon) localAddresses() []string {
	ev := netpick.Evidence{PeerObserved: d.observedAddresses()}
	if d.browser != nil {
		ev.SendFailed = d.browser.SendFailures()
		ev.PeerOnLink = netpick.IfacesFacingPeers(d.peerAddresses())
	}
	return netpick.LocalCandidates(ev)
}

// setObservedAddresses replaces the peer-proven address set relayed from
// node-info and re-ranks if it changed anything. The set is replaced rather than
// merged so an address peers have stopped reaching stops counting as proof.
func (d *daemon) setObservedAddresses(addrs []string) {
	observed := make(map[string]bool, len(addrs))
	for _, a := range addrs {
		if a != "" {
			observed[a] = true
		}
	}
	d.observedMu.Lock()
	changed := !maps.Equal(d.observed, observed)
	d.observed = observed
	d.observedMu.Unlock()
	if !changed {
		return
	}
	slog.Debug("peer-observed local addresses updated", "addresses", addrs)
	d.refreshAdvertisedAddressesOnce()
}

func (d *daemon) observedAddresses() map[string]bool {
	d.observedMu.Lock()
	defer d.observedMu.Unlock()
	return maps.Clone(d.observed)
}

// peerAddresses returns every address known for other nodes, for the on-link
// inference in localAddresses. Self is excluded: our own record loops back
// through mDNS, and seeing our own address proves nothing about the link.
func (d *daemon) peerAddresses() []string {
	self := ""
	if d.reg != nil {
		self = d.reg.record().HostUUID
	}
	var out []string
	for _, n := range d.dir.snapshot("") {
		if n.HostUUID == self {
			continue
		}
		if n.IP != "" {
			out = append(out, n.IP)
		}
		out = append(out, n.IPs...)
	}
	return out
}

// refreshAdvertisedAddressesOnce re-ranks this host's published addresses. It
// exists because the strongest address evidence is not available at startup: no
// peer has been discovered yet, and no send has failed yet. Without a periodic
// re-rank a node would publish its cold-start guess until the next network change,
// which on a stable host is never.
//
// It deliberately does not go through reloadIdentity: identity needs a file read
// and a trust-store re-read, and neither can change because a peer appeared. The
// common case here is "nothing changed", which must stay cheap.
func (d *daemon) refreshAdvertisedAddressesOnce() {
	d.selfMu.Lock()
	defer d.selfMu.Unlock()
	if !d.reg.setAddresses(d.localAddresses()) {
		return
	}
	rec := d.reg.record()
	slog.Info("advertised addresses re-ranked", "ip", rec.IP, "candidates", rec.IPs)
	if d.responder != nil {
		d.responder.UpdateTXT(d.reg.txt())
	}
	d.publishSelfLocked()
}

// dropSelf removes this node's stale directory entry after a uuid re-stamp,
// carrying its enrichment cache onto the new uuid so the republish below reuses
// it (and can't blank the card), and emits removed(old) so consumers reconcile.
// The new entry is (re)published by the caller via publishSelfLocked. Must be
// called with selfMu held.
func (d *daemon) dropSelf(oldHostUUID, newHostUUID string) {
	node, existed := d.dir.get(oldHostUUID)
	d.dir.remove(oldHostUUID)
	d.infoMu.Lock()
	if info, ok := d.lastInfo[oldHostUUID]; ok {
		d.lastInfo[newHostUUID] = info
	}
	if at, ok := d.lastInfoAt[oldHostUUID]; ok {
		d.lastInfoAt[newHostUUID] = at
	}
	if models, ok := d.lastModels[oldHostUUID]; ok {
		d.lastModels[newHostUUID] = models
	}
	if byEngine, ok := d.lastModelsByEngine[oldHostUUID]; ok {
		d.lastModelsByEngine[newHostUUID] = byEngine
	}
	if loaded, ok := d.lastLoadedByEngine[oldHostUUID]; ok {
		d.lastLoadedByEngine[newHostUUID] = loaded
	}
	delete(d.lastInfo, oldHostUUID)
	delete(d.lastInfoAt, oldHostUUID)
	delete(d.lastModels, oldHostUUID)
	delete(d.lastModelsByEngine, oldHostUUID)
	delete(d.lastLoadedByEngine, oldHostUUID)
	if value, ok := d.lastLoadedVRAMByEngine[oldHostUUID]; ok {
		d.lastLoadedVRAMByEngine[newHostUUID] = value
		delete(d.lastLoadedVRAMByEngine, oldHostUUID)
	}
	d.infoMu.Unlock()
	if existed {
		d.emit(noderec.NotifyNodeRemoved, noderec.DirectoryNode{HostUUID: oldHostUUID, Name: node.Name})
	}
}

// publishSelf (re)builds this node's own directory entry from the local registry
// and folds it in, emitting discovered/updated. It is the sole source of the
// self entry: self is anchored to the identity + services we already own, not to
// hearing our own mDNS record, so the local node stays present regardless of
// Windows self-multicast loopback loss. Called at startup, on each
// service (un)register, and after an identity/address change.
func (d *daemon) publishSelf() {
	d.selfMu.Lock()
	defer d.selfMu.Unlock()
	d.publishSelfLocked()
}

// publishSelfLocked is publishSelf's body; the caller must hold selfMu (so a
// registration burst and a concurrent reloadIdentity can't tear the self entry).
func (d *daemon) publishSelfLocked() {
	rec := d.reg.record()
	if rec.HostUUID == "" {
		return // no identity resolved yet — nothing to anchor self on
	}
	// Local consumers reach our own services over loopback, which is never
	// firewalled; fall back to it when the LAN ranker hasn't picked an address
	// yet so the self card always has a dialable host.
	ip := rec.IP
	ips := rec.IPs
	if ip == "" {
		ip = loopbackHost
		ips = []string{loopbackHost}
	}
	trusted := false
	if rec.ClusterUUID != "" {
		d.mesh.Refresh()
		trusted = d.mesh.HasPin(rec.ClusterUUID)
	}
	svcs := make(map[noderec.ServiceKey]noderec.ServiceStatus, len(rec.Services))
	for k, port := range rec.Services {
		svcs[k] = noderec.ServiceStatus{Port: port}
	}
	node := noderec.DirectoryNode{
		HostUUID:    rec.HostUUID,
		Name:        d.instance,
		IP:          ip,
		IPs:         ips,
		ClusterUUID: rec.ClusterUUID,
		Trusted:     trusted,
		Services:    svcs,
		LastSeen:    time.Now().Unix(),
	}
	// Enrich over loopback, never the advertised LAN ip=: a first-run Windows
	// firewall block on inbound to our own LAN address must not blank the local
	// node's metrics/models (the "no metrics" failure mode).
	d.enrichAt(&node, loopbackHost)

	method := noderec.NotifyNodeUpdated
	if isNew := d.dir.upsert(node); isNew {
		method = noderec.NotifyNodeDiscovered
	}
	d.emit(method, node)
}

// watchAddress re-derives the advertised ip= whenever the host's network changes
// (interface or address add/remove — VPN/dock toggles, DHCP re-lease, sleep/wake
// re-IP). Without it the ip= would be frozen at daemon startup and could point at
// an address the node no longer holds; since consumers prefer ip= over the live A
// records, a stale value would misroute them. netmon debounces, so a reload per
// change is cheap. Best-effort: if the monitor can't start, ip= stays static
// (the responder's A records still refresh on their own netmon watch).
func (d *daemon) watchAddress(ctx context.Context) {
	mon, err := netmon.Watch(ctx)
	if err != nil {
		slog.Warn("network monitor unavailable; advertised ip= is static", "err", err)
		return
	}
	for range mon.Subscribe() {
		d.reloadIdentity()
	}
}

// reachableTimeout bounds each liveness probe attempt (node-info GET or em TCP).
const reachableTimeout = time.Second

// probeIdentityTimeout bounds the liveness probe's node-info read. It is the
// probe's own budget, deliberately not the enrichment client's
// nodeInfoFetchTimeout: enrichment is decorating a card and can afford to wait,
// whereas this runs per threshold-missed node inside the browser's scan cycle
// (shared/discovery). It matches reachableTimeout because the peer
// has already answered a TCP connect by the time we ask — a node that is up and
// listening but cannot serve node-info within a second is one we treat as
// unidentifiable and fall back on, not one worth stalling the scan for.
const probeIdentityTimeout = time.Second

// reachableInfoFreshness is how long a successful node-info enrichment keeps a
// threshold-missed node without a new probe (~2× default discovery scan interval).
const reachableInfoFreshness = 10 * time.Second

// activityFreshness is how long a peer's reported inference traffic vouches for
// it without a probe of its own.
//
// This is the strongest liveness evidence available and the cheapest, because it
// is a by-product of work the node is already doing: the local proxies see
// response bytes arrive from a peer's engine, which proves the peer's OS,
// network path, and engine process are all alive. It matters most in exactly the
// case the probes get wrong — a node saturated by inference is the one least
// able to answer a control-plane probe and the one most certainly not offline.
//
// It is deliberately longer than reachableInfoFreshness. A single long
// generation streams for tens of seconds with gaps between chunks, and the point
// is to cover a whole burst of load rather than to require continuous traffic.
const activityFreshness = 60 * time.Second

// loopbackHost is the address the daemon uses to reach THIS node's own services.
// Loopback is never firewalled, so enriching/probing self over it (rather than
// the advertised LAN ip=) makes the local node immune to a first-run Windows
// firewall block on inbound to its own LAN address.
const loopbackHost = "127.0.0.1"

// reachable reports whether a threshold-missed node is still THERE — an mDNS
// miss is not proof a node is gone, so a missed record is probed before it is
// evicted.
//
// Two kinds of evidence can vouch for a node without dialing it: a fresh
// lastInfo enrichment, and fresh inference traffic reported by the local proxies
// (lastActivityAt). Otherwise it answers in two stages, cheap then precise.
//
// First the TCP sweep: dial node-info (ni) then engine-manager (em) and stop at
// the first that connects. Inference ports (ol/lm) are never dialed — they are
// the promoted proxies and SYN spam there contributed to accept-queue
// exhaustion. Nothing answering means the node is gone, and no further question
// is worth asking — that is the ordinary departure, and it is settled without
// any HTTP.
//
// Something answering is NOT the end of it, and this is the bug that made a
// wiped node permanent. A connect only proves SOMETHING is listening at an
// address, not that it is the node the record describes. A node whose appdata is
// wiped mints a fresh hostUuid but keeps its LAN address and re-binds the same
// fixed service ports, so the sweep reaches the machine's NEW incarnation and
// reports the OLD record alive. The miss counter is reset on every pass and the
// stale record never ages out — the fleet carries a permanent duplicate of one
// machine. So when something does answer, we ask the machine who it is
// (identityAlive) and let a definite answer overrule the connect.
//
// Running the sweep first is what keeps that identity check off the hot path: it
// only runs when the node would otherwise have been KEPT, which is exactly when
// the two cases need telling apart. Ordering the other way would spend a
// node-info timeout on every departed node inside the browser's scan cycle. The
// verdicts are identical either way — a conclusive identity answer
// requires node-info to have responded, so its port is open, so the sweep would
// have succeeded regardless.
//
// A node advertising neither ni nor em (identity-only, or ol/lm alone) has
// nothing to probe and evicts normally.
func (d *daemon) reachable(n RawNode) bool {
	rec := noderec.ParseTXT(n.TXT)
	candidates := netpick.Candidates(n.TXT, n.Addresses)
	if len(candidates) == 0 {
		return false
	}
	if rec.HostUUID != "" && d.reg != nil && rec.HostUUID == d.reg.record().HostUUID {
		candidates = []string{loopbackHost}
	}

	if rec.HostUUID != "" {
		d.infoMu.Lock()
		at, had := d.lastInfoAt[rec.HostUUID]
		d.infoMu.Unlock()
		if had && time.Since(at) < reachableInfoFreshness {
			return true
		}
		if since, ok := d.activitySince(rec.HostUUID); ok && since < activityFreshness {
			slog.Debug("liveness probe skipped: the node was recently serving inference",
				"host_uuid", rec.HostUUID, "last_activity_ago", since)
			return true
		}
	}

	answered := false
	answeredIP := ""
	answeredPort := 0
	for _, ip := range candidates {
		for _, svc := range []noderec.ServiceKey{noderec.ServiceNodeInfo, noderec.ServiceEngineManager} {
			port, ok := rec.Port(svc)
			if !ok {
				continue
			}
			conn, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), reachableTimeout)
			if err == nil {
				_ = conn.Close()
				answered = true
				answeredIP = ip
				answeredPort = port
				break
			}
		}
		if answered {
			break
		}
	}
	if !answered {
		return false
	}
	if alive, conclusive := d.identityAlive(rec, candidates); conclusive {
		return alive
	}
	// Kept on the sweep's word alone. Record which port answered next to the
	// node-info port we then failed to read: the pair separates "node-info is
	// down but engine-manager answered" from "node-info accepted the connection
	// and did not respond in time", which are indistinguishable in a report that
	// only says the node flickered.
	if niPort, ok := rec.Port(noderec.ServiceNodeInfo); ok {
		slog.Debug("liveness probe kept a node without settling its identity",
			"dial_ip", answeredIP, "advertised_ip", rec.IP, "answered_port", answeredPort, "ni_port", niPort)
	}
	return true
}

// identityAlive asks the machines at ips whether any of them is still the node rec
// describes, by reading hostUuid from node-info. In the eviction path it is called
// only after the TCP sweep has already found an address answering, so its job is
// to overrule that verdict when the machine turns out to be someone else.
//
// It walks every address rather than only the record's first because one wrong
// answer is not proof of a wipe: a direct-connect link uses the same subnet on
// every pair of machines that has one, so a record's leading address can resolve
// locally to a different host entirely. An address naming the record's own host
// settles it in the record's favour, and a mismatch counts only once no address
// has.
//
// conclusive reports whether the question could be answered at all. It is false
// — meaning "keep the TCP sweep's verdict" — when the record advertises no ni
// port, when it carries no uuid= to compare against, when no address answers
// within probeIdentityTimeout, or when every answer omits hostUuid. Only a
// node-info response that actually names a host is treated as proof, in either
// direction.
//
// Two properties bound what this can get wrong, and both are worth knowing
// before changing it:
//
//   - It only ever concludes for an UNCLUSTERED peer. A clustered node's
//     node-info admits pinned mTLS callers only (nvpair-node-info's handler
//     refuses everyone else with 403), so this plaintext probe simply fails
//     against a member and falls back to the TCP sweep. That is the right
//     coverage for this bug: a wiped node is unclustered by definition, so the
//     case we need to catch is exactly the case we can see.
//   - It assumes node-info's hostUuid and the scanner's advertised uuid= name
//     the same identity. They are deliberately kept in agreement — the broker
//     passes its resolved node id to node-info, and cluster-manager ADOPTS an
//     existing node-id rather than minting a rival one (see its
//     TestFirstMintAdoptsExistingNodeUUID). If that invariant is ever broken,
//     this probe would read the skew as a wipe and evict a live peer.
func (d *daemon) identityAlive(rec noderec.NodeRecord, ips []string) (alive, conclusive bool) {
	if rec.HostUUID == "" || (d.http == nil && d.tlsHTTP == nil) {
		return false, false
	}
	port, ok := rec.Port(noderec.ServiceNodeInfo)
	if !ok {
		return false, false
	}
	mismatchedAt, mismatchedUUID := "", ""
	for _, ip := range ips {
		if ip == "" {
			continue
		}
		info, ok := d.probeIdentity(ip, port)
		if !ok || info.HostUUID == "" {
			continue
		}
		if info.HostUUID == rec.HostUUID {
			return true, true
		}
		mismatchedAt, mismatchedUUID = ip, info.HostUUID
	}
	if mismatchedAt == "" {
		return false, false
	}
	// Every address that answered named someone else, so the record describes a
	// node that no longer exists: the machine was reset, or the address was
	// reassigned. Warn — this is the signal that a node was wiped, and it is the
	// only place the fleet can observe it.
	slog.Warn("node record superseded: no address it published still answers for it",
		"ip", mismatchedAt, "recordUuid", rec.HostUUID, "actualUuid", mismatchedUUID)
	return false, true
}

// probeIdentity reads one address's node-info under the identity probe's own
// budget, which is tighter than enrichment's: this runs inside the browser's scan
// cycle, and each address gets the same allowance so a slow first candidate cannot
// consume the rest of the list's time.
func (d *daemon) probeIdentity(ip string, port int) (NodeInfoResponse, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), probeIdentityTimeout)
	defer cancel()
	return d.fetchNodeInfoWithin(ctx, ip, port)
}

// enrich decorates a node with the data that lives off the mDNS record: its
// node-info inventory (GPU/CPU/memory), fetched over plain HTTP per the transport
// policy, and its engine-manager model list, which is cluster data and so is
// fetched from a peer only over cluster mTLS (see fetchModels). Both are
// independent (a node may advertise one without the other). Peers are dialed at
// each address they published until one answers for the expected identity; self
// is dialed over loopback (see enrichAt).
func (d *daemon) enrich(node *noderec.DirectoryNode) {
	hosts := node.CandidateIPs()
	d.enrichInfoCandidates(node, hosts)
	d.enrichModelsCandidates(node, hosts)
}

// enrichAt is enrich against an explicit dial host. onBrowse enriches a peer at
// its advertised ip=; publishSelf enriches this node over loopbackHost so a
// first-run firewall block on our own LAN address can't blank the local card.
// The enrichment cache stays keyed by hostUuid, so the resilience
// (reuse-last-good on a transient miss) is identical for self and peers.
func (d *daemon) enrichAt(node *noderec.DirectoryNode, host string) {
	d.enrichInfoCandidates(node, []string{host})
	d.enrichModelsCandidates(node, []string{host})
}

// enrichInfoAt decorates a node that advertises node-info (ni) with its
// GPU/CPU/memory inventory, fetched at the ni port on host. By default this is
// PLAIN HTTP — node-info is plain per the transport policy, and the daemon must
// NEVER infer mTLS from the node-level cluster-uuid= (the subtlest correctness
// requirement in the consolidation). The only way TLS is used is if the operator
// opts in globally via the --client-cert/--client-key/--ca-bundle flags (see
// fetchNodeInfo). A transient fetch failure reuses the node's last successful
// metrics rather than blanking its card; a node never enriched stays un-enriched.
func (d *daemon) enrichInfoAt(node *noderec.DirectoryNode, host string) {
	d.enrichInfoCandidates(node, []string{host})
}

// enrichInfoCandidates is enrichInfoAt with address failover. A raw TCP accept
// is not enough here: identically configured direct-connect links can reuse an
// address on different machines, so the response's host UUID is checked before
// the candidate is accepted. Older peers that omit hostUuid remain compatible.
//
// Which address is asked, and in what order, is askRemembered's business: the one
// that answered last sweep by itself, and the rest together only if it stopped.
func (d *daemon) enrichInfoCandidates(node *noderec.DirectoryNode, hosts []string) {
	ni, ok := node.Services[noderec.ServiceNodeInfo]
	if !ok || len(hosts) == 0 {
		return
	}
	ask := func(host string) (NodeInfoResponse, bool) {
		candidate, fetched := d.fetchNodeInfo(host, ni.Port)
		if !fetched {
			return NodeInfoResponse{}, false
		}
		if candidate.HostUUID != "" && node.HostUUID != "" && candidate.HostUUID != node.HostUUID {
			slog.Debug("node-info candidate answered for a different host; trying next",
				"expected_host_uuid", node.HostUUID, "reported_host_uuid", candidate.HostUUID, "ip", host)
			return NodeInfoResponse{}, false
		}
		return candidate, true
	}
	key := hostKey{hostUUID: node.HostUUID, service: noderec.ServiceNodeInfo}
	selectedHost, info, _ := askRemembered(&d.enrichHosts, key, hosts, ask)

	endpointHost := selectedHost
	if endpointHost == "" {
		endpointHost = hosts[0]
	}
	d.noteNodeInfo(node.HostUUID, net.JoinHostPort(endpointHost, strconv.Itoa(ni.Port)), selectedHost != "")
	if selectedHost != "" {
		d.infoMu.Lock()
		if d.lastInfo == nil {
			d.lastInfo = make(map[string]NodeInfoResponse)
		}
		d.lastInfo[node.HostUUID] = info
		if d.lastInfoAt == nil {
			d.lastInfoAt = make(map[string]time.Time)
		}
		d.lastInfoAt[node.HostUUID] = time.Now()
		d.infoMu.Unlock()
	} else {
		d.infoMu.Lock()
		cached, had := d.lastInfo[node.HostUUID]
		d.infoMu.Unlock()
		if !had {
			return
		}
		info = cached
	}
	node.GPUs = info.GPUs
	node.CPU = info.CPU
	node.Memory = info.Memory
}

// noteNodeInfo reports a peer's node-info endpoint going quiet, and coming back,
// at a level someone reading the log will see.
//
// Only the transitions are reported. The enrichment sweep repeats every
// peerRefreshInterval for as long as a node is advertised, so a peer whose
// address accepts nothing produces an unbounded run of identical failures; one
// line each time is why this was a debug line, and being a debug line is why an
// address that never once answered looked, in the log, exactly like one that was
// working. A node that is failing is stated once, with where it was asked, and
// its recovery is stated once.
//
// Silence between those two lines does not mean the peer is fine — it means
// nothing has changed and the node's card is still showing the last inventory
// that was actually read.
func (d *daemon) noteNodeInfo(hostUUID, endpoint string, ok bool) {
	d.infoMu.Lock()
	was := d.nodeInfoDown[hostUUID]
	if ok {
		delete(d.nodeInfoDown, hostUUID)
	} else {
		if d.nodeInfoDown == nil {
			d.nodeInfoDown = make(map[string]bool)
		}
		d.nodeInfoDown[hostUUID] = true
	}
	d.infoMu.Unlock()

	switch {
	case !ok && !was:
		slog.Warn("node-info is not answering; this node's inventory will not update",
			"hostUuid", hostUUID, "endpoint", endpoint)
	case ok && was:
		slog.Info("node-info is answering again", "hostUuid", hostUUID, "endpoint", endpoint)
	}
}

// fetchNodeInfo does one GET of a node's /v1/node-info, returning the parsed
// inventory and whether it succeeded. It uses plain HTTP by default; when the
// operator configured a TLS client (--client-cert/--client-key/--ca-bundle) it
// fetches over HTTPS on the same ni port. The choice is purely flag-gated — never
// per-node — so the transport policy stays static.
func (d *daemon) fetchNodeInfo(ip string, port int) (NodeInfoResponse, bool) {
	return d.fetchNodeInfoWithin(context.Background(), ip, port)
}

// fetchNodeInfoWithin is fetchNodeInfo bounded by ctx as well as the client's own
// timeout. The liveness probe uses it to hold node-info to a tighter budget than
// enrichment without needing a second HTTP client (which would have to duplicate
// the TLS choice above); every other caller passes a background context and gets
// the client timeout unchanged.
func (d *daemon) fetchNodeInfoWithin(ctx context.Context, ip string, port int) (NodeInfoResponse, bool) {
	client, scheme := d.http, "http"
	if d.tlsHTTP != nil {
		client, scheme = d.tlsHTTP, "https"
	}
	if client == nil {
		return NodeInfoResponse{}, false
	}
	origin := scheme + "://" + net.JoinHostPort(ip, strconv.Itoa(port))
	release, acquired := d.nodeInfoOrigins.acquire(ctx, origin)
	if !acquired {
		return NodeInfoResponse{}, false
	}
	defer release()

	url := origin + "/v1/node-info"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		slog.Debug("node-info request build failed", "ip", ip, "url", url, "err", err)
		return NodeInfoResponse{}, false
	}
	resp, err := client.Do(req)
	if err != nil {
		slog.Debug("node-info fetch failed", "ip", ip, "url", url, "err", err)
		return NodeInfoResponse{}, false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, nodeInfoResponseDrainLimit))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return NodeInfoResponse{}, false
	}
	var info NodeInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return NodeInfoResponse{}, false
	}
	return info, true
}

// enrichModelsAt decorates a node that advertises engine-manager (em) with its
// model list, fetched at the em port (/v1/models) on host. The list moved off the
// size-limited mDNS TXT onto HTTP so it can grow unbounded and be owned by
// engine-manager (which already owns engine state). A peer is fetched over cluster
// mTLS pinned to the principal it advertises, and skipped when this node holds no
// pin for it — which models a machine holds is cluster data. A transient failure
// reuses the last successful list rather than blanking it; a node advertising no em
// has no model list (nothing is carried in the record). Self is dialed over
// loopback (see enrichAt), which stays plaintext, peers at their advertised address.
func (d *daemon) enrichModelsAt(node *noderec.DirectoryNode, host string) {
	d.enrichModelsCandidates(node, []string{host})
}

// enrichModelsCandidates is enrichModelsAt with address failover. For a peer,
// fetchModels performs the pinned mTLS handshake, so a shared address belonging
// to another machine is rejected and the next published address is tried.
func (d *daemon) enrichModelsCandidates(node *noderec.DirectoryNode, hosts []string) {
	em, ok := node.Services[noderec.ServiceEngineManager]
	if !ok || len(hosts) == 0 {
		return
	}
	ask := func(host string) (modelInventory, bool) {
		models, byEngine, loadedByEngine, loadedVRAMByEngine, fetched := d.fetchModels(host, em.Port, node.ClusterUUID)
		if !fetched {
			return modelInventory{}, false
		}
		return modelInventory{models: models, byEngine: byEngine, loadedByEngine: loadedByEngine, loadedVRAMByEngine: loadedVRAMByEngine}, true
	}
	key := hostKey{hostUUID: node.HostUUID, service: noderec.ServiceEngineManager}
	if _, inventory, ok := askRemembered(&d.enrichHosts, key, hosts, ask); ok {
		d.infoMu.Lock()
		if d.lastModels == nil {
			d.lastModels = make(map[string][]string)
		}
		if d.lastModelsByEngine == nil {
			d.lastModelsByEngine = make(map[string]map[string][]string)
		}
		if d.lastLoadedByEngine == nil {
			d.lastLoadedByEngine = make(map[string]map[string][]string)
		}
		d.lastModels[node.HostUUID] = inventory.models
		d.lastModelsByEngine[node.HostUUID] = inventory.byEngine
		d.lastLoadedByEngine[node.HostUUID] = inventory.loadedByEngine
		if d.lastLoadedVRAMByEngine == nil {
			d.lastLoadedVRAMByEngine = make(map[string]map[string]map[string]uint64)
		}
		d.lastLoadedVRAMByEngine[node.HostUUID] = inventory.loadedVRAMByEngine
		d.infoMu.Unlock()
		node.Models = inventory.models
		node.ModelsByEngine = inventory.byEngine
		node.LoadedByEngine = inventory.loadedByEngine
		node.LoadedVRAMByEngine = inventory.loadedVRAMByEngine
		return
	}
	d.infoMu.Lock()
	cached, had := d.lastModels[node.HostUUID]
	cachedByEngine := d.lastModelsByEngine[node.HostUUID]
	cachedLoaded := d.lastLoadedByEngine[node.HostUUID]
	cachedVRAM := d.lastLoadedVRAMByEngine[node.HostUUID]
	d.infoMu.Unlock()
	if had {
		node.Models = cached
		node.ModelsByEngine = cachedByEngine
		node.LoadedByEngine = cachedLoaded
		node.LoadedVRAMByEngine = cachedVRAM
	}
}

// fetchModels does one GET of a node's /v1/models, returning the
// flat model-name union, the per-engine attribution (engine name -> models),
// the per-engine loaded (in-memory) set, and whether it succeeded. Shape:
// {"models":["name",...],"modelsByEngine":{"ollama":[...]},"loadedByEngine":{"ollama":[...]}}.
// modelsByEngine and loadedByEngine are additive (omitempty on the wire); a peer
// that doesn't send them yields nil maps, and the flat union still enriches the
// node. A present engine key with [] is a successful empty inventory.
// A peer's model inventory is cluster data, so a peer is fetched over cluster
// mTLS keyed on the cluster principal it advertises (clusterUUID, from its
// cluster-uuid= TXT): this node must hold a pin for that principal, or there is
// nothing to fetch and nothing is attempted. THIS node's own engine-manager is
// dialed over loopback (both callers force that for self), which stays plain and
// therefore keeps working when this node belongs to no cluster — a standalone
// machine still has to show its own models.
func (d *daemon) fetchModels(ip string, port int, clusterUUID string) ([]string, map[string][]string, map[string][]string, map[string]map[string]uint64, bool) {
	client, scheme, ok := d.modelsClient(ip, clusterUUID)
	if !ok {
		slog.Debug("model enrichment skipped: peer is not a pinned cluster member",
			"ip", ip, "clusterUuid", clusterUUID)
		return nil, nil, nil, nil, false
	}
	url := scheme + "://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "/v1/models"
	resp, err := client.Get(url)
	if err != nil {
		slog.Debug("model enrichment failed", "ip", ip, "url", url, "err", err)
		return nil, nil, nil, nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, nil, nil, false
	}
	var body struct {
		Models             []string                     `json:"models"`
		ModelsByEngine     map[string][]string          `json:"modelsByEngine"`
		LoadedByEngine     map[string][]string          `json:"loadedByEngine"`
		LoadedVRAMByEngine map[string]map[string]uint64 `json:"loadedVramByEngine"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, nil, nil, nil, false
	}
	return body.Models, body.ModelsByEngine, body.LoadedByEngine, body.LoadedVRAMByEngine, true
}

// modelsClient picks the transport for a model fetch: the plain client over
// loopback (this node's own engine-manager, which serves loopback in plaintext in
// every membership state), or a per-peer pinned mTLS client for a LAN peer. ok is
// false when a peer holds no pin here, which is the client-side cluster gate —
// there is deliberately no plaintext fallback for a peer.
func (d *daemon) modelsClient(ip, clusterUUID string) (*http.Client, string, bool) {
	if isLoopbackHost(ip) {
		return d.modelsHTTP, "http", true
	}
	d.mesh.Refresh()
	if d.modelsClients == nil {
		return nil, "", false
	}
	client, ok := d.modelsClients.Client(clusterUUID)
	if !ok {
		return nil, "", false
	}
	return client, "https", true
}

// isLoopbackHost reports whether a dial host addresses this machine.
func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// refreshPeersLoop periodically re-reads what each peer reports about itself over
// HTTP and folds any change back into the directory, independent of mDNS browse
// events. It is the convergence mechanism for facts that change without moving
// the raw mDNS record, or whose announcement was simply not received — neither of
// which produces a browse event, so neither reaches the enrichment in onBrowse.
// Stops when ctx is cancelled (the daemon's existing shutdown path).
//
// Cluster membership is refreshed before models on the same tick, because the
// model fetch is pinned mTLS keyed on the peer's principal: reading membership
// first means a peer that just joined or left is dialed under its current
// identity rather than the previous one.
func (d *daemon) refreshPeersLoop(ctx context.Context) {
	ticker := time.NewTicker(peerRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.refreshAdvertisedAddressesOnce()
			d.mesh.Refresh()
			if d.modelsClients != nil {
				d.modelsClients.DropUnpinned()
			}
			d.refreshClusterIdentityOnce()
			d.refreshModelsOnce()
		}
	}
}

func (d *daemon) refreshTelemetryLoop(ctx context.Context) {
	hostUUID := ""
	if d.reg != nil {
		hostUUID = d.reg.record().HostUUID
	}
	d.runTelemetryLoop(ctx, telemetryIntervalForNode(hostUUID))
}

func telemetryIntervalForNode(hostUUID string) time.Duration {
	if hostUUID == "" {
		return telemetryRefreshInterval
	}
	hash := uint32(2166136261)
	for i := 0; i < len(hostUUID); i++ {
		hash ^= uint32(hostUUID[i])
		hash *= 16777619
	}
	windowMillis := int64(2*telemetryRefreshJitter/time.Millisecond) + 1
	offsetMillis := int64(hash%uint32(windowMillis)) - windowMillis/2
	return telemetryRefreshInterval + time.Duration(offsetMillis)*time.Millisecond
}

// runTelemetryLoop dispatches compact scheduler telemetry on a fixed cadence.
// Requests may span ticks, but telemetryRetries excludes another request for
// the same node and the loop-owned semaphore bounds total active work. Shutdown
// cancels and drains every worker before returning.
func (d *daemon) runTelemetryLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	sem := make(chan struct{}, telemetryRefreshConcurrency)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.dispatchTelemetry(ctx, time.Now(), sem, &workers)
		}
	}
}

// refreshTelemetryOnce synchronously dispatches one telemetry pass. Production
// uses runTelemetryLoop's persistent semaphore and workers; this wrapper keeps
// focused callers and tests able to wait for the pass they requested.
func (d *daemon) refreshTelemetryOnce(ctx context.Context) {
	sem := make(chan struct{}, telemetryRefreshConcurrency)
	var workers sync.WaitGroup
	d.dispatchTelemetry(ctx, time.Now(), sem, &workers)
	workers.Wait()
}

// dispatchTelemetry starts every due node without waiting for requests already
// in flight. sem and workers belong to the enclosing loop so work remains
// bounded and drainable across ticks.
func (d *daemon) dispatchTelemetry(
	ctx context.Context,
	now time.Time,
	sem chan struct{},
	workers *sync.WaitGroup,
) {
	self := ""
	if d.reg != nil {
		self = d.reg.record().HostUUID
	}
	for _, n := range d.dir.snapshot(noderec.ServiceNodeInfo) {
		ni, ok := n.Services[noderec.ServiceNodeInfo]
		if !ok {
			continue
		}
		hosts := append([]string(nil), n.CandidateIPs()...)
		if n.HostUUID == self {
			hosts = []string{loopbackHost}
		}
		if len(hosts) == 0 {
			continue
		}
		targetKey := telemetryTargetKey(hosts, ni.Port)
		retryToken, due := d.telemetryRetries.claim(n.HostUUID, targetKey, now)
		if !due {
			continue
		}

		workers.Add(1)
		go func(hostUUID string, hosts []string, port int, token uint64, backoff bool) {
			defer workers.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				d.telemetryRetries.finish(hostUUID, token, true, time.Now())
				return
			}
			ok := d.refreshNodeTelemetryCandidates(ctx, hostUUID, hosts, port)
			if !backoff || ctx.Err() != nil {
				ok = true
			}
			d.telemetryRetries.finish(hostUUID, token, ok, time.Now())
		}(n.HostUUID, hosts, ni.Port, retryToken, n.HostUUID != self)
	}
}

const maxTelemetryAge = int64(1<<63 - 1)

// refreshNodeTelemetry emits one node's current GPU utilization. Failed fetches
// stay silent so downstream freshness expiry handles temporary unreachability.
// A response naming another host is never attributed to the directory entry
// whose address was queried.
func (d *daemon) refreshNodeTelemetry(ctx context.Context, hostUUID, ip string, niPort int) bool {
	return d.refreshNodeTelemetryCandidates(ctx, hostUUID, []string{ip}, niPort)
}

// telemetrySample is one node-info answer plus when the request that produced it
// began. The reported age has to be advanced by that request's own latency, and a
// failover attempt asks several addresses at once, so the time belongs to the
// answer rather than to the sweep.
type telemetrySample struct {
	info      NodeInfoResponse
	startedAt time.Time
}

// refreshNodeTelemetryCandidates is refreshNodeTelemetry with address failover.
//
// Healthy nodes are sampled every two seconds. Failed remote nodes back off, and
// each due attempt goes through the same node-info memory the enrichment sweeps
// use: the address that answered is asked by itself, and the rest of the list
// only once it stops answering. Without that, a peer whose canonical address is
// a link this host cannot reach reported no telemetry at all, and the scheduler
// then assigned it neutral pressure and scheduled it blind.
func (d *daemon) refreshNodeTelemetryCandidates(ctx context.Context, hostUUID string, hosts []string, niPort int) bool {
	ask := func(host string) (telemetrySample, bool) {
		startedAt := time.Now()
		candidate, ok := d.fetchNodeInfoWithin(ctx, host, niPort)
		if !ok {
			return telemetrySample{}, false
		}
		if candidate.HostUUID != hostUUID {
			slog.Debug("skipping telemetry reported by a different host at this address; trying next",
				"expected_host_uuid", hostUUID, "reported_host_uuid", candidate.HostUUID, "ip", host)
			return telemetrySample{}, false
		}
		return telemetrySample{info: candidate, startedAt: startedAt}, true
	}
	key := hostKey{hostUUID: hostUUID, service: noderec.ServiceNodeInfo}
	_, sample, ok := askRemembered(&d.enrichHosts, key, hosts, ask)
	if !ok {
		return false
	}
	info, startedAt := sample.info, sample.startedAt

	age := int64(0)
	if info.TelemetryValid {
		age = info.MSSince
		if age < 0 {
			age = 0
		}
		elapsed := time.Since(startedAt).Milliseconds()
		if elapsed > maxTelemetryAge-age {
			age = maxTelemetryAge
		} else {
			age += elapsed
		}
	}
	var utilization uint32
	for i := range info.GPUs {
		if info.GPUs[i].UtilizationPercent > utilization {
			utilization = info.GPUs[i].UtilizationPercent
		}
	}
	d.emitTelemetry(noderec.NodeTelemetry{
		HostUUID:          hostUUID,
		GPUUtilizationPct: utilization,
		TelemetryValid:    info.TelemetryValid,
		MSSince:           age,
	})
	return true
}

// refreshClusterIdentityOnce re-reads every peer's cluster membership from its
// node-info endpoint and folds any change into the directory.
//
// Membership otherwise reaches this daemon only as the cluster-uuid= key on a
// peer's mDNS record, which is read exactly once per browse *change* event. A
// peer that leaves its cluster and re-advertises produces such an event — but if
// it is not received, nothing asks again: the browse diff has no notion of a fact
// it never saw, and the liveness probe keeps the entry alive precisely because the
// peer is still reachable. The stale principal then outlives the cluster, and a
// client suppresses the invite that would bring that peer back.
//
// node-info is the right place to ask because it is the one inter-node surface
// deliberately kept plain (see the broker's spawnNodeInfo), so it answers whether
// or not this node and the peer share a cluster — and it is reached over TCP at
// the address the peer advertised, sharing nothing with multicast reception.
//
// Self is excluded: this node's own entry is registry-driven (publishSelf owns its
// principal), not something to learn from a loopback HTTP read.
func (d *daemon) refreshClusterIdentityOnce() {
	self := ""
	if d.reg != nil {
		self = d.reg.record().HostUUID
	}
	// One refresh for the whole sweep: every peer is then annotated against the
	// same pin set, and 16 concurrent re-reads of the trust store are avoided.
	d.mesh.Refresh()
	sem := make(chan struct{}, modelsRefreshConcurrency)
	var wg sync.WaitGroup
	for _, n := range d.dir.snapshot(noderec.ServiceNodeInfo) {
		ni, ok := n.Services[noderec.ServiceNodeInfo]
		if !ok || n.IP == "" || n.HostUUID == self {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		hosts := append([]string(nil), n.CandidateIPs()...)
		go func(hostUUID string, hosts []string, port int) {
			defer wg.Done()
			defer func() { <-sem }()
			d.refreshNodeClusterIdentityCandidates(hostUUID, hosts, port)
		}(n.HostUUID, hosts, ni.Port)
	}
	wg.Wait()
}

// refreshNodeClusterIdentity reads one peer's reported membership and applies it,
// emitting one discovery:node-updated when it moved. Returns whether it emitted
// (used by tests).
//
// Two answers mean "leave the annotation alone": a failed fetch, and a peer that
// reports no clusterUuid field at all because it predates it. Only the peer's own
// explicit value is acted on — inferring "unclustered" from silence would mark a
// clustered peer invitable, which is the failure this annotation exists to
// prevent.
//
// A third: the machine answering may not be the one this entry describes. An
// address outlives its occupant — a wiped and reinstalled node, or a reused DHCP
// lease — and node-info names who it really is, so a mismatched hostUuid means
// writing a stranger's membership onto someone else's entry. Skipped rather than
// corrected here: the entry ages out through the same identity check in the
// liveness probe (see identityAlive), which owns that decision. A peer reporting
// no hostUuid predates the field and cannot be checked either way, so it is taken
// at its word — the same fallback that probe applies to the same answer.
func (d *daemon) refreshNodeClusterIdentity(hostUUID, ip string, niPort int) bool {
	return d.refreshNodeClusterIdentityCandidates(hostUUID, []string{ip}, niPort)
}

// refreshNodeClusterIdentityCandidates is refreshNodeClusterIdentity with
// address failover. A response from a different host is not authoritative for
// this entry, but it is also not the end of the candidate list: shared
// direct-connect addresses can legitimately resolve to a different machine.
func (d *daemon) refreshNodeClusterIdentityCandidates(hostUUID string, hosts []string, niPort int) bool {
	ask := func(host string) (NodeInfoResponse, bool) {
		candidate, ok := d.fetchNodeInfo(host, niPort)
		if !ok {
			return NodeInfoResponse{}, false
		}
		if candidate.HostUUID != "" && candidate.HostUUID != hostUUID {
			slog.Debug("skipping cluster membership reported by a different host at this address; trying next",
				"expected_host_uuid", hostUUID, "reported_host_uuid", candidate.HostUUID, "ip", host)
			return NodeInfoResponse{}, false
		}
		return candidate, true
	}
	// Shares the node-info memory with the enrichment sweep, so the two node-info
	// callers converge on one address per peer rather than each finding it again.
	key := hostKey{hostUUID: hostUUID, service: noderec.ServiceNodeInfo}
	_, info, _ := askRemembered(&d.enrichHosts, key, hosts, ask)
	if info.ClusterUUID == nil {
		return false
	}
	node, changed := d.dir.applyClusterIdentity(hostUUID, info.ClusterUUID, d.mesh.HasPin)
	if !changed {
		return false
	}
	slog.Info("peer cluster membership changed", "host_uuid", hostUUID,
		"clustered", node.Clustered(), "trusted", node.Trusted)
	d.emit(noderec.NotifyNodeUpdated, node)
	return true
}

// refreshModelsOnce snapshots every directory node advertising engine-manager
// (em) with a usable IP and refreshes each concurrently, bounded by
// modelsRefreshConcurrency so an unexpectedly large network can't spawn an
// unbounded burst of fetches. Running the sweep to completion (wg.Wait) before
// the loop reads the next tick makes overlapping sweeps impossible, and each
// per-node goroutine uses modelsHTTP's own timeout, so one slow peer never
// consumes another's budget.
func (d *daemon) refreshModelsOnce() {
	sem := make(chan struct{}, modelsRefreshConcurrency)
	var wg sync.WaitGroup
	for _, n := range d.dir.snapshot(noderec.ServiceEngineManager) {
		em, ok := n.Services[noderec.ServiceEngineManager]
		if !ok || n.IP == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		hosts := append([]string(nil), n.CandidateIPs()...)
		go func(hostUUID, guardIP string, hosts []string, port int, clusterUUID string) {
			defer wg.Done()
			defer func() { <-sem }()
			d.refreshNodeModelsCandidates(hostUUID, guardIP, hosts, port, clusterUUID)
		}(n.HostUUID, n.IP, hosts, em.Port, n.ClusterUUID)
	}
	wg.Wait()
}

// refreshNodeModels fetches one peer's /v1/models and, on success, applies it to
// the directory under the removed/re-addressed guard (directory.applyModels).
// A failed fetch retains the last successful inventory: no cache write, no
// directory change, no event. On a change it refreshes the enrichment caches
// (so a later browse-driven fallback can't revert the directory to a stale
// value) and emits exactly one discovery:node-updated carrying the full node.
// Returns whether it emitted an update (used by tests).
//
// The fetch dials our OWN engine-manager over loopback, never the advertised
// LAN ip=: this loop — not the registry-driven publishSelf — is what converges
// model-only changes (a pull/delete, a late engine start) that don't move the
// registry, so on the Windows self-LAN firewall block this daemon guards
// against, a LAN dial would make self's model list freeze at its
// startup value. The stored ip is still passed to applyModels as the
// stale-address guard, so an in-flight result can't overwrite a re-addressed
// node. Peers are still dialed at their advertised address.
func (d *daemon) refreshNodeModels(hostUUID, ip string, emPort int, clusterUUID string) bool {
	return d.refreshNodeModelsCandidates(hostUUID, ip, []string{ip}, emPort, clusterUUID)
}

// refreshNodeModelsCandidates is refreshNodeModels with address failover. The
// canonical IP remains the stale-result guard even when a later address is the
// one that answered.
//
// Which address is asked is askRemembered's business — the one that answered last
// sweep, alone — and it shares that memory with the enrichment path, so both
// converge on one address per peer instead of each rediscovering it.
func (d *daemon) refreshNodeModelsCandidates(hostUUID, guardIP string, hosts []string, emPort int, clusterUUID string) bool {
	if d.reg != nil && hostUUID == d.reg.record().HostUUID {
		hosts = []string{loopbackHost}
	}
	ask := func(host string) (modelInventory, bool) {
		models, byEngine, loadedByEngine, loadedVRAMByEngine, fetched := d.fetchModels(host, emPort, clusterUUID)
		if !fetched {
			return modelInventory{}, false
		}
		return modelInventory{models: models, byEngine: byEngine, loadedByEngine: loadedByEngine, loadedVRAMByEngine: loadedVRAMByEngine}, true
	}
	key := hostKey{hostUUID: hostUUID, service: noderec.ServiceEngineManager}
	_, inventory, fetched := askRemembered(&d.enrichHosts, key, hosts, ask)
	if !fetched {
		return false
	}
	models, byEngine, loadedByEngine := inventory.models, inventory.byEngine, inventory.loadedByEngine
	// Refresh the enrichment cache BEFORE touching the directory: the caches
	// (infoMu) and the directory (dir.mu) are guarded by different locks, so if
	// the directory were updated first, a concurrent onBrowse->enrichModelsAt whose
	// own fetch fails would fall back to a still-stale lastModels and upsert the
	// old inventory, reverting this update. Writing the cache first means that
	// fallback reads the fresh value. The only side effect — caching a value for
	// a node applyModels then finds removed — is benign (it is the last-known-good
	// value the cache exists to preserve, and forget() clears it on eviction).
	d.infoMu.Lock()
	if d.lastModels == nil {
		d.lastModels = make(map[string][]string)
	}
	if d.lastModelsByEngine == nil {
		d.lastModelsByEngine = make(map[string]map[string][]string)
	}
	if d.lastLoadedByEngine == nil {
		d.lastLoadedByEngine = make(map[string]map[string][]string)
	}
	d.lastModels[hostUUID] = models
	d.lastModelsByEngine[hostUUID] = byEngine
	d.lastLoadedByEngine[hostUUID] = loadedByEngine
	if d.lastLoadedVRAMByEngine == nil {
		d.lastLoadedVRAMByEngine = make(map[string]map[string]map[string]uint64)
	}
	d.lastLoadedVRAMByEngine[hostUUID] = inventory.loadedVRAMByEngine
	d.infoMu.Unlock()

	node, changed, valid := d.dir.applyModels(hostUUID, guardIP, emPort, models, byEngine, loadedByEngine, inventory.loadedVRAMByEngine)
	if !valid {
		return false
	}
	if changed {
		d.emit(noderec.NotifyNodeUpdated, node)
	}
	return changed
}

// forget drops a node's cached enrichment when it's evicted, so a later
// re-discovery re-probes from scratch rather than resurrecting stale metrics.
func (d *daemon) forget(hostUUID string) {
	d.telemetryRetries.forget(hostUUID)
	d.infoMu.Lock()
	delete(d.lastInfo, hostUUID)
	delete(d.lastInfoAt, hostUUID)
	delete(d.lastModels, hostUUID)
	delete(d.lastModelsByEngine, hostUUID)
	delete(d.lastLoadedByEngine, hostUUID)
	delete(d.lastLoadedVRAMByEngine, hostUUID)
	delete(d.nodeInfoDown, hostUUID)
	d.infoMu.Unlock()
	d.activityMu.Lock()
	delete(d.lastActivityAt, hostUUID)
	d.activityMu.Unlock()
}

// noteActivity records that a peer returned inference response bytes msSince
// milliseconds ago. The broker relays this from whichever proxy saw the bytes;
// an age rather than a timestamp keeps the two processes off each other's clock.
//
// Our own uuid is dropped. A proxy identifies targets by URL and port and cannot
// tell which uuid is this machine's, so it reports every node it streamed from
// and the filtering happens here, where identity actually lives. Self is never
// evicted by a browse miss anyway (onBrowse ignores our own uuid), so recording
// it would only grow the map.
func (d *daemon) noteActivity(hostUUID string, msSince int64) {
	if hostUUID == "" {
		return
	}
	if d.reg != nil && hostUUID == d.reg.record().HostUUID {
		return
	}
	// Bound the age at both ends before it becomes a duration. A negative value,
	// or one large enough to overflow the multiplication, would place the
	// observation in the future — and a future observation never expires, so the
	// node could never be probed again. This is the consumer, so it validates the
	// number rather than assuming the relay already did.
	if msSince < 0 {
		msSince = 0
	}
	if maxMS := int64(activityFreshness / time.Millisecond); msSince > maxMS {
		msSince = maxMS
	}
	at := time.Now().Add(-time.Duration(msSince) * time.Millisecond)
	d.activityMu.Lock()
	defer d.activityMu.Unlock()
	// Reports from two proxies can arrive out of order; keep the newest.
	if prev, ok := d.lastActivityAt[hostUUID]; ok && prev.After(at) {
		return
	}
	d.lastActivityAt[hostUUID] = at
}

// activitySince reports how long ago a peer last returned inference bytes, and
// whether anything has ever been reported for it.
func (d *daemon) activitySince(hostUUID string) (time.Duration, bool) {
	d.activityMu.Lock()
	defer d.activityMu.Unlock()
	at, ok := d.lastActivityAt[hostUUID]
	if !ok {
		return 0, false
	}
	return time.Since(at), true
}

// handle dispatches the discovery:* methods, returning true if msg was one of
// them. register/unregister/update-txt update the registry and re-advertise on a
// real change; register is acked when sent as a request. get-nodes returns the
// filtered directory snapshot.
func (d *daemon) handle(msg *Message) bool {
	switch msg.Method {
	case noderec.MethodRegister, noderec.MethodUpdateTXT:
		var p noderec.RegisterParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			if msg.IsRequest() {
				_ = d.codec.RespondError(msg.ID, -32602, err.Error())
			}
			return true
		}
		if d.reg.register(p) {
			if d.responder != nil {
				d.responder.UpdateTXT(d.reg.txt())
			}
			// Reflect the new service set into our own directory entry. Async so a
			// registration burst at startup can't serialize the read loop behind
			// the loopback enrichment fetch.
			go d.publishSelf()
		}
		if msg.IsRequest() {
			_ = d.codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
		return true

	case noderec.MethodUnregister:
		var p noderec.UnregisterParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			if msg.IsRequest() {
				_ = d.codec.RespondError(msg.ID, -32602, err.Error())
			}
			return true
		}
		if d.reg.unregister(p.Service) {
			if d.responder != nil {
				d.responder.UpdateTXT(d.reg.txt())
			}
			// Drop the service from our own directory entry (e.g. an engine going
			// away). Self stays present; only its service set shrinks.
			go d.publishSelf()
		}
		if msg.IsRequest() {
			_ = d.codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
		return true

	case noderec.MethodGetNodes:
		var p noderec.GetNodesParams
		_ = json.Unmarshal(msg.Params, &p) // an unparseable/empty filter returns all
		if msg.IsRequest() {
			_ = d.codec.Respond(msg.ID, noderec.GetNodesResult{Nodes: d.dir.snapshot(p.Service)})
		}
		return true

	case noderec.MethodReloadIdentity:
		d.reloadIdentity()
		if msg.IsRequest() {
			_ = d.codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
		return true

	case noderec.MethodReloadTrust:
		d.reloadTrust()
		if msg.IsRequest() {
			_ = d.codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
		return true

	case noderec.MethodSetObservedAddresses:
		var p noderec.ObservedAddressesParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			if msg.IsRequest() {
				_ = d.codec.RespondError(msg.ID, -32602, err.Error())
			}
			return true
		}
		d.setObservedAddresses(p.Addresses)
		if msg.IsRequest() {
			_ = d.codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
		return true

	case noderec.MethodNodeActivity:
		var p noderec.NodeActivityParams
		if err := json.Unmarshal(msg.Params, &p); err != nil {
			if msg.IsRequest() {
				_ = d.codec.RespondError(msg.ID, -32602, err.Error())
			}
			return true
		}
		d.noteActivity(p.HostUUID, p.MSSince)
		if msg.IsRequest() {
			_ = d.codec.Respond(msg.ID, map[string]bool{"ok": true})
		}
		return true
	}
	return false
}
