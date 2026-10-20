// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/discovery"
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
	// modelsHTTP fetches the local /v1/models. It gets a longer timeout than node-info
	// (node-info): engine-manager's models sweep is bounded by its own, larger
	// deadline, so a peer with several engines can legitimately take longer than
	// a node-info probe. A shorter client would give up mid-sweep and silently
	// drop that peer's models.
	modelsHTTP *http.Client
	// modelsClients is the long-lived per-peer mTLS pool for LAN /v1/models
	// fetches. A throwaway Transport every 15s paid a full handshake each time.
	modelsClients *clustertrust.PeerClientPool
	// modelsWake coalesces newly verified peers into the next model sweep.
	modelsWake chan struct{}
	// nodeInfoClients uses exact cluster pins, independently of public display HTTP.
	nodeInfoClients *clustertrust.PeerClientPool
	// peerMu serializes candidate generations, proof invalidation, and accepted
	// results. No network request runs while it is held.
	peerMu           sync.Mutex
	candidates       map[string]*discoveryCandidate
	peerCertificates map[string]*x509.Certificate
	authWake         chan struct{}
	authRetries      telemetryRetryGate

	// nodeInfoOrigins queues verified same-origin reads before http.Client starts
	// its timeout. Candidate authentication skips busy origins and retries later.
	// The peer client pool enforces the matching connection bound.
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

	// selfMu serializes registry publications, identity/address changes and owned
	// hardware snapshots so they agree on this node's identity. Background model
	// refreshes use dir.mu independently.
	selfMu sync.Mutex
	// localInfo is supplied only by the owned worker IPC path, under selfMu.
	localInfo *noderec.NodeInfo

	// infoMu guards peer read timestamps, model inventories, and outage state.
	infoMu sync.Mutex
	// lastInfoAt records accepted node-info reads for authenticated peer liveness.
	lastInfoAt map[string]time.Time
	// These model caches preserve owned-local inventory when publication rebuilds
	// the local record during an outage. Remote records retain models in the directory.
	lastModels         map[string][]string
	lastModelsByEngine map[string]map[string][]string
	lastLoadedByEngine map[string]map[string][]string
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
	telemetryRetries telemetryRetryGate

	// observedMu guards observed, this host's own addresses that remote peers
	// have actually connected to. nvpair-node-info sees those connections and the
	// broker relays the set here, because this daemon decides what the node
	// publishes and a peer-completed connection is the only direct proof that an
	// address works from somewhere other than this machine.
	observedMu sync.Mutex
	observed   map[string]bool
}

// nodeInfoFetchTimeout bounds authenticated node-info requests after origin queuing.
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

// modelsFetchTimeout bounds a peer /v1/models GET. It exceeds engine-manager's
// own models-sweep cap (modelsTimeout, 5s) so the fetch waits for a bounded
// sweep to finish instead of timing out partway through it.
const modelsFetchTimeout = 6 * time.Second

// peerRefreshInterval is the cadence of the mDNS-independent convergence loop
// (refreshPeersLoop). Newly authenticated peers wake model refresh immediately;
// this is the background sweep that catches changes that never move
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

// newDaemon builds the registry publisher and pinned peer discovery daemon.
func newDaemon(codec *Codec, mesh *clustertrust.Mesh, clusterDir string) *daemon {
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
		modelsHTTP:         &http.Client{Timeout: modelsFetchTimeout},
		modelsClients:      clustertrust.NewPeerClientPool(mesh, modelsFetchTimeout),
		modelsWake:         make(chan struct{}, 1),
		nodeInfoClients:    newNodeInfoClients(mesh),
		candidates:         make(map[string]*discoveryCandidate),
		peerCertificates:   make(map[string]*x509.Certificate),
		authWake:           make(chan struct{}, 1),
		lastInfoAt:         make(map[string]time.Time),
		lastModels:         make(map[string][]string),
		lastModelsByEngine: make(map[string]map[string][]string),
		lastLoadedByEngine: make(map[string]map[string][]string),
		nodeInfoDown:       make(map[string]bool),
		lastActivityAt:     make(map[string]time.Time),
	}
	// Track connection candidates independently of identity claims. Promoted
	// peers survive multicast gaps using authenticated reads or inference.
	d.browser = NewDiscovery(noderec.ServiceType, noderec.Domain,
		WithLivenessProbe(func(n RawNode) bool { return d.reachable(n) }),
		WithKeyFunc(nodeDiscoveryRecordID),
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
	// Refresh peer inventory periodically and models after authentication, so
	// model changes converge independently of mDNS. Stops with ctx.
	go d.refreshPeersLoop(ctx)
	go d.authenticateCandidatesLoop(ctx)
	go d.refreshTelemetryLoop(ctx)
	for {
		select {
		case <-ctx.Done():
			if d.modelsClients != nil {
				d.modelsClients.CloseIdle()
			}
			if d.nodeInfoClients != nil {
				d.nodeInfoClients.CloseIdle()
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

type discoveryCandidate struct {
	node noderec.DirectoryNode
	// Retain the last authenticated source link for expiry while a changed
	// announcement awaits proof. It does not authenticate the pending claims.
	peerUUID string
	pending  bool
}

type peerInfoSample struct {
	info             NodeInfoResponse
	certificate      *x509.Certificate
	localCertificate []byte
	receivedAt       time.Time
}

func newNodeInfoClients(mesh *clustertrust.Mesh) *clustertrust.PeerClientPool {
	return clustertrust.NewPeerClientPoolOpts(mesh, clustertrust.PeerClientOptions{
		Timeout: nodeInfoFetchTimeout, DialTimeout: nodeInfoDialTimeout, MaxConnsPerHost: 1,
	})
}

func (d *daemon) wakeAuthentication() {
	select {
	case d.authWake <- struct{}{}:
	default:
	}
}

// authenticateCandidatesLoop has its own permits and retry state. Pending
// discoveries cannot occupy the verified telemetry workers or their host memory.
func (d *daemon) authenticateCandidatesLoop(ctx context.Context) {
	ticker := time.NewTicker(telemetryRefreshInterval)
	defer ticker.Stop()
	sem := make(chan struct{}, telemetryRefreshConcurrency)
	var workers sync.WaitGroup
	defer workers.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.authWake:
		case <-ticker.C:
		}
		d.mesh.Refresh()
		d.reconcileTrust()
		d.dispatchCandidates(ctx, sem, &workers)
	}
}

func (d *daemon) dispatchCandidates(ctx context.Context, sem chan struct{}, workers *sync.WaitGroup) {
	d.peerMu.Lock()
	defer d.peerMu.Unlock()
	for id, candidate := range d.candidates {
		if !candidate.pending {
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			return
		}
		target := telemetryTargetKey(candidate.node.CandidateIPs(), candidate.node.Services[noderec.ServiceNodeInfo].Port) + candidate.node.ClusterUUID
		token, due := d.authRetries.claim(id, target, time.Now())
		if !due {
			<-sem
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-sem }()
			ok := d.authenticateCandidate(ctx, id, candidate)
			d.authRetries.finish(id, token, ok, time.Now())
		}()
	}
}

func (d *daemon) authenticateCandidate(ctx context.Context, id string, candidate *discoveryCandidate) bool {
	sample, ok := d.readPeerInfo(ctx, candidate.node)
	if !ok {
		return false
	}
	d.peerMu.Lock()
	defer d.peerMu.Unlock()
	if d.candidates[id] != candidate || !d.currentPin(sample) {
		return false
	}
	if sample.info.HostUUID == d.reg.record().HostUUID {
		return false
	}
	candidate.peerUUID = sample.info.HostUUID
	candidate.pending = false
	d.publishPeerInfo(id, sample)
	return true
}

// readPeerInfo authenticates the exact endpoint and response together. Claimed
// host identity cannot switch a known host to another pinned principal.
func (d *daemon) readPeerInfo(ctx context.Context, node noderec.DirectoryNode) (peerInfoSample, bool) {
	principal := node.ClusterUUID
	if known, ok := d.dir.get(node.HostUUID); ok && known.IdentityVerification == noderec.IdentityPinned {
		principal = known.ClusterUUID
	}
	if principal == "" {
		principal = node.HostUUID
	}
	d.mesh.Refresh()
	if principal == "" || principal != node.HostUUID || !d.mesh.HasPin(principal) || d.nodeInfoClients == nil {
		return peerInfoSample{}, false
	}
	port := node.Services[noderec.ServiceNodeInfo].Port
	if port < 1 || port > 65535 {
		return peerInfoSample{}, false
	}
	config, ok := d.mesh.ClientTLSConfig(principal)
	if !ok {
		return peerInfoSample{}, false
	}
	ask := func(host string) (peerInfoSample, bool) {
		started := time.Now()
		origin := "https://" + net.JoinHostPort(host, strconv.Itoa(port))
		var release func()
		var acquired bool
		if node.IdentityVerification == noderec.IdentityPinned {
			release, acquired = d.nodeInfoOrigins.acquire(ctx, origin)
		} else {
			release, acquired = d.nodeInfoOrigins.tryAcquire(origin)
		}
		if !acquired {
			return peerInfoSample{}, false
		}
		defer release()
		requestCtx, cancel := context.WithTimeout(ctx, nodeInfoFetchTimeout)
		defer cancel()
		client, ok := d.nodeInfoClients.Client(principal)
		if !ok {
			return peerInfoSample{}, false
		}
		req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, origin+"/v1/node-info", nil)
		if err != nil {
			return peerInfoSample{}, false
		}
		resp, err := client.Do(req)
		if err != nil {
			return peerInfoSample{}, false
		}
		defer func() {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, nodeInfoResponseDrainLimit))
			_ = resp.Body.Close()
		}()
		if resp.StatusCode != http.StatusOK || resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
			return peerInfoSample{}, false
		}
		var info NodeInfoResponse
		body, err := io.ReadAll(io.LimitReader(resp.Body, nodeInfoResponseDrainLimit+1))
		if err != nil || len(body) > nodeInfoResponseDrainLimit {
			return peerInfoSample{}, false
		}
		if err := json.Unmarshal(body, &info); err != nil {
			return peerInfoSample{}, false
		}
		if info.HostUUID != principal || info.ClusterUUID == nil || *info.ClusterUUID != principal || !validPeerMetadata(info.Peer) {
			return peerInfoSample{}, false
		}
		sample := peerInfoSample{info: info.Aged(time.Since(started)), certificate: resp.TLS.PeerCertificates[0], localCertificate: config.Certificates[0].Certificate[0], receivedAt: time.Now()}
		if !d.currentPin(sample) {
			return peerInfoSample{}, false
		}
		return sample, true
	}
	// Candidate address memory must never replace a verified host's target.
	key := node.DiscoveryRecordID
	_, sample, ok := askRemembered(&d.enrichHosts, hostKey{hostUUID: key, service: noderec.ServiceNodeInfo}, node.CandidateIPs(), ask)
	return sample, ok
}

func validPeerMetadata(peer *noderec.PeerMetadata) bool {
	if peer == nil || peer.Name == "" || len(peer.Addresses) == 0 || len(peer.Addresses) > noderec.MaxAdvertisedIPs {
		return false
	}
	for _, ip := range peer.Addresses {
		if net.ParseIP(ip) == nil {
			return false
		}
	}
	if peer.Services[noderec.ServiceNodeInfo] == 0 {
		return false
	}
	for _, port := range peer.Services {
		if port < 1 || port > 65535 {
			return false
		}
	}
	return true
}

func (d *daemon) currentPin(sample peerInfoSample) bool {
	d.mesh.Refresh()
	config, ok := d.mesh.ClientTLSConfig(sample.info.HostUUID)
	return ok && bytes.Equal(sample.localCertificate, config.Certificates[0].Certificate[0]) && d.mesh.VerifyPeerCertificate(sample.info.HostUUID, sample.certificate)
}

// currentPeerResult also rejects results for an endpoint replaced while a read
// was queued or in flight. peerMu must be held through the subsequent write.
func (d *daemon) currentPeerResult(target noderec.DirectoryNode, sample peerInfoSample) bool {
	if !d.currentPin(sample) {
		return false
	}
	node, ok := d.dir.get(target.HostUUID)
	return ok && node.IdentityVerification == noderec.IdentityPinned && node.DiscoveryRecordID == target.DiscoveryRecordID && node.ClusterUUID == target.ClusterUUID && node.Services[noderec.ServiceNodeInfo].Port == target.Services[noderec.ServiceNodeInfo].Port && slices.Equal(node.CandidateIPs(), target.CandidateIPs()) && d.mesh.VerifyPeerCertificate(node.ClusterUUID, d.peerCertificates[node.HostUUID])
}

func (d *daemon) recordPeerInfoSuccess(uuid string) {
	d.infoMu.Lock()
	defer d.infoMu.Unlock()
	if d.lastInfoAt == nil {
		d.lastInfoAt = make(map[string]time.Time)
	}
	d.lastInfoAt[uuid] = time.Now()
}

// publishPeerInfo stores authenticated inventory and emits directory changes.
// The caller must hold peerMu to serialize proof, cache, and directory updates
// with peer invalidation.
func (d *daemon) publishPeerInfo(candidateID string, sample peerInfoSample) {
	info := sample.info.Aged(time.Since(sample.receivedAt))
	services := make(map[noderec.ServiceKey]noderec.ServiceStatus, len(info.Peer.Services))
	for key, port := range info.Peer.Services {
		services[key] = noderec.ServiceStatus{Port: port}
	}
	node := noderec.DirectoryNode{
		DiscoveryRecordID: "peer:" + info.HostUUID, IdentityVerification: noderec.IdentityPinned,
		HostUUID: info.HostUUID, ClusterUUID: *info.ClusterUUID, Trusted: true,
		Name: info.Peer.Name, IP: info.Peer.Addresses[0], IPs: info.Peer.Addresses, Services: services,
		GPUs: info.GPUs, CPU: info.CPU, Memory: info.Memory,
	}
	if d.peerCertificates == nil {
		d.peerCertificates = make(map[string]*x509.Certificate)
	}
	d.peerCertificates[info.HostUUID] = sample.certificate
	d.recordPeerInfoSuccess(info.HostUUID)
	removed, discovered, changed := d.dir.promote(candidateID, node)
	if removed.DiscoveryRecordID != "" {
		d.emit(noderec.NotifyNodeRemoved, removed)
	}
	if changed {
		method := noderec.NotifyNodeUpdated
		if discovered {
			method = noderec.NotifyNodeDiscovered
		}
		current, _ := d.dir.get(info.HostUUID)
		d.emit(method, current)
	}
	if discovered {
		select {
		case d.modelsWake <- struct{}{}:
		default:
		}
	}
	var utilization uint32
	for _, gpu := range info.GPUs {
		if gpu.UtilizationPercent > utilization {
			utilization = gpu.UtilizationPercent
		}
	}
	info = sample.info.Aged(time.Since(sample.receivedAt))
	d.emitTelemetry(noderec.NodeTelemetry{HostUUID: info.HostUUID, GPUUtilizationPct: utilization, TelemetryValid: info.TelemetryValid, MSSince: info.MSSince})
	d.emitNodeInfo(node, sample.info.Aged(time.Since(sample.receivedAt)))
}

// onBrowse stores each discovery independently. Network claims never mutate a
// canonical host, including when a discovery changes or disappears.
func (d *daemon) onBrowse(ev DiscoveryEvent) {
	var node noderec.DirectoryNode
	alive := false
	if ev.Type == discovery.Removed {
		// Find the stored discovery even if its latest announcement changed or
		// dropped the UUID claim. Cleanup must not depend on validating that claim.
		d.peerMu.Lock()
		candidate := d.candidates[nodeDiscoveryRecordID(ev.Node)]
		peerUUID := ""
		if candidate != nil {
			node = candidate.node
			peerUUID = candidate.peerUUID
		}
		d.peerMu.Unlock()
		if candidate == nil {
			return
		}
		// Judge the canonical peer separately from the expiring observation.
		// A failed update may still have an older authenticated source link.
		alive = d.peerReachable(peerUUID)
	} else {
		if UUIDFromTXT(ev.Node.TXT) == d.reg.record().HostUUID {
			return
		}
		var ok bool
		node, ok = toDirectoryNode(ev.Node)
		if !ok {
			return
		}
	}
	d.peerMu.Lock()
	defer d.peerMu.Unlock()
	if ev.Type == discovery.Removed {
		candidate := d.candidates[node.DiscoveryRecordID]
		delete(d.candidates, node.DiscoveryRecordID)
		d.authRetries.forget(node.DiscoveryRecordID)
		if d.dir.remove(node.DiscoveryRecordID) {
			d.emit(noderec.NotifyNodeRemoved, node)
		}
		if candidate != nil && candidate.peerUUID != "" && !alive {
			// Another authenticated discovery still represents this peer.
			for _, other := range d.candidates {
				if other.peerUUID == candidate.peerUUID {
					return
				}
			}
			if peer, present := d.dir.get(candidate.peerUUID); present {
				d.dir.remove(peer.HostUUID)
				delete(d.peerCertificates, peer.HostUUID)
				d.forget(peer.HostUUID)
				d.emit(noderec.NotifyNodeRemoved, peer)
			}
		}
		return
	}
	if d.candidates == nil {
		d.candidates = make(map[string]*discoveryCandidate)
	}
	peerUUID := ""
	if previous := d.candidates[node.DiscoveryRecordID]; previous != nil {
		peerUUID = previous.peerUUID
	}
	d.candidates[node.DiscoveryRecordID] = &discoveryCandidate{node: node, peerUUID: peerUUID, pending: true}
	method := noderec.NotifyNodeUpdated
	if d.dir.upsert(node) {
		method = noderec.NotifyNodeDiscovered
	}
	d.emit(method, node)
	d.wakeAuthentication()
}

// reloadTrust invalidates stale proofs and schedules authentication after a pin change.
func (d *daemon) reloadTrust() {
	d.mesh.Refresh()
	// A pin can flip membership on its own (a cluster dir predating admission.json
	// is clustered on pins alone), which changes what we advertise, so settle
	// identity before reconciling peer proofs.
	d.reconcileIdentity()
	d.reconcileTrust()
	d.peerMu.Lock()
	for id := range d.candidates {
		d.authRetries.forget(id)
	}
	d.peerMu.Unlock()
	d.wakeAuthentication()
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

// reconcileTrust removes proofs whose exact pin is no longer current.
// A new pin schedules authentication; it cannot make an announcement trusted.
func (d *daemon) reconcileTrust() {
	d.peerMu.Lock()
	defer d.peerMu.Unlock()
	for uuid, cert := range d.peerCertificates {
		if d.mesh.VerifyPeerCertificate(uuid, cert) {
			continue
		}
		delete(d.peerCertificates, uuid)
		for _, candidate := range d.candidates {
			if candidate.peerUUID == uuid {
				candidate.peerUUID = ""
				candidate.pending = true
			}
		}
		if node, ok := d.dir.get(uuid); ok {
			d.dir.remove(uuid)
			d.forget(uuid)
			d.emit(noderec.NotifyNodeRemoved, node)
		}
	}
	if d.nodeInfoClients != nil {
		d.nodeInfoClients.DropUnpinned()
	}
	if d.modelsClients != nil {
		d.modelsClients.DropUnpinned()
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

func (d *daemon) emitNodeInfo(node noderec.DirectoryNode, info noderec.NodeInfo) {
	if d.codec == nil {
		return
	}
	if err := d.codec.Notify(noderec.NotifyNodeInfo, noderec.NodeInfoEvent{
		DiscoveryRecordID: node.DiscoveryRecordID, HostUUID: node.HostUUID,
		ClusterUUID: node.ClusterUUID, IdentityVerification: node.IdentityVerification, Info: info,
	}); err != nil {
		slog.Warn("failed to emit node-info", "host_uuid", node.HostUUID, "err", err)
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
		if n.HostUUID == self || n.IdentityVerification != noderec.IdentityPinned {
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
// carries its model caches onto the new uuid, and emits removed(old) so consumers
// reconcile.
// The new entry is (re)published by the caller via publishSelfLocked. Must be
// called with selfMu held.
func (d *daemon) dropSelf(oldHostUUID, newHostUUID string) {
	node, existed := d.dir.get(oldHostUUID)
	d.dir.remove(oldHostUUID)
	d.infoMu.Lock()
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
	delete(d.lastInfoAt, oldHostUUID)
	delete(d.lastModels, oldHostUUID)
	delete(d.lastModelsByEngine, oldHostUUID)
	delete(d.lastLoadedByEngine, oldHostUUID)
	d.infoMu.Unlock()
	if existed {
		d.emit(noderec.NotifyNodeRemoved, node)
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
		DiscoveryRecordID:    "local:" + rec.HostUUID,
		IdentityVerification: noderec.IdentityLocal,
		HostUUID:             rec.HostUUID,
		Name:                 d.instance,
		IP:                   ip,
		IPs:                  ips,
		ClusterUUID:          rec.ClusterUUID,
		Trusted:              trusted,
		Services:             svcs,
		LastSeen:             time.Now().Unix(),
	}
	if d.localInfo != nil && d.localInfo.HostUUID == rec.HostUUID {
		node.GPUs, node.CPU, node.Memory = d.localInfo.GPUs, d.localInfo.CPU, d.localInfo.Memory
	}
	// Publication never waits for HTTP while holding selfMu. The model sweep
	// refreshes the owned engine-manager independently and retains its last answer.
	// Serialize the cache copy and publication with its model apply/cache write.
	d.peerMu.Lock()
	defer d.peerMu.Unlock()
	if node.HasService(noderec.ServiceEngineManager) {
		d.infoMu.Lock()
		node.Models = d.lastModels[rec.HostUUID]
		node.ModelsByEngine = d.lastModelsByEngine[rec.HostUUID]
		node.LoadedByEngine = d.lastLoadedByEngine[rec.HostUUID]
		d.infoMu.Unlock()
	}

	method := noderec.NotifyNodeUpdated
	if isNew := d.dir.upsert(node); isNew {
		method = noderec.NotifyNodeDiscovered
	}
	d.emit(method, node)
	select {
	case d.modelsWake <- struct{}{}:
	default:
	}
}

// acceptLocalNodeInfo applies an owned IPC snapshot to its matching local
// registry record. It updates changed inventory and emits maximum GPU usage
// for scheduling, including time spent processing the sample in its age.
func (d *daemon) acceptLocalNodeInfo(info noderec.NodeInfo) {
	receivedAt := time.Now()
	d.selfMu.Lock()
	defer d.selfMu.Unlock()
	self := d.reg.record().HostUUID
	if self == "" || info.HostUUID != self {
		return
	}
	info = info.Aged(0)
	d.localInfo = &info
	node, result := d.dir.applyLocalInfo(info)
	switch result {
	case localInfoMissing:
		// Build identity and services from the registry, not the hardware snapshot.
		d.publishSelfLocked()
	case localInfoUpdated:
		d.emit(noderec.NotifyNodeUpdated, node)
	}
	var utilization uint32
	for _, gpu := range info.GPUs {
		if gpu.UtilizationPercent > utilization {
			utilization = gpu.UtilizationPercent
		}
	}
	aged := info.Aged(time.Since(receivedAt))
	d.emitTelemetry(noderec.NodeTelemetry{
		HostUUID: self, GPUUtilizationPct: utilization,
		TelemetryValid: aged.TelemetryValid, MSSince: aged.MSSince,
	})
	if current, ok := d.dir.get(self); ok {
		d.emitNodeInfo(current, info.Aged(time.Since(receivedAt)))
	}
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

// reachableInfoFreshness is how long a successful node-info enrichment keeps a
// threshold-missed discovery (~2× default discovery scan interval).
const reachableInfoFreshness = 10 * time.Second

// retainedPeerGrace gives peers whose last discovery expired a minute without
// authenticated inventory before removal. Recent inference also keeps them.
const retainedPeerGrace = time.Minute

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

// loopbackHost reaches the owned engine-manager for model inventory. Local
// node-info inventory and telemetry arrive over private worker IPC.
const loopbackHost = "127.0.0.1"

// reachable retains a promoted discovery using its canonical peer's authenticated
// inventory or approved inference activity. An open port is not identity proof.
func (d *daemon) reachable(n RawNode) bool {
	// An unverified observation cannot borrow an established host's liveness.
	d.peerMu.Lock()
	candidate := d.candidates[nodeDiscoveryRecordID(n)]
	if candidate == nil || candidate.pending || candidate.peerUUID == "" {
		d.peerMu.Unlock()
		return false
	}
	uuid := candidate.peerUUID
	d.peerMu.Unlock()
	return d.peerReachable(uuid)
}

func (d *daemon) peerReachable(uuid string) bool {
	if uuid == "" {
		return false
	}
	d.peerMu.Lock()
	node, ok := d.dir.get(uuid)
	cert := d.peerCertificates[uuid]
	d.mesh.Refresh()
	valid := ok && d.mesh.VerifyPeerCertificate(node.ClusterUUID, cert)
	d.peerMu.Unlock()
	if !valid {
		return false
	}
	d.infoMu.Lock()
	at := d.lastInfoAt[node.HostUUID]
	d.infoMu.Unlock()
	if !at.IsZero() && time.Since(at) < reachableInfoFreshness {
		return true
	}
	if since, ok := d.activitySince(node.HostUUID); ok && since < activityFreshness {
		return true
	}
	return false
}

// noteNodeInfo logs outage and recovery transitions once each. Failed samples
// leave the last accepted inventory in place while scheduling measurements age.
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

// fetchModels reads /v1/models and retains the server certificate so a remote
// result can be rechecked before updating the directory or caches. Response shape:
// {"models":["name",...],"modelsByEngine":{"ollama":[...]},"loadedByEngine":{"ollama":[...]}}.
// modelsByEngine and loadedByEngine are additive (omitempty on the wire); a peer
// that doesn't send them yields nil maps, and the flat union still enriches the
// node. A present engine key with [] is a successful empty inventory.
// Peers use pinned mTLS and their verified clusterUUID. The local engine-manager
// uses loopback HTTP, including outside a cluster.
func (d *daemon) fetchModels(ip string, port int, clusterUUID string) (modelInventory, bool) {
	client, scheme, ok := d.modelsClient(ip, clusterUUID)
	if !ok {
		slog.Debug("model enrichment skipped: peer is not a pinned cluster member",
			"ip", ip, "clusterUuid", clusterUUID)
		return modelInventory{}, false
	}
	url := scheme + "://" + net.JoinHostPort(ip, strconv.Itoa(port)) + "/v1/models"
	resp, err := client.Get(url)
	if err != nil {
		slog.Debug("model enrichment failed", "ip", ip, "url", url, "err", err)
		return modelInventory{}, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return modelInventory{}, false
	}
	var body struct {
		Models         []string            `json:"models"`
		ModelsByEngine map[string][]string `json:"modelsByEngine"`
		LoadedByEngine map[string][]string `json:"loadedByEngine"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return modelInventory{}, false
	}
	inventory := modelInventory{models: body.Models, byEngine: body.ModelsByEngine, loadedByEngine: body.LoadedByEngine}
	if scheme == "https" {
		d.mesh.Refresh()
		if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 || !d.mesh.VerifyPeerCertificate(clusterUUID, resp.TLS.PeerCertificates[0]) {
			return modelInventory{}, false
		}
		inventory.certificate = resp.TLS.PeerCertificates[0]
	}
	return inventory, true
}

// modelsClient picks the transport for a model fetch: the plain client over
// loopback (this node's own engine-manager, which serves loopback in plaintext in
// every membership state), or a per-peer pinned mTLS client for a LAN peer. ok is
// false when a peer holds no pin here, which is the client-side cluster gate —
// there is deliberately no plaintext fallback for a peer.
func (d *daemon) modelsClient(ip, clusterUUID string) (*http.Client, string, bool) {
	if clusterUUID == "" && isLoopbackHost(ip) {
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

// refreshPeersLoop refreshes models and local address selection periodically.
// Local publications and newly authenticated peers wake models immediately. Sweeps
// finish before the next starts, sharing the same model concurrency limit.
func (d *daemon) refreshPeersLoop(ctx context.Context) {
	ticker := time.NewTicker(peerRefreshInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.modelsWake:
			d.refreshModelsOnce()
		case <-ticker.C:
			d.refreshAdvertisedAddressesOnce()
			d.mesh.Refresh()
			if d.modelsClients != nil {
				d.modelsClients.DropUnpinned()
			}
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
		if n.HostUUID == self || n.IdentityVerification != noderec.IdentityPinned {
			continue // local telemetry arrives through discovery:local-node-info
		}
		hosts := append([]string(nil), n.CandidateIPs()...)
		if len(hosts) == 0 {
			continue
		}
		targetKey := telemetryTargetKey(hosts, ni.Port)
		retryToken, due := d.telemetryRetries.claim(n.HostUUID, targetKey, now)
		if !due {
			continue
		}

		workers.Add(1)
		go func(hostUUID string, hosts []string, port int, token uint64) {
			defer workers.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				d.telemetryRetries.finish(hostUUID, token, true, time.Now())
				return
			}
			ok := d.refreshNodeTelemetryCandidates(ctx, hostUUID, hosts, port)
			if ctx.Err() != nil {
				ok = true
			}
			d.telemetryRetries.finish(hostUUID, token, ok, time.Now())
		}(n.HostUUID, hosts, ni.Port, retryToken)
	}
}

// refreshNodeTelemetryCandidates reads the current canonical peer target once
// for inventory and scheduling. Failed reads retain inventory while samples age;
// a retained peer can expire after its discoveries and availability evidence end.
func (d *daemon) refreshNodeTelemetryCandidates(ctx context.Context, hostUUID string, hosts []string, niPort int) bool {
	node, ok := d.dir.get(hostUUID)
	if !ok || node.IdentityVerification != noderec.IdentityPinned || node.Services[noderec.ServiceNodeInfo].Port != niPort || !slices.Equal(node.CandidateIPs(), hosts) {
		return false
	}
	sample, ok := d.readPeerInfo(ctx, node)
	if !ok {
		if ctx.Err() == nil {
			d.noteNodeInfo(hostUUID, net.JoinHostPort(node.IP, strconv.Itoa(niPort)), false)
			d.expireRetainedPeer(node)
		}
		return false
	}
	d.peerMu.Lock()
	defer d.peerMu.Unlock()
	if !d.currentPeerResult(node, sample) {
		return false
	}
	d.noteNodeInfo(hostUUID, net.JoinHostPort(node.IP, strconv.Itoa(niPort)), true)
	d.publishPeerInfo("", sample)
	return true
}

// expireRetainedPeer removes an unavailable peer after its last discovery has
// expired. The caller has just failed to refresh target. Membership and pins stay.
func (d *daemon) expireRetainedPeer(target noderec.DirectoryNode) {
	d.peerMu.Lock()
	defer d.peerMu.Unlock()
	for _, candidate := range d.candidates {
		if candidate.peerUUID == target.HostUUID {
			return
		}
	}
	peer, present := d.dir.get(target.HostUUID)
	if !present || peer.IdentityVerification != noderec.IdentityPinned ||
		peer.DiscoveryRecordID != target.DiscoveryRecordID || peer.ClusterUUID != target.ClusterUUID ||
		peer.Services[noderec.ServiceNodeInfo].Port != target.Services[noderec.ServiceNodeInfo].Port ||
		!slices.Equal(peer.CandidateIPs(), target.CandidateIPs()) {
		return
	}
	d.infoMu.Lock()
	at := d.lastInfoAt[peer.HostUUID]
	d.infoMu.Unlock()
	if time.Since(at) < retainedPeerGrace {
		return
	}
	if since, ok := d.activitySince(peer.HostUUID); ok && since < activityFreshness {
		return
	}
	d.dir.remove(peer.HostUUID)
	delete(d.peerCertificates, peer.HostUUID)
	d.forget(peer.HostUUID)
	d.emit(noderec.NotifyNodeRemoved, peer)
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
		if !ok || n.IP == "" || (n.IdentityVerification != noderec.IdentityLocal && n.IdentityVerification != noderec.IdentityPinned) {
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
	node, present := d.dir.get(hostUUID)
	if !present {
		return false
	}
	return d.refreshNodeModelsCandidates(hostUUID, ip, append([]string(nil), node.CandidateIPs()...), emPort, clusterUUID)
}

// refreshNodeModelsCandidates is refreshNodeModels with address failover. The
// complete remote address list guards against queued or in-flight results from
// an endpoint removed by a newer authenticated inventory response.
//
// askRemembered retains a working address across model sweeps until it fails or
// is withdrawn from the current record.
func (d *daemon) refreshNodeModelsCandidates(hostUUID, guardIP string, hosts []string, emPort int, clusterUUID string) bool {
	if d.reg != nil && hostUUID == d.reg.record().HostUUID {
		hosts = []string{loopbackHost}
		clusterUUID = ""
	}
	ask := func(host string) (modelInventory, bool) {
		return d.fetchModels(host, emPort, clusterUUID)
	}
	key := hostKey{hostUUID: hostUUID, service: noderec.ServiceEngineManager}
	_, inventory, fetched := askRemembered(&d.enrichHosts, key, hosts, ask)
	if !fetched {
		return false
	}
	models, byEngine, loadedByEngine := inventory.models, inventory.byEngine, inventory.loadedByEngine
	d.peerMu.Lock()
	defer d.peerMu.Unlock()
	d.mesh.Refresh()
	if target, present := d.dir.get(hostUUID); present && target.IdentityVerification == noderec.IdentityPinned {
		if target.ClusterUUID != clusterUUID ||
			!slices.Equal(target.CandidateIPs(), hosts) ||
			!d.mesh.VerifyPeerCertificate(clusterUUID, inventory.certificate) ||
			!d.mesh.VerifyPeerCertificate(clusterUUID, d.peerCertificates[hostUUID]) {
			return false
		}
	}
	node, changed, valid := d.dir.applyModels(hostUUID, guardIP, emPort, models, byEngine, loadedByEngine)
	if !valid {
		return false
	}
	if node.IdentityVerification == noderec.IdentityLocal {
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
		d.infoMu.Unlock()
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
	d.enrichHosts.forget(hostKey{hostUUID: "peer:" + hostUUID, service: noderec.ServiceNodeInfo})
	d.enrichHosts.forget(hostKey{hostUUID: hostUUID, service: noderec.ServiceEngineManager})
	d.infoMu.Lock()
	delete(d.lastInfoAt, hostUUID)
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
	case noderec.MethodLocalNodeInfo:
		var params noderec.LocalNodeInfoParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			slog.Warn("invalid local node-info snapshot", "err", err)
			return true
		}
		d.acceptLocalNodeInfo(params.Info)
		return true

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
