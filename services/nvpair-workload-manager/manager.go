// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"sync"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/noderec"
)

// discoveryInterval is how often we refresh the peer set from the discovery
// seam. Matches the scanner's cadence so membership tracks node churn at a
// comparable rate.
const discoveryInterval = 5 * time.Second

// resyncInterval is how often each node re-asserts its own active + recently-
// terminal workloads to peers (the anti-entropy heartbeat). A later
// re-assertion can repair a missed lifecycle event or a peer's wrong node-loss
// guess if it is queued and delivered. terminalRetention keeps a finished
// workload in the re-sync set for two intervals, allowing roughly two
// re-assertions before it ages out. Removals are not kept in that set.
//
// resyncInterval is load-bearing OUTSIDE this binary. nvpair-ui-broker's
// staleness sweep (workloadOriginSilenceTimeout in its broker.go) retires a
// remote workload after a fixed multiple of this interval's worth of silence, on
// the assumption that a live origin re-asserts every active workload every
// interval, forever. Lengthening this value past that budget — or shipping a peer
// that stops tagging re-assertions with resync:true, which would make the
// receiver's dedup swallow them — makes peers mark genuinely running work as
// failed. Change the broker's budget in the same commit.
const (
	resyncInterval    = 30 * time.Second
	terminalRetention = 2 * resyncInterval
)

// workloadKey identifies a workload by its global identity: the origin node,
// the engine, the proxy's per-process runId nonce, and the origin-assigned id.
// The bare (origin,id) isn't unique — each engine counts from 1 and resets on
// restart — so engine + runId keep concurrent cross-engine and post-restart
// same-id workloads distinct in the re-sync set.
type workloadKey struct {
	origin string
	engine string
	runID  string
	id     string
}

// workloadEvent is a stored lifecycle notification (method + params) for a
// workloadKey. A terminal event is retained until expiresAt so the heartbeat
// can re-assert it a couple of times (offering another chance after a missed
// delivery or a peer's wrong node-loss guess); an active event has a zero
// expiresAt and is retained until it terminates or is removed.
type workloadEvent struct {
	method    string
	params    json.RawMessage
	terminal  bool
	expiresAt time.Time
}

// removedKey is the identity an ingress removal tombstones: the (origin, id)
// pair the removal wire carries, which has no engine or runId.
type removedKey struct {
	origin string
	id     string
}

// removedEntry is one tombstone in Manager.removedOrder.
type removedEntry struct {
	key       removedKey
	expiresAt time.Time
}

// maxRemovedIngress bounds the removal memory. A producer can remove workloads
// it never reported, so without a bound the memory would grow with the removal
// rate for as long as terminalRetention. At the bound the oldest tombstone is
// evicted, which only means a very old removal can no longer suppress a replay.
const maxRemovedIngress = 4096

// ReadyParams is the payload of the startup "ready" notification, matching
// the convention used by the other subprocesses.
type ReadyParams struct {
	Version string `json:"version"`
}

// Manager owns the local interface (stdin/stdout JSON-RPC) and ties together
// the peer set, broadcaster, inter-node server, and mDNS advertise/discover.
type Manager struct {
	codec       *Codec
	dedup       *dedupIndex
	peers       *peerSet
	broadcaster *Broadcaster
	server      *Server
	peerSource  PeerSource
	relaySource *relayPeerSource // relay-fed peer set; the concrete peerSource
	mesh        *clustertrust.Mesh

	// activeLocal is this node's re-sync set: the latest event per local-origin
	// (origin,id) — active workloads plus recently-terminal ones (retained
	// until they expire). It backfills a newly-discovered peer (pushActiveSnapshot)
	// and is re-asserted on the heartbeat (resyncLoop) to help peers reconcile
	// to this node's authoritative state. Guarded by activeMu.
	activeMu    sync.Mutex
	activeLocal map[workloadKey]workloadEvent

	// removedIngress tombstones the workloads a producer removed through the
	// loopback ingress, keyed by the (origin, id) pair a removal carries, until
	// terminalRetention has passed. The broker replays its store to a restarted
	// worker, and that replay can land after the removal, when nothing is
	// tracked yet; a replayed lifecycle frame for a tombstoned pair is dropped
	// instead of re-tracked and re-broadcast. A fresh ingress lifecycle frame
	// for the pair clears the tombstone. At most maxRemovedIngress are kept:
	// removedOrder holds them oldest first (every entry expires terminalRetention
	// after it was written or refreshed, so that is also expiry order), which makes
	// pruning and eviction amortized O(1). Guarded by activeMu, and only written or read under
	// broadcastMu (commitLocal), so it stays ordered with activeLocal.
	removedIngress map[removedKey]*list.Element
	removedOrder   *list.List

	// broadcastMu keeps snapshot enqueueing ordered with local lifecycle and
	// removal updates. Always acquire it before activeMu.
	broadcastMu sync.Mutex

	// broadcastCh serializes outbound inter-node frames in the order the
	// read loop produced them. broadcastFrame only enqueues (never blocks on
	// network I/O), and a single worker drains the queue in order — so a
	// queued remove cannot overtake the lifecycle upsert it follows. Without
	// this, each frame fanned out in its own goroutine and a late upsert
	// could resurrect a workload on peers that had already removed it.
	broadcastCh chan []byte

	// selfUUID is this node's stable UUID; the loopback ingress stamps it as
	// originatedFrom on frames whose producer left it empty and refuses any
	// other origin.
	selfUUID string

	// ingress is the optional loopback listener for third-party local
	// producers (localingress.go); nil unless EnableLocalIngress was called.
	ingress *localIngress

	// ingestMu serializes ingress frames from validation through the enqueue
	// for peers, so for one workload the order frames reach the broker equals
	// the order peers are queued. Lock order: ingestMu, then broadcastMu, then
	// activeMu. Only the ingress takes ingestMu, so a broker write that stalls
	// under it holds up other ingress POSTs but never the stdio read loop or
	// the re-sync heartbeat.
	ingestMu sync.Mutex

	ctx    context.Context
	cancel context.CancelFunc
}

// NewManager builds the manager. selfUUID is our stable per-host UUID, used to
// exclude our own advertisement from the broadcast peer set (by UUID, so a
// same-named peer isn't wrongly dropped).
func NewManager(codec *Codec, port int, selfUUID, clusterDir string) *Manager {
	peers := newPeerSet(port)
	dedup := newDedupIndex(defaultDedupCapacity)
	// Inter-node traffic is cluster mTLS scoped to pinned peers, unconditionally:
	// while this node is not a live cluster member it neither serves nor
	// broadcasts. The mesh is a live view of the cluster dir, not a snapshot:
	// membership is gated on an active admission or a pin (never on keypair
	// presence, which outlives a membership by design) and is re-derived as the
	// cluster-manager writes it, so a node that joins or leaves while this process
	// runs follows along.
	mesh := clustertrust.Open(clusterDir)
	// Peers come solely from the broker's discovery relay. This service runs no
	// mDNS of its own: the node-scanner daemon advertises this node on its single
	// _nvpair-node record (wl=<port>), and peers arrive as discovery:nodes
	// snapshots. selfUUID drives the relay source's self-filter.
	relaySource := newRelayPeerSource(selfUUID)
	m := &Manager{
		codec:       codec,
		dedup:       dedup,
		peers:       peers,
		mesh:        mesh,
		broadcaster: NewBroadcaster(peers, mesh),
		peerSource:  relaySource,
		relaySource: relaySource,
		activeLocal: make(map[workloadKey]workloadEvent),
		broadcastCh: make(chan []byte, broadcastQueueDepth),
		selfUUID:    selfUUID,
	}
	m.server = NewServer(port, dedup, mesh, m.emitUpsert, m.emitRemove)
	return m
}

// EnableLocalIngress turns on the plaintext loopback workload ingress for
// third-party local producers, bound to addr when Run starts. It must be called
// before Run. A non-loopback address is an error, because that listener is
// plaintext by design.
func (m *Manager) EnableLocalIngress(addr string) error {
	li, err := newLocalIngress(addr, m.ingestLocal)
	if err != nil {
		return err
	}
	m.ingress = li
	return nil
}

func (m *Manager) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	m.ctx = ctx
	m.cancel = cancel
	defer cancel()
	defer m.broadcaster.CloseIdle()

	if err := m.codec.Notify("ready", ReadyParams{Version: Version}); err != nil {
		return fmt.Errorf("failed to send ready notification: %w", err)
	}

	// Subscribe to the broker's discovery relay for wl peer targets. They arrive
	// as discovery:nodes snapshots (the full filtered wl set, handled in
	// handleMessage), each replacing the broadcast peer set wholesale. Non-fatal:
	// the peer set just stays empty if the parent isn't a relay-aware broker.
	if err := m.codec.Notify(noderec.MethodSubscribe, noderec.SubscribeParams{Services: []noderec.ServiceKey{noderec.ServiceWorkload}}); err != nil {
		slog.Warn("failed to subscribe to discovery relay", "err", err)
	}

	serverErrCh := make(chan error, 1)
	go func() {
		if err := m.server.Run(ctx); err != nil {
			serverErrCh <- err
		}
	}()

	// The loopback ingress binds synchronously so a port already in use is a
	// startup failure the broker's supervisor sees, not a silent no-listener.
	if m.ingress != nil {
		if err := m.ingress.Listen(); err != nil {
			return err
		}
		go func() {
			if err := m.ingress.Serve(ctx); err != nil {
				serverErrCh <- err
			}
		}()
	}

	go m.discoveryLoop(ctx)
	go m.resyncLoop(ctx)
	// The single ordered broadcast consumer: frames go out in the order the
	// read loop produced them, so a queued remove cannot overtake the
	// lifecycle event it follows.
	go m.broadcastLoop(ctx)
	// Follow this node into and out of a cluster. Every gate already reads live
	// membership, so the watch exists to notice a change with no traffic flowing
	// and to re-assert our workloads immediately: peers that could not receive
	// them while we were unclustered would otherwise wait for the next heartbeat.
	go m.mesh.Watch(ctx, func(clustered bool) {
		slog.Info("inter-node interface switched personality",
			"clustered", clustered, "peers", m.peers.count())
		// Retire pooled connections to peers the new membership no longer pins.
		// The fan-out does this per round too, but a node that just left a
		// cluster stops fanning out entirely, so without this its sockets would
		// stay open with nothing left to send on them.
		m.broadcaster.DropUnpinned()
		// Detached: the fan-out blocks per peer, and the watch is this node's
		// self-healing loop — it must stay free to observe the next change.
		go m.pushActiveSnapshot()
	})

	readErrCh := make(chan error, 1)
	go func() {
		readErrCh <- m.readLoop(ctx)
	}()

	select {
	case err := <-serverErrCh:
		// A fatal listen failure (e.g. port in use) is reported up; main
		// logs and exits.
		cancel()
		return err
	case err := <-readErrCh:
		// readLoop returns nil on clean EOF/shutdown.
		return err
	case <-ctx.Done():
		return nil
	}
}

// emitUpsert / emitRemove forward a translated remote event to the broker.
func (m *Manager) emitUpsert(w *Workload) error {
	return m.codec.Notify(MethodUpsert, lifecycleParams{WorkloadInfo: w})
}

func (m *Manager) emitRemove(workloadID, nodeID string) error {
	return m.codec.Notify(MethodRemove, removeParams{WorkloadID: workloadID, OriginatedFrom: nodeID})
}

func (m *Manager) discoveryLoop(ctx context.Context) {
	m.refreshPeers(ctx)
	ticker := time.NewTicker(discoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.refreshPeers(ctx)
		}
	}
}

func (m *Manager) refreshPeers(ctx context.Context) {
	nodes, err := m.peerSource.Nodes(ctx)
	if err != nil {
		slog.Debug("discovery refresh failed", "err", err)
		return
	}
	added, removed := m.peers.Replace(nodes)
	if len(added) > 0 || len(removed) > 0 {
		slog.Info("peer set changed",
			"added", added, "removed", removed, "total", m.peers.count(), "source", "relay")
	}
	// A newly-discovered peer (e.g. a node just joining the cluster) never saw
	// our currently in-flight workloads start, so it can't route/load-balance
	// against them. Backfill it by re-broadcasting our active local-origin set;
	// peers dedup, so nodes that already have these ignore the re-push.
	if len(added) > 0 {
		m.pushActiveSnapshot()
	}
}

func (m *Manager) readLoop(ctx context.Context) error {
	for {
		msg, err := m.codec.Read()
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				// Local interface severed (broker exited) — the canonical
				// shutdown signal (spec §12). Stop cleanly.
				slog.Info("local interface closed, shutting down")
				m.cancel()
				return nil
			}
			log.Printf("JSON-RPC read error: %v", err)
			continue
		}
		m.handleMessage(msg)
		if ctx.Err() != nil {
			return nil
		}
	}
}

func (m *Manager) handleMessage(msg *Message) {
	// We issue no requests to the broker, so any response frame is unexpected;
	// ignore it rather than misroute it.
	if msg.IsResponse() {
		return
	}

	if msg.Method == applog.SetLevelMethod {
		resolved, err := applog.HandleSetLevelParams(msg.Params)
		if msg.IsRequest() {
			if err != nil {
				m.codec.RespondError(msg.ID, -32602, err.Error())
				return
			}
			m.codec.Respond(msg.ID, map[string]string{"level": resolved})
		}
		if err != nil {
			slog.Warn("log/set-level rejected", "err", err)
		} else {
			slog.Info("log level changed", "level", resolved)
		}
		return
	}

	switch {
	case msg.Method == noderec.NotifyNodes:
		m.applyDiscovery(msg)

	case isLifecycleMethod(msg.Method):
		m.handleLocalLifecycle(msg)

	case msg.Method == MethodRemove:
		m.handleLocalRemove(msg)

	case msg.Method == "shutdown":
		if msg.IsRequest() {
			m.codec.Respond(msg.ID, nil)
		}
		slog.Info("shutdown requested via JSON-RPC")
		m.cancel()

	default:
		// Unknown method on the local interface: respond with an error if it
		// was a request, otherwise drop-and-log (never broadcast).
		if msg.IsRequest() {
			m.codec.RespondError(msg.ID, -32601, fmt.Sprintf("method not found: %s", msg.Method))
			return
		}
		slog.Debug("ignoring unknown local notification", "method", msg.Method)
	}
}

// applyDiscovery replaces the relay-fed peer source from a discovery:nodes
// snapshot pushed down by the broker relay. The next discovery tick reconciles
// the broadcast target set against the new peer set.
func (m *Manager) applyDiscovery(msg *Message) {
	var res noderec.GetNodesResult
	if err := json.Unmarshal(msg.Params, &res); err != nil {
		slog.Debug("invalid discovery:nodes snapshot", "err", err)
		return
	}
	m.relaySource.set(res.Nodes)
}

// localFrame is a validated local-origin lifecycle or removal frame. Exactly
// one of wl (lifecycle) and removeID (removal) is set. ingress marks a frame
// that came in through the loopback ingress rather than from the broker.
type localFrame struct {
	method     string
	params     json.RawMessage
	wl         *Workload
	removeID   string
	removeNode string
	ingress    bool
}

// parseLocalFrame validates a local-origin frame without side effects, so a
// malformed payload is rejected before anything is tracked, broadcast or
// emitted (spec §7).
func parseLocalFrame(method string, params json.RawMessage) (localFrame, error) {
	switch {
	case isLifecycleMethod(method):
		wl, err := parseLifecycle(params)
		if err != nil {
			return localFrame{}, err
		}
		return localFrame{method: method, params: params, wl: wl}, nil
	case method == MethodRemove:
		id, node, err := parseRemove(params)
		if err != nil {
			return localFrame{}, err
		}
		return localFrame{method: method, params: params, removeID: id, removeNode: node}, nil
	}
	return localFrame{}, fmt.Errorf("unknown method: %s", method)
}

// commitLocal records a validated frame in the re-sync set and enqueues it for
// peers. broadcastMu is held across both steps, so a snapshot can never
// interleave and enqueue a workload after its removal was enqueued. The
// original params are broadcast unchanged so any nodeId the producer supplied
// is preserved for peers' dedup. The removal wire carries only (workloadId,
// originatedFrom) — no engine/runId — so a removal drops every composite key
// matching that pair.
//
// A removal through the ingress also tombstones its (origin, id) pair, and a
// non-ingress lifecycle frame for a tombstoned pair is skipped entirely (see
// Manager.removedIngress). A re-sync-tagged frame is an explicit re-assertion
// rather than a replay, so it is not skipped.
func (m *Manager) commitLocal(f localFrame) {
	m.broadcastMu.Lock()
	defer m.broadcastMu.Unlock()
	if f.wl != nil {
		key := workloadKey{origin: f.wl.OriginatedFrom, engine: f.wl.Engine, runID: f.wl.RunID, id: f.wl.ID}
		if f.ingress {
			m.clearRemoved(f.wl.OriginatedFrom, f.wl.ID)
		} else if !isResyncFrame(f.params) && m.wasRemoved(f.wl.OriginatedFrom, f.wl.ID) {
			slog.Debug("dropping replayed lifecycle for a workload removed through the ingress", "method", f.method, "id", f.wl.ID)
			return
		}
		m.trackActive(key, f.method, f.params, f.wl.State)
		slog.Debug("broadcasting local lifecycle", "method", f.method, "id", f.wl.ID, "state", f.wl.State, "peers", m.peers.count())
	} else {
		m.untrackActive(f.removeNode, f.removeID)
		if f.ingress {
			m.markRemoved(f.removeNode, f.removeID)
		}
		slog.Debug("broadcasting local removal", "workloadId", f.removeID, "node", f.removeNode, "peers", m.peers.count())
	}
	m.broadcastFrame(f.method, f.params)
}

// handleLocalLifecycle applies a Broker-originated lifecycle notification.
// The broker has already applied it to its own store before forwarding, so
// nothing is emitted back up.
func (m *Manager) handleLocalLifecycle(msg *Message) {
	f, err := parseLocalFrame(msg.Method, msg.Params)
	if err != nil {
		slog.Warn("dropping malformed local lifecycle", "method", msg.Method, "err", err)
		return
	}
	m.commitLocal(f)
}

func (m *Manager) handleLocalRemove(msg *Message) {
	f, err := parseLocalFrame(msg.Method, msg.Params)
	if err != nil {
		slog.Warn("dropping malformed local removal", "err", err)
		return
	}
	m.commitLocal(f)
}

// ingestLocal is the loopback ingress's entry: a frame from a local producer
// that is NOT the broker. The broker only applies what this process sends it,
// so the frame is first emitted UP as the translated workloads:upsert /
// workloads:remove — that upward path is what updates the local store, the Jobs
// list, the persisted history and the scheduler's counts — and only on success
// committed like a Broker-originated frame (re-sync set + peer broadcast).
//
// A failed broker write therefore leaves nothing tracked and nothing queued, so
// peers never show a workload the origin broker does not hold; the producer
// gets an error and may retry. ingestMu keeps concurrent POSTs in one order at
// the broker and in the peer queue, and is deliberately not broadcastMu: the
// broker write can block, and broadcastMu is taken by the stdio read loop.
func (m *Manager) ingestLocal(method string, params json.RawMessage) error {
	m.ingestMu.Lock()
	defer m.ingestMu.Unlock()
	f, err := parseIngressFrame(method, params, m.selfUUID)
	if err != nil {
		return err
	}
	if f.wl != nil {
		err = m.emitUpsert(f.wl)
	} else {
		err = m.emitRemove(f.removeID, f.removeNode)
	}
	if err != nil {
		return fmt.Errorf("%w: %v", errBrokerWrite, err)
	}
	m.commitLocal(f)
	return nil
}

// broadcastQueueDepth bounds how many outbound frames can wait for the
// ordered broadcast worker. This bounds queued memory without blocking the
// read loop: a full queue drops each new frame with a warning. Heartbeat and
// backfill can reassert tracked lifecycle state, but neither replays removals.
// If this is too small, we can make it bigger or configurable.
const broadcastQueueDepth = 1024

// broadcastFrame re-marshals a single notification and enqueues it for the
// ordered broadcast worker. Delivery is still asynchronous — a slow peer never
// blocks the read loop — but frames now go out in the order they were produced
// (no batching/conflation): the origin's own view already updated
// synchronously in the broker. Each queued frame is sent separately, but
// delivery remains best-effort and a full queue drops the new frame.
func (m *Manager) broadcastFrame(method string, params json.RawMessage) {
	frame, err := json.Marshal(&Message{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		slog.Error("failed to marshal broadcast frame", "method", method, "err", err)
		return
	}
	if m.broadcastCh == nil {
		// Only reachable by hand-built Managers (tests); NewManager always
		// installs the queue.
		slog.Warn("broadcast queue not initialized, dropping frame", "method", method)
		return
	}
	select {
	case m.broadcastCh <- frame:
	default:
		slog.Warn("broadcast queue full, dropping frame", "method", method)
	}
}

// broadcastLoop is the single ordered consumer of broadcastCh. One worker (not
// a goroutine per frame) preserves queue order, so a queued remove cannot
// overtake the lifecycle event it follows. It does not guarantee delivery.
// Broadcast aborts in-flight attempts on ctx cancellation, so at shutdown the
// loop just exits; frames still queued are dropped with the process.
func (m *Manager) broadcastLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-m.broadcastCh:
			m.broadcaster.Broadcast(ctx, frame)
		}
	}
}

// trackActive records the latest event for a local-origin workload. A
// non-terminal event is retained until it terminates or is removed; a terminal
// event is retained (with an expiry) so the heartbeat re-asserts it a couple of
// times, then it ages out. This is the set a newly-discovered peer is backfilled
// with and the heartbeat re-syncs.
func (m *Manager) trackActive(key workloadKey, method string, params json.RawMessage, state WorkloadState) {
	terminal := state == StateCompleted || state == StateFailed || state == StateCancelled
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	// Monotonic: a single inference goes running→terminal and never back, so a
	// non-terminal event arriving for an identity we already hold terminal is
	// stale (a reordered live event, or a rehydration/heartbeat replay racing a
	// terminal). Dropping it keeps a stale "running" from resurrecting a
	// finished job and, worse, stripping its expiry so it never ages out.
	if cur, ok := m.activeLocal[key]; ok && cur.terminal && !terminal {
		return
	}
	if terminal {
		m.activeLocal[key] = workloadEvent{method: method, params: params, terminal: true, expiresAt: time.Now().Add(terminalRetention)}
	} else {
		m.activeLocal[key] = workloadEvent{method: method, params: params}
	}
}

// untrackActive drops every re-sync entry matching (origin, id) — a removal /
// retirement. It matches on the pair (not the full key) because the removal
// wire carries no engine/runId.
func (m *Manager) untrackActive(origin, id string) {
	m.activeMu.Lock()
	for k := range m.activeLocal {
		if k.origin == origin && k.id == id {
			delete(m.activeLocal, k)
		}
	}
	m.activeMu.Unlock()
}

// markRemoved tombstones the (origin, id) pair of an ingress removal, whether
// or not anything was tracked for it. It drops the tombstones that have expired
// and, when the memory is still full, the oldest ones (maxRemovedIngress).
func (m *Manager) markRemoved(origin, id string) {
	now := time.Now()
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	if m.removedIngress == nil {
		m.removedIngress = make(map[removedKey]*list.Element)
		m.removedOrder = list.New()
	}
	for front := m.removedOrder.Front(); front != nil && now.After(front.Value.(removedEntry).expiresAt); front = m.removedOrder.Front() {
		m.dropRemoved(front)
	}
	key := removedKey{origin, id}
	if el, ok := m.removedIngress[key]; ok {
		m.dropRemoved(el)
	}
	for len(m.removedIngress) >= maxRemovedIngress {
		m.dropRemoved(m.removedOrder.Front())
	}
	m.removedIngress[key] = m.removedOrder.PushBack(removedEntry{key: key, expiresAt: now.Add(terminalRetention)})
}

// dropRemoved removes one tombstone. The caller holds activeMu.
func (m *Manager) dropRemoved(el *list.Element) {
	delete(m.removedIngress, m.removedOrder.Remove(el).(removedEntry).key)
}

// wasRemoved reports whether (origin, id) holds an unexpired ingress-removal
// tombstone.
func (m *Manager) wasRemoved(origin, id string) bool {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	el, ok := m.removedIngress[removedKey{origin, id}]
	if !ok {
		return false
	}
	if time.Now().After(el.Value.(removedEntry).expiresAt) {
		m.dropRemoved(el)
		return false
	}
	return true
}

func (m *Manager) clearRemoved(origin, id string) {
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	if el, ok := m.removedIngress[removedKey{origin, id}]; ok {
		m.dropRemoved(el)
	}
}

// activeSnapshot copies the current re-sync set (active + not-yet-expired
// terminal events), pruning expired terminals as it goes.
func (m *Manager) activeSnapshot() []workloadEvent {
	now := time.Now()
	m.activeMu.Lock()
	defer m.activeMu.Unlock()
	if len(m.activeLocal) == 0 {
		return nil
	}
	out := make([]workloadEvent, 0, len(m.activeLocal))
	for k, ev := range m.activeLocal {
		if ev.terminal && !ev.expiresAt.IsZero() && now.After(ev.expiresAt) {
			delete(m.activeLocal, k)
			continue
		}
		out = append(out, ev)
	}
	return out
}

// pushActiveSnapshot re-broadcasts this node's current re-sync set so a
// newly-discovered peer learns about jobs it never saw (and recent terminals).
// Each goes out as an ordinary per-event broadcast; receivers dedup, so peers
// that already hold these ignore the re-push, and ordering is irrelevant because
// the receiving broker's store merge is monotonic.
func (m *Manager) pushActiveSnapshot() {
	m.broadcastSnapshot("pushing active workload snapshot to peers")
}

// resyncLoop is the anti-entropy heartbeat: every resyncInterval it re-asserts
// this node's own active + recently-terminal workloads to all peers. Because the
// origin is the single writer for its workloads, a peer that missed a delivery
// or made a wrong node-loss guess may reconcile to the origin's authoritative
// state when later reassertions are queued and delivered. A full queue can drop
// those reassertions, and removals are not part of the re-sync set.
func (m *Manager) resyncLoop(ctx context.Context) {
	ticker := time.NewTicker(resyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.broadcastSnapshot("re-syncing workloads to peers")
		}
	}
}

// broadcastSnapshot re-broadcasts the current re-sync set, one per-event frame
// each. Shared by the discovery backfill and the heartbeat.
func (m *Manager) broadcastSnapshot(reason string) {
	m.broadcastMu.Lock()
	defer m.broadcastMu.Unlock()
	snapshot := m.activeSnapshot()
	if len(snapshot) == 0 {
		return
	}
	slog.Debug(reason, "count", len(snapshot), "peers", m.peers.count())
	for _, ev := range snapshot {
		m.broadcastResync(ev.method, ev.params)
	}
}

// broadcastResync re-broadcasts a stored frame tagged resync:true so the
// receiver bypasses its lifecycle dedup. A re-assertion carries the same
// (originatedFrom, engine, runId, id, state) as the original, which the dedup
// would otherwise drop — defeating the whole point of the heartbeat/backfill.
// The receiving broker's store is the real idempotency authority, so letting
// re-syncs through is safe (an unchanged state is a no-op merge). Falls back to
// a plain broadcast if the params can't be re-wrapped.
func (m *Manager) broadcastResync(method string, params json.RawMessage) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(params, &env); err != nil {
		m.broadcastFrame(method, params)
		return
	}
	env["resync"] = json.RawMessage("true")
	reframed, err := json.Marshal(env)
	if err != nil {
		m.broadcastFrame(method, params)
		return
	}
	m.broadcastFrame(method, reframed)
}
