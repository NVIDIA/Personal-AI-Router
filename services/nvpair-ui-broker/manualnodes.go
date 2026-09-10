// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"nvpair-shared/appdir"
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
}

type manualNodeStatusEntry struct {
	status     manualNodeStatus
	receivedAt time.Time
}

type persistedManualNode struct {
	ID      string `json:"id,omitempty"`
	Address string `json:"address"`
	Name    string `json:"name,omitempty"`
	TLSPort int    `json:"tls_port,omitempty"`
	MTLS    bool   `json:"mtls,omitempty"`
}

func (e persistedManualNode) key() string {
	if e.ID != "" {
		return e.ID
	}
	if e.Name != "" {
		return e.Name
	}
	return "manual:" + e.Address
}

func defaultManualNodesConfigPath() string {
	if path, err := appdir.Path("configs", "manual-nodes.json"); err == nil {
		return path
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "configs", "manual-nodes.json")
	}
	return filepath.Join("configs", "manual-nodes.json")
}

func loadPersistedManualNodes(path string) ([]persistedManualNode, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []persistedManualNode
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	valid := entries[:0]
	for _, entry := range entries {
		if entry.Address != "" {
			valid = append(valid, entry)
		}
	}
	return valid, nil
}

func writePersistedManualNodes(path string, entries []persistedManualNode) error {
	sort.Slice(entries, func(i, j int) bool { return entries[i].key() < entries[j].key() })
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func (b *Broker) persistManualNode(entry persistedManualNode) error {
	if entry.ID == "" {
		entry.ID = entry.key()
	}
	b.manualPersistMu.Lock()
	defer b.manualPersistMu.Unlock()
	entries, err := loadPersistedManualNodes(b.manualNodesConfigPath)
	if err != nil {
		return err
	}
	replaced := false
	for i := range entries {
		if entries[i].key() == entry.key() {
			entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, entry)
	}
	return writePersistedManualNodes(b.manualNodesConfigPath, entries)
}

func (b *Broker) removePersistedManualNode(id string) error {
	b.manualPersistMu.Lock()
	defer b.manualPersistMu.Unlock()
	entries, err := loadPersistedManualNodes(b.manualNodesConfigPath)
	if err != nil {
		return err
	}
	out := entries[:0]
	for _, entry := range entries {
		if entry.key() != id && entry.ID != id && entry.Name != id {
			out = append(out, entry)
		}
	}
	return writePersistedManualNodes(b.manualNodesConfigPath, out)
}

func (b *Broker) replayPersistedManualNodes(worker *rpcWorker) {
	b.manualMutationMu.Lock()
	defer b.manualMutationMu.Unlock()
	b.manualPersistMu.Lock()
	entries, err := loadPersistedManualNodes(b.manualNodesConfigPath)
	b.manualPersistMu.Unlock()
	if err != nil {
		slog.Warn("failed to load persisted manual nodes", "err", err)
		return
	}
	for _, entry := range entries {
		params, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		if _, rpcErr, err := worker.Call(context.Background(), "node/add", params); err != nil {
			slog.Warn("failed to replay manual node", "id", entry.key(), "err", err)
		} else if rpcErr != nil {
			slog.Warn("manual node replay rejected", "id", entry.key(), "code", rpcErr.Code, "msg", rpcErr.Message)
		}
	}
}

func (b *Broker) manualAliasForKey(id string) string {
	b.manualMu.Lock()
	defer b.manualMu.Unlock()
	if _, ok := b.manualNodeKeys[id]; ok {
		return id
	}
	best := ""
	for alias, key := range b.manualNodeKeys {
		if key == id && (best == "" || alias < best) {
			best = alias
		}
	}
	if best != "" {
		return best
	}
	return id
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
		Models:         mergeModels(s.OllamaModels, s.LMStudioModels),
		ModelsByEngine: manualModelsByEngine(s),
	}
	if s.Address != "" {
		en.Addresses = []string{s.Address}
	}
	return en
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
