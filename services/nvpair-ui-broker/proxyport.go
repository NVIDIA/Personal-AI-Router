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

	"nvpair-shared/engines"
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
// These restate values the engine table already carries. They stay untyped
// constants because they feed int32 atomics throughout the package, where
// converting a table field at every use would be pure noise;
// TestBrokerConstantsMatchTheEngineTable fails if they ever disagree with the
// table. The prefix is ComponentName, which is also the relay prefix and the
// supervisor label.
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

// ollamaProxyProfile is the descriptor entry this file's Ollama-specific
// callers plan against. It is resolved once; a missing entry is a programming
// error in engineproxy.go, not a runtime condition.
var ollamaProxyProfile = mustEngineProxyProfile("ollama")

// planManagedOllamaPorts plans Ollama's managed ports. Ollama is an adopted
// engine, so the shared policy refuses to move it while it is running and
// blocks up front on an occupied facade port.
func planManagedOllamaPorts(enabled bool, st ollamaPortStatus, available func(int) bool) managedPortPlan {
	return planManagedEnginePorts(ollamaProxyProfile, enabled, st, available)
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
	b.ollamaState().managedFacade.Store(false)
	b.managedOllamaBackend.Store(0)
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

// finishOllamaProxyTerminal releases the ownership gate when ollama-proxy will
// never come back, mirroring finishLMStudioProxyTerminal. Without it a
// terminally-failed proxy leaves the gate closed for the life of the process,
// so every engine:status / engine:get-installed for Ollama waits out
// rpcWorkerCallTimeout and answers "retry" forever.
//
// Ollama needs more than closing the channel. Its gate re-checks
// ollamaFacadeIsPendingBackend after the wait, so a pending move left behind by
// the dead proxy would keep rejecting requests even with the channel closed.
// Clearing the move state is what actually reopens the path; LM Studio has no
// equivalent second check.
func (b *Broker) finishOllamaProxyTerminal() {
	b.ollamaState().managedFacade.Store(false)
	b.managedOllamaBackend.Store(0)
	b.ollamaMoveInFlight.Store(false)
	// The inherited OLLAMA_HOST alias is reserved with engine-manager during
	// preparation, before enablement is known, so every terminal path has to
	// give it back. Releasing it here rather than at each call site is what
	// makes that true: it was previously released on the two live-failure paths
	// and missed on the disabled-engine one, which left the alias port refusing
	// engine:set-port for the life of the process, citing a proxy alias that
	// did not exist.
	b.disableOllamaHostAliasReservation()
	b.markOllamaPortReady()
}

// configureProxySupervisorCallbacks wires the one proxy supervisor.
//
// Every engine's facade lives in the supervised process, so a crash is reported
// once for the process and clears every engine's handle — shared fate is the
// accepted cost of putting all the facades' reservations in one place. Each
// engine's ownership gate is still released individually, because a caller
// waiting on LM Studio's gate must not be held by Ollama's state.
func (b *Broker) configureProxySupervisorCallbacks(sup *supervisor) {
	sup.onCrash, sup.onRecovered = b.supervisedWorkerCallbacks(engines.ProxyComponent, func() {
		for _, profile := range engineProxyProfiles {
			b.setEngineProxyHandle(profile, nil)
		}
	})
	sup.onExhausted = func(attempt int) {
		slog.Warn("the proxy is terminally unavailable; releasing every engine's ownership gate",
			"attempt", attempt)
		for _, profile := range engineProxyProfiles {
			b.finishEngineProxyStartup(profile)
		}
	}
	sup.onSpawned = b.replayProxyStateAfterSpawn
}

// finishEngineProxyStartup releases one engine's port-ownership gate when its
// facade will not be coming up, so callers stop waiting on it.
//
// Ollama needs more than closing a channel, which is why this dispatches rather
// than looping over a single helper: its gate re-checks whether a backend move
// is pending, so clearing that move state is what actually reopens the path.
func (b *Broker) finishEngineProxyStartup(profile engineProxyProfile) {
	switch profile.Name {
	case ollamaProxyProfile.Name:
		b.finishOllamaProxyTerminal()
	case lmstudioProxyProfile.Name:
		b.finishLMStudioProxyTerminal()
	default:
		slog.Warn("no startup-gate finisher for engine", "engine", profile.Name)
	}
}

// rebindOllamaProxy moves the live Ollama facade onto a port, mirroring
// rebindLMStudioProxy. Best-effort: the caller has already given up the managed
// claim and released the gate, so a failure here leaves the facade where it is
// rather than blocking startup.
//
// No process restart. Every engine's facade shares this process, so restarting
// to move Ollama would drop LM Studio's listener and the inference on it.
func (b *Broker) rebindOllamaProxy(p *proxyProcess, port int, from int) {
	if p == nil || port == 0 || port == from {
		return
	}
	body, err := json.Marshal(map[string]int{"port": port})
	if err != nil {
		slog.Warn("failed to encode Ollama facade rebind", "port", port, "err", err)
		return
	}
	result, rpcErr, callErr := p.Call(context.Background(), ollamaProxyProfile.addressed("set-port"), body)
	if callErr != nil || rpcErr != nil {
		slog.Warn("failed to move the Ollama facade off the compatibility port",
			"from", from, "to", port, "err", callErr, "rpcErr", rpcErr)
		return
	}
	var ready proxyReadyParams
	if json.Unmarshal(result, &ready) != nil || ready.Port != port {
		slog.Warn("Ollama facade did not confirm the rebind", "from", from, "to", port)
		return
	}
	slog.Info("moved the Ollama facade off the compatibility port", "from", from, "to", port)
}

// setOllamaProxyFallback picks an explicit port for the proxy to start on when
// the managed facade is not in play. The engine's own configured port is
// excluded here rather than at each call site, so no caller can forget it.
func (b *Broker) setOllamaProxyFallback(excludedPorts ...int) int {
	if aliasPort := b.currentOllamaHostAlias().Port; aliasPort > 0 {
		excludedPorts = append(excludedPorts, aliasPort)
	}
	for port := range b.siblingEngineProxyPorts(ollamaProxyProfile) {
		excludedPorts = append(excludedPorts, port)
	}
	if backend := int(b.ollamaState().backendPort.Load()); backend > 0 {
		excludedPorts = append(excludedPorts, backend)
	} else {
		// No authoritative backend port yet, so a free port is not evidence
		// the engine will not claim it. The stock backend port in particular
		// is where a managed Ollama gets placed, and taking it now means the
		// proxy is squatting the engine's port when it starts.
		excludedPorts = append(excludedPorts, managedOllamaFacadePort, managedOllamaBackendStart)
	}
	fallback := nextAvailablePortExcluding(managedOllamaBackendStart, excludedPorts, tcpPortAvailable)
	b.ollamaState().startupPort.Store(int32(fallback))
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
	if b.prepareExplicitEngineSettings("ollama") {
		return
	}
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
	b.ollamaState().backendPort.Store(int32(st.Port))
	b.prepareOllamaHostAlias(policy.Value, st.Port)
	if !policy.Value {
		// Opting out after a managed run commonly leaves stopped Ollama on
		// :11435. Start the proxy explicitly elsewhere so it cannot squat that
		// configured backend port before Ollama starts.
		b.ollamaState().managedFacade.Store(false)
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
	b.ollamaState().managedFacade.Store(plan.Enabled)
	b.managedOllamaBackend.Store(int32(plan.BackendPort))
	b.ollamaState().startupPort.Store(int32(managedOllamaFacadePort))
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
		b.ollamaState().backendPort.Store(int32(st.Port))
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

// reconcileProxyPortOnReady runs after the proxy announces a bound port
// (notably its restored port on startup). If a running engine now holds that
// port, it steers the proxy to a free one — engines take precedence — and
// surfaces the move. It must run on its own goroutine (never the proxy reader
// goroutine that calls forwardProxyNotification), so the p.Call round-trip
// can't deadlock the very reader that would deliver its response.
func (b *Broker) reconcileProxyPortOnReady(boundPort int) {
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()
	if b.loadEngineSettingsLocked() != nil {
		b.markOllamaPortReady()
		return
	}
	if _, explicit := b.explicitEngineSettingsLocked("ollama"); explicit {
		b.markOllamaPortReady()
		return
	}
	if b.ollamaState().managedFacade.Load() {
		p := b.getProxy()
		if p == nil {
			return
		}
		if boundPort != managedOllamaFacadePort {
			body, _ := json.Marshal(map[string]int{"port": managedOllamaFacadePort})
			if _, rpcErr, err := p.Call(context.Background(), ollamaProxyProfile.addressed("set-port"), body); err != nil || rpcErr != nil {
				slog.Warn("failed to bind managed Ollama facade", "err", err, "rpcErr", rpcErr)
				// Give up the managed claim, then actually move the live facade
				// off the port it is on. blockManagedOllamaFacade only plans a
				// fallback for the next spawn, so without this the facade stays
				// where it bound — possibly on the engine's own backend port —
				// and nothing corrects it until a restart. LM Studio has had
				// this since its port recovery became facade-scoped; Ollama's
				// half was missing.
				fallback := b.blockManagedOllamaFacade("the proxy could not bind the compatibility port")
				b.rebindOllamaProxy(p, fallback, boundPort)
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
					if _, moveErr, callErr := p.Call(context.Background(), ollamaProxyProfile.addressed("set-port"), body); callErr != nil || moveErr != nil {
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
			b.ollamaState().backendPort.Store(int32(backend))
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
	if _, rpcErr, err := p.Call(context.Background(), ollamaProxyProfile.addressed("set-port"), body); err != nil || rpcErr != nil {
		slog.Warn("failed to reconcile proxy port on ready", "err", err, "rpcErr", rpcErr)
	}
}

func (b *Broker) takePendingManagedOllamaBackend(boundPort int) int {
	if !b.ollamaState().managedFacade.Load() || boundPort != managedOllamaFacadePort {
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
	if port := int(b.ollamaState().backendPort.Load()); port > 0 {
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
