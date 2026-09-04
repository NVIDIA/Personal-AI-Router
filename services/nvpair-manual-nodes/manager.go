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
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/errors"
	"nvpair-shared/netpick"
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

// Default service ports for a manual node. A manual node is remote, so its ports
// are assumed rather than resolved (the engine manager governs only the local
// engine) — an entry's "ports" object overrides any of them for a host that runs
// something somewhere else.
const (
	defaultOllamaPort   = 11434
	defaultLMStudioPort = 1234
	defaultVLLMPort     = 8000
	defaultNodeInfoPort = 14318
	defaultClusterPort  = 14321
)

type GPUInfo struct {
	Name               string `json:"name"`
	VramBytes          uint64 `json:"vram_bytes,omitempty"`
	VramUsedBytes      uint64 `json:"vram_used_bytes,omitempty"`
	UtilizationPercent uint32 `json:"utilization_percent,omitempty"`
}

// CPUInfo and MemoryInfo mirror the top-level objects nvpair-node-info
// now emits. Pointer-optional so manually-added nodes that aren't
// actually running nvpair-node-info (Ollama-only routing targets) don't
// falsely surface as having an unknown CPU + zero memory — the whole
// object stays absent until we see one in a real response.
type CPUInfo struct {
	Name               string `json:"name,omitempty"`
	Cores              uint32 `json:"cores,omitempty"`
	UtilizationPercent uint32 `json:"utilization_percent,omitempty"`
}

type MemoryInfo struct {
	TotalBytes uint64 `json:"total_bytes,omitempty"`
	UsedBytes  uint64 `json:"used_bytes,omitempty"`
}

type NodeInfoResponse struct {
	GPUs           []GPUInfo   `json:"GPUs"`
	CPU            *CPUInfo    `json:"cpu,omitempty"`
	Memory         *MemoryInfo `json:"memory,omitempty"`
	TelemetryValid bool        `json:"telemetryValid"`
	MSSince        int64       `json:"msSince"`
	// HostUUID is the remote node's stable per-host identity, reported by
	// node-info. We surface it so a manual node is tracked by the same permanent
	// identity as the rest of the fleet — including deduping with the same
	// machine if it's also discovered over mDNS. Empty when the remote
	// predates this field or isn't a NVPAIR node-info server.
	HostUUID string `json:"hostUuid,omitempty"`
	// ClusterUUID is the remote's cluster principal. Tri-state on the wire and
	// therefore a pointer here: absent means the remote does not know its own
	// membership, present-and-empty means it belongs to no cluster, and a value is
	// that principal. Reading absent as unclustered would have this node clear a
	// correct annotation and dial a clustered peer's mTLS surfaces in plaintext.
	ClusterUUID *string `json:"clusterUuid,omitempty"`
	// Services is the remote's {service key: port} set. Its presence is what
	// identifies the remote as a PAIR node: a bare Ollama or LM Studio host serves
	// no /v1/node-info at all, and a PAIR node too old to report the set is
	// treated as a bare host, which is what it can be reached as.
	Services map[noderec.ServiceKey]int `json:"services,omitempty"`
}

// peerModels is the inventory read from a paired PAIR node's engine-manager. It
// is the same body the node-scanner fetches for a discovered peer, so a manual
// peer's models reach the fleet through the same shape.
type peerModels struct {
	Models         []string            `json:"models"`
	ModelsByEngine map[string][]string `json:"modelsByEngine"`
	LoadedByEngine map[string][]string `json:"loadedByEngine"`
}

// ManualEntry is the user-supplied identity of a manually added
// node. Address + Name are the historical fields. TLSPort is an
// optional hint that node-info on this host listens for HTTPS on
// the given port (typically 14319 with our default node-info
// flags); a zero TLSPort means "probe plain HTTP on 14318" — the
// historical behavior. MTLS is informational only on the entry
// (we don't probe differently), but it's persisted so the UI can
// show the operator the configuration they set.
type ManualEntry struct {
	Address string `json:"address"`
	Name    string `json:"name"`
	TLSPort int    `json:"tls_port,omitempty"`
	MTLS    bool   `json:"mtls,omitempty"`
	// Ports overrides the assumed port of any single service on this host. It
	// exists because the defaults are assumptions about a machine this process
	// cannot introspect: a peer may run its engine on a second port, a whole PAIR
	// node may be reachable on a forwarded range, and two nodes may share one
	// loopback in a test. A zero or absent field keeps that service's default, so
	// an entry overrides only what it means to.
	Ports *ManualPorts `json:"ports,omitempty"`
}

// ManualPorts is the per-service port override set for one manual entry. Field
// names follow the service names manual-nodes already uses rather than the
// compact mDNS TXT keys, because this is what an operator types.
//
// VLLM is carried and persisted but not probed here yet; it is in the set so an
// entry written today keeps its meaning when the vLLM leg lands.
type ManualPorts struct {
	NodeInfo int `json:"node_info,omitempty"`
	Cluster  int `json:"cluster,omitempty"`
	Ollama   int `json:"ollama,omitempty"`
	LMStudio int `json:"lmstudio,omitempty"`
	VLLM     int `json:"vllm,omitempty"`
}

// resolved returns this entry's ports with every unset field filled from the
// default for that service, so probe code reads one value per service and never
// repeats the defaulting rule.
func (e ManualEntry) resolved() ManualPorts {
	p := ManualPorts{}
	if e.Ports != nil {
		p = *e.Ports
	}
	p.NodeInfo = portOr(p.NodeInfo, defaultNodeInfoPort)
	p.Cluster = portOr(p.Cluster, defaultClusterPort)
	p.Ollama = portOr(p.Ollama, defaultOllamaPort)
	p.LMStudio = portOr(p.LMStudio, defaultLMStudioPort)
	p.VLLM = portOr(p.VLLM, defaultVLLMPort)
	return p
}

func portOr(override, fallback int) int {
	if override > 0 {
		return override
	}
	return fallback
}

// validateManualAddress rejects what cannot work as a manual address, with the
// reason rather than a silent permanently-down entry.
//
// A "host:port" string is the common mistake: every probe appends its own
// service port to this value, so such an entry dials host:port:port and reads
// down forever. Per-service ports belong in "ports". Bare IPv6 literals are
// accepted in both plain and bracketed form; net.JoinHostPort re-brackets on the
// way out.
func validateManualAddress(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return "", fmt.Errorf("address is required")
	}
	if unbracketed, ok := strings.CutPrefix(addr, "["); ok {
		if inner, closed := strings.CutSuffix(unbracketed, "]"); closed {
			addr = inner
		}
	}
	if net.ParseIP(addr) != nil {
		return addr, nil
	}
	if host, port, err := net.SplitHostPort(addr); err == nil && host != "" && port != "" {
		return "", fmt.Errorf("address %q carries a port; give the host on its own (%q) and put per-service ports in \"ports\" — a service port is appended to this value, so a host:port entry can never be reached", addr, host)
	}
	if !netpick.Hostname(addr) {
		return "", fmt.Errorf("address %q is neither an IP address nor a host name", addr)
	}
	return addr, nil
}

// ManualNodeStatus mirrors a manual entry plus the latest probe
// outcome. TLSEnabled is the entry-level configuration echoed to
// the UI so the node card can render a lock icon without having to
// cross-reference the entries list. NodeInfoPort reflects whichever
// port we actually reached (TLS port if TLS is configured, plain
// 14318 otherwise) so existing UI that displays the port stays
// correct.
type ManualNodeStatus struct {
	ID           string   `json:"id"`
	Name         string   `json:"name,omitempty"`
	Address      string   `json:"address"`
	OllamaUp     bool     `json:"ollama_up"`
	OllamaPort   int      `json:"ollama_port"`
	OllamaModels []string `json:"ollama_models,omitempty"`
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
	// Ports echoes the entry's port overrides so a caller can render what it
	// configured without holding its own copy.
	Ports *ManualPorts `json:"ports,omitempty"`
	// PairNode is true when node-info answered AND reported a service map — the
	// remote is a PAIR node, not a bare inference host. It is the switch between
	// the two kinds of manual node: a PAIR node is folded into the directory as a
	// peer and reached through its proxies over cluster mTLS, and a bare host is
	// bridged into the local proxies by its raw engine ports. The engine fields
	// below stay zero for a PAIR node, because its 11434 / 1234 are proxy facades
	// that refuse plaintext from anything but loopback.
	PairNode bool `json:"pair_node"`
	// ClusterUUID is the remote's cluster principal, carried through with its
	// three states intact (see NodeInfoResponse.ClusterUUID). A consumer keys the
	// peer's certificate pin on it.
	ClusterUUID *string `json:"cluster_uuid,omitempty"`
	// Services is the remote's {service key: port} set, verbatim from node-info.
	Services map[noderec.ServiceKey]int `json:"services,omitempty"`
	// Models / ModelsByEngine / LoadedByEngine are a paired PAIR node's inventory,
	// read from its engine manager over cluster mTLS. Empty until this node holds
	// a pin for the peer: model names are not served to a stranger.
	Models         []string            `json:"models,omitempty"`
	ModelsByEngine map[string][]string `json:"models_by_engine,omitempty"`
	LoadedByEngine map[string][]string `json:"loaded_by_engine,omitempty"`
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
}

type Manager struct {
	codec  *Codec
	cancel context.CancelFunc

	// client is the plain-HTTP probe client (Ollama on :11434 and
	// nvpair-node-info's HTTP listener). tlsClient is used only when
	// the entry sets TLSPort > 0; if the operator hasn't given us
	// any TLS material via flags, tlsClient still works because
	// the default *http.Client respects the system trust store
	// (which covers public CAs and any roots an enterprise has
	// installed). client_test.go reaches in and replaces the
	// transports on both for unit tests, so they're plain
	// *http.Client values rather than something more elaborate.
	client    *http.Client
	tlsClient *http.Client

	// mesh is non-nil when this node is clustered (--cluster-dir): a TLS manual
	// node is probed over cluster mTLS (presenting our leaf, accepting any
	// currently-pinned server cert) since a clustered peer's node-info is
	// pin-gated and serves no plaintext listener.
	mesh *clustertrust.Mesh

	mu    sync.RWMutex
	nodes map[string]*trackedNode
}

func NewManager(codec *Codec, tlsOpts tlsClientOptions, mesh *clustertrust.Mesh) (*Manager, error) {
	tlsClient, err := buildTLSClient(tlsOpts, probeTimeout)
	if err != nil {
		return nil, fmt.Errorf("build TLS client: %w", err)
	}
	if tlsClient == nil {
		tlsClient = &http.Client{Timeout: probeTimeout, Transport: noKeepAliveTransport()}
	}
	return &Manager{
		codec: codec,
		mesh:  mesh,
		client: &http.Client{
			Timeout:   probeTimeout,
			Transport: noKeepAliveTransport(),
		},
		tlsClient: tlsClient,
		nodes:     make(map[string]*trackedNode),
	}, nil
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

	if err := m.codec.Notify("ready", ReadyParams{Version: Version}); err != nil {
		return fmt.Errorf("failed to send ready notification: %w", err)
	}

	go m.probeLoop(ctx)

	return m.readLoop(ctx)
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

// lastProbe returns a node's previous status and consecutive-failure count, and
// whether it is still tracked at all (it can be removed mid-probe).
func (m *Manager) lastProbe(id string) (ManualNodeStatus, int, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	tn, ok := m.nodes[id]
	if !ok {
		return ManualNodeStatus{}, 0, false
	}
	return tn.status, tn.consecutiveFails, true
}

func (m *Manager) probeNode(entry ManualEntry) {
	addr := entry.Address
	id := nodeID(entry)
	ports := entry.resolved()

	// node-info is asked FIRST, because its answer decides what kind of node this
	// is and therefore what else may be probed at all.
	//
	// A PAIR node's 11434 and 1234 are its proxy facades, not its engines: the
	// engines bind loopback and the facades refuse plaintext from anything but
	// loopback. Probing them would 403 every cycle and report a healthy peer as
	// having no engines, so a node that identifies itself as PAIR is never probed
	// there. Its engines are read from its engine manager instead, and it is
	// routed to through those same facades over cluster mTLS.
	nodeInfoUp, info := m.probeNodeInfo(entry, ports)

	// What this node was on the previous cycle, read before anything else runs:
	// the sticky decision below has to be made before the engine legs, not after.
	last, lastFails, tracked := m.lastProbe(id)
	if !tracked {
		return
	}

	// A single missed node-info answer must not turn a peer back into a stranger.
	// Three seconds is a routine gap across an overlay network — a relayed path, a
	// wake from sleep, a peer under load — and without hysteresis that gap would
	// probe the peer's proxy facades in plaintext, blank its service map and
	// withdraw it from every consumer, only to restore it 10 seconds later.
	// Discovery tolerates three consecutive misses before evicting an mDNS node;
	// this is the same tolerance, bounded by the same counter, so a node that
	// genuinely stops being a PAIR node still reverts within one failure episode.
	pairNode := nodeInfoUp && len(info.Services) > 0
	sticky := !pairNode && last.PairNode && lastFails < probeFailThreshold
	if sticky {
		pairNode = true
	}

	newStatus := ManualNodeStatus{
		ID:             id,
		Name:           entry.Name,
		Address:        addr,
		OllamaPort:     ports.Ollama,
		LMStudioPort:   ports.LMStudio,
		NodeInfoUp:     nodeInfoUp,
		NodeInfoPort:   m.nodeInfoProbePort(entry, ports),
		TLSEnabled:     entry.TLSPort > 0,
		MTLSRequired:   entry.TLSPort > 0 && entry.MTLS,
		Ports:          entry.Ports,
		PairNode:       pairNode,
		ClusterUUID:    info.ClusterUUID,
		Services:       info.Services,
		GPUs:           info.GPUs,
		CPU:            info.CPU,
		Memory:         info.Memory,
		TelemetryValid: info.TelemetryValid,
		MSSince:        info.MSSince,
		HostUUID:       info.HostUUID,
	}

	if sticky {
		// Carry the peer's identity forward across the gap, exactly as HostUUID is
		// carried below: what a consumer holds must not oscillate because one
		// probe timed out. NodeInfoUp stays false, so the node still reads as
		// unreachable and the failure counter still runs.
		newStatus.ClusterUUID = last.ClusterUUID
		newStatus.Services = last.Services
		newStatus.Models = last.Models
		newStatus.ModelsByEngine = last.ModelsByEngine
		newStatus.LoadedByEngine = last.LoadedByEngine
		newStatus.GPUs = last.GPUs
		newStatus.CPU = last.CPU
		newStatus.Memory = last.Memory
	} else if pairNode {
		models := m.fetchPeerModels(addr, info)
		newStatus.Models = models.Models
		newStatus.ModelsByEngine = models.ModelsByEngine
		newStatus.LoadedByEngine = models.LoadedByEngine
	} else {
		// A bare inference host: the engines themselves are on these ports, in
		// plaintext, which is what manual nodes were originally for.
		for _, leg := range m.engineLegs(ports) {
			up, models := leg.probe(addr, leg.port)
			leg.apply(&newStatus, up, models)
		}
	}

	reachable := newStatus.OllamaUp || newStatus.LMStudioUp || newStatus.NodeInfoUp

	m.mu.Lock()
	tn, exists := m.nodes[id]
	if !exists {
		m.mu.Unlock()
		return
	}

	prev := tn.status
	prevFails := tn.consecutiveFails
	// A failed node-info probe returns no HostUUID; preserve the last-learned one
	// so a transient node-info blip (while Ollama stays reachable) doesn't rekey
	// a live node UUID -> manual-id -> UUID — the broker keys the discovery store
	// by HostUUID.
	if !nodeInfoUp && newStatus.HostUUID == "" {
		newStatus.HostUUID = prev.HostUUID
	}
	tn.status = newStatus
	if reachable {
		tn.consecutiveFails = 0
	} else {
		tn.consecutiveFails++
	}
	curFails := tn.consecutiveFails
	m.mu.Unlock()

	changed := prev.OllamaUp != newStatus.OllamaUp ||
		prev.LMStudioUp != newStatus.LMStudioUp ||
		prev.NodeInfoUp != newStatus.NodeInfoUp ||
		prev.HostUUID != newStatus.HostUUID ||
		prev.PairNode != newStatus.PairNode ||
		!clusterUUIDEqual(prev.ClusterUUID, newStatus.ClusterUUID) ||
		!servicesEqual(prev.Services, newStatus.Services) ||
		!sliceEqual(prev.Models, newStatus.Models) ||
		!byEngineEqual(prev.ModelsByEngine, newStatus.ModelsByEngine) ||
		!byEngineEqual(prev.LoadedByEngine, newStatus.LoadedByEngine) ||
		!sliceEqual(prev.OllamaModels, newStatus.OllamaModels) ||
		!sliceEqual(prev.LMStudioModels, newStatus.LMStudioModels) ||
		!gpusEqual(prev.GPUs, newStatus.GPUs) ||
		!cpuEqual(prev.CPU, newStatus.CPU) ||
		!memoryEqual(prev.Memory, newStatus.Memory) ||
		prev.TelemetryValid != newStatus.TelemetryValid ||
		prev.MSSince != newStatus.MSSince

	if changed {
		slog.Info("manual node state changed",
			"node_id", id, "addr", addr,
			"ollama_up", newStatus.OllamaUp, "node_info_up", newStatus.NodeInfoUp,
			"models", len(newStatus.OllamaModels), "gpus", len(newStatus.GPUs))
		m.codec.Notify("node/updated", newStatus)
	} else {
		slog.Debug("manual node probe stable",
			"node_id", id, "addr", addr,
			"ollama_up", newStatus.OllamaUp, "node_info_up", newStatus.NodeInfoUp)
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

// engineLeg is one plain-HTTP inference-engine probe against a bare host. The
// legs are a table rather than a sequence of calls so adding an engine is one
// entry: its port, how it is probed, and where its result lands on the status.
type engineLeg struct {
	name  string
	port  int
	probe func(addr string, port int) (bool, []string)
	apply func(s *ManualNodeStatus, up bool, models []string)
}

func (m *Manager) engineLegs(ports ManualPorts) []engineLeg {
	return []engineLeg{
		{
			name:  "ollama",
			port:  ports.Ollama,
			probe: m.probeOllama,
			apply: func(s *ManualNodeStatus, up bool, models []string) {
				s.OllamaUp, s.OllamaModels = up, models
			},
		},
		{
			name:  "lmstudio",
			port:  ports.LMStudio,
			probe: m.probeLMStudio,
			apply: func(s *ManualNodeStatus, up bool, models []string) {
				s.LMStudioUp, s.LMStudioModels = up, models
			},
		},
	}
}

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

// nodeInfoProbePort is the port node-info is actually reached on: the entry's
// tls_port when one is set (it names an HTTPS listener, so it also names the
// port), otherwise the resolved plain node-info port.
func (m *Manager) nodeInfoProbePort(entry ManualEntry, ports ManualPorts) int {
	if entry.TLSPort > 0 {
		return entry.TLSPort
	}
	return ports.NodeInfo
}

// nodeInfoClient picks the scheme, port and transport for the node-info probe.
// The operator decides which scheme a manual node uses; we do not probe both.
// tls_port > 0 means HTTPS on that port via the TLS client (carrying the
// operator's client cert, if configured), or over cluster mTLS while this node is
// a cluster member — a clustered peer running node-info standalone serves its
// inventory only to pinned peers. Otherwise it is plain HTTP on the resolved
// node-info port, which is what a peer under the broker serves: node-info is the
// one inter-node surface deliberately left readable by any peer.
func (m *Manager) nodeInfoClient(entry ManualEntry, ports ManualPorts) (*http.Client, string, int) {
	if entry.TLSPort == 0 {
		return m.client, "http", ports.NodeInfo
	}
	// Refresh first so a cluster joined, or a peer paired, after startup is seen;
	// falls back to the BYO tlsClient while unclustered.
	m.mesh.Refresh()
	if cfg, ok := m.mesh.ClientTLSConfigAny(); ok {
		return &http.Client{Timeout: probeTimeout, Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}}, "https", entry.TLSPort
	}
	return m.tlsClient, "https", entry.TLSPort
}

// fetchPeerModels reads a paired PAIR node's model inventory from its engine
// manager, the same body the node-scanner fetches for a discovered peer.
//
// It is pinned mTLS or nothing: engine-manager's LAN surface serves plaintext
// only over loopback, and a peer we hold no pin for is a stranger. An empty
// result is therefore the correct answer before pairing, not a failure — the node
// still appears with its hardware, and its models arrive once it is paired.
func (m *Manager) fetchPeerModels(addr string, info NodeInfoResponse) peerModels {
	port, ok := info.Services[noderec.ServiceEngineManager]
	if !ok || port <= 0 {
		return peerModels{}
	}
	if info.ClusterUUID == nil || *info.ClusterUUID == "" {
		slog.Debug("manual peer model fetch skipped: peer reports no cluster principal", "addr", addr)
		return peerModels{}
	}
	m.mesh.Refresh()
	cfg, ok := m.mesh.ClientTLSConfig(*info.ClusterUUID)
	if !ok {
		slog.Debug("manual peer model fetch skipped: no pin held for peer", "addr", addr)
		return peerModels{}
	}
	client := &http.Client{Timeout: probeTimeout, Transport: &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}}
	url := "https://" + net.JoinHostPort(addr, strconv.Itoa(port)) + "/v1/models"
	resp, err := client.Get(url)
	if err != nil {
		slog.Debug("manual peer model fetch failed", "addr", addr, "port", port, "err", err)
		return peerModels{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Debug("manual peer model fetch non-OK", "addr", addr, "port", port, "status", resp.StatusCode)
		return peerModels{}
	}
	var models peerModels
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		slog.Debug("manual peer model fetch decode failed", "addr", addr, "port", port, "err", err)
		return peerModels{}
	}
	slog.Debug("manual peer models fetched", "addr", addr, "port", port, "models", len(models.Models))
	return models
}

func (m *Manager) probeNodeInfo(entry ManualEntry, ports ManualPorts) (bool, NodeInfoResponse) {
	client, scheme, port := m.nodeInfoClient(entry, ports)
	addr := entry.Address
	url := scheme + "://" + net.JoinHostPort(addr, strconv.Itoa(port)) + "/v1/node-info"
	start := time.Now()
	resp, err := client.Get(url)
	if err != nil {
		slog.Debug("manual probe node-info failed",
			"addr", addr, "port", port, "scheme", scheme,
			"duration_ms", time.Since(start).Milliseconds(), "err", err)
		return false, NodeInfoResponse{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		slog.Debug("manual probe node-info non-OK",
			"addr", addr, "port", port, "scheme", scheme, "status", resp.StatusCode,
			"duration_ms", time.Since(start).Milliseconds())
		return false, NodeInfoResponse{}
	}

	var result NodeInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		slog.Debug("manual probe node-info decode failed",
			"addr", addr, "port", port, "scheme", scheme, "err", err,
			"duration_ms", time.Since(start).Milliseconds())
		return false, NodeInfoResponse{}
	}
	slog.Debug("manual probe node-info up",
		"addr", addr, "port", port, "scheme", scheme, "gpus", len(result.GPUs),
		"has_cpu", result.CPU != nil, "has_memory", result.Memory != nil,
		"duration_ms", time.Since(start).Milliseconds())
	return true, result
}

func (m *Manager) addNode(entry ManualEntry) ManualNodeStatus {
	id := nodeID(entry)

	ports := entry.resolved()
	status := ManualNodeStatus{
		ID:           id,
		Name:         entry.Name,
		Address:      entry.Address,
		OllamaPort:   ports.Ollama,
		LMStudioPort: ports.LMStudio,
		NodeInfoPort: m.nodeInfoProbePort(entry, ports),
		TLSEnabled:   entry.TLSPort > 0,
		MTLSRequired: entry.TLSPort > 0 && entry.MTLS,
		Ports:        entry.Ports,
	}

	m.mu.Lock()
	m.nodes[id] = &trackedNode{entry: entry, status: status}
	m.mu.Unlock()

	go func() {
		m.probeNode(entry)
		m.mu.RLock()
		tn, exists := m.nodes[id]
		if !exists {
			m.mu.RUnlock()
			return
		}
		status = tn.status
		m.mu.RUnlock()
		m.codec.Notify("node/discovered", status)
	}()

	return status
}

func (m *Manager) removeNode(id string) bool {
	m.mu.Lock()
	_, exists := m.nodes[id]
	if exists {
		delete(m.nodes, id)
	}
	m.mu.Unlock()

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
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]ManualNodeStatus, 0, len(m.nodes))
	for _, tn := range m.nodes {
		result = append(result, tn.status)
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
		address, err := validateManualAddress(entry.Address)
		if err != nil {
			m.codec.RespondError(msg.ID, -32602, err.Error())
			return
		}
		entry.Address = address
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

// clusterUUIDEqual compares the tri-state principal by value, so a peer that
// went from "unknown" to "unclustered" reports as changed rather than as the same
// empty string.
func clusterUUIDEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func servicesEqual(a, b map[noderec.ServiceKey]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func byEngineEqual(a, b map[string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		other, ok := b[k]
		if !ok || !sliceEqual(v, other) {
			return false
		}
	}
	return true
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

func gpusEqual(a, b []GPUInfo) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name ||
			a[i].VramBytes != b[i].VramBytes ||
			a[i].VramUsedBytes != b[i].VramUsedBytes ||
			a[i].UtilizationPercent != b[i].UtilizationPercent {
			return false
		}
	}
	return true
}

// cpuEqual and memoryEqual are nil-aware equality helpers for the new
// pointer-optional fields. Both sides being nil is equal (the node
// doesn't report that subsystem); exactly one being nil is a real
// change (the node started or stopped reporting it, e.g. an OS-level
// upgrade of nvpair-node-info); both non-nil compares the underlying
// struct by value. This mirrors what sliceEqual / gpusEqual do for
// their respective fields — any change, including a utilization tick
// moving from 41% to 42%, fires a node/updated event so downstream
// consumers can refresh their view.
func cpuEqual(a, b *CPUInfo) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func memoryEqual(a, b *MemoryInfo) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
