// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
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
// terminal workloads to peers (the anti-entropy heartbeat), so a dropped
// delivery or a peer's wrong node-loss guess reconciles within a couple of
// intervals. terminalRetention keeps a finished workload in the re-sync set for
// two intervals — so it is re-asserted ~twice before ageing out.
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
// re-asserts it a couple of times (covering a dropped delivery / a peer's wrong
// node-loss guess); an active event has a zero expiresAt and is retained until
// it terminates or is removed.
type workloadEvent struct {
	method    string
	params    json.RawMessage
	terminal  bool
	expiresAt time.Time
}

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
	// and is re-asserted on the heartbeat (resyncLoop) so peers reconcile to this
	// node's authoritative state. Guarded by activeMu.
	activeMu    sync.Mutex
	activeLocal map[workloadKey]workloadEvent

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
	}
	m.server = NewServer(port, dedup, mesh, m.emitUpsert, m.emitRemove)
	return m
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

	go m.discoveryLoop(ctx)
	go m.resyncLoop(ctx)
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

// handleLocalLifecycle validates a Broker-originated lifecycle notification
// and broadcasts it to peers. Malformed payloads are dropped-and-logged and
// never broadcast (spec §7).
func (m *Manager) handleLocalLifecycle(msg *Message) {
	wl, err := parseLifecycle(msg.Params)
	if err != nil {
		slog.Warn("dropping malformed local lifecycle", "method", msg.Method, "err", err)
		return
	}
	key := workloadKey{origin: wl.OriginatedFrom, engine: wl.Engine, runID: wl.RunID, id: wl.ID}
	m.trackActive(key, msg.Method, msg.Params, wl.State)
	slog.Debug("broadcasting local lifecycle", "method", msg.Method, "id", wl.ID, "state", wl.State, "peers", m.peers.count())
	m.broadcastFrame(msg.Method, msg.Params)
}

func (m *Manager) handleLocalRemove(msg *Message) {
	workloadID, nodeID, err := parseRemove(msg.Params)
	if err != nil {
		slog.Warn("dropping malformed local removal", "err", err)
		return
	}
	// Broadcast the original params unchanged so any nodeId the broker supplied
	// is preserved for peers' dedup. The removal wire carries only
	// (workloadId, originatedFrom) — no engine/runId — so drop every composite
	// key matching that pair.
	m.untrackActive(nodeID, workloadID)
	slog.Debug("broadcasting local removal", "workloadId", workloadID, "node", nodeID, "peers", m.peers.count())
	m.broadcastFrame(msg.Method, msg.Params)
}

// broadcastFrame re-marshals a single notification and fans it out to every peer
// asynchronously, so a slow peer never blocks the read loop. Delivery is
// immediate and per-event (no batching/conflation): the origin's own view
// already updated synchronously in the broker, and peers must see each
// transition promptly and individually — a batching window would add latency and
// drop intermediate states, skewing each node's independent scheduling view.
func (m *Manager) broadcastFrame(method string, params json.RawMessage) {
	frame, err := json.Marshal(&Message{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		slog.Error("failed to marshal broadcast frame", "method", method, "err", err)
		return
	}
	go m.broadcaster.Broadcast(m.ctx, frame)
}

// trackActive records the latest event for a local-origin workload. A
// non-terminal event is retained until it terminates or is removed; a terminal
// event is retained (with an expiry) so the heartbeat re-asserts it a couple of
// times, then it ages out. This is the set a newly-discovered peer is backfilled
// with and the heartbeat re-syncs.
func (m *Manager) trackActive(key workloadKey, method string, params json.RawMessage, state WorkloadState) {
	terminal := state == StateCompleted || state == StateFailed
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
// or made a wrong node-loss guess reconciles to the origin's authoritative state
// within a couple of intervals.
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
