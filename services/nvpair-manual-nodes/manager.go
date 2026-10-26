// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/errors"
	"nvpair-shared/noderec"
)

// Version is stamped at build time via -ldflags "-X main.Version=...".
// See versions.json at the repo root for the source of truth.
var Version = "dev"

const (
	probeInterval = 10 * time.Second
	probeTimeout  = 3 * time.Second

	// probeFailThreshold is the number of CONSECUTIVE probes (at
	// probeInterval = 10s, so 30 s of unreachability) where neither
	// Ollama nor node-info answered before we surface the node as
	// "probe failed" through the errors pipeline. This surfaces an error;
	// it does not evict, so it is deliberately shorter than the ~60 s
	// shared/discovery waits before dropping an mDNS node.
	//
	// One-shot emit: the report fires only on the transition INTO
	// the failed state, so re-emitting on every probe-while-failed
	// would defeat ack-until-reemit. A subsequent successful probe
	// resets the counter; the next failure cycle is its own event.
	probeFailThreshold = 3
)

type (
	GPUInfo    = noderec.GPUInfo
	CPUInfo    = noderec.CPUInfo
	MemoryInfo = noderec.MemoryInfo
)

// NodeInfoResponse adds the local proof and receipt time to the shared snapshot.
// Those fields never travel over the hardware response's JSON boundary.
type NodeInfoResponse struct {
	noderec.NodeInfo
	discoveryRecordID string
	receivedAt        time.Time
}

// ManualEntry identifies a user-configured endpoint by its bare hostname or IP
// address and optional name. TLSPort overrides the node-info port, defaulting
// to 14318 when zero. Hardware probes always use HTTPS with current cluster
// pins and our client certificate, regardless of TLSPort or MTLS. MTLS is an
// informational configuration flag echoed in status, not a transport switch.
// The separate Ollama and LM Studio engine probes use plain HTTP.
type ManualEntry struct {
	Address string `json:"address"`
	Name    string `json:"name"`
	TLSPort int    `json:"tls_port,omitempty"`
	MTLS    bool   `json:"mtls,omitempty"`
}

// ManualNodeStatus combines a manual entry with its latest probe results.
// TLSEnabled and MTLSRequired echo the entry's configuration hints for the UI;
// they do not describe whether the hardware probe uses TLS (it always does).
// NodeInfoPort is the attempted HTTPS port, even when that probe fails.
type ManualNodeStatus struct {
	DiscoveryRecordID    string                       `json:"discoveryRecordId,omitempty"`
	IdentityVerification noderec.IdentityVerification `json:"identityVerification"`
	ID                   string                       `json:"id"`
	Name                 string                       `json:"name,omitempty"`
	Address              string                       `json:"address"`
	OllamaUp             bool                         `json:"ollama_up"`
	OllamaPort           int                          `json:"ollama_port"`
	OllamaModels         []string                     `json:"ollama_models,omitempty"`
	// LM Studio is probed on its default OpenAI-API port the same way Ollama
	// is on 11434, so a manually-added node running LM Studio can be bridged
	// into lmstudio-proxy by a supervising broker.
	LMStudioUp     bool        `json:"lmstudio_up"`
	LMStudioPort   int         `json:"lmstudio_port"`
	LMStudioModels []string    `json:"lmstudio_models,omitempty"`
	NodeInfoUp     bool        `json:"node_info_up"`
	NodeInfoPort   int         `json:"node_info_port"`
	TLSEnabled     bool        `json:"tls_enabled,omitempty"`
	MTLSRequired   bool        `json:"mtls_required,omitempty"`
	GPUs           []GPUInfo   `json:"gpus,omitempty"`
	CPU            *CPUInfo    `json:"cpu,omitempty"`
	Memory         *MemoryInfo `json:"memory,omitempty"`
	TelemetryValid bool        `json:"telemetryValid"`
	MSSince        int64       `json:"msSince"`
	// HostUUID is the remote's stable per-host identity from node-info, so a
	// manual node carries the same permanent identity the rest of the system
	// keys on. Empty when node-info didn't report one.
	HostUUID string `json:"hostUuid,omitempty"`
}

// sameAvailability compares identity, configuration, engine models, and service
// availability. Every authenticated hardware sample is published independently.
func (s ManualNodeStatus) sameAvailability(other ManualNodeStatus) bool {
	return s.DiscoveryRecordID == other.DiscoveryRecordID &&
		s.IdentityVerification == other.IdentityVerification &&
		s.ID == other.ID &&
		s.Name == other.Name &&
		s.Address == other.Address &&
		s.OllamaUp == other.OllamaUp &&
		s.OllamaPort == other.OllamaPort &&
		sliceEqual(s.OllamaModels, other.OllamaModels) &&
		s.LMStudioUp == other.LMStudioUp &&
		s.LMStudioPort == other.LMStudioPort &&
		sliceEqual(s.LMStudioModels, other.LMStudioModels) &&
		s.NodeInfoUp == other.NodeInfoUp &&
		s.NodeInfoPort == other.NodeInfoPort &&
		s.TLSEnabled == other.TLSEnabled &&
		s.MTLSRequired == other.MTLSRequired &&
		s.HostUUID == other.HostUUID
}

func (s ManualNodeStatus) withoutVerification() ManualNodeStatus {
	s.IdentityVerification = noderec.IdentityUnverified
	s.DiscoveryRecordID, s.HostUUID = "", ""
	s.NodeInfoUp = false
	s.GPUs, s.CPU, s.Memory = nil, nil, nil
	s.TelemetryValid, s.MSSince = false, 0
	return s
}

type ReadyParams struct {
	Version string `json:"version"`
}

type trackedNode struct {
	entry  ManualEntry
	status ManualNodeStatus

	// consecutiveFails counts back-to-back probes where neither
	// service answered (OllamaUp && NodeInfoUp both false). Reset
	// to 0 on any probe where at least one service responded.
	// Used to gate probe-failed errors:report emits at
	// probeFailThreshold so a single transient failure doesn't
	// generate UI noise.
	consecutiveFails int
	observedAt       time.Time
}

// statusAt projects cached hardware at send time without resetting its age.
func (n *trackedNode) statusAt(now time.Time) ManualNodeStatus {
	status := n.status
	if status.TelemetryValid {
		status.MSSince = (noderec.NodeInfo{TelemetryValid: true, MSSince: status.MSSince}).Aged(now.Sub(n.observedAt)).MSSince
	}
	return status
}

// Manager keeps manual entries in memory and publishes their probe results.
// The supervising broker owns durable configuration and downstream routing.
type Manager struct {
	codec  *Codec
	cancel context.CancelFunc

	// client probes Ollama and LM Studio over plain HTTP. Fresh connections
	// let hostname entries recover after their remote address changes.
	client *http.Client

	// mesh supplies current membership, our client certificate, and exact
	// peer pins. Hardware probes require these even without a TLS port hint.
	mesh *clustertrust.Mesh

	// peerClients reuses clients bound to a previously verified host UUID.
	// Unknown endpoints first use mesh.ClientTLSConfigAny to learn which
	// pinned peer answered, and probeNodeInfo checks that binding afterward.
	peerClients *clustertrust.PeerClientPool

	mu    sync.RWMutex
	nodes map[string]*trackedNode
}

func NewManager(codec *Codec, mesh *clustertrust.Mesh) *Manager {
	return &Manager{
		codec:       codec,
		mesh:        mesh,
		peerClients: clustertrust.NewPeerClientPool(mesh, probeTimeout),
		client: &http.Client{
			Timeout:   probeTimeout,
			Transport: noKeepAliveTransport(),
		},
		nodes: make(map[string]*trackedNode),
	}
}

// noKeepAliveTransport returns a probe transport with idle-connection reuse
// disabled. Probes run every probeInterval, so the cost of a fresh dial is
// negligible, and it guarantees each probe re-resolves the entry's address —
// otherwise a hostname entry could keep reusing a pooled connection to the
// peer's old IP after a sleep/wake or DHCP change instead of picking up the
// new one.
func noKeepAliveTransport() *http.Transport {
	return &http.Transport{DisableKeepAlives: true}
}

func (m *Manager) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	defer cancel()
	defer m.peerClients.CloseIdle()

	if err := m.codec.Notify("ready", ReadyParams{Version: Version}); err != nil {
		return fmt.Errorf("failed to send ready notification: %w", err)
	}

	go m.probeLoop(ctx)
	go m.watchTrust(ctx)

	return m.readLoop(ctx)
}

func (m *Manager) watchTrust(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcileTrust()
		}
	}
}

// reconcileTrust withdraws cached proofs independently of slow engine probes.
// Status writes and notifications share mu with removal and probe completion.
func (m *Manager) reconcileTrust() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mesh.Refresh()
	for _, node := range m.nodes {
		status := node.status
		if status.IdentityVerification == noderec.IdentityPinned && (status.DiscoveryRecordID == "" || status.DiscoveryRecordID != m.mesh.PeerRecordID(status.HostUUID)) {
			node.status = status.withoutVerification()
			if err := m.codec.Notify("node/updated", node.status); err != nil {
				slog.Warn("failed to withdraw manual hardware", "node_id", status.ID, "err", err)
			}
		}
	}
	m.peerClients.DropUnpinned()
}

func (m *Manager) probeLoop(ctx context.Context) {
	ticker := time.NewTicker(probeInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.probeAll(ctx)
		}
	}
}

func (m *Manager) probeAll(ctx context.Context) {
	m.mu.RLock()
	entries := make([]ManualEntry, 0, len(m.nodes))
	for _, tn := range m.nodes {
		entries = append(entries, tn.entry)
	}
	m.mu.RUnlock()

	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		m.probeNode(entry)
	}
}

// probeNode refreshes Ollama and LM Studio reachability and attempts a pinned
// mTLS node-info read for one manual entry. It saves the combined status if the
// entry is still tracked, publishes changes and every accepted hardware sample,
// and reports sustained outages. An answering engine alone does not verify the
// host's identity or authorize its hardware data for scheduling.
func (m *Manager) probeNode(entry ManualEntry) {
	addr := entry.Address
	id := nodeID(entry)
	m.mu.RLock()
	tracked := m.nodes[id]
	m.mu.RUnlock()
	if tracked == nil || tracked.entry != entry {
		return
	}

	ollamaUp, ollamaModels := m.probeOllama(addr, 11434)
	lmStudioUp, lmStudioModels := m.probeLMStudio(addr, lmStudioPort)

	// The entry selects a port, not a protocol: even the default 14318 is
	// probed with pinned HTTPS, never through the public HTTP hardware path.
	nodeInfoPort := 14318
	if entry.TLSPort > 0 {
		nodeInfoPort = entry.TLSPort
	}
	// Refresh membership before selecting a client. A previously verified entry
	// uses its host's exact pin; an unknown endpoint may present any current
	// pin, then must bind its response identities to that certificate.
	m.mesh.Refresh()
	localRecordID := m.mesh.PeerRecordID(m.mesh.NodeUUID())
	nodeInfoUp, info := false, NodeInfoResponse{}
	m.mu.RLock()
	var knownUUID string
	if current := m.nodes[id]; current != nil && current.status.IdentityVerification == noderec.IdentityPinned {
		knownUUID = current.status.HostUUID
	}
	m.mu.RUnlock()
	if client, ok := m.peerClients.Client(knownUUID); knownUUID != "" && ok {
		nodeInfoUp, info = m.probeNodeInfo(client, addr, nodeInfoPort, localRecordID)
	} else if cfg, ok := m.mesh.ClientTLSConfigAny(); ok {
		client := &http.Client{
			Timeout: probeTimeout,
			Transport: &http.Transport{
				TLSClientConfig:   cfg,
				DisableKeepAlives: true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		nodeInfoUp, info = m.probeNodeInfo(client, addr, nodeInfoPort, localRecordID)
	}
	m.peerClients.DropUnpinned()

	newStatus := ManualNodeStatus{
		DiscoveryRecordID:    info.discoveryRecordID,
		IdentityVerification: noderec.IdentityUnverified,
		ID:                   id,
		Name:                 entry.Name,
		Address:              addr,
		OllamaUp:             ollamaUp,
		OllamaPort:           11434,
		OllamaModels:         ollamaModels,
		LMStudioUp:           lmStudioUp,
		LMStudioPort:         lmStudioPort,
		LMStudioModels:       lmStudioModels,
		NodeInfoUp:           nodeInfoUp,
		NodeInfoPort:         nodeInfoPort,
		TLSEnabled:           entry.TLSPort > 0,
		MTLSRequired:         entry.TLSPort > 0 && entry.MTLS,
		GPUs:                 info.GPUs,
		CPU:                  info.CPU,
		Memory:               info.Memory,
		TelemetryValid:       info.TelemetryValid,
		MSSince:              info.MSSince,
		HostUUID:             info.HostUUID,
	}
	if newStatus.NodeInfoUp {
		newStatus.IdentityVerification = noderec.IdentityPinned
	}

	m.mu.Lock()
	tn, exists := m.nodes[id]
	if !exists || tn != tracked {
		m.mu.Unlock()
		return
	}
	defer m.mu.Unlock()
	m.mesh.Refresh()
	if newStatus.NodeInfoUp && (newStatus.DiscoveryRecordID == "" || newStatus.DiscoveryRecordID != m.mesh.PeerRecordID(newStatus.HostUUID)) {
		newStatus = newStatus.withoutVerification()
	}
	reachable := newStatus.OllamaUp || newStatus.LMStudioUp || newStatus.NodeInfoUp

	prev := tn.status
	prevFails := tn.consecutiveFails
	// A failed authenticated read clears service measurements and proof. The
	// configured engine target remains independently available when it answers.
	tn.status = newStatus
	tn.observedAt = info.receivedAt
	if reachable {
		tn.consecutiveFails = 0
	} else {
		tn.consecutiveFails++
	}
	curFails := tn.consecutiveFails

	changed := !prev.sameAvailability(newStatus)

	if changed {
		slog.Info("manual node state changed",
			"node_id", id, "addr", addr,
			"ollama_up", newStatus.OllamaUp, "node_info_up", newStatus.NodeInfoUp,
			"models", len(newStatus.OllamaModels), "gpus", len(newStatus.GPUs))
	} else {
		slog.Debug("manual node probe stable",
			"node_id", id, "addr", addr,
			"ollama_up", newStatus.OllamaUp, "node_info_up", newStatus.NodeInfoUp)
	}
	if changed || newStatus.NodeInfoUp {
		if err := m.codec.Notify("node/updated", tn.statusAt(time.Now())); err != nil {
			slog.Warn("failed to publish manual node status", "node_id", id, "err", err)
		}
	}

	// Surface the consecutive-failure transition through the errors
	// pipeline. The supervising broker (dispatchSubprocessErrorsNotif)
	// reads these off our stdio and forwards to nvpair-errors. NodeID
	// and Timestamp are intentionally left empty/zero — the broker
	// stamps both with the canonical local-node-id and wall clock.
	//
	// Report fires exactly once per failure episode (on the probe
	// where the counter first hits the threshold). A recovery (one
	// reachable probe after having been above threshold) emits
	// errors:clear; further successful probes are no-ops.
	switch {
	case !reachable && prevFails < probeFailThreshold && curFails == probeFailThreshold:
		msg := fmt.Sprintf("Manual node %q has not responded to probes for %d consecutive cycles (%s)", id, probeFailThreshold, probeFailThreshold*probeInterval)
		// A node added by raw IP can't recover on its own if the device was
		// reassigned a new address (sleep/wake, DHCP lease change); a hostname
		// entry re-resolves on the next probe and recovers automatically.
		if net.ParseIP(addr) != nil {
			msg += ". This node was added by IP address, so if the device was reassigned a new IP it will not recover automatically — re-add it with the current address, or add it by hostname so it can be re-resolved."
		}
		if err := m.codec.Notify("errors:report", errors.ServiceError{
			ID:       probeFailedID(id),
			Message:  msg,
			Severity: "warning",
			Action:   "none",
		}); err != nil {
			log.Printf("failed to send errors:report for %s: %v", id, err)
		}
	case reachable && prevFails >= probeFailThreshold:
		if err := m.codec.Notify("errors:clear", errors.ClearParams{
			ID: probeFailedID(id),
		}); err != nil {
			log.Printf("failed to send errors:clear for %s: %v", id, err)
		}
	}
}

// probeFailedID is the canonical ServiceError id for a manual node
// whose consecutive-failure counter has crossed the threshold. Single
// function so the report and clear can never drift (the dispatcher
// matches by literal string).
func probeFailedID(nodeID string) string {
	return "manual-nodes:probe-failed:" + nodeID
}

// lmStudioPort is LM Studio's default OpenAI-API server port, probed the same
// way Ollama is hardcoded to 11434. A manual node is remote, so (like Ollama)
// we assume the engine's default port rather than resolving it via the engine
// manager (which only governs the local engine).
const lmStudioPort = 1234

// probeLMStudio checks LM Studio's OpenAI-compatible server on addr:port. A
// single GET /v1/models doubles as the liveness check and the model list (the
// response is {"data":[{"id":"..."}],...}). Returns whether it is up and the
// model ids it serves.
func (m *Manager) probeLMStudio(addr string, port int) (bool, []string) {
	url := "http://" + net.JoinHostPort(addr, strconv.Itoa(port)) + "/v1/models"
	start := time.Now()
	resp, err := m.client.Get(url)
	if err != nil {
		slog.Debug("manual probe lmstudio failed",
			"addr", addr, "port", port, "duration_ms", time.Since(start).Milliseconds(), "err", err)
		return false, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Debug("manual probe lmstudio non-OK",
			"addr", addr, "port", port, "status", resp.StatusCode,
			"duration_ms", time.Since(start).Milliseconds())
		return false, nil
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		// Reachable, but the model list didn't parse — still report it up.
		slog.Debug("manual probe lmstudio up (models parse failed)",
			"addr", addr, "port", port, "err", err)
		return true, nil
	}
	models := make([]string, 0, len(result.Data))
	for _, d := range result.Data {
		if d.ID != "" {
			models = append(models, d.ID)
		}
	}
	slog.Debug("manual probe lmstudio up",
		"addr", addr, "port", port, "models", len(models),
		"duration_ms", time.Since(start).Milliseconds())
	return true, models
}

func (m *Manager) probeOllama(addr string, port int) (bool, []string) {
	url := "http://" + net.JoinHostPort(addr, strconv.Itoa(port)) + "/"
	start := time.Now()
	resp, err := m.client.Get(url)
	if err != nil {
		slog.Debug("manual probe ollama failed",
			"addr", addr, "port", port, "duration_ms", time.Since(start).Milliseconds(), "err", err)
		return false, nil
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Debug("manual probe ollama non-OK",
			"addr", addr, "port", port, "status", resp.StatusCode,
			"duration_ms", time.Since(start).Milliseconds())
		return false, nil
	}

	models := m.fetchOllamaModels(addr, port)
	slog.Debug("manual probe ollama up",
		"addr", addr, "port", port, "models", len(models),
		"duration_ms", time.Since(start).Milliseconds())
	return true, models
}

func (m *Manager) fetchOllamaModels(addr string, port int) []string {
	url := "http://" + net.JoinHostPort(addr, strconv.Itoa(port)) + "/api/tags"
	resp, err := m.client.Get(url)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}

	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil
	}
	names := make([]string, 0, len(result.Models))
	for _, m := range result.Models {
		names = append(names, m.Name)
	}
	return names
}

// probeNodeInfo reads a bounded HTTPS hardware snapshot and binds its reported
// host and principal to the response certificate and current cluster pins.
// Success means the remote identity was authenticated, not that GPU telemetry
// is valid. Any transport, decoding, or identity failure returns false and an
// empty response so stale or unverified measurements cannot survive the probe.
func (m *Manager) probeNodeInfo(client *http.Client, addr string, port int, localRecordID string) (bool, NodeInfoResponse) {
	url := "https://" + net.JoinHostPort(addr, strconv.Itoa(port)) + "/v1/node-info"
	start := time.Now()
	// Capture authorization before writing the request, including on reused
	// connections and when the endpoint's peer UUID was initially unknown.
	var recordID string
	trace := &httptrace.ClientTrace{GotConn: func(connection httptrace.GotConnInfo) {
		if conn, ok := connection.Conn.(*tls.Conn); ok {
			state := conn.ConnectionState()
			if len(state.PeerCertificates) > 0 {
				recordID = m.mesh.PeerRecordID(clustertrust.UUIDFromCert(state.PeerCertificates[0]))
			}
		}
	}}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(context.Background(), trace), http.MethodGet, url, nil)
	if err != nil {
		return false, NodeInfoResponse{}
	}
	// Resolve manual hostnames again on every probe, just as engine reads do,
	// so a reassigned name cannot retain the previous host's hardware identity.
	request.Close = true
	resp, err := client.Do(request)
	if err != nil {
		slog.Debug("manual probe node-info failed",
			"addr", addr, "port", port,
			"duration_ms", time.Since(start).Milliseconds(), "err", err)
		return false, NodeInfoResponse{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Debug("manual probe node-info non-OK",
			"addr", addr, "port", port, "status", resp.StatusCode,
			"duration_ms", time.Since(start).Milliseconds())
		return false, NodeInfoResponse{}
	}

	var result NodeInfoResponse
	body, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(body) > 64<<10 {
		// Partial or oversized data cannot supply a complete accepted snapshot.
		return false, NodeInfoResponse{}
	}
	if err := json.Unmarshal(body, &result); err != nil {
		slog.Debug("manual probe node-info decode failed",
			"addr", addr, "port", port, "err", err,
			"duration_ms", time.Since(start).Milliseconds())
		return false, NodeInfoResponse{}
	}
	m.mesh.Refresh()
	if localRecordID == "" || localRecordID != m.mesh.PeerRecordID(m.mesh.NodeUUID()) {
		return false, NodeInfoResponse{}
	}
	if resp.TLS == nil || len(resp.TLS.PeerCertificates) == 0 {
		// Without the server's TLS certificate, the response has no identity
		// proof to check against our pins.
		return false, NodeInfoResponse{}
	}
	if result.HostUUID == "" || result.HostUUID == m.mesh.NodeUUID() {
		// The snapshot must name a remote host. Our own measurements come from
		// the locally owned worker, never from a manually entered HTTP endpoint.
		return false, NodeInfoResponse{}
	}
	if result.ClusterUUID == nil || *result.ClusterUUID != result.HostUUID {
		// The response's principal must name the same host as its hardware data.
		// A missing or conflicting principal cannot authorize that snapshot.
		return false, NodeInfoResponse{}
	}
	if !m.mesh.VerifyPeerCertificate(result.HostUUID, resp.TLS.PeerCertificates[0]) {
		// Recheck the actual server certificate after reading the body: its pin
		// may have been removed or replaced since the TLS handshake completed.
		return false, NodeInfoResponse{}
	}
	if recordID == "" || recordID != m.mesh.PeerRecordID(result.HostUUID) {
		return false, NodeInfoResponse{}
	}
	result.NodeInfo = result.NodeInfo.Aged(time.Since(start))
	result.discoveryRecordID = recordID
	result.receivedAt = time.Now()
	slog.Debug("manual probe node-info up",
		"addr", addr, "port", port, "gpus", len(result.GPUs),
		"has_cpu", result.CPU != nil, "has_memory", result.Memory != nil,
		"duration_ms", time.Since(start).Milliseconds())
	return true, result
}

func (m *Manager) addNode(entry ManualEntry) ManualNodeStatus {
	id := nodeID(entry)

	nodeInfoPort := 14318
	if entry.TLSPort > 0 {
		nodeInfoPort = entry.TLSPort
	}
	status := ManualNodeStatus{
		IdentityVerification: noderec.IdentityUnverified,
		ID:                   id,
		Name:                 entry.Name,
		Address:              entry.Address,
		OllamaPort:           11434,
		NodeInfoPort:         nodeInfoPort,
		TLSEnabled:           entry.TLSPort > 0,
		MTLSRequired:         entry.TLSPort > 0 && entry.MTLS,
	}

	tracked := &trackedNode{entry: entry, status: status}
	m.mu.Lock()
	m.nodes[id] = tracked
	m.mu.Unlock()

	go func() {
		m.probeNode(entry)
		m.mu.Lock()
		defer m.mu.Unlock()
		tn, exists := m.nodes[id]
		if !exists || tn != tracked {
			return
		}
		m.codec.Notify("node/discovered", tn.statusAt(time.Now()))
	}()

	return status
}

func (m *Manager) removeNode(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, exists := m.nodes[id]
	if exists {
		delete(m.nodes, id)
	}

	if exists {
		m.codec.Notify("node/removed", ManualNodeStatus{ID: id})
		// Defensive clear: if this node had a probe-failed entry it
		// would outlive the node itself otherwise — the UI would
		// show an error for an id the user no longer has in their
		// list. nvpair-errors treats the clear as a no-op when the id
		// is absent, so this is safe to fire unconditionally.
		if err := m.codec.Notify("errors:clear", errors.ClearParams{
			ID: probeFailedID(id),
		}); err != nil {
			log.Printf("failed to send errors:clear on remove for %s: %v", id, err)
		}
	}
	return exists
}

func (m *Manager) listNodes() []ManualNodeStatus {
	m.reconcileTrust()
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]ManualNodeStatus, 0, len(m.nodes))
	for _, tn := range m.nodes {
		result = append(result, tn.statusAt(time.Now()))
	}
	return result
}

func (m *Manager) readLoop(ctx context.Context) error {
	for {
		msg, err := m.codec.Read()
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
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

	if !msg.IsRequest() {
		if msg.IsNotification() {
			log.Printf("ignoring incoming notification: %s", msg.Method)
		}
		return
	}

	switch msg.Method {
	case "node/add":
		var entry ManualEntry
		if err := json.Unmarshal(msg.Params, &entry); err != nil {
			m.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"address\": \"...\"}")
			return
		}
		if entry.Address == "" {
			m.codec.RespondError(msg.ID, -32602, "address is required")
			return
		}
		status := m.addNode(entry)
		if err := m.codec.Respond(msg.ID, status); err != nil {
			log.Printf("failed to respond to node/add: %v", err)
		}
		log.Printf("manual node added: %s (%s)", status.ID, entry.Address)

	case "node/remove":
		var params struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			m.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"id\": \"...\"}")
			return
		}
		removed := m.removeNode(params.ID)
		if err := m.codec.Respond(msg.ID, map[string]bool{"removed": removed}); err != nil {
			log.Printf("failed to respond to node/remove: %v", err)
		}
		if removed {
			log.Printf("manual node removed: %s", params.ID)
		}

	case "nodes/list":
		nodes := m.listNodes()
		if err := m.codec.Respond(msg.ID, map[string][]ManualNodeStatus{"nodes": nodes}); err != nil {
			log.Printf("failed to respond to nodes/list: %v", err)
		}

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

func nodeID(entry ManualEntry) string {
	if entry.Name != "" {
		return entry.Name
	}
	return "manual:" + entry.Address
}

func sliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
