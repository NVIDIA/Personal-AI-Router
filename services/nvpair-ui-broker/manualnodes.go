// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"nvpair-shared/clustertrust"
	"nvpair-shared/noderec"
)

// manualNodeStatus is the subset of nvpair-manual-nodes' ManualNodeStatus the
// broker needs. Its JSON tags match the producer's so node/discovered|
// updated|removed payloads unmarshal straight into it (the GPU/CPU/memory
// sub-objects reuse the broker's discovery types, whose tags are identical).
// The ollama_* / lmstudio_* fields drive the per-engine manual→proxy bridge
// (bridgeManualNode); the rest project into the discovery store via
// manualToEnriched.
type manualNodeStatus struct {
	ID             string      `json:"id"`
	Address        string      `json:"address"`
	OllamaUp       bool        `json:"ollama_up"`
	OllamaPort     int         `json:"ollama_port"`
	OllamaModels   []string    `json:"ollama_models,omitempty"`
	LMStudioUp     bool        `json:"lmstudio_up"`
	LMStudioPort   int         `json:"lmstudio_port"`
	LMStudioModels []string    `json:"lmstudio_models,omitempty"`
	NodeInfoPort   int         `json:"node_info_port"`
	GPUs           []GPUInfo   `json:"gpus"`
	CPU            *CPUInfo    `json:"cpu"`
	Memory         *MemoryInfo `json:"memory"`
	TelemetryValid bool        `json:"telemetryValid"`
	MSSince        int64       `json:"msSince"`
	// HostUUID is the remote's stable per-host identity, learned from its
	// node-info /v1/node-info. It lets a manual node key by the same permanent
	// identity as mDNS-discovered nodes (and dedup with itself when the same
	// machine is also discovered). Empty until the node-info probe succeeds.
	HostUUID string `json:"hostUuid,omitempty"`
	// PairNode is true when the prober's node-info leg answered AND reported a
	// service map: the remote is a PAIR node, not a bare inference host. It is the
	// switch between the two ways a manual node reaches inference. A bare host is
	// bridged into the local proxies by its raw engine ports; a PAIR node is
	// folded into the discovery relay instead and reached through its own proxies
	// over cluster mTLS, because its 11434 / 1234 are proxy facades that refuse
	// plaintext from anything but loopback.
	PairNode bool `json:"pair_node"`
	// ClusterUUID is the remote's cluster principal, tri-state (see the prober's
	// NodeInfoResponse): absent means it does not know, present-and-empty means it
	// belongs to no cluster. It is the key a consumer pins the peer's certificate
	// on, so getting it wrong is the difference between mTLS and a 403.
	ClusterUUID *string `json:"cluster_uuid,omitempty"`
	// Services is the remote's {service key: port} set, read from its node-info.
	// It is everything this host would otherwise have read off the peer's mDNS
	// record, which never arrives across a routed or overlay network.
	Services map[noderec.ServiceKey]int `json:"services,omitempty"`
	// Models / ModelsByEngine / LoadedByEngine are a paired PAIR node's inventory,
	// read from its engine manager over cluster mTLS. Empty until this node holds
	// a pin for the peer.
	Models         []string            `json:"models,omitempty"`
	ModelsByEngine map[string][]string `json:"models_by_engine,omitempty"`
	LoadedByEngine map[string][]string `json:"loaded_by_engine,omitempty"`
}

// clusterUUID flattens the tri-state principal for the consumers that only need
// a value. Unknown and unclustered both answer "" — which is correct for every
// use here, since both mean "we hold no principal to pin on".
func (s manualNodeStatus) clusterUUID() string {
	if s.ClusterUUID == nil {
		return ""
	}
	return *s.ClusterUUID
}

type manualNodeStatusEntry struct {
	status     manualNodeStatus
	receivedAt time.Time
}

func manualNodeTelemetry(status manualNodeStatus, hostUUID string) noderec.NodeTelemetry {
	var utilization uint32
	for i := range status.GPUs {
		if status.GPUs[i].UtilizationPercent > utilization {
			utilization = status.GPUs[i].UtilizationPercent
		}
	}
	return noderec.NodeTelemetry{
		HostUUID:          hostUUID,
		GPUUtilizationPct: utilization,
		TelemetryValid:    status.TelemetryValid,
		MSSince:           status.MSSince,
	}
}

// manualToEnriched projects a manual node's status onto the EnrichedNode
// the discovery store holds, so manual nodes share the single
// discovery:get-nodes / discovery:nodes-changed snapshot with mDNS nodes.
// Port is the node-info HTTP port the prober reached (mirroring the SRV
// port the scanner records for mDNS nodes); the user-entered address
// becomes the node's host/address so the snapshot's ipAddress projection
// can resolve it when the address is an IP literal.
func manualToEnriched(s manualNodeStatus) EnrichedNode {
	// Every node carries a non-empty operational key by the time it reaches the
	// store: the remote's real hostUuid once node-info reports it, else the
	// manual id (the user's name or "manual:<address>") until then. This is the
	// manual-node ingestion boundary — downstream keys off HostUUID with no
	// fallback. Once the real UUID is learned, a manually-added machine
	// that's also discovered over mDNS collapses to the one hostUuid-keyed entry.
	hostUUID := s.HostUUID
	if hostUUID == "" {
		hostUUID = s.ID
	}
	en := EnrichedNode{
		ID:             s.ID,
		HostUUID:       hostUUID,
		Host:           s.Address,
		Port:           s.NodeInfoPort,
		GPUs:           s.GPUs,
		CPU:            s.CPU,
		Memory:         s.Memory,
		Clustered:      s.clusterUUID() != "",
		Models:         mergeModels(s.Models, s.OllamaModels, s.LMStudioModels),
		ModelsByEngine: manualModelsByEngine(s),
		LoadedByEngine: s.LoadedByEngine,
	}
	if s.Address != "" {
		en.Addresses = []string{s.Address}
		en.TXT = []string{noderec.KeyIP + "=" + s.Address}
	}
	return en
}

// manualEnriched is manualToEnriched plus the one field it cannot derive from
// the status alone: whether this node pins the peer's certificate. That answer
// lives in the trust store, and it is read live rather than cached, because it
// changes the moment a pairing completes or a member is removed.
func (b *Broker) manualEnriched(s manualNodeStatus) EnrichedNode {
	en := manualToEnriched(s)
	en.Trusted = b.holdsPinFor(s.clusterUUID())
	return en
}

// manualDirectoryNode synthesizes the directory record a manual PAIR node would
// have had if it had been discovered, so every consumer of the discovery relay —
// both inference proxies, the scheduler's inventory, engine-manager's remote
// operations, the workload relay and the errors peer sync — treats it exactly
// like a pinned peer found over mDNS. That is the whole point: a peer reachable
// only across an overlay network is not a second kind of node, it is the same
// node with no multicast between here and there.
//
// ok is false when the node must NOT enter the directory:
//   - it is a bare inference host (no service map): it has no proxy or engine
//     manager to be a peer with, and is bridged into the local proxies instead.
//   - it is this host (its hostUuid is ours): a manual entry naming ourselves
//     must never become a routing target, or the proxy would forward to its own
//     ingress. Identity is the exact test; an address comparison would miss the
//     overlay name for this same machine.
//   - the scanner already holds the node: the daemon's record is authoritative
//     and carries evidence this synthesis cannot (its full ranked address list,
//     its liveness probes), and two writers on one key would fight.
func (b *Broker) manualDirectoryNode(s manualNodeStatus, key string) (noderec.DirectoryNode, bool) {
	if !s.PairNode || len(s.Services) == 0 || key == "" || s.Address == "" {
		return noderec.DirectoryNode{}, false
	}
	if key == b.nodeID {
		slog.Debug("manual node names this host; not folding it into the directory", "id", s.ID)
		return noderec.DirectoryNode{}, false
	}
	if b.store.hasSource(key, sourceScanner) {
		return noderec.DirectoryNode{}, false
	}
	services := make(map[noderec.ServiceKey]noderec.ServiceStatus, len(s.Services))
	for svc, port := range s.Services {
		if svc != "" && port > 0 {
			services[svc] = noderec.ServiceStatus{Port: port}
		}
	}
	if len(services) == 0 {
		return noderec.DirectoryNode{}, false
	}
	clusterUUID := s.clusterUUID()
	return noderec.DirectoryNode{
		HostUUID: key,
		Name:     s.ID,
		// The address the operator typed is this node's canonical one: it is the
		// only route we know works, and unlike a discovered peer there is no
		// published ranking to defer to.
		IP:             s.Address,
		ClusterUUID:    clusterUUID,
		Trusted:        b.holdsPinFor(clusterUUID),
		Services:       services,
		GPUs:           s.GPUs,
		CPU:            s.CPU,
		Memory:         s.Memory,
		Models:         s.Models,
		ModelsByEngine: s.ModelsByEngine,
		LoadedByEngine: s.LoadedByEngine,
		LastSeen:       time.Now().Unix(),
	}, true
}

// holdsPinFor reports whether this node pins the given cluster principal's
// certificate — the same question the scanner answers for a browsed peer, asked
// against live membership rather than a cached annotation.
func (b *Broker) holdsPinFor(clusterUUID string) bool {
	if clusterUUID == "" || b.clusterDir == "" {
		return false
	}
	mesh := clustertrust.Open(b.clusterDir)
	mesh.Refresh()
	return mesh.HasPin(clusterUUID)
}

// applyManualDirectory folds a manual PAIR node into the discovery relay, or
// withdraws it when it stopped qualifying (it went down, it turned out to be a
// bare host, or the scanner took the record over).
func (b *Broker) applyManualDirectory(s manualNodeStatus, key string) {
	node, ok := b.manualDirectoryNode(s, key)
	if !ok {
		b.releaseManualDirectory(key)
		return
	}
	b.manualMu.Lock()
	b.manualRelayKeys[key] = true
	b.manualMu.Unlock()
	b.relayDir.Apply(noderec.NotifyNodeUpdated, node)
}

// releaseManualDirectory withdraws a record this broker synthesized. It withdraws
// only what it put there: the relay directory is keyed by hostUuid and the
// scanner writes the same keys, so removing one the daemon owns would evict a
// live discovered peer from every consumer until its next browse event.
func (b *Broker) releaseManualDirectory(key string) {
	if key == "" {
		return
	}
	b.manualMu.Lock()
	synthesized := b.manualRelayKeys[key]
	delete(b.manualRelayKeys, key)
	b.manualMu.Unlock()
	if !synthesized || b.store.hasSource(key, sourceScanner) {
		return
	}
	b.relayDir.Apply(noderec.NotifyNodeRemoved, noderec.DirectoryNode{HostUUID: key})
}

// manualModelsByEngine builds the per-engine attribution for a manual node from
// the per-engine lists the prober already collected, keyed by the same
// engine-manager engine names discovered nodes use ("ollama", "lmstudio") so the
// two discovery sources present ModelsByEngine identically. An engine with no
// models adds no key; returns nil when neither engine reports any.
func manualModelsByEngine(s manualNodeStatus) map[string][]string {
	byEngine := map[string][]string{}
	if len(s.OllamaModels) > 0 {
		byEngine["ollama"] = s.OllamaModels
	}
	if len(s.LMStudioModels) > 0 {
		byEngine["lmstudio"] = s.LMStudioModels
	}
	if len(byEngine) == 0 {
		return nil
	}
	return byEngine
}

// mergeModels unions per-engine model lists into one de-duplicated,
// order-preserving slice, so a manual node surfaces its models on
// AvailableNode.models the same way a discovered node does. Manual nodes are
// probed directly by nvpair-manual-nodes (models arrive on the status), rather than
// enriched over engine-manager's em HTTP endpoint like discovered nodes.
func mergeModels(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, m := range l {
			if m != "" && !seen[m] {
				seen[m] = true
				out = append(out, m)
			}
		}
	}
	return out
}

// proxyManualNode is the node/add-manual payload the broker hands
// ollama-proxy to bridge a reachable manual node into inference routing.
// It mirrors the proxy's Node wire shape (id/host/port/addresses[/txt]);
// the proxy requires a non-empty address list and a port to forward to.
type proxyManualNode struct {
	ID        string   `json:"id"`
	Host      string   `json:"host"`
	Port      int      `json:"port"`
	Addresses []string `json:"addresses"`
	TXT       []string `json:"txt,omitempty"`
	Models    []string `json:"models,omitempty"`
}

// bridgeManualNode keeps every supervised proxy's manual-node set in step with
// a manual node's per-engine reachability: a node whose Ollama is up is bridged
// into ollama-proxy and one whose LM Studio is up into lmstudio-proxy
// (idempotent — each proxy upserts on a repeat), while an engine that is not
// (or no longer) reachable is removed from its proxy. Each leg is a no-op when
// that proxy isn't supervised — the bridge only applies when the broker owns
// both ends.
//
// Manual nodes are, by definition, the nodes that never appear in the discovery
// relay's snapshots — they advertise no _nvpair-node record for the scanner
// daemon to carry — so without this explicit add the proxies can't route
// inference to them even though both workers are broker-owned.
func (b *Broker) bridgeManualNode(s manualNodeStatus, key string) {
	if s.PairNode {
		// A PAIR node reaches the proxies through the discovery relay instead,
		// which is what gets it dialed over cluster mTLS against its pinned
		// certificate. Bridging it here as well would add a second candidate for
		// the same node that the proxy dials in plaintext — straight into the
		// peer's loopback-only refusal — so the raw-engine bridge is withdrawn
		// rather than left alongside.
		b.removeManualNodeFromProxies(key)
		return
	}
	b.bridgeToProxy(b.getProxy(), "ollama", s, key, s.OllamaUp, s.OllamaPort, s.OllamaModels)
	b.bridgeToProxy(b.getLMStudioProxy(), "lmstudio", s, key, s.LMStudioUp, s.LMStudioPort, s.LMStudioModels)
}

// bridgeToProxy adds the node to p when its engine is reachable, or removes it
// otherwise. up/port/models are the engine-specific fields the caller pulled
// off the node's status. The proxy candidate is keyed by `key` — the node's
// operational identity (its hostUuid once node-info reports it, else the manual
// id) — the same key the discovery store and scheduler use, so the scheduler's
// priority list and scheduledOn resolve to this candidate.
func (b *Broker) bridgeToProxy(p *proxyProcess, engine string, s manualNodeStatus, key string, up bool, port int, models []string) {
	if p == nil {
		return
	}
	if up && s.Address != "" && port > 0 {
		node := proxyManualNode{
			ID:        key,
			Host:      s.Address,
			Port:      port,
			Addresses: []string{s.Address},
			Models:    models,
		}
		b.callProxyManual(p, engine, "node/add-manual", node, key)
		return
	}
	// Engine unreachable (down, or this node doesn't run it): make sure the
	// proxy isn't left holding a stale manual entry it would try to route to.
	b.callProxyManual(p, engine, "node/remove-manual", map[string]string{"id": key}, key)
}

// removeManualNodeFromProxies drops a manual node from every supervised proxy.
// Idempotent: a no-op for a proxy where the node was never bridged or that
// isn't supervised (the proxy's RemoveManual just reports removed=false).
func (b *Broker) removeManualNodeFromProxies(id string) {
	b.callProxyManual(b.getProxy(), "ollama", "node/remove-manual", map[string]string{"id": id}, id)
	b.callProxyManual(b.getLMStudioProxy(), "lmstudio", "node/remove-manual", map[string]string{"id": id}, id)
}

// callProxyManual issues a best-effort node/add-manual|remove-manual to a
// proxy. The bridge is advisory plumbing the broker owns, not part of the
// manual-node request's own response, so a failure is logged and swallowed
// rather than surfaced to the client. A nil proxy (not supervised) is a no-op.
// Runs synchronously on the manual-nodes reader goroutine; a proxy answers
// these control-plane calls locally in well under proxyCallTimeout, and
// manual-node events are infrequent (one per 10s probe), so the brief inline
// call keeps add/remove strictly ordered without a queue.
func (b *Broker) callProxyManual(p *proxyProcess, engine, method string, params any, id string) {
	if p == nil {
		return
	}
	raw, err := json.Marshal(params)
	if err != nil {
		slog.Warn("manual->proxy bridge: marshal failed", "engine", engine, "method", method, "id", id, "err", err)
		return
	}
	if _, rpcErr, err := p.Call(context.Background(), method, raw); err != nil {
		slog.Warn("manual->proxy bridge failed", "engine", engine, "method", method, "id", id, "err", err)
	} else if rpcErr != nil {
		slog.Warn("manual->proxy bridge rejected", "engine", engine, "method", method, "id", id, "code", rpcErr.Code, "msg", rpcErr.Message)
	}
}
