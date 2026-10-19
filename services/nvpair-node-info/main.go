// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/clustertrust"
	"nvpair-shared/jsonrpc"
	"nvpair-shared/nodeid"
	"nvpair-shared/noderec"
	"nvpair-shared/splitlisten"
)

type GPUInfo struct {
	Name               string `json:"name"`
	VramBytes          uint64 `json:"vram_bytes,omitempty"`
	VramUsedBytes      uint64 `json:"vram_used_bytes,omitempty"`
	UtilizationPercent uint32 `json:"utilization_percent,omitempty"`

	// statsKey is the opaque per-adapter identifier used to join this
	// static GPUInfo against statsCollector.Snapshot() results. Its
	// form is platform-specific: on Windows it's the PDH instance-name
	// form of the adapter's LUID (e.g. "luid_0x00000000_0x000054f0_phys_0");
	// on Linux it's the NVIDIA GPU UUID reported by nvidia-smi; on macOS it's
	// the IORegistry entry ID. Empty on hosts with no dynamic GPU source.
	// Unexported + json:"-" so it never travels over the wire.
	statsKey string `json:"-"`

	// usesSystemMemoryUsage is set by Linux static detection when nvidia-smi
	// cannot report GPU memory usage. Response assembly then maps the
	// independently collected system-memory usage onto VramUsedBytes.
	usesSystemMemoryUsage bool `json:"-"`
}

// CPUInfo is the node-level CPU readout. Name and Cores are filled once
// at startup from ghw; UtilizationPercent is refreshed every collector tick
// from PDH on Windows, /proc/stat on Linux, or Mach through gopsutil on macOS.
// Every field is omitempty so a value we couldn't read drops from JSON rather
// than appearing as a misleading literal zero — the one exception is
// UtilizationPercent on a CPU that really is idle, which renders the same as
// "unknown". That ambiguity is benign because it's visually identical to the
// user either way.
type CPUInfo struct {
	Name               string `json:"name,omitempty"`
	Cores              uint32 `json:"cores,omitempty"`
	UtilizationPercent uint32 `json:"utilization_percent,omitempty"`
}

// MemoryInfo is the node-level physical-RAM readout. TotalBytes is reported by
// ghw at startup except on macOS, where gopsutil supplies both total and used
// memory through Mach. Windows used memory comes from GlobalMemoryStatusEx and
// Linux used memory comes from /proc/meminfo.
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
	// HostUUID is this node's stable per-host identity (the same value the
	// node-scanner advertises as uuid= and the cluster uses as nodeUuid). It lets
	// a consumer that reaches this node only over HTTP — notably a user-added
	// manual node, which never sees this host's mDNS record — key it by its
	// permanent identity instead of the address it was typed under.
	HostUUID string `json:"hostUuid,omitempty"`
	// ClusterUUID is the cluster principal this node currently holds, empty when
	// it belongs to no cluster, and absent when this node does not (yet) know. It
	// is the mDNS-independent way for a peer to learn this node's membership:
	// membership otherwise travels only as the cluster-uuid= TXT key on this
	// host's mDNS record, so a peer that stops receiving that record keeps its
	// last observed value forever — and a peer still carrying a departed node's
	// principal suppresses the invite that would bring it back.
	//
	// A pointer with omitempty, so all three states are distinct on the wire:
	// absent means unknown, present-and-empty means unclustered, present means
	// that principal. A consumer must not read absent as unclustered — that is
	// how a node too old to report the field answers, and also how this node
	// answers before its parent has told it anything. Claiming "no cluster" on no
	// evidence would have a peer clear a correct annotation and offer an invite
	// its target will reject.
	ClusterUUID *string `json:"clusterUuid,omitempty"`
}

type peerNodeInfoResponse struct {
	NodeInfoResponse
	Peer noderec.PeerMetadata `json:"peer"`
}

// clusterIdentity holds the principal supplied over private broker IPC for
// launches without a cluster directory. With --cluster-dir, responses use live
// mesh membership instead. Guarded because the stdin reader and HTTP handlers
// can access it concurrently.
//
// `told` distinguishes "the broker says we hold no principal" from "the broker
// has not said anything yet", which are the same empty string but must not be
// the same answer: the first is reportable, the second has to stay absent.
type clusterIdentity struct {
	mu   sync.RWMutex
	uuid string
	told bool
}

func (c *clusterIdentity) set(uuid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uuid = uuid
	c.told = true
}

// get returns the principal and whether one has been pushed at all.
func (c *clusterIdentity) get() (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.uuid, c.told
}

// handleClusterIdentity applies a MethodSetClusterIdentity notification. A
// malformed payload is dropped rather than latching a wrong membership: the
// broker re-pushes on every change, so the next one corrects us.
func handleClusterIdentity(msg applog.StdinMessage, identity *clusterIdentity) {
	if msg.Method != noderec.MethodSetClusterIdentity {
		return
	}
	var params noderec.ClusterIdentityParams
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		slog.Warn("ignoring malformed cluster identity push", "err", err)
		return
	}
	identity.set(params.ClusterUUID)
	slog.Info("cluster identity updated", "clustered", params.ClusterUUID != "")
}

// peerMetadata holds complete, immutable snapshots supplied by the broker from
// scanner's owned local registry. Public HTTP responses never populate it.
type peerMetadata struct {
	// Readers must see a complete snapshot rather than partially replaced fields.
	mu    sync.RWMutex
	value noderec.PeerMetadataParams
	known bool
}

// get returns the latest registry snapshot and whether the broker supplied one.
func (m *peerMetadata) get() (noderec.PeerMetadataParams, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.value, m.known
}

// handlePeerMetadata replaces registry metadata from the broker, rejecting
// malformed payloads or a different host identity. Id-bearing requests receive
// an acknowledgement or invalid-params error.
func handlePeerMetadata(msg applog.StdinMessage, hostUUID string, metadata *peerMetadata, out *applog.Notifier) {
	if msg.Method != noderec.MethodSetPeerMetadata {
		return
	}
	var params noderec.PeerMetadataParams
	err := json.Unmarshal(msg.Params, &params)
	valid := err == nil && hostUUID != "" && params.HostUUID == hostUUID
	if valid {
		metadata.mu.Lock()
		metadata.value, metadata.known = params, true
		metadata.mu.Unlock()
	}
	if len(msg.ID) == 0 {
		return
	}
	response := jsonrpc.Message{JSONRPC: "2.0", ID: &msg.ID}
	if valid {
		response.Result = json.RawMessage(`{"ok":true}`)
	} else {
		response.Error = &jsonrpc.RPCError{Code: -32602, Message: "invalid local peer metadata"}
	}
	if err := json.NewEncoder(out).Encode(response); err != nil {
		slog.Warn("failed to acknowledge peer metadata", "err", err)
	}
}

type infoUpdateParams struct {
	Info NodeInfoResponse `json:"info"`
}

// publishInfoUpdates sends inventory directly to the owning broker at startup
// and on each tick until cancellation, using the same builder as public HTTP.
func publishInfoUpdates(ctx context.Context, out *applog.Notifier, snapshot func() NodeInfoResponse, ticks <-chan time.Time) {
	for {
		if err := out.Notify(noderec.NotifyInfoUpdated, infoUpdateParams{Info: snapshot()}); err != nil {
			slog.Warn("failed to publish local inventory", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticks:
		}
	}
}

// detectGPUs lives in gpu_windows.go (DXGI), gpu_linux.go (nvidia-smi with a
// ghw fallback), gpu_darwin.go (IORegistry), and gpu_other.go (ghw fallback).
// detectCPU lives in cpu_detect.go; memory detection is split so macOS can use
// gopsutil where ghw has no implementation. The persistent stats collector that
// supplies dynamic CPU / VRAM / utilization / memory-used numbers lives
// in stats_windows.go / stats_linux.go / stats_darwin.go / stats_other.go.

// buildResponse merges the static identity pulled once at startup with
// the latest dynamic stats snapshot and returns a typed hardware response.
// HTTP and IPC writers encode it at their boundaries. Unit tests can
// exercise the merge without standing up a collector or HTTP server.
//
// GPUs whose statsKey doesn't appear in snap.GPU (host with no dynamic GPU
// source, counter absent, or a just-plugged-in adapter the collector hasn't
// seen yet) keep their zero-valued dynamic fields, which the omitempty tags
// drop from JSON. GPUs explicitly marked usesSystemMemoryUsage are the
// exception: their VRAM-used value is the independently sampled system-memory
// usage, so it remains available even when dynamic nvidia-smi collection is not.
//
// cpuStatic is nil when static CPU introspection failed; memTotal is zero when
// physical-memory introspection failed. Both conditions omit their respective
// top-level object from the JSON entirely.
func buildResponse(gpus []GPUInfo, cpuStatic *CPUInfo, memTotal uint64, snap statsSnapshot, hostUUID string, clusterUUID *string) NodeInfoResponse {
	return buildResponseAt(gpus, cpuStatic, memTotal, snap, hostUUID, clusterUUID, time.Now())
}

func buildResponseAt(gpus []GPUInfo, cpuStatic *CPUInfo, memTotal uint64, snap statsSnapshot, hostUUID string, clusterUUID *string, now time.Time) NodeInfoResponse {
	outGPUs := mergeGPUInventory(gpus, snap.GPUInventory)
	for i := range outGPUs {
		gpu := &outGPUs[i]
		if gpu.usesSystemMemoryUsage {
			gpu.VramUsedBytes = snap.MemUsedBytes
		}
		if s, ok := snap.GPU[gpu.statsKey]; ok {
			if !gpu.usesSystemMemoryUsage {
				gpu.VramUsedBytes = s.VRAMUsed
			}
			gpu.UtilizationPercent = s.UtilizationPct
		}
	}

	telemetryValid, msSince := telemetryStatus(snap.GPUSampledAt, now)
	resp := NodeInfoResponse{
		GPUs:           outGPUs,
		TelemetryValid: telemetryValid,
		MSSince:        msSince,
		HostUUID:       hostUUID,
		ClusterUUID:    clusterUUID,
	}
	if cpuStatic != nil {
		cpu := *cpuStatic
		cpu.UtilizationPercent = snap.CPUUtilPct
		resp.CPU = &cpu
	}
	if memTotal > 0 {
		resp.Memory = &MemoryInfo{
			TotalBytes: memTotal,
			UsedBytes:  snap.MemUsedBytes,
		}
	}
	return resp
}

func telemetryStatus(sampledAt, now time.Time) (bool, int64) {
	if sampledAt.IsZero() {
		return false, 0
	}
	age := now.Sub(sampledAt)
	if age < 0 {
		age = 0
	}
	return true, age.Milliseconds()
}

func mergeGPUInventory(static, recovered []GPUInfo) []GPUInfo {
	merged := make([]GPUInfo, len(static))
	copy(merged, static)
	byStatsKey := make(map[string]int, len(merged))
	for i := range merged {
		if merged[i].statsKey != "" {
			byStatsKey[merged[i].statsKey] = i
		}
	}
	for _, gpu := range recovered {
		if gpu.statsKey == "" {
			continue
		}
		if index, ok := byStatsKey[gpu.statsKey]; ok {
			if merged[index].Name == "" {
				merged[index].Name = gpu.Name
			}
			if merged[index].VramBytes == 0 {
				merged[index].VramBytes = gpu.VramBytes
			}
			continue
		}
		byStatsKey[gpu.statsKey] = len(merged)
		merged = append(merged, gpu)
	}
	return merged
}

// publicNodeInfoHandler preserves the display response, including while this
// node is clustered. Public readings carry no authoritative peer metadata.
func publicNodeInfoHandler(mesh *clustertrust.Mesh, snapshot func() NodeInfoResponse) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mesh.Refresh()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(snapshot()); err != nil {
			slog.Warn("failed to serve public node inventory", "err", err)
		}
	}
}

// nodeInfoHandler serves peer node inventory only to currently authenticated peers.
// Metadata and hardware must agree with this node's live certificate identity.
func nodeInfoHandler(mesh *clustertrust.Mesh, metadata *peerMetadata, snapshot func() NodeInfoResponse) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mesh.Refresh()
		if _, ok := mesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden: not a pinned cluster peer", http.StatusForbidden)
			return
		}
		info := snapshot()
		// Recheck after building the response: departure or pin replacement must
		// also revoke a request that was already in progress on an old connection.
		mesh.Refresh()
		if _, ok := mesh.VerifyClientPin(r); !ok {
			http.Error(w, "forbidden: not a pinned cluster peer", http.StatusForbidden)
			return
		}
		local, known := metadata.get()
		principal := mesh.NodeUUID()
		port := local.Peer.Services[noderec.ServiceNodeInfo]
		if !known || local.HostUUID != principal || local.ClusterUUID != principal ||
			info.HostUUID != principal || info.ClusterUUID == nil || *info.ClusterUUID != principal ||
			local.Peer.Name == "" || len(local.Peer.Addresses) == 0 || port <= 0 || port > 65535 {
			http.Error(w, "local peer metadata is not ready", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(peerNodeInfoResponse{NodeInfoResponse: info, Peer: local.Peer}); err != nil {
			slog.Warn("failed to serve peer node inventory", "err", err)
		}
	}
}

// Version is stamped at build time via -ldflags "-X main.Version=...".
// See versions.json at the repo root for the source of truth.
var Version = "dev"

// resolveHostUUID returns the stable per-host UUID node-info reports. An
// explicit --node-id (which the broker passes as its own already-resolved
// b.nodeID) wins verbatim, so node-info reports exactly the identity the rest of
// the fleet keys on, including deployments with a custom cluster directory.
// Standalone (no --node-id) it resolves the shared local identity
// itself, preferring the --cluster-dir parent when set.
func resolveHostUUID(nodeID, clusterDir string) string {
	if nodeID != "" {
		return nodeID
	}
	base := ""
	if clusterDir != "" {
		base = filepath.Dir(clusterDir)
	}
	return nodeid.Resolve(base)
}

func main() {
	port := flag.Int("port", 14318, "primary port for public HTTP and, with --cluster-dir, peer node inventory over pinned mTLS")
	tlsPort := flag.Int("tls-port", 14319, "operator HTTPS port (requires --cert and --key; ignored with --cluster-dir)")
	certPath := flag.String("cert", "", "path to operator TLS server certificate (PEM); requires --key; ignored with --cluster-dir")
	keyPath := flag.String("key", "", "path to operator TLS server private key (PEM); requires --cert; ignored with --cluster-dir")
	clientCAPath := flag.String("client-ca", "", "PEM CA bundle for operator HTTPS client authentication; requires --cert and --key; ignored with --cluster-dir")
	acceptHTTP := flag.Bool("accept-http", false, "keep public HTTP on --port alongside operator HTTPS; --cluster-dir always keeps public HTTP available")
	clusterDir := flag.String("cluster-dir", "", "cluster config dir (node.crt/node.key + trusted/); serves public hardware displays over HTTP and peer node inventory over pinned mTLS, both at /v1/node-info on the primary port")
	nodeID := flag.String("node-id", "", "stable per-host UUID to report on /v1/node-info; when empty it is resolved from the local identity store. The broker passes its own resolved node UUID so both agree even on a custom data root")
	showVersion := flag.Bool("version", false, "print version and exit")
	resolveLevel := applog.RegisterFlag(nil, slog.LevelInfo)
	flag.Parse()

	if *showVersion {
		fmt.Println(Version)
		os.Exit(0)
	}

	applog.Init("nvpair-node-info", resolveLevel())

	// This node's stable per-host UUID (same source as the scanner's uuid= and
	// the cluster principal) so /v1/node-info can report it. A manual node's
	// prober reads it here to key the node by its permanent identity rather than
	// the address it was added under.
	hostUUID := resolveHostUUID(*nodeID, *clusterDir)

	// The cluster listener keeps public HTTP displays available while serving
	// peer node inventory to pinned peers over mTLS on the same primary port.
	mesh := clustertrust.Open(*clusterDir)
	clusterGated := *clusterDir != ""

	tlsOpts := tlsOptions{
		CertPath:     *certPath,
		KeyPath:      *keyPath,
		ClientCAPath: *clientCAPath,
	}
	if err := tlsOpts.validate(); err != nil {
		log.Fatalf("invalid TLS configuration: %v", err)
	}

	// httpEnabled controls the separate public HTTP listener used without
	// --cluster-dir. It runs by default; operator certificates disable it unless
	// --accept-http is set. The cluster directory mode supplies its own public
	// HTTP listener through the split listener below.
	httpEnabled := !tlsOpts.Enabled() || *acceptHTTP
	if clusterGated {
		// The split listener below owns *port and serves both public HTTP and
		// peer node inventory over pinned mTLS. Skip the separate HTTP listener;
		// public HTTP remains available regardless of membership.
		if tlsOpts.Enabled() {
			log.Print("--cluster-dir set: ignoring --cert/--key (cluster mTLS takes precedence)")
		}
		httpEnabled = false
	}

	gpus := detectGPUs()
	log.Printf("detected %d GPU(s)", len(gpus))
	for i, g := range gpus {
		if g.VramBytes > 0 {
			log.Printf("  GPU %d: %s (%d MiB)", i, g.Name, g.VramBytes/(1024*1024))
		} else {
			log.Printf("  GPU %d: %s", i, g.Name)
		}
	}

	cpu := detectCPU()
	if cpu != nil {
		log.Printf("detected CPU: %s (%d cores)", cpu.Name, cpu.Cores)
	} else {
		log.Print("CPU detection returned no info")
	}

	memTotal := detectMemoryTotal()
	if memTotal > 0 {
		log.Printf("detected memory: %d MiB total", memTotal/(1024*1024))
	} else {
		log.Print("memory total detection returned no info")
	}

	collector := startStatsCollector()
	defer collector.Stop()

	// With --cluster-dir, clusterUuid comes from the mesh: this node's principal
	// while clustered, or an empty string otherwise. HTTP handlers refresh the
	// mesh per request, and the watcher below also updates it for TLS handshakes
	// and private inventory snapshots. Without a cluster directory, use the
	// identity sent over stdin and omit the field until the first update arrives.
	identity := &clusterIdentity{}
	clusterPrincipal := func() *string {
		if !clusterGated {
			uuid, told := identity.get()
			if !told {
				return nil
			}
			return &uuid
		}
		principal := ""
		if mesh.Clustered() {
			principal = mesh.NodeUUID()
		}
		return &principal
	}

	snapshot := func() NodeInfoResponse {
		return buildResponse(gpus, cpu, memTotal, collector.Snapshot(), hostUUID, clusterPrincipal())
	}
	metadata := &peerMetadata{}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/node-info", publicNodeInfoHandler(mesh, snapshot))

	// With --cluster-dir, one split listener on *port (default 14318) serves
	// public HTTP and peer node inventory over pinned mTLS. Otherwise, public
	// HTTP uses *port when httpEnabled, and operator HTTPS uses *tlsPort (default
	// 14319) when --cert/--key are configured. Operator HTTPS-only mode leaves
	// *port unbound. At least one listener is enabled in every valid mode.
	var httpServer, tlsServer *http.Server
	// split dispatches public HTTP and peer mTLS connections on the primary port.
	// Closing it during shutdown releases both sub-listeners.
	var split *splitlisten.Splitter

	// Observe local addresses reached by non-loopback HTTP clients on either
	// transport. These reports describe reachability rather than peer identity.
	observer := newAddressObserver()

	if httpEnabled {
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
		if err != nil {
			log.Fatalf("failed to listen on port %d: %v", *port, err)
		}
		httpServer = &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ConnState:         observer.connState,
		}
		go func() {
			log.Printf("HTTP server listening on :%d", *port)
			if err := httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
				log.Fatalf("HTTP server error: %v", err)
			}
		}()
	}

	// With --cluster-dir, public HTTP and peer node inventory over pinned mTLS
	// share the primary node-info port (*port). This avoids colliding with
	// nvpair-errors on :14319, node-info's default --tls-port.
	// Without --cluster-dir, operator-supplied --cert/--key enable HTTPS on
	// *tlsPort; --accept-http keeps public HTTP available on *port.
	if clusterGated {
		base, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
		if err != nil {
			log.Fatalf("failed to listen on port %d: %v", *port, err)
		}
		split = splitlisten.New(base)
		peerMux := http.NewServeMux()
		peerMux.HandleFunc("/v1/node-info", nodeInfoHandler(mesh, metadata, snapshot))
		httpServer = &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ConnState:         observer.connState,
			IdleTimeout:       clustertrust.PeerListenerIdleTimeout,
		}
		tlsServer = &http.Server{
			Handler:           peerMux,
			ReadHeaderTimeout: 5 * time.Second,
			ConnState:         observer.connState,
			IdleTimeout:       clustertrust.PeerListenerIdleTimeout,
		}
		// The cluster leaf is resolved per handshake from the live mesh, so a
		// non-member presents none and the handshake is refused. The independent
		// plain sub-listener continues serving frontend hardware displays.
		serve := func(server *http.Server, l net.Listener) {
			if err := server.Serve(l); err != nil && err != http.ErrServerClosed && err != net.ErrClosed {
				log.Fatalf("node-info server error: %v", err)
			}
		}
		log.Printf("node-info listening on :%d (public HTTP and pinned mTLS; clustered=%v)", *port, mesh.Clustered())
		go serve(tlsServer, tls.NewListener(split.TLS(), mesh.ServerTLSConfig()))
		go serve(httpServer, split.Plain())
	} else if tlsOpts.Enabled() {
		tlsCfg, err := buildTLSConfig(tlsOpts)
		if err != nil {
			log.Fatalf("failed to build TLS config: %v", err)
		}
		mode := "TLS"
		if tlsOpts.MutualAuth() {
			mode = "mTLS"
		}
		listener, err := net.Listen("tcp", fmt.Sprintf(":%d", *tlsPort))
		if err != nil {
			log.Fatalf("failed to listen on TLS port %d: %v", *tlsPort, err)
		}
		tlsServer = &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			TLSConfig:         tlsCfg,
			ConnState:         observer.connState,
		}
		go func() {
			log.Printf("HTTPS server listening on :%d (%s)", *tlsPort, mode)
			// Cert/key are already in TLSConfig.Certificates, so the empty
			// strings tell ServeTLS to use those rather than re-reading files.
			if err := tlsServer.ServeTLS(listener, "", ""); err != nil && err != http.ErrServerClosed {
				log.Fatalf("HTTPS server error: %v", err)
			}
		}()
	}

	// The broker registers this service's ni port with scanner, which advertises
	// it in this host's single _nvpair-node record. Public HTTP supports hardware
	// displays, pinned mTLS serves peer node inventory, and private stdout
	// notifications supply local service measurements.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Refresh membership and certificate identity even without HTTP requests.
	// TLS chooses the server certificate during the handshake, before a handler
	// can refresh the mesh. The watcher makes membership changes visible to new
	// handshakes and private snapshots without requiring a public HTTP read.
	if clusterGated {
		go mesh.Watch(ctx, nil)
	}

	log.Printf("serving /v1/node-info on port %d (discovered via the node-scanner daemon)", *port)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case sig := <-sigCh:
			log.Printf("received %s, shutting down", sig)
			cancel()
		case <-ctx.Done():
		}
	}()

	// Serialize RPC replies, inventory snapshots, and observed-address reports
	// through one stdout writer so JSON-RPC frames cannot interleave.
	notifier := applog.NewNotifier(os.Stdout)
	go observer.reportLoop(ctx.Done(), notifier)
	infoTicks := time.NewTicker(2 * time.Second)
	defer infoTicks.Stop()
	go publishInfoUpdates(ctx, notifier, snapshot, infoTicks.C)

	go applog.StdinRPC(notifier, func(msg applog.StdinMessage) {
		handleClusterIdentity(msg, identity)
		handlePeerMetadata(msg, hostUUID, metadata, notifier)
	}, func() {
		log.Print("stdin closed, shutting down")
		cancel()
	})

	<-ctx.Done()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shutdownCancel()
	if httpServer != nil {
		httpServer.Shutdown(shutdownCtx)
	}
	if tlsServer != nil {
		tlsServer.Shutdown(shutdownCtx)
	}
	if split != nil {
		_ = split.Close()
	}

	log.Print("shutdown complete")
}
