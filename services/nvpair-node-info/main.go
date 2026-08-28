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

// clusterIdentity is the cluster principal this node reports, kept current by
// the parent broker over stdin (noderec.MethodSetClusterIdentity). node-info
// cannot derive it under the broker: it is spawned without a cluster dir so its
// inventory stays readable by any LAN peer, which also denies it the trust store
// membership is read from. Guarded because the stdin reader and every HTTP
// handler touch it.
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

// detectGPUs lives in gpu_windows.go (DXGI), gpu_linux.go (nvidia-smi with a
// ghw fallback), gpu_darwin.go (IORegistry), and gpu_other.go (ghw fallback).
// detectCPU lives in cpu_detect.go; memory detection is split so macOS can use
// gopsutil where ghw has no implementation. The persistent stats collector that
// supplies dynamic CPU / VRAM / utilization / memory-used numbers lives
// in stats_windows.go / stats_linux.go / stats_darwin.go / stats_other.go.

// buildResponse merges the static identity pulled once at startup with
// the latest dynamic stats snapshot and returns the marshaled
// NodeInfoResponse body. Kept as a free function so unit tests can
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
func buildResponse(gpus []GPUInfo, cpuStatic *CPUInfo, memTotal uint64, snap statsSnapshot, hostUUID string, clusterUUID *string) []byte {
	return buildResponseAt(gpus, cpuStatic, memTotal, snap, hostUUID, clusterUUID, time.Now())
}

func buildResponseAt(gpus []GPUInfo, cpuStatic *CPUInfo, memTotal uint64, snap statsSnapshot, hostUUID string, clusterUUID *string, now time.Time) []byte {
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
	body, _ := json.Marshal(resp)
	return body
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

// nodeInfoHandler serves the marshaled node inventory from body(). While this
// node is a cluster member the endpoint is cluster-gated: the authenticated
// client must present a cert pinned for its UUID (or this node's own leaf, for a
// local self-read), or it gets a 403 — so a non-cluster peer can't read this
// host's GPU inventory in the clear, and neither can a plain-HTTP caller on the
// shared port. Refresh picks up a membership change or a peer paired after
// startup.
func nodeInfoHandler(mesh *clustertrust.Mesh, body func() []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		mesh.Refresh()
		if mesh.Clustered() {
			if _, ok := mesh.VerifyClientPin(r); !ok {
				http.Error(w, "forbidden: not a pinned cluster peer", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body())
	}
}

// Version is stamped at build time via -ldflags "-X main.Version=...".
// See versions.json at the repo root for the source of truth.
var Version = "dev"

// resolveHostUUID returns the stable per-host UUID node-info reports. An
// explicit --node-id (which the broker passes as its own already-resolved
// b.nodeID) wins verbatim, so node-info reports exactly the identity the rest of
// the fleet keys on — even when the broker runs on a custom --cluster-dir data
// root while node-info is spawned plaintext with no cluster-dir of its own.
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
	port := flag.Int("port", 14318, "HTTP port to listen on")
	tlsPort := flag.Int("tls-port", 14319, "HTTPS port to listen on (only used when --cert and --key are set)")
	certPath := flag.String("cert", "", "path to TLS server certificate (PEM); enables HTTPS when set together with --key")
	keyPath := flag.String("key", "", "path to TLS server private key (PEM); enables HTTPS when set together with --cert")
	clientCAPath := flag.String("client-ca", "", "path to PEM bundle of CAs trusted to sign client certificates; enables mTLS when set")
	acceptHTTP := flag.Bool("accept-http", false, "keep the plaintext HTTP listener alive even when TLS is enabled (default: HTTPS-only when --cert is set)")
	clusterDir := flag.String("cluster-dir", "", "cluster config dir (node.crt/node.key + trusted/); when set, /v1/node-info is served only over mTLS scoped to pinned cluster peers instead of plaintext")
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

	// With a --cluster-dir, /v1/node-info is served on one port that carries both
	// personalities (first-byte split): while this node is a cluster member only
	// pin-gated mTLS callers are admitted, closing the plaintext GPU-inventory leak
	// to non-cluster peers, and while it is not a member the plaintext inventory is
	// served as usual. The membership answer is live, so a node that joins or
	// leaves while this process runs changes personality in place. Without a
	// --cluster-dir the bring-your-own --cert path and the plaintext default are
	// unchanged.
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

	// httpEnabled: when no cert is configured the only listener the
	// service has *is* HTTP, so the flag is meaningless and we don't
	// pay attention to it. With TLS on, --accept-http opts into
	// keeping the legacy port alive alongside HTTPS for clients that
	// haven't migrated.
	httpEnabled := !tlsOpts.Enabled() || *acceptHTTP
	if clusterGated {
		// The cluster-gated listener below owns *port and serves both
		// personalities on it, so there is no separate plaintext listener to
		// start. A member admits only pinned peers (the local scanner reads us
		// over mTLS via self-trust); a non-member serves the plaintext inventory.
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

	// Where the reported cluster principal comes from is decided by the
	// deployment, not by a fallback chain. With --cluster-dir this process holds
	// the trust store and reads membership itself, so the answer is always known
	// (the handler refreshes the mesh before building the body). Under the broker
	// (no cluster dir, so the inventory stays plain for every LAN peer) it cannot
	// read membership at all and the broker pushes it instead — until that first
	// push the answer is genuinely unknown and the field is omitted. The two
	// sources are mutually exclusive by construction.
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

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/node-info", nodeInfoHandler(mesh, func() []byte {
		return buildResponse(gpus, cpu, memTotal, collector.Snapshot(), hostUUID, clusterPrincipal())
	}))

	// Listener layout (set up below depending on flags). Exactly one of the two
	// blocks that own *port runs:
	//   * With --cluster-dir: one cluster-gated listener on *port (default
	//     14318) carrying both personalities behind a first-byte split. Which
	//     callers it admits follows live cluster membership.
	//   * Otherwise: httpServer on *port when httpEnabled, and, with
	//     --cert/--key, tlsServer on *tlsPort (default 14319) — a separate port
	//     so clients can be migrated independently.
	// At least one listener is always running (the flag validation above
	// guarantees we don't end up with none: --accept-http only matters when TLS
	// is on, and the default with no TLS keeps HTTP on).
	var httpServer, tlsServer *http.Server
	// split is the first-byte dispatcher behind the cluster-gated listener; it is
	// closed on shutdown so both sub-listeners unwind.
	var split *splitlisten.Splitter

	// Every listener feeds the same observer: which local address a peer reached
	// is evidence regardless of which personality served the request.
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

	// The cluster-gated listener takes over the *primary* node-info port (*port)
	// rather than the separate *tlsPort. That deliberately avoids colliding with
	// nvpair-errors, which the broker also spawns on :14319 — node-info's default
	// --tls-port. The BYO --cert path keeps its own *tlsPort so a plaintext and a
	// TLS listener can coexist there.
	if clusterGated {
		base, err := net.Listen("tcp", fmt.Sprintf(":%d", *port))
		if err != nil {
			log.Fatalf("failed to listen on port %d: %v", *port, err)
		}
		split = splitlisten.New(base)
		tlsServer = &http.Server{
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			ConnState:         observer.connState,
			IdleTimeout:       clustertrust.PeerListenerIdleTimeout,
		}
		// The cluster leaf is resolved per handshake from the live mesh, so a
		// non-member presents none and the handshake is refused; the plain
		// sub-listener carries unclustered callers, whom the handler admits only
		// while this node is not a member.
		serve := func(l net.Listener) {
			if err := tlsServer.Serve(l); err != nil && err != http.ErrServerClosed && err != net.ErrClosed {
				log.Fatalf("node-info server error: %v", err)
			}
		}
		log.Printf("node-info listening on :%d (cluster-gated; clustered=%v)", *port, mesh.Clustered())
		go serve(tls.NewListener(split.TLS(), mesh.ServerTLSConfig()))
		go serve(split.Plain())
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

	// Discovery consolidation: node-info no longer advertises its own
	// _nvpair-node-info mDNS record. It's discovered through the node-scanner
	// daemon's single _nvpair-node record (the broker registers this node's ni=
	// port) and enriched over plain HTTP at that port. node-info is now a
	// headless inventory server — the identity/address (uuid=/ip=) it used to
	// stamp is carried by the daemon for the whole node.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Keep membership converging on its own when cluster-gated. Every other gate
	// here is refreshed by the request handler, but the TLS personality is chosen
	// during the handshake — which happens BEFORE any handler runs — so without an
	// independent watch a node that joined while this process ran could not present
	// a leaf until some plaintext caller happened to arrive and refresh membership
	// as a side effect. That would make the cluster surface unreachable exactly
	// when it is supposed to take over.
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

	// One writer for both directions of stdout traffic: request responses and the
	// observed-address reports below share its lock so their frames never
	// interleave.
	notifier := applog.NewNotifier(os.Stdout)
	go observer.reportLoop(ctx.Done(), notifier)

	go applog.StdinRPC(notifier, func(msg applog.StdinMessage) {
		handleClusterIdentity(msg, identity)
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
