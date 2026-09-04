// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net"

	"nvpair-shared/errors"
)

// proxyport.go owns the broker's proxy/engine port ordering. With managed
// ownership enabled it reserves :11434, then moves a stopped backend when its
// configured port is the facade or is occupied. A running or unknown owner is never killed.
// With managed ownership disabled the legacy engine-wins bump remains.

// proxyPortBumpedID is the sticky warning id surfaced when the broker moves
// the proxy off a port a running engine holds. Sticky (no timestamp suffix)
// so repeated bumps upsert one entry; cleared when the user later picks a
// proxy port that doesn't collide.
const (
	proxyPortBumpedID         = "ollama-proxy:port-bumped"
	portOwnershipBlockedID    = "ollama-proxy:port-ownership-blocked"
	managedOllamaFacadePort   = 11434
	managedOllamaBackendStart = 11435
)

type ollamaPortStatus struct {
	Running bool `json:"running"`
	Port    int  `json:"port"`
}

type managedPortPlan struct {
	Enabled     bool
	BackendPort int
	Blocked     string
}

// planManagedOllamaPorts is the policy core kept separate from RPC so every
// safety branch is deterministic in tests. It never plans a move for a
// running engine; a stopped backend whose configured port is occupied advances
// to the next available managed backend port.
func planManagedOllamaPorts(enabled bool, st ollamaPortStatus, available func(int) bool) managedPortPlan {
	if !enabled {
		return managedPortPlan{}
	}
	if st.Running && (st.Port == managedOllamaFacadePort || st.Port == 0) {
		return managedPortPlan{Blocked: "Ollama is already running on the compatibility port"}
	}
	if !available(managedOllamaFacadePort) {
		return managedPortPlan{Blocked: "the compatibility port is already in use"}
	}
	if st.Port > 0 && st.Port != managedOllamaFacadePort {
		if !st.Running && !available(st.Port) {
			backend := nextAvailablePort(managedOllamaBackendStart, available)
			if backend == 0 {
				return managedPortPlan{Blocked: "no free backend port is available"}
			}
			return managedPortPlan{Enabled: true, BackendPort: backend}
		}
		return managedPortPlan{Enabled: true}
	}
	backend := nextAvailablePort(managedOllamaBackendStart, available)
	if backend == 0 {
		return managedPortPlan{Blocked: "no free backend port is available"}
	}
	return managedPortPlan{Enabled: true, BackendPort: backend}
}

func nextAvailablePort(start int, available func(int) bool) int {
	for port := start; port <= 65535; port++ {
		if available(port) {
			return port
		}
	}
	return 0
}

func nextAvailablePortExcluding(start int, excludedPorts []int, available func(int) bool) int {
	excluded := map[int]bool{}
	for _, port := range excludedPorts {
		excluded[port] = true
	}
	return nextAvailablePort(start, func(port int) bool {
		return !excluded[port] && available(port)
	})
}

// tcpPortAvailable uses the exact wildcard bind shape ollama-proxy uses. The
// listener is only a preflight; the proxy's bind remains authoritative and a
// process winning the race is handled as a normal, non-destructive failure.
func tcpPortAvailable(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

func (b *Broker) reportPortOwnershipBlocked(reason string) {
	b.forwardErrorsReport(errors.ServiceError{
		ID:        portOwnershipBlockedID,
		Message:   fmt.Sprintf("NVPAIR could not safely reserve port %d: %s. No running or unknown process was stopped.", managedOllamaFacadePort, reason),
		Timestamp: nowMillis(),
		NodeID:    b.nodeID,
		Severity:  "warning",
		Action:    "none",
	})
}

// blockManagedOllamaFacade disables the managed attempt and chooses an
// explicit free fallback for the proxy. Explicit startup matters here: a
// persisted :11434 must not make a blocked proxy crash-loop behind an existing
// owner. It returns the fallback so a live proxy can rebind there too.
func (b *Broker) blockManagedOllamaFacade(reason string, excludedPorts ...int) int {
	b.managedOllamaFacade.Store(false)
	b.managedOllamaBackend.Store(0)
	if backend := int(b.ollamaBackendPort.Load()); backend > 0 {
		excludedPorts = append(excludedPorts, backend)
	}
	fallback := b.setOllamaProxyFallback(excludedPorts...)
	b.reportPortOwnershipBlocked(reason)
	b.markOllamaPortReady()
	return fallback
}

func (b *Broker) markOllamaPortReady() {
	if b.ollamaPortReady != nil {
		b.ollamaPortReadyOnce.Do(func() { close(b.ollamaPortReady) })
	}
}

func (b *Broker) setOllamaProxyFallback(excludedPorts ...int) int {
	// An explicit env override is authoritative: tests pin the port so spawn
	// never fights a cohabiting process, and the fallback probe must not
	// stomp it.
	if p := b.servicePorts.OllamaProxy; p != 0 {
		b.ollamaProxyStartupPort.Store(int32(p))
		return p
	}
	if aliasPort := b.currentOllamaHostAlias().Port; aliasPort > 0 {
		excludedPorts = append(excludedPorts, aliasPort)
	}
	fallback := nextAvailablePortExcluding(managedOllamaBackendStart, excludedPorts, tcpPortAvailable)
	b.ollamaProxyStartupPort.Store(int32(fallback))
	return fallback
}

// prepareManagedOllamaFacade runs before the proxy is spawned. It only builds
// a plan; the stopped backend is not persisted onto its new port until a proxy
// ready event proves :11434 has actually been reserved. Adopted/external
// engines are never moved because this slice has no process-takeover authority.
func (b *Broker) prepareManagedOllamaFacade() {
	b.prepareManagedOllamaFacadeWithPortCheck(tcpPortAvailable)
}

func (b *Broker) prepareManagedOllamaFacadeWithPortCheck(portAvailable func(int) bool) {
	b.setOllamaHostAlias(ollamaHostAlias{})
	// Resolve the worker inside the deferred method: engine-manager can respawn
	// while the blocking settings/status calls below are preparing the alias.
	defer b.syncCurrentEngineOllamaHostAliasReservation()

	settings := b.getSettings()
	if settings == nil {
		b.cacheOllamaPortStatus()
		b.blockManagedOllamaFacade("managed-port policy is unavailable")
		return
	}
	result, rpcErr, err := settings.Call(context.Background(), "settings/get-force-ports", nil)
	if err != nil || rpcErr != nil {
		slog.Warn("failed to read managed-port setting", "err", err, "rpcErr", rpcErr)
		b.cacheOllamaPortStatus()
		b.blockManagedOllamaFacade("managed-port policy could not be verified")
		return
	}
	var policy struct {
		Value bool `json:"value"`
	}
	if err := json.Unmarshal(result, &policy); err != nil {
		b.cacheOllamaPortStatus()
		b.blockManagedOllamaFacade("managed-port policy could not be decoded")
		return
	}

	em := b.getEngineMgr()
	if em == nil {
		if !policy.Value {
			b.setOllamaProxyFallback()
			b.forwardErrorsClear(portOwnershipBlockedID)
			b.markOllamaPortReady()
			return
		}
		b.blockManagedOllamaFacade("engine manager is unavailable")
		return
	}
	params, _ := json.Marshal(map[string]string{"engine": "ollama"})
	result, rpcErr, err = em.Call(context.Background(), "engine:status", params)
	if err != nil || rpcErr != nil {
		if !policy.Value {
			b.setOllamaProxyFallback()
			b.forwardErrorsClear(portOwnershipBlockedID)
			b.markOllamaPortReady()
			return
		}
		b.blockManagedOllamaFacade("Ollama status could not be verified")
		return
	}
	var st ollamaPortStatus
	if err := json.Unmarshal(result, &st); err != nil {
		if !policy.Value {
			b.setOllamaProxyFallback()
			b.forwardErrorsClear(portOwnershipBlockedID)
			b.markOllamaPortReady()
			return
		}
		b.blockManagedOllamaFacade("Ollama status could not be decoded")
		return
	}
	b.ollamaBackendPort.Store(int32(st.Port))
	b.prepareOllamaHostAlias(policy.Value, st.Port)
	if !policy.Value {
		// Opting out after a managed run commonly leaves stopped Ollama on
		// :11435. Start the proxy explicitly elsewhere so it cannot squat that
		// configured backend port before Ollama starts.
		b.managedOllamaFacade.Store(false)
		b.managedOllamaBackend.Store(0)
		b.setOllamaProxyFallback(st.Port)
		b.forwardErrorsClear(portOwnershipBlockedID)
		b.markOllamaPortReady()
		return
	}
	plan := planManagedOllamaPorts(true, st, b.availableOffOllamaHostAlias(portAvailable))
	if plan.Blocked != "" {
		b.blockManagedOllamaFacade(plan.Blocked, st.Port)
		return
	}
	b.managedOllamaFacade.Store(plan.Enabled)
	b.managedOllamaBackend.Store(int32(plan.BackendPort))
	b.ollamaProxyStartupPort.Store(managedOllamaFacadePort)
	if plan.BackendPort == 0 {
		slog.Info("preserving custom Ollama backend port", "port", st.Port)
		b.markOllamaPortReady()
	}
	b.forwardErrorsClear(portOwnershipBlockedID)
}

// cacheOllamaPortStatus best-effort remembers the configured backend port for
// fallback exclusion when the settings policy itself cannot be read.
func (b *Broker) cacheOllamaPortStatus() {
	em := b.getEngineMgr()
	if em == nil {
		return
	}
	params, _ := json.Marshal(map[string]string{"engine": "ollama"})
	result, rpcErr, err := em.Call(context.Background(), "engine:status", params)
	if err != nil || rpcErr != nil {
		return
	}
	var st ollamaPortStatus
	if json.Unmarshal(result, &st) == nil && st.Port > 0 {
		b.ollamaBackendPort.Store(int32(st.Port))
	}
}

func enginePortAssignmentRequest(method string, params json.RawMessage) (string, int, bool) {
	var request struct {
		Engine string `json:"engine"`
		Port   int    `json:"port"`
		Start  bool   `json:"start"`
	}
	if json.Unmarshal(params, &request) != nil || request.Engine == "" || request.Port <= 0 {
		return "", 0, false
	}
	switch method {
	case "engine:set-port", "engine:start":
		return request.Engine, request.Port, true
	case "engine:install":
		return request.Engine, request.Port, request.Start
	default:
		return "", 0, false
	}
}

func (b *Broker) rejectOllamaHostAliasPort(msg *Message, port int, owner string) bool {
	aliasPort := b.currentOllamaHostAlias().Port
	if aliasPort <= 0 || port != aliasPort {
		return false
	}
	if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf(
		"port %d is configured as the inherited OLLAMA_HOST proxy alias; change or unset OLLAMA_HOST and restart NVPAIR before assigning that port to %s",
		port, owner)); err != nil {
		log.Printf("failed to reject port assignment conflicting with OLLAMA_HOST alias: %v", err)
	}
	return true
}

func (b *Broker) handleLMStudioProxySetPort(msg *Message) {
	var params struct {
		Port int `json:"port"`
	}
	if json.Unmarshal(msg.Params, &params) == nil &&
		b.rejectOllamaHostAliasPort(msg, params.Port, "the LM Studio proxy") {
		return
	}
	b.relayToLMStudioProxy(msg)
}

// needsOllamaPortGate reports the client requests that can probe and adopt the
// configured Ollama port. While :11434 is changing from backend to facade,
// those probes must wait or they can mistake the proxy for a local engine.
func needsOllamaPortGate(method string, params json.RawMessage) bool {
	if method == "engine:get-installed" {
		return true
	}
	if method != "engine:status" && method != "engine:install" && method != "engine:start" && method != "engine:restart" {
		return false
	}
	var request struct {
		Engine string `json:"engine"`
	}
	return json.Unmarshal(params, &request) == nil && request.Engine == "ollama"
}

// handleProxySetPort intercepts proxy:set-port (rather than relaying it
// verbatim like the rest of the proxy: namespace) so it can resolve a port
// conflict against the running engines before handing the proxy the port to
// bind. It responds with the proxy's own set-port result (which echoes the
// actually-bound port).
func (b *Broker) handleProxySetPort(msg *Message) {
	p := b.getProxy()
	if p == nil {
		if err := b.codec.RespondError(msg.ID, -32000, "ollama-proxy not available"); err != nil {
			log.Printf("failed to respond to proxy:set-port: %v", err)
		}
		return
	}
	var params struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(msg.Params, &params); err != nil {
		if err := b.codec.RespondError(msg.ID, -32602, "invalid params: expected {\"port\": <int>}"); err != nil {
			log.Printf("failed to respond to proxy:set-port: %v", err)
		}
		return
	}
	if params.Port < 1 || params.Port > 65535 {
		if err := b.codec.RespondError(msg.ID, -32602, "port must be between 1 and 65535"); err != nil {
			log.Printf("failed to respond to proxy:set-port: %v", err)
		}
		return
	}

	effective := b.resolveProxyPort(params.Port)

	body, err := json.Marshal(map[string]int{"port": effective})
	if err != nil {
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("encode set-port: %v", err)); err != nil {
			log.Printf("failed to respond to proxy:set-port: %v", err)
		}
		return
	}
	result, rpcErr, err := p.Call(context.Background(), "set-port", body)
	switch {
	case err != nil:
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("proxy set-port failed: %v", err)); err != nil {
			log.Printf("failed to respond to proxy:set-port: %v", err)
		}
	case rpcErr != nil:
		if err := b.codec.RespondError(msg.ID, rpcErr.Code, rpcErr.Message); err != nil {
			log.Printf("failed to relay proxy set-port error: %v", err)
		}
	default:
		if err := b.codec.Respond(msg.ID, result); err != nil {
			log.Printf("failed to relay proxy set-port result: %v", err)
		}
	}
}

// resolveProxyPort returns the port the proxy should actually bind for a
// requested port: the request itself when free, or the next free port when a
// running engine already holds it (engines take precedence). A bump surfaces
// a sticky warning into the errors pipeline; a clean request clears any stale
// one so the notice doesn't outlive the conflict.
func (b *Broker) resolveProxyPort(requested int) int {
	if b.managedOllamaFacade.Load() {
		b.forwardErrorsClear(proxyPortBumpedID)
		return managedOllamaFacadePort
	}
	taken := b.runningEnginePorts()
	if aliasPort := b.currentOllamaHostAlias().Port; aliasPort > 0 {
		taken[aliasPort] = true
	}
	effective := nextFreeProxyPort(requested, taken)
	if effective != requested {
		b.forwardErrorsReport(errors.ServiceError{
			ID:        proxyPortBumpedID,
			Message:   fmt.Sprintf("Port %d is in use by a running engine; the proxy was moved to %d.", requested, effective),
			Timestamp: nowMillis(),
			NodeID:    b.nodeID,
			Severity:  "warning",
			Action:    "none",
		})
	} else {
		b.forwardErrorsClear(proxyPortBumpedID)
	}
	return effective
}

// reconcileProxyPortOnReady runs after the proxy announces a bound port
// (notably its restored port on startup). If a running engine now holds that
// port, it steers the proxy to a free one — engines take precedence — and
// surfaces the move. It must run on its own goroutine (never the proxy reader
// goroutine that calls forwardProxyNotification), so the p.Call round-trip
// can't deadlock the very reader that would deliver its response.
func (b *Broker) reconcileProxyPortOnReady(boundPort int) {
	if b.managedOllamaFacade.Load() {
		p := b.getProxy()
		if p == nil {
			return
		}
		if boundPort != managedOllamaFacadePort {
			body, _ := json.Marshal(map[string]int{"port": managedOllamaFacadePort})
			if _, rpcErr, err := p.Call(context.Background(), "set-port", body); err != nil || rpcErr != nil {
				b.blockManagedOllamaFacade("the proxy could not bind the compatibility port")
				slog.Warn("failed to bind managed Ollama facade", "err", err, "rpcErr", rpcErr)
			}
			return
		}

		// Commit the pending stopped-engine move only after :11434 is ours.
		if backend := b.takePendingManagedOllamaBackend(boundPort); backend != 0 {
			defer b.ollamaMoveInFlight.Store(false)
			em := b.getEngineMgr()
			params, _ := json.Marshal(map[string]any{"engine": "ollama", "port": backend})
			var rpcErr *RPCError
			var err error
			if em == nil {
				err = fmt.Errorf("engine manager unavailable")
			} else {
				_, rpcErr, err = em.Call(context.Background(), "engine:set-port", params)
			}
			if err != nil || rpcErr != nil {
				// The RPC may have applied and then lost its response. Keep the
				// fallback off both candidate ports, vacate :11434 first, then
				// best-effort restore the original stopped-engine configuration.
				sourcePort := b.ollamaBackendSourcePort()
				fallback := b.blockManagedOllamaFacade("the stopped Ollama backend could not be moved", backend)
				vacated := false
				if fallback != 0 {
					body, _ := json.Marshal(map[string]int{"port": fallback})
					if _, moveErr, callErr := p.Call(context.Background(), "set-port", body); callErr != nil || moveErr != nil {
						slog.Warn("failed to vacate managed Ollama facade", "err", callErr, "rpcErr", moveErr)
					} else {
						vacated = true
					}
				}
				if vacated && em != nil {
					rollback, _ := json.Marshal(map[string]any{"engine": "ollama", "port": sourcePort})
					if _, rollbackErr, callErr := em.Call(context.Background(), "engine:set-port", rollback); callErr != nil || rollbackErr != nil {
						slog.Warn("failed to restore original Ollama backend port", "err", callErr, "rpcErr", rollbackErr)
					}
				}
				b.markOllamaPortReady()
				return
			}
			b.managedOllamaBackend.Store(0)
			b.ollamaBackendPort.Store(int32(backend))
			slog.Info("configured managed Ollama backend port", "port", backend)
			b.ollamaMoveInFlight.Store(false)
		}
		if b.ollamaMoveInFlight.Load() {
			return
		}
		b.forwardErrorsClear(portOwnershipBlockedID)
		b.markOllamaPortReady()
		return
	}
	taken := b.runningEnginePorts()
	if aliasPort := b.currentOllamaHostAlias().Port; aliasPort > 0 {
		taken[aliasPort] = true
	}
	if !taken[boundPort] {
		return
	}
	effective := nextFreeProxyPort(boundPort, taken)
	if effective == boundPort {
		return
	}
	p := b.getProxy()
	if p == nil {
		return
	}
	b.forwardErrorsReport(errors.ServiceError{
		ID:        proxyPortBumpedID,
		Message:   fmt.Sprintf("Port %d is in use by a running engine; the proxy was moved to %d.", boundPort, effective),
		Timestamp: nowMillis(),
		NodeID:    b.nodeID,
		Severity:  "warning",
		Action:    "none",
	})
	body, err := json.Marshal(map[string]int{"port": effective})
	if err != nil {
		slog.Warn("failed to encode corrective proxy set-port", "err", err)
		return
	}
	if _, rpcErr, err := p.Call(context.Background(), "set-port", body); err != nil || rpcErr != nil {
		slog.Warn("failed to reconcile proxy port on ready", "err", err, "rpcErr", rpcErr)
	}
}

func (b *Broker) takePendingManagedOllamaBackend(boundPort int) int {
	if !b.managedOllamaFacade.Load() || boundPort != managedOllamaFacadePort {
		return 0
	}
	if !b.ollamaMoveInFlight.CompareAndSwap(false, true) {
		return 0
	}
	backend := int(b.managedOllamaBackend.Swap(0))
	if backend == 0 {
		b.ollamaMoveInFlight.Store(false)
	}
	return backend
}

func (b *Broker) ollamaBackendSourcePort() int {
	if port := int(b.ollamaBackendPort.Load()); port > 0 {
		return port
	}
	return managedOllamaFacadePort
}

// runningEnginePorts asks nvpair-engine-manager which ports its running engines
// hold, so the proxy can be steered clear of them. A missing/slow
// engine-manager yields an empty set (no bump) — safe, because a port with no
// live listener can't actually collide at bind time; only a running engine is
// a real conflict.
func (b *Broker) runningEnginePorts() map[int]bool {
	ports := map[int]bool{}
	em := b.getEngineMgr()
	if em == nil {
		return ports
	}
	ctx, cancel := context.WithTimeout(context.Background(), rpcWorkerCallTimeout)
	defer cancel()
	result, rpcErr, err := em.Call(ctx, "engine:get-installed", nil)
	if err != nil || rpcErr != nil {
		slog.Warn("engine port lookup failed; skipping proxy port conflict check", "err", err, "rpcErr", rpcErr)
		return ports
	}
	var resp struct {
		Engines []struct {
			Running bool `json:"running"`
			Port    int  `json:"port"`
		} `json:"engines"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		slog.Warn("engine port lookup returned unparseable result", "err", err)
		return ports
	}
	for _, e := range resp.Engines {
		if e.Running && e.Port > 0 {
			ports[e.Port] = true
		}
	}
	return ports
}

// nextFreeProxyPort returns requested when it's free, otherwise the lowest
// port above it not in taken (capped at 65535).
func nextFreeProxyPort(requested int, taken map[int]bool) int {
	p := requested
	for p < 65535 && taken[p] {
		p++
	}
	return p
}
