// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"slices"
	"sort"
	"sync"

	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/errors"
	"nvpair-shared/noderec"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
// See versions.json at the repo root for the source of truth.
var Version = "dev"

// ServiceError and ClearParams are aliased to the shared wire types so
// every component (nvpair-errors, the supervising broker, the integration
// tests) speaks the same struct. The full field-by-field documentation
// — including the cross-node forward-compatibility contract for NodeID
// and ClearedBy — lives on the shared definitions in
// nvpair-shared/errors/errors.go.
type (
	ServiceError = errors.ServiceError
	ClearParams  = errors.ClearParams
)

// ReadyParams matches the shape every other NVPAIR subprocess emits on
// startup so the supervising broker's existing event readers do not
// need a special case for us.
type ReadyParams struct {
	Version string `json:"version"`
}

// errKey is the composite map key. Error ids are only unique WITHIN a node
// (two nodes both emit "engine-manager:start-failed:ollama"), so a store that
// holds peers' errors alongside its own would collide on the id alone. We key
// by (NodeID, ID) so every node's entries coexist; the on-wire list shapes
// (errors:update / errors:get-initial) are a flat []ServiceError, sorted
// deterministically.
type errKey struct {
	nodeID string
	id     string
}

type Manager struct {
	codec  *Codec
	cancel context.CancelFunc
	ctx    context.Context // set in Run; guards relay-event injection

	// relayEvents, when non-nil (peer-sync enabled), receives DiscoveryEvents
	// diffed from the broker relay's discovery:nodes snapshot stream, fed into the
	// same channel PeerSync consumes from. nil leaves the errors service a
	// stdio-only datastore.
	relayEvents chan<- DiscoveryEvent

	// relayPeers is the last er-peer set projected from the relay snapshot, keyed
	// by nodeId. applyDiscovery diffs each incoming snapshot against it to emit
	// only the discovered/updated/removed events PeerSync needs (its downstream is
	// event-driven), avoiding a cold-start re-push for unchanged peers. Only ever
	// touched on the single read-loop goroutine, so it needs no lock.
	relayPeers map[string]RawNode

	// localNodeID is this node's origin id. It must match the value the broker
	// stamps on outbound reports (the stable per-host UUID it resolves and passes
	// via --node-id) and this node's advertised hostUuid, so "is this entry
	// mine?" is a single equality check that survives a PC rename. Used
	// to decide what we serve/push to peers (only our own origin) and what
	// clear-by-id targets. Defaults to the hostname for the bare/standalone
	// binary; the broker overrides it with the UUID via SetLocalNodeID.
	localNodeID string

	mu     sync.RWMutex
	errors map[errKey]ServiceError

	// onLocalChange, if set, is invoked (without holding mu) after a
	// LOCAL-origin change commits — a producer/broker report upsert or
	// clear. PeerSync subscribes to it to push our updated snapshot to
	// peers. Peer-originated changes (reconcilePeer / evictNode) do NOT
	// fire it: those never alter our local-origin set, so re-pushing
	// would be wasted work and risks an echo loop.
	onLocalChange func()
}

// NewManager builds a Manager whose local node id defaults to the hostname for
// the bare/standalone binary; the broker overrides it with this node's stable
// per-host UUID via SetLocalNodeID (--node-id) so cluster attribution survives a
// PC rename. Networking is wired separately in main.go; a Manager with no HTTP
// server / discovery behaves exactly like the pre-sync datastore.
func NewManager(codec *Codec) *Manager {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return &Manager{
		codec:       codec,
		localNodeID: host,
		errors:      make(map[errKey]ServiceError),
		relayPeers:  make(map[string]RawNode),
	}
}

// LocalNodeID exposes the resolved local node id so the networking
// layer advertises the same instance name the store attributes local
// errors to.
func (m *Manager) LocalNodeID() string { return m.localNodeID }

// SetLocalNodeID overrides the local origin id (the broker passes the stable
// per-host UUID via --node-id). Called once during startup wiring, before the
// read loop / peer-sync begin, so the field needs no lock.
func (m *Manager) SetLocalNodeID(id string) {
	if id != "" {
		m.localNodeID = id
	}
}

// SetOnLocalChange registers the push trigger. Called once during
// startup wiring, before the read loop begins, so no locking is
// needed around the field itself.
func (m *Manager) SetOnLocalChange(fn func()) { m.onLocalChange = fn }

// SetRelayEvents wires the discovery-relay peer stream. Called once during
// startup wiring (before the read loop begins) when peer-sync is enabled, so no
// locking is needed. Enabling it also makes Run subscribe to the broker relay
// for er nodes.
func (m *Manager) SetRelayEvents(ch chan<- DiscoveryEvent) { m.relayEvents = ch }

// notifyLocalChange fires the push hook if one is registered. Kept
// separate so the lock-ordering rule (never call out while holding
// mu) is obvious at every call site.
func (m *Manager) notifyLocalChange() {
	if m.onLocalChange != nil {
		m.onLocalChange()
	}
}

func (m *Manager) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	m.ctx = ctx
	m.cancel = cancel
	defer cancel()

	if err := m.codec.Notify("ready", ReadyParams{Version: Version}); err != nil {
		return fmt.Errorf("failed to send ready notification: %w", err)
	}

	// When cross-node sync is on, subscribe to the broker's discovery relay for
	// er peer targets. They arrive as discovery:nodes snapshots (the full filtered
	// er set, handled below), which applyDiscovery reconciles into PeerSync's
	// events. Non-fatal: peer-sync just stays empty if the parent isn't a
	// relay-aware broker.
	if m.relayEvents != nil {
		if err := m.codec.Notify(noderec.MethodSubscribe, noderec.SubscribeParams{Services: []noderec.ServiceKey{noderec.ServiceErrors}}); err != nil {
			slog.Warn("failed to subscribe to discovery relay", "err", err)
		}
	}

	return m.readLoop(ctx)
}

func (m *Manager) readLoop(ctx context.Context) error {
	for {
		msg, err := m.codec.Read()
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			log.Printf("JSON-RPC read error: %v", err)
			var de *DecodeError
			if stderrors.As(err, &de) {
				log.Printf("JSON-RPC decode error (skipping frame): %v", err)
				continue
			}
			// Terminal transport/scanner error (e.g. an over-long frame —
			// bufio.Scanner cannot resync) — stop instead of spinning.
			log.Printf("JSON-RPC read error (terminal): %v", err)
			return err
		}
		m.handleMessage(msg)
		if ctx.Err() != nil {
			return nil
		}
	}
}

// upsert applies an incoming ServiceError to the in-memory map using
// highest-timestamp-wins resolution. Returns true if the stored state
// changed (insert OR replace), false if the report was a no-op (dropped
// because an existing entry with the same id had a newer or equal
// timestamp).
//
// The timestamp rule is what keeps cross-node propagation convergent in
// the future: out-of-order delivery from different cluster paths can
// flip "last arrival" without flipping "most recent emit", so we tie-
// break on timestamp rather than arrival order. For the single-node
// case the two are equivalent because each id has a single
// producer.
//
// Equal-timestamp collisions are a real possibility on low-resolution
// clocks; we choose "incoming wins on equal timestamp" so that a
// producer re-emitting a steady-state error (same id, same ms) still
// refreshes the stored message and metadata. This means equal-timestamp
// is NOT a no-op — the caller will get changed=true and emit an
// errors:update. That is the right behavior: a re-emit is the
// producer saying "still broken right now."
func (m *Manager) upsert(e ServiceError) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := errKey{nodeID: e.NodeID, id: e.ID}
	if existing, ok := m.errors[k]; ok && existing.Timestamp > e.Timestamp {
		return false
	}
	m.errors[k] = e
	return true
}

// clearByID removes the LOCAL-origin entry with the given id. Returns
// true if an entry was actually deleted (so the caller knows whether
// to broadcast errors:update); false on no-op clears (id was already
// absent — common when a clear races with an earlier dismissal or when
// a producer fires a defensive clear-on-success without having reported
// failure first).
//
// Clear is scoped to (localNodeID, id) on purpose: clears arrive from
// local producers / the broker for errors THIS node owns. A peer's
// error is owned by its origin node, so a local clear must not delete
// it — the origin would just re-push it on the next sync, and silently
// dropping it locally for one heartbeat would only cause UI flicker.
func (m *Manager) clearByID(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := errKey{nodeID: m.localNodeID, id: id}
	if _, ok := m.errors[k]; !ok {
		return false
	}
	delete(m.errors, k)
	return true
}

// sortServiceErrors orders by id first (the pre-sync contract the UI
// relies on for cheap diff-by-position) then by nodeId so entries that
// share an id across nodes have a stable, deterministic order.
func sortServiceErrors(out []ServiceError) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].NodeID < out[j].NodeID
	})
}

// snapshot returns the current full list of errors (every node's),
// sorted deterministically. Both errors:get-initial responses and
// errors:update push payloads use this, so the UI sees the merged
// cross-node view.
func (m *Manager) snapshot() []ServiceError {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ServiceError, 0, len(m.errors))
	for _, e := range m.errors {
		out = append(out, e)
	}
	sortServiceErrors(out)
	return out
}

// localSnapshot returns only this node's local-origin errors, sorted.
// This is exactly what we serve at GET /v1/errors and push to peers —
// never foreign entries, which is the loop-prevention guarantee.
func (m *Manager) localSnapshot() []ServiceError {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]ServiceError, 0, len(m.errors))
	for k, e := range m.errors {
		if k.nodeID == m.localNodeID {
			out = append(out, e)
		}
	}
	sortServiceErrors(out)
	return out
}

// reconcilePeer applies a peer's full local-origin set authoritatively
// for that nodeID: upsert every entry present (timestamp tie-break) and
// evict any stored entry for the same nodeID absent from the set. The
// envelope's NodeID is stamped onto every entry so a peer can only ever
// mutate its own slice of the keyspace. Returns true if stored state
// changed (caller emits errors:update).
//
// nodeID == localNodeID is rejected as a no-op: a peer must never be
// able to rewrite our own origin's errors.
func (m *Manager) reconcilePeer(nodeID string, errs []ServiceError) bool {
	if nodeID == "" || nodeID == m.localNodeID {
		return false
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	incoming := make(map[string]ServiceError, len(errs))
	for _, e := range errs {
		if e.ID == "" {
			continue
		}
		e.NodeID = nodeID
		incoming[e.ID] = e
	}

	changed := false

	for id, e := range incoming {
		k := errKey{nodeID: nodeID, id: id}
		if existing, ok := m.errors[k]; ok && existing.Timestamp > e.Timestamp {
			continue
		}
		if existing, ok := m.errors[k]; ok && existing == e {
			continue
		}
		m.errors[k] = e
		changed = true
	}

	for k := range m.errors {
		if k.nodeID != nodeID {
			continue
		}
		if _, ok := incoming[k.id]; !ok {
			delete(m.errors, k)
			changed = true
		}
	}

	return changed
}

// evictNode removes every stored entry that originated on nodeID.
// Called when a peer drops out of the relay's discovery snapshot so a node that
// leaves the network stops haunting everyone else's error list. Returns true if
// anything was removed.
func (m *Manager) evictNode(nodeID string) bool {
	if nodeID == "" || nodeID == m.localNodeID {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := false
	for k := range m.errors {
		if k.nodeID == nodeID {
			delete(m.errors, k)
			changed = true
		}
	}
	return changed
}

// ReconcilePeer is the exported entry point the HTTP ingest handler
// calls. It applies the peer set and, if anything changed, broadcasts
// the merged list to the local UI. It deliberately does NOT fire
// onLocalChange — see the field docs on why peer-driven changes don't
// trigger an outbound push.
func (m *Manager) ReconcilePeer(nodeID string, errs []ServiceError) {
	if m.reconcilePeer(nodeID, errs) {
		m.emitUpdate()
		slog.Info("reconciled peer errors", "nodeId", nodeID, "count", len(errs))
	}
}

// EvictNode is the exported entry point the discovery layer calls when
// a peer is removed. Broadcasts the merged list if anything changed.
func (m *Manager) EvictNode(nodeID string) {
	if m.evictNode(nodeID) {
		m.emitUpdate()
		slog.Info("evicted departed peer errors", "nodeId", nodeID)
	}
}

// LocalSnapshot is the exported accessor PeerSync and the HTTP server
// use to read this node's pushable error set.
func (m *Manager) LocalSnapshot() []ServiceError { return m.localSnapshot() }

// emitUpdate pushes the current snapshot as an errors:update
// notification. Callers fire this AFTER any mutating operation (upsert
// / clearByID returned true) so consumers see a strict order:
// request-response (or notification accepted), then update broadcast.
// Failure to send is logged but not propagated — the consumer can
// always re-fetch via errors:get-initial.
func (m *Manager) emitUpdate() {
	if err := m.codec.Notify("errors:update", m.snapshot()); err != nil {
		slog.Warn("failed to emit errors:update", "err", err)
	}
}

func (m *Manager) handleMessage(msg *Message) {
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

	// Notifications for errors:report / errors:clear are the fire-and-
	// forget form producers use on their existing stdio. They share all
	// the upsert / clear / emitUpdate plumbing with the request form;
	// the only difference is no response goes out.
	if msg.IsNotification() {
		switch msg.Method {
		case "errors:report":
			m.handleReport(nil, msg.Params)
		case "errors:clear":
			m.handleClear(nil, msg.Params)
		case noderec.NotifyNodes:
			m.applyDiscovery(msg)
		default:
			log.Printf("ignoring incoming notification: %s", msg.Method)
		}
		return
	}

	if !msg.IsRequest() {
		return
	}

	switch msg.Method {
	case "errors:get-initial":
		m.codec.Respond(msg.ID, m.snapshot())

	case "errors:report":
		m.handleReport(msg.ID, msg.Params)

	case "errors:clear":
		m.handleClear(msg.ID, msg.Params)

	case "shutdown":
		if err := m.codec.Respond(msg.ID, nil); err != nil {
			log.Printf("failed to respond to shutdown: %v", err)
		}
		log.Println("shutdown requested via JSON-RPC")
		m.cancel()

	default:
		if err := m.codec.RespondError(msg.ID, -32601, fmt.Sprintf("method not found: %s", msg.Method)); err != nil {
			log.Printf("failed to send error response: %v", err)
		}
	}
}

// applyDiscovery reconciles the er-peer set against a discovery:nodes snapshot
// pushed down by the broker relay, emitting only the DiscoveryEvents that bring
// PeerSync in line: a newly-present peer -> "discovered", a peer whose dialable
// details changed -> "updated", a peer that dropped out -> "removed". PeerSync's
// downstream is event-driven (a "discovered"/"updated" triggers a cold-start push
// and "removed" evicts), so re-emitting unchanged peers on every snapshot would
// storm pushes; the snapshot is diffed against the last one to avoid that. The
// snapshot is authoritative, so any prior drift is self-corrected.
func (m *Manager) applyDiscovery(msg *Message) {
	if m.relayEvents == nil {
		return
	}
	var res noderec.GetNodesResult
	if err := json.Unmarshal(msg.Params, &res); err != nil {
		slog.Debug("invalid discovery:nodes snapshot", "err", err)
		return
	}
	next := make(map[string]RawNode, len(res.Nodes))
	for _, n := range res.Nodes {
		peer, ok := directoryToPeer(n)
		if !ok {
			continue
		}
		next[peer.ID] = peer
	}
	for id, peer := range next {
		prev, had := m.relayPeers[id]
		switch {
		case !had:
			m.emitRelayEvent(DiscoveryEvent{Type: "discovered", Node: peer})
		case !rawNodeEqual(prev, peer):
			m.emitRelayEvent(DiscoveryEvent{Type: "updated", Node: peer})
		}
	}
	for id := range m.relayPeers {
		if _, still := next[id]; !still {
			m.emitRelayEvent(DiscoveryEvent{Type: "removed", Node: RawNode{ID: id}})
		}
	}
	m.relayPeers = next
}

// emitRelayEvent hands one event to PeerSync, abandoning the send if the process
// is shutting down (ctx cancelled) so a full channel can't wedge the read loop.
func (m *Manager) emitRelayEvent(ev DiscoveryEvent) {
	select {
	case m.relayEvents <- ev:
	case <-m.ctx.Done():
	}
}

// directoryToPeer projects a relay DirectoryNode onto the errors service's er
// peer, returning false when it doesn't advertise er or has no address. The nodeId is
// the peer's stable hostUuid, matching the UUID each peer stamps as the origin
// of its errors and uses as its own localNodeID — so the self-check and
// EvictNode key on an identity that survives a peer's PC rename. A
// relay DirectoryNode always carries a hostUuid (the scanner guarantees it), so
// there is no name fallback. Host stays the hostname (display / dial name);
// cluster-uuid= is reconstructed into TXT so PeerSync's mTLS pin lookup is
// unchanged.
func directoryToPeer(n noderec.DirectoryNode) (RawNode, bool) {
	svc, ok := n.Services[noderec.ServiceErrors]
	addresses := n.CandidateIPs()
	if !ok || len(addresses) == 0 {
		return RawNode{}, false
	}
	txt := n.AddressTXT()
	if n.ClusterUUID != "" {
		txt = append(txt, clustertrust.ClusterUUIDTXTKey+"="+n.ClusterUUID)
	}
	return RawNode{
		ID:        n.HostUUID,
		Host:      n.Name,
		Port:      svc.Port,
		Addresses: addresses,
		TXT:       txt,
	}, true
}

// rawNodeEqual reports whether two projected peers carry the same dialable
// identity — the fields PeerSync uses to build a push target. Only these matter:
// a change in any of them warrants a fresh cold-start push, anything else does
// not.
func rawNodeEqual(a, b RawNode) bool {
	if a.ID != b.ID || a.Host != b.Host || a.Port != b.Port {
		return false
	}
	if !slices.Equal(a.Addresses, b.Addresses) || !slices.Equal(a.TXT, b.TXT) {
		return false
	}
	return true
}

// handleReport processes an errors:report frame in either request or
// notification form. id == nil means notification (no response sent);
// id != nil means request and a null result is returned on success.
//
// Validation is intentionally light: id and message are required, but
// anything else (severity / action / engineType / operation / modelName)
// passes through. The producer is the authority on what those fields
// mean — nvpair-errors is a passive datastore.
func (m *Manager) handleReport(id *json.RawMessage, params json.RawMessage) {
	var e ServiceError
	if err := json.Unmarshal(params, &e); err != nil {
		if id != nil {
			m.codec.RespondError(id, -32602, "invalid params: "+err.Error())
		} else {
			slog.Warn("errors:report notification: invalid params", "err", err)
		}
		return
	}
	if e.ID == "" {
		if id != nil {
			m.codec.RespondError(id, -32602, `invalid params: "id" is required`)
		} else {
			slog.Warn(`errors:report notification: "id" is required`)
		}
		return
	}
	if e.Message == "" {
		if id != nil {
			m.codec.RespondError(id, -32602, `invalid params: "message" is required`)
		} else {
			slog.Warn(`errors:report notification: "message" is required`)
		}
		return
	}

	changed := m.upsert(e)

	if id != nil {
		// Respond first so the peer's "report acknowledged" callback
		// always observes the response strictly before the update push.
		if err := m.codec.Respond(id, nil); err != nil {
			log.Printf("failed to respond to errors:report: %v", err)
		}
	}

	if changed {
		m.emitUpdate()
		m.notifyLocalChange()
		slog.Info("errors:report upserted", "id", e.ID, "nodeId", e.NodeID, "timestamp", e.Timestamp)
	} else {
		slog.Debug("errors:report dropped: existing entry has newer timestamp", "id", e.ID)
	}
}

// handleClear processes an errors:clear frame in either request or
// notification form. id == nil means notification.
//
// ClearedBy is accepted but not stored or acted upon — it exists for
// cross-node clear propagation (broker uses it to filter outbound
// clears). For now we just delete by id.
func (m *Manager) handleClear(id *json.RawMessage, params json.RawMessage) {
	var p ClearParams
	if err := json.Unmarshal(params, &p); err != nil {
		if id != nil {
			m.codec.RespondError(id, -32602, "invalid params: "+err.Error())
		} else {
			slog.Warn("errors:clear notification: invalid params", "err", err)
		}
		return
	}
	if p.ID == "" {
		if id != nil {
			m.codec.RespondError(id, -32602, `invalid params: "id" is required`)
		} else {
			slog.Warn(`errors:clear notification: "id" is required`)
		}
		return
	}

	changed := m.clearByID(p.ID)

	if id != nil {
		if err := m.codec.Respond(id, nil); err != nil {
			log.Printf("failed to respond to errors:clear: %v", err)
		}
	}

	if changed {
		m.emitUpdate()
		m.notifyLocalChange()
		slog.Info("errors:clear removed entry", "id", p.ID, "clearedBy", p.ClearedBy)
	} else {
		slog.Debug("errors:clear no-op: id not present", "id", p.ID)
	}
}
