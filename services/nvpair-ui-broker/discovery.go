// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"sort"
	"sync"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/netpick"
	"nvpair-shared/noderec"

	"nvpair-ui-broker/relay"
)

// pushRegisterTimeout bounds a relayed registration call to the daemon.
const pushRegisterTimeout = 5 * time.Second

// GPUInfo, CPUInfo, MemoryInfo mirror the node-info enrichment shape emitted by
// nvpair-node-scanner; single-sourced in nvpair-shared/noderec so the broker, the
// daemon, and clients share one shape (the wire stays byte-identical).
type (
	GPUInfo    = noderec.GPUInfo
	CPUInfo    = noderec.CPUInfo
	MemoryInfo = noderec.MemoryInfo
)

type EnrichedNode struct {
	DiscoveryRecordID    string                       `json:"discoveryRecordId"`
	IdentityVerification noderec.IdentityVerification `json:"identityVerification"`
	ID                   string                       `json:"id"`
	// HostUUID identifies the host for local or pinned records.
	// For unverified mDNS records it is the advertised UUID, which is only a claim.
	// For unverified manual entries it is "manual:" + the entry ID, matching the
	// configured engine route's key. The store uses DiscoveryRecordID for both
	// kinds of unverified record so they stay separate from verified hosts.
	HostUUID  string      `json:"-"`
	Host      string      `json:"host"`
	Port      int         `json:"port"`
	Addresses []string    `json:"addresses"`
	TXT       []string    `json:"txt"`
	GPUs      []GPUInfo   `json:"gpus"`
	CPU       *CPUInfo    `json:"cpu,omitempty"`
	Memory    *MemoryInfo `json:"memory,omitempty"`
	// Trusted carries the cluster-pin annotation from the daemon
	// directory through to the client AvailableNode. Absent on the legacy
	// node/* feed (defaults false).
	Trusted bool `json:"trusted,omitempty"`
	// Clustered reports whether the node advertises a cluster-uuid (it belongs to
	// some cluster), independent of whether we are paired with it (Trusted). Lets
	// a client suppress an invite that would be rejected by an already-clustered
	// peer. Absent on the legacy node/* feed (defaults false).
	Clustered bool `json:"clustered,omitempty"`
	// Models is the node's model list, enriched by the daemon from the peer's
	// engine-manager /v1/models endpoint (models-http).
	Models []string `json:"models,omitempty"`
	// ModelsByEngine attributes Models to the engine serving each model, keyed by
	// engine-manager engine name (e.g. "ollama", "lmstudio"). Additive alongside
	// the flat Models union; carried through to AvailableNode for per-engine
	// consumers.
	ModelsByEngine map[string][]string `json:"modelsByEngine,omitempty"`
	// LoadedByEngine names the models currently resident in memory per engine
	// (normally a subset of ModelsByEngine), enriched by the daemon from the
	// peer's engine-manager /v1/models loadedByEngine field. Carried through to
	// AvailableNode so a remote node's cards can reflect loaded state.
	LoadedByEngine map[string][]string `json:"loadedByEngine,omitempty"`
}

// directoryToEnriched preserves scanner provenance in the client-facing store.
// ID remains the display name; Port is the node-info port.
//
// The node's whole ranked address list travels through, both as Addresses and as
// the ip= / ips= TXT the netpick projection reads, so its own order survives to
// every consumer. Reducing it to one address here would discard the ranking the
// node derived from evidence only it can observe, and leave a consumer whose
// chosen address happens to be a link it cannot reach with nothing else to try.
func directoryToEnriched(n noderec.DirectoryNode) EnrichedNode {
	addrs := n.CandidateIPs()
	txt := n.AddressTXT()
	if n.HostUUID != "" {
		txt = append(txt, "uuid="+n.HostUUID)
	}
	port := 0
	if ni, ok := n.Services[noderec.ServiceNodeInfo]; ok {
		port = ni.Port
	}
	return EnrichedNode{
		DiscoveryRecordID:    n.DiscoveryRecordID,
		IdentityVerification: n.IdentityVerification,
		ID:                   n.Name,
		HostUUID:             n.HostUUID,
		Host:                 n.Name,
		Port:                 port,
		Addresses:            addrs,
		TXT:                  txt,
		GPUs:                 n.GPUs,
		CPU:                  n.CPU,
		Memory:               n.Memory,
		Trusted:              n.Trusted,
		Clustered:            n.Clustered(),
		Models:               n.Models,
		ModelsByEngine:       n.ModelsByEngine,
		LoadedByEngine:       n.LoadedByEngine,
	}
}

// storeKey merges only proved host identities. Unverified discoveries and
// manual configurations retain independent record keys.
func (n EnrichedNode) storeKey() string {
	if n.IdentityVerification == noderec.IdentityLocal || n.IdentityVerification == noderec.IdentityPinned {
		return n.HostUUID
	}
	return n.DiscoveryRecordID
}

// nodeSource identifies which feed contributed a store record. The same node
// (one hostUuid) can be claimed by both the scanner (mDNS) and a manual entry
// once the manual node learns its real UUID, so the store tracks per-source
// ownership and only forgets a record when no source claims it — a manual
// remove (or a manual-nodes crash) must not evict a still-live scanner node.
type nodeSource int

const (
	sourceScanner nodeSource = iota
	sourceManual
)

// storedNode keeps a verified host by UUID or an unverified discovery by its
// separate record ID. It keeps each source's last full EnrichedNode payload
// (including GPU / CPU / memory the wire doesn't expose) so a record can be
// re-projected from the surviving source when the other is removed. lastSeen is
// bumped on every discovered/updated event, giving clients a freshness signal.
type storedNode struct {
	scanner  *EnrichedNode
	manual   *EnrichedNode
	lastSeen time.Time
}

// AcceptInfo reports whether hardware belongs to the scanner's current verified
// record. A candidate with the same claimed UUID cannot supply its measurements.
func (s *discoveryStore) AcceptInfo(value noderec.NodeInfoEvent) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sn, ok := s.nodes[value.HostUUID]
	if !ok || sn.scanner == nil {
		return false
	}
	node := sn.scanner
	if node.DiscoveryRecordID != value.DiscoveryRecordID || node.IdentityVerification != value.IdentityVerification || value.Info.HostUUID != value.HostUUID {
		return false
	}
	if value.IdentityVerification != noderec.IdentityLocal {
		if value.IdentityVerification != noderec.IdentityPinned || !node.Trusted {
			// Remote hardware needs both proved identity and current trust.
			return false
		}
		if value.ClusterUUID != value.HostUUID {
			// The event's authenticated principal must identify this same host.
			return false
		}
		if value.Info.ClusterUUID == nil || *value.Info.ClusterUUID != value.ClusterUUID {
			// The hardware body must name that principal too; a missing or
			// conflicting identity cannot supply verified measurements.
			return false
		}
	}
	return true
}

func (s *discoveryStore) verifiedSource(hostUUID string, source nodeSource) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sn := s.nodes[hostUUID]
	node := sn.scanner
	if source == sourceManual {
		node = sn.manual
	}
	if node == nil {
		return false
	}
	if node.IdentityVerification == noderec.IdentityLocal {
		return true
	}
	return node.IdentityVerification == noderec.IdentityPinned && node.Trusted
}

// projected returns the EnrichedNode a consumer sees. When both sources claim
// the node the scanner (mDNS) view wins — it's the authoritative discovery
// record and carries trusted/clusterUuid and per-engine models — falling back
// to the manual probe when only that source is present.
func (sn storedNode) projected() EnrichedNode {
	if sn.scanner != nil {
		return *sn.scanner
	}
	return *sn.manual
}

// claimed reports whether any source still owns this record.
func (sn storedNode) claimed() bool { return sn.scanner != nil || sn.manual != nil }

// setSource records (source == nil clears) one source's payload.
func (sn *storedNode) setSource(source nodeSource, n *EnrichedNode) {
	switch source {
	case sourceScanner:
		sn.scanner = n
	case sourceManual:
		sn.manual = n
	}
}

// hasSource reports whether the given source currently claims this record.
func (sn storedNode) hasSource(source nodeSource) bool {
	switch source {
	case sourceScanner:
		return sn.scanner != nil
	case sourceManual:
		return sn.manual != nil
	}
	return false
}

// discoveryStore is the broker's live view of network-discovered nodes.
// It's the single source of truth that "discovery:get-nodes" reads from.
// Scanner, manual-worker, trust, and restart updates are serialized by the
// broker's discoveryMu; RPC readers use the store's own read lock.
//
// onChange, when non-nil, is invoked after every Upsert and Remove
// AFTER the write lock has been released. It's the broker's hook for
// emitting discovery:nodes-changed notifications. Holding the callback
// off the lock means a caller is free to snapshot inside it without
// deadlocking. The store does not guarantee that two rapid mutations
// produce two separate callback invocations in strict happens-before
// order with the lock. The broker orders mutations and callbacks at the call
// sites with discoveryMu.
type discoveryStore struct {
	mu       sync.RWMutex
	nodes    map[string]storedNode
	onChange func()
}

func newDiscoveryStore() *discoveryStore {
	return &discoveryStore{nodes: make(map[string]storedNode)}
}

func (s *discoveryStore) sourceNodes(source nodeSource) []EnrichedNode {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var nodes []EnrichedNode
	for _, stored := range s.nodes {
		node := stored.scanner
		if source == sourceManual {
			node = stored.manual
		}
		if node != nil {
			nodes = append(nodes, *node)
		}
	}
	return nodes
}

func (s *discoveryStore) sourceNode(key string, source nodeSource) (EnrichedNode, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stored := s.nodes[key]
	node := stored.scanner
	if source == sourceManual {
		node = stored.manual
	}
	if node == nil {
		return EnrichedNode{}, false
	}
	return *node, true
}

// SetOnChange installs (or removes, with nil) the post-mutation hook.
// Setting it after a sequence of mutations is the broker's way of
// silencing the notification stream during startup: by attaching the
// hook only after "app:ready" has been flushed we avoid emitting a
// discovery:nodes-changed before app:ready.
func (s *discoveryStore) SetOnChange(fn func()) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// Upsert records one source's view of a node (node/discovered or node/updated).
// Verified sources share a host UUID key; unverified discoveries keep their
// separate record keys. The client snapshot is always the
// latest full picture, not a diff stream.
func (s *discoveryStore) Upsert(n EnrichedNode, source nodeSource) {
	key := n.storeKey()
	if key == "" {
		slog.Warn("ignoring node payload with empty id")
		return
	}
	cp := n
	s.mu.Lock()
	sn := s.nodes[key]
	sn.setSource(source, &cp)
	sn.lastSeen = time.Now()
	s.nodes[key] = sn
	cb := s.onChange
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
}

// Remove drops one source's claim on a node and reports whether that removed the
// FINAL claim — i.e. the node is now truly gone from the directory. The record
// is deleted only when no source claims it anymore; while another source still
// owns it (e.g. the scanner sees a node a removed manual alias also pointed at)
// the record stays, re-projects from the survivor, and Remove returns false — so
// removing a manual node, or a manual-nodes crash, never evicts a live mDNS node
// AND never reports a still-present node as lost. Returns false when
// the key is empty or this source didn't own the record.
func (s *discoveryStore) Remove(key string, source nodeSource) bool {
	if key == "" {
		return false
	}
	s.mu.Lock()
	sn, existed := s.nodes[key]
	if !existed || !sn.hasSource(source) {
		s.mu.Unlock()
		slog.Debug("remove for a node this source doesn't own", "key", key)
		return false
	}
	sn.setSource(source, nil)
	gone := !sn.claimed()
	if gone {
		delete(s.nodes, key)
	} else {
		sn.lastSeen = time.Now()
		s.nodes[key] = sn
	}
	cb := s.onChange
	s.mu.Unlock()
	if cb != nil {
		cb()
	}
	return gone
}

// Snapshot returns the narrow wire-format view used by
// discovery:get-nodes and discovery:nodes-changed, sorted by id for
// stable rendering. The rich EnrichedNode payload stays in the store
// for future RPCs; only the fields the external schema requires are
// projected out here.
func (s *discoveryStore) Snapshot() []AvailableNode {
	return s.snapshot(false)
}

// SchedulingSnapshot includes local discoveries, pinned and trusted peers,
// and explicitly configured manual engine targets. Unverified scanner claims
// are excluded. Manual hardware still requires separate authentication.
func (s *discoveryStore) SchedulingSnapshot() []AvailableNode {
	return s.snapshot(true)
}

func (s *discoveryStore) snapshot(scheduling bool) []AvailableNode {
	s.mu.RLock()
	out := make([]AvailableNode, 0, len(s.nodes))
	for _, sn := range s.nodes {
		// Explicit manual configurations independently authorize engine routing.
		if scheduling && sn.manual == nil {
			verification := sn.scanner.IdentityVerification
			if verification != noderec.IdentityLocal && verification != noderec.IdentityPinned {
				// An unverified advertisement cannot create a scheduling target.
				continue
			}
			if verification == noderec.IdentityPinned && !sn.scanner.Trusted {
				// Proving a remote identity is insufficient without current trust.
				// Local records come from the owned worker and need no peer pin.
				continue
			}
		}
		out = append(out, sn.toAvailable())
	}
	s.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == out[j].ID {
			return out[i].DiscoveryRecordID < out[j].DiscoveryRecordID
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// toAvailable projects a storedNode onto the wire format. Name currently mirrors
// ID (the mDNS instance name is already human-readable for our use case); lastSeen
// is emitted as Unix seconds to match the external schema's "number of seconds"
// expectation.
//
// Addresses are carried through as a ranked list with the canonical one first,
// rather than collapsed to a single preferred IP. The node ranked them from
// evidence no observer has, and a client that must connect needs somewhere to go
// when the first address turns out to be a link only one other machine can reach.
func (sn storedNode) toAvailable() AvailableNode {
	n := sn.projected()
	candidates := netpick.Candidates(n.TXT, n.Addresses)
	var primary string
	if len(candidates) > 0 {
		primary = candidates[0]
	}
	// A lone address says nothing IPAddress does not; omitting it keeps the
	// payload honest about which nodes are actually multi-homed.
	if len(candidates) < 2 {
		candidates = nil
	}
	return AvailableNode{
		DiscoveryRecordID:    n.DiscoveryRecordID,
		IdentityVerification: n.IdentityVerification,
		ID:                   n.ID,
		Name:                 n.ID,
		HostUUID:             n.HostUUID,
		IPAddress:            primary,
		IPAddresses:          candidates,
		Port:                 n.Port,
		LastSeen:             sn.lastSeen.Unix(),
		Trusted:              n.Trusted && n.IdentityVerification != noderec.IdentityUnverified,
		Clustered:            n.Clustered,
		Models:               n.Models,
		ModelsByEngine:       n.ModelsByEngine,
		LoadedByEngine:       n.LoadedByEngine,
	}
}

// scannerProcess is the broker's handle on its child nvpair-node-scanner. It speaks
// bidirectional newline-delimited JSON-RPC over stdio on the shared jsonrpc.Peer
// (id-correlation + read pump), so the broker can Call the scanner and push
// notifications down to it — the foundation for relaying discovery:register /
// subscribe to the promoted daemon and reading its acks — while
// the scanner's discovery:node-* notifications enter the discovery store.
type scannerProcess struct {
	stateMu  *sync.Mutex
	mesh     *clustertrust.Mesh
	stopped  bool // protected by stateMu, alongside directory mutations
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	peer     *Peer
	store    *discoveryStore
	relayDir *relay.Directory
	// onNodeLost, when non-nil, is invoked the moment the daemon evicts a node's
	// FINAL directory claim (it's truly gone, not just dropped by one of two
	// sources) — the broker's hook for marking workloads pinned to a departed
	// node as failed. It's called with the node's stable HostUUID (the identity
	// workloads are keyed by) plus its display name (for the error
	// message). Kept distinct from the store's own onChange (which is id-less)
	// because the sweep needs to know *which* node departed.
	onNodeLost         func(uuid, name string)
	onTelemetry        func(noderec.NodeTelemetry)
	onTelemetryRemoved func(hostUUID string)
	onLocalNode        func(noderec.DirectoryNode)
	onNodeInfo         func(noderec.NodeInfoEvent)
	localInfo          chan localInfoObservation
	// activity queues peer liveness reports for the scanner. It exists so the
	// proxy-reader goroutine that receives them never waits on the scanner's
	// pipe: reports arrive while inference is streaming, and a wedged scanner
	// must not be able to stall the reader that also carries workload events.
	activity chan nodeActivityReport
	done     chan struct{}
}

// nodeActivityReport is a queued liveness report. It carries the observation
// time rather than an age so the age on the wire is measured when the frame is
// actually written, and therefore includes however long the report sat here.
type nodeActivityReport struct {
	hostUUID   string
	observedAt time.Time
}

// activityQueueDepth bounds the queued liveness reports. Each proxy coalesces to
// one report per node every couple of seconds, so this holds several seconds of
// a large cluster's worth. Past it, reports are dropped rather than buffered:
// the signal is a hint that something was alive recently, so a fresher report is
// always a better use of the slot than a backlog of stale ones.
const activityQueueDepth = 64

// SetLogLevel forwards an already-validated log level as a log/set-level
// notification through the peer's codec.
func (s *scannerProcess) SetLogLevel(level string) error {
	return s.peer.Notify(applog.SetLevelMethod, applog.SetLevelParams{Level: level})
}

// Done implements supervisedHandle: the returned channel closes once the
// scanner process has exited (cmd.Wait returned).
func (s *scannerProcess) Done() <-chan struct{} { return s.done }

// startScanner spawns nvpair-node-scanner with the given binary path, forwards
// the broker's current log level via --log-level, hides the console
// window on Windows, and launches a goroutine to consume the scanner's
// stdout into the supplied store. Scanner stderr is plumbed to the
// broker's stderr unmodified so its [nvpair-node-scanner] applog prefix
// flows through to whoever is reading the broker's stderr.
func startScanner(
	binaryPath, logLevel string,
	store *discoveryStore,
	stateMu *sync.Mutex,
	mesh *clustertrust.Mesh,
	relayDir *relay.Directory,
	onNodeLost func(uuid, name string),
	onTelemetry func(noderec.NodeTelemetry),
	onTelemetryRemoved func(hostUUID string),
	onLocalNode func(noderec.DirectoryNode),
	onNodeInfo func(noderec.NodeInfoEvent),
	extraArgs ...string,
) (*scannerProcess, error) {
	cmd := exec.Command(binaryPath, append([]string{"--log-level", logLevel}, extraArgs...)...)
	configureSubprocess(cmd)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = stderrOut

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start %s: %w", binaryPath, err)
	}

	sp := &scannerProcess{
		stateMu:            stateMu,
		mesh:               mesh,
		cmd:                cmd,
		stdin:              stdin,
		peer:               NewPeer(NewCodec(readWriter{stdout, stdin})),
		store:              store,
		relayDir:           relayDir,
		onNodeLost:         onNodeLost,
		onTelemetry:        onTelemetry,
		onTelemetryRemoved: onTelemetryRemoved,
		onLocalNode:        onLocalNode,
		onNodeInfo:         onNodeInfo,
		localInfo:          make(chan localInfoObservation, 1),
		activity:           make(chan nodeActivityReport, activityQueueDepth),
		done:               make(chan struct{}),
	}

	go sp.peer.Serve(nil, sp.handleNotify)
	go sp.drainNodeActivity()
	go func() {
		_ = cmd.Wait()
		sp.peer.Close()
		close(sp.done)
	}()

	return sp, nil
}

// Stop signals the scanner to exit (by closing its stdin, which the scanner
// observes as EOF) and waits for it to exit, escalating if it does not — see
// waitForStdinClose. Safe to call multiple times.
func (s *scannerProcess) Stop() {
	waitForStdinClose("scanner", s.cmd, s.stdin, s.done)
}

// pushRegister relays a service registration down to the daemon (an id-bearing
// request the daemon acks); failures are logged, not fatal.
func (s *scannerProcess) pushRegister(p noderec.RegisterParams) {
	params, err := json.Marshal(p)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushRegisterTimeout)
	defer cancel()
	if _, rpcErr, err := s.peer.Call(ctx, noderec.MethodRegister, params); err != nil {
		slog.Warn("relay register to daemon failed", "service", p.Service, "err", err)
	} else if rpcErr != nil {
		slog.Warn("daemon rejected register", "service", p.Service, "code", rpcErr.Code, "msg", rpcErr.Message)
	}
}

// reloadIdentity asks the daemon to re-resolve and re-advertise this node's
// identity. The broker calls it once cluster-manager is up so the scanner's
// advertised uuid= converges on the cluster principal (the scanner spawns first
// and mints its own node-id before cluster-manager writes identity.json).
func (s *scannerProcess) reloadIdentity() {
	ctx, cancel := context.WithTimeout(context.Background(), pushRegisterTimeout)
	defer cancel()
	if _, rpcErr, err := s.peer.Call(ctx, noderec.MethodReloadIdentity, nil); err != nil {
		slog.Warn("relay reload-identity to daemon failed", "err", err)
	} else if rpcErr != nil {
		slog.Warn("daemon rejected reload-identity", "code", rpcErr.Code, "msg", rpcErr.Message)
	}
}

// reloadTrust promptly invalidates stale peer proofs and schedules candidate
// authentication after a pin change. Scanner also reconciles live trust on its
// periodic refresh loops, independently of mDNS changes.
func (s *scannerProcess) reloadTrust() {
	ctx, cancel := context.WithTimeout(context.Background(), pushRegisterTimeout)
	defer cancel()
	if _, rpcErr, err := s.peer.Call(ctx, noderec.MethodReloadTrust, nil); err != nil {
		slog.Warn("relay reload-trust to daemon failed", "err", err)
	} else if rpcErr != nil {
		slog.Warn("daemon rejected reload-trust", "code", rpcErr.Code, "msg", rpcErr.Message)
	}
}

// pushObservedAddresses relays the local addresses peers have reached this node
// on down to the daemon, which ranks a peer-proven address above one it inferred
// from local link state alone.
func (s *scannerProcess) pushObservedAddresses(addrs []string) {
	params, err := json.Marshal(noderec.ObservedAddressesParams{Addresses: addrs})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushRegisterTimeout)
	defer cancel()
	if _, rpcErr, err := s.peer.Call(ctx, noderec.MethodSetObservedAddresses, params); err != nil {
		slog.Debug("relay observed addresses to daemon failed", "err", err)
	} else if rpcErr != nil {
		slog.Debug("daemon rejected observed addresses", "code", rpcErr.Code, "msg", rpcErr.Message)
	}
}

// reportNodeActivity queues a peer's liveness report for the scanner, which
// counts it as proof the peer is alive and skips the liveness probe that would
// otherwise evict it.
//
// This never blocks and never waits for a reply. Reports arrive once per node
// per couple of seconds for as long as inference is streaming, so anything that
// waited here would be paid on the goroutine reading the proxy's pipe — the same
// one carrying workload events. A dropped report costs nothing: it only means
// the scanner falls back to probing a node that is very likely to answer.
func (s *scannerProcess) reportNodeActivity(hostUUID string, observedAt time.Time) {
	select {
	case s.activity <- nodeActivityReport{hostUUID: hostUUID, observedAt: observedAt}:
	default:
		slog.Debug("dropped a node activity report; the scanner relay is backed up", "host_uuid", hostUUID)
	}
}

// reportLocalInfo queues an owned-worker snapshot without blocking the caller.
// A full queue drops the new sample, allowing existing measurements to age.
func (s *scannerProcess) reportLocalInfo(observation localInfoObservation) {
	select {
	case s.localInfo <- observation:
	default:
	}
}

// drainNodeActivity forwards queued liveness reports and owned inventory to the
// daemon until the scanner exits.
//
// They go as notifications, not requests. The daemon has nothing to say back —
// it records the report and returns — so an id-bearing call would buy a pending
// entry and a blocked goroutine per report in exchange for an ack nobody reads.
func (s *scannerProcess) drainNodeActivity() {
	for {
		select {
		case <-s.done:
			return
		case observation := <-s.localInfo:
			info := observation.info.Aged(time.Since(observation.receivedAt))
			if err := s.peer.Notify(noderec.MethodLocalNodeInfo, noderec.LocalNodeInfoParams{Info: info}); err != nil {
				slog.Debug("relay local node-info to daemon failed", "err", err)
			}
		case report := <-s.activity:
			// Aged at send time, not enqueue time, so any wait in the queue is
			// included: the daemon measures freshness against this number.
			if err := s.peer.Notify(noderec.MethodNodeActivity, noderec.NodeActivityParams{
				HostUUID: report.hostUUID,
				MSSince:  time.Since(report.observedAt).Milliseconds(),
			}); err != nil {
				slog.Debug("relay node activity to daemon failed", "host_uuid", report.hostUUID, "err", err)
			}
		}
	}
}

// pushUnregister relays a service removal down to the daemon.
func (s *scannerProcess) pushUnregister(svc noderec.ServiceKey) {
	params, err := json.Marshal(noderec.UnregisterParams{Service: svc})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushRegisterTimeout)
	defer cancel()
	if _, _, err := s.peer.Call(ctx, noderec.MethodUnregister, params); err != nil {
		slog.Warn("relay unregister to daemon failed", "service", svc, "err", err)
	}
}

// handleNotify routes the scanner's notifications, running on the peer's
// read-pump goroutine. Post-cutover the scanner speaks only the promoted
// daemon's discovery:node-* stream (the legacy node/* stream is gone); those
// events fold into both the client-facing store and the relay directory. ready
// is logged. Responses to broker-issued Calls are demuxed by the peer itself and
// never reach here; any other notification is ignored.
func (s *scannerProcess) handleNotify(method string, params json.RawMessage) {
	receivedAt := time.Now()
	if s.stateMu != nil {
		s.stateMu.Lock()
		defer s.stateMu.Unlock()
	}
	if s.stopped {
		return
	}
	if s.mesh != nil {
		s.mesh.Refresh()
	}
	switch method {
	case noderec.NotifyNodeDiscovered, noderec.NotifyNodeUpdated, noderec.NotifyNodeRemoved:
		var ev noderec.NodeEvent
		if err := json.Unmarshal(params, &ev); err != nil {
			slog.Warn("daemon emitted invalid node event", "method", method, "err", err)
			return
		}
		if method != noderec.NotifyNodeRemoved && !s.currentRecord(ev.Node.HostUUID, ev.Node.DiscoveryRecordID, ev.Node.IdentityVerification) {
			return
		}
		if method == noderec.NotifyNodeRemoved && s.store != nil {
			// A delayed removal for a previous pin must not remove its newly
			// authenticated replacement under the same canonical host UUID.
			if node, present := s.store.sourceNode(directoryToEnriched(ev.Node).storeKey(), sourceScanner); present && node.DiscoveryRecordID != ev.Node.DiscoveryRecordID {
				return
			}
		}
		if method != noderec.NotifyNodeRemoved && s.onLocalNode != nil {
			s.onLocalNode(ev.Node)
		}
		// Fold into the relay directory (internal subscribers: proxies, mesh
		// services) and the client-facing store (discovery:get-nodes /
		// nodes-changed, merging manual nodes), carrying the trusted annotation.
		if s.relayDir != nil {
			s.relayDir.Apply(method, ev.Node)
		}
		verified := ev.Node.IdentityVerification == noderec.IdentityLocal || ev.Node.IdentityVerification == noderec.IdentityPinned
		if method == noderec.NotifyNodeRemoved && verified && s.onTelemetryRemoved != nil {
			s.onTelemetryRemoved(ev.Node.HostUUID)
		}
		if s.store != nil {
			if method == noderec.NotifyNodeRemoved {
				// Drop the scanner's claim by the same UUID key Upsert used. Only
				// when that was the FINAL claim (no co-located manual alias
				// survives) is the node really gone — its proxy will never emit a
				// terminal for the workloads it was running, so hand THAT
				// departure to the broker to mark them failed and clear the stale
				// "in progress" lines. A node still held by a manual claim is
				// present, so we must not fail its jobs. Identify the departed
				// node by HostUUID, the value workloads are stamped/keyed with,
				// not the display hostname.
				if s.store.Remove(directoryToEnriched(ev.Node).storeKey(), sourceScanner) && verified && s.onNodeLost != nil {
					s.onNodeLost(ev.Node.HostUUID, ev.Node.Name)
				}
			} else {
				previous, present := s.store.sourceNode(directoryToEnriched(ev.Node).storeKey(), sourceScanner)
				if present && previous.DiscoveryRecordID != ev.Node.DiscoveryRecordID && s.onTelemetryRemoved != nil {
					s.onTelemetryRemoved(ev.Node.HostUUID)
				}
				s.store.Upsert(directoryToEnriched(ev.Node), sourceScanner)
			}
		}
	case noderec.NotifyNodeTelemetry:
		var telemetry noderec.NodeTelemetry
		if err := json.Unmarshal(params, &telemetry); err != nil {
			slog.Warn("daemon emitted invalid node telemetry", "err", err)
			return
		}
		if telemetry.HostUUID == "" {
			slog.Warn("daemon emitted node telemetry without hostUuid")
			return
		}
		if !s.currentHost(telemetry.HostUUID) {
			return
		}
		if s.store != nil && s.store.verifiedSource(telemetry.HostUUID, sourceScanner) && s.onTelemetry != nil {
			telemetry = telemetry.Aged(time.Since(receivedAt))
			s.onTelemetry(telemetry)
		}
	case noderec.NotifyNodeInfo:
		var value noderec.NodeInfoEvent
		if json.Unmarshal(params, &value) != nil || s.store == nil || !s.currentRecord(value.HostUUID, value.DiscoveryRecordID, value.IdentityVerification) {
			return
		}
		if !s.store.AcceptInfo(value) {
			return
		}
		if s.onNodeInfo != nil {
			value.Info = value.Info.Aged(time.Since(receivedAt))
			s.onNodeInfo(value)
		}
	case "ready":
		slog.Info("scanner reported ready", "params", string(params))
	}
}

func (s *scannerProcess) currentRecord(uuid, recordID string, verification noderec.IdentityVerification) bool {
	if s.mesh == nil || verification != noderec.IdentityPinned {
		return true
	}
	return recordID != "" && recordID == s.mesh.PeerRecordID(uuid)
}

func (s *scannerProcess) currentHost(uuid string) bool {
	if s.store == nil {
		return false
	}
	node, present := s.store.sourceNode(uuid, sourceScanner)
	return present && node.IdentityVerification != noderec.IdentityUnverified && s.currentRecord(uuid, node.DiscoveryRecordID, node.IdentityVerification)
}

// writeSetLevelFrame marshals a newline-delimited log/set-level
// notification and writes it to a child's stdin under mu. The level is
// assumed already validated by the caller (applog.HandleSetLevelParams in
// the broker's own handler), so the child can apply it without echoing a
// result. Shared by both child handles (scanner and node-info).
func writeSetLevelFrame(mu *sync.Mutex, w io.Writer, level string) error {
	frame := struct {
		JSONRPC string                `json:"jsonrpc"`
		Method  string                `json:"method"`
		Params  applog.SetLevelParams `json:"params"`
	}{
		JSONRPC: "2.0",
		Method:  applog.SetLevelMethod,
		Params:  applog.SetLevelParams{Level: level},
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	data = append(data, '\n')

	mu.Lock()
	defer mu.Unlock()
	_, err = w.Write(data)
	return err
}
