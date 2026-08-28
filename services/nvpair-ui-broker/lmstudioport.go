// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"nvpair-shared/errors"
)

const (
	managedLMStudioFacadePort      = 1234
	managedLMStudioBackendStart    = 1235
	lmstudioPortOwnershipBlockedID = "lmstudio-proxy:port-ownership-blocked"
)

// planManagedLMStudioPorts keeps the ownership policy deterministic. LM Studio
// is the one engine that engine-manager may move while running: its identified
// command-mode runtime has an official stop command. Process-mode and unknown
// owners are still refused by engine:set-port.
func planManagedLMStudioPorts(enabled bool, st ollamaPortStatus, available func(int) bool) managedPortPlan {
	if !enabled {
		return managedPortPlan{}
	}
	if st.Port > 0 && st.Port != managedLMStudioFacadePort {
		if !available(managedLMStudioFacadePort) {
			return managedPortPlan{Blocked: "the compatibility port is already in use"}
		}
		if !st.Running && !available(st.Port) {
			backend := nextAvailablePort(managedLMStudioBackendStart, available)
			if backend == 0 {
				return managedPortPlan{Blocked: "no free backend port is available"}
			}
			return managedPortPlan{Enabled: true, BackendPort: backend}
		}
		return managedPortPlan{Enabled: true}
	}
	backend := nextAvailablePort(managedLMStudioBackendStart, available)
	if backend == 0 {
		return managedPortPlan{Blocked: "no free backend port is available"}
	}
	return managedPortPlan{Enabled: true, BackendPort: backend}
}

func (b *Broker) markLMStudioPortReady() {
	if b.lmstudioPortReady != nil {
		b.lmstudioPortReadyOnce.Do(func() { close(b.lmstudioPortReady) })
	}
}

func (b *Broker) waitForManagedPortOwnership(ctx context.Context) bool {
	for _, ready := range []<-chan struct{}{b.ollamaPortReady, b.lmstudioPortReady} {
		select {
		case <-ctx.Done():
			return false
		case <-ready:
		}
	}
	return true
}

func (b *Broker) managedPortOwnershipReady() bool {
	for _, ready := range []<-chan struct{}{b.ollamaPortReady, b.lmstudioPortReady} {
		select {
		case <-ready:
		default:
			return false
		}
	}
	return true
}

func (b *Broker) lmstudioPortOwnershipPending() bool {
	if b.lmstudioPortReady == nil {
		return false
	}
	select {
	case <-b.lmstudioPortReady:
		return false
	default:
		return true
	}
}

func needsLMStudioPortGate(method string, params json.RawMessage) bool {
	if method == "engine:get-installed" {
		return true
	}
	if method != "engine:status" && method != "engine:start" && method != "engine:restart" {
		return false
	}
	var request struct {
		Engine string `json:"engine"`
	}
	return json.Unmarshal(params, &request) == nil && request.Engine == "lmstudio"
}

func lmstudioSetPortRequest(method string, params json.RawMessage) (int, bool) {
	if method != "engine:set-port" {
		return 0, false
	}
	var request struct {
		Engine string `json:"engine"`
		Port   int    `json:"port"`
	}
	if json.Unmarshal(params, &request) != nil || request.Engine != "lmstudio" || request.Port <= 0 {
		return 0, false
	}
	return request.Port, true
}

func (b *Broker) reportLMStudioPortOwnershipBlocked(reason string) {
	b.forwardErrorsReport(errors.ServiceError{
		ID:        lmstudioPortOwnershipBlockedID,
		Message:   fmt.Sprintf("NVPAIR could not safely reserve LM Studio port %d: %s. No unknown process was stopped.", managedLMStudioFacadePort, reason),
		Timestamp: nowMillis(),
		NodeID:    b.nodeID,
		Severity:  "warning",
		Action:    "none",
	})
}

func (b *Broker) setLMStudioProxyFallback(excludedPorts ...int) int {
	if aliasPort := b.currentOllamaHostAlias().Port; aliasPort > 0 {
		excludedPorts = append(excludedPorts, aliasPort)
	}
	if backend := int(b.lmstudioBackendPort.Load()); backend > 0 {
		excludedPorts = append(excludedPorts, backend)
	} else {
		// With no authoritative backend yet, neither LM Studio's compatibility
		// port nor engine-manager's bundled backend default is safe evidence of a
		// free proxy port.
		excludedPorts = append(excludedPorts, managedLMStudioFacadePort, managedLMStudioBackendStart)
	}
	fallback := nextAvailablePortExcluding(managedLMStudioBackendStart, excludedPorts, tcpPortAvailable)
	b.lmstudioProxyStartupPort.Store(int32(fallback))
	return fallback
}

func (b *Broker) rebindLMStudioProxy(p *proxyProcess, port int) bool {
	if p == nil || port == 0 {
		return false
	}
	body, _ := json.Marshal(map[string]int{"port": port})
	result, rpcErr, err := p.Call(context.Background(), "set-port", body)
	if err != nil || rpcErr != nil {
		slog.Warn("failed to rebind LM Studio proxy", "port", port, "err", err, "rpcErr", rpcErr)
		return false
	}
	var ready proxyReadyParams
	return json.Unmarshal(result, &ready) == nil && ready.Port == port
}

// blockManagedLMStudioFacade records an explicit fallback and optionally
// rebinds a live proxy. It does not open the ownership gate: only a confirmed
// bound proxy generation or an exhausted supervisor may do that.
func (b *Broker) blockManagedLMStudioFacade(reason string, p *proxyProcess, excludedPorts ...int) (int, bool) {
	b.managedLMStudioFacade.Store(false)
	fallback := b.setLMStudioProxyFallback(excludedPorts...)
	b.reportLMStudioPortOwnershipBlocked(reason)
	return fallback, b.rebindLMStudioProxy(p, fallback)
}

func (b *Broker) cacheLMStudioPortStatus() (ollamaPortStatus, bool) {
	em := b.getEngineMgr()
	if em == nil {
		return ollamaPortStatus{}, false
	}
	params, _ := json.Marshal(map[string]string{"engine": "lmstudio"})
	result, rpcErr, err := em.Call(context.Background(), "engine:status", params)
	if err != nil || rpcErr != nil {
		return ollamaPortStatus{}, false
	}
	var st ollamaPortStatus
	if json.Unmarshal(result, &st) != nil {
		return ollamaPortStatus{}, false
	}
	if st.Port <= 0 {
		return st, false
	}
	b.lmstudioBackendPort.Store(int32(st.Port))
	return st, true
}

func (b *Broker) configureUnmanagedLMStudioFacade() {
	b.managedLMStudioFacade.Store(false)
	b.lmstudioProxyStartupPort.Store(0)
	b.forwardErrorsClear(lmstudioPortOwnershipBlockedID)
}

// prepareManagedLMStudioFacade runs after engine-manager starts and before the
// LM Studio proxy is spawned. Unlike Ollama, the backend move happens first:
// engine-manager is the authority that may safely stop/restart an identified
// command-mode LM Studio engine. Only after :1234 is verified free does the
// broker force the proxy onto the compatibility port.
func (b *Broker) prepareManagedLMStudioFacade() {
	b.prepareManagedLMStudioFacadeWithPortCheck(tcpPortAvailable)
}

func (b *Broker) prepareManagedLMStudioFacadeWithPortCheck(portAvailable func(int) bool) {
	// Ollama's facade is prepared first, so an inherited OLLAMA_HOST alias is
	// already reserved here and must stay out of LM Studio's backend search.
	portAvailable = b.availableOffOllamaHostAlias(portAvailable)
	settings := b.getSettings()
	if settings == nil {
		b.cacheLMStudioPortStatus()
		_, _ = b.blockManagedLMStudioFacade("managed-port policy is unavailable", nil)
		return
	}
	result, rpcErr, err := settings.Call(context.Background(), "settings/get-force-ports", nil)
	if err != nil || rpcErr != nil {
		slog.Warn("failed to read managed LM Studio port setting", "err", err, "rpcErr", rpcErr)
		b.cacheLMStudioPortStatus()
		_, _ = b.blockManagedLMStudioFacade("managed-port policy could not be verified", nil)
		return
	}
	var policy struct {
		Value bool `json:"value"`
	}
	if json.Unmarshal(result, &policy) != nil {
		b.cacheLMStudioPortStatus()
		_, _ = b.blockManagedLMStudioFacade("managed-port policy could not be decoded", nil)
		return
	}

	em := b.getEngineMgr()
	if em == nil {
		if !policy.Value {
			b.configureUnmanagedLMStudioFacade()
			return
		}
		_, _ = b.blockManagedLMStudioFacade("engine manager is unavailable", nil)
		return
	}

	params, _ := json.Marshal(map[string]any{"engine": "lmstudio", "port": managedLMStudioFacadePort})
	result, rpcErr, err = em.Call(context.Background(), "engine:status", params)
	if err != nil || rpcErr != nil {
		if !policy.Value {
			b.configureUnmanagedLMStudioFacade()
			return
		}
		_, _ = b.blockManagedLMStudioFacade("LM Studio status could not be verified", nil)
		return
	}
	var st ollamaPortStatus
	if json.Unmarshal(result, &st) != nil {
		if !policy.Value {
			b.configureUnmanagedLMStudioFacade()
			return
		}
		_, _ = b.blockManagedLMStudioFacade("LM Studio status could not be decoded", nil)
		return
	}
	if st.Port > 0 {
		b.lmstudioBackendPort.Store(int32(st.Port))
	}
	if !policy.Value {
		b.configureUnmanagedLMStudioFacade()
		return
	}

	plan := planManagedLMStudioPorts(true, st, portAvailable)
	if plan.Blocked != "" {
		_, _ = b.blockManagedLMStudioFacade(plan.Blocked, nil, st.Port)
		return
	}
	if plan.BackendPort != 0 {
		params, _ = json.Marshal(map[string]any{"engine": "lmstudio", "port": plan.BackendPort})
		result, rpcErr, err = em.CallNoTimeout(context.Background(), "engine:set-port", params)
		if err != nil || rpcErr != nil {
			b.cacheLMStudioPortStatus()
			_, _ = b.blockManagedLMStudioFacade("the LM Studio backend could not be moved", nil, st.Port, plan.BackendPort)
			return
		}
		b.lmstudioBackendPort.Store(int32(plan.BackendPort))
		var moved ollamaPortStatus
		if json.Unmarshal(result, &moved) == nil && moved.Port > 0 {
			b.lmstudioBackendPort.Store(int32(moved.Port))
		}
	}
	if !portAvailable(managedLMStudioFacadePort) {
		_, _ = b.blockManagedLMStudioFacade("the compatibility port is already in use", nil, st.Port)
		return
	}

	b.managedLMStudioFacade.Store(plan.Enabled)
	b.lmstudioProxyStartupPort.Store(managedLMStudioFacadePort)
	b.forwardErrorsClear(lmstudioPortOwnershipBlockedID)
}

func (b *Broker) lmstudioProxyGenerationIsCurrent(generation uint64, p *proxyProcess) bool {
	return b.lmstudioProxyGeneration.Load() == generation &&
		b.lmstudioProxyPublishedGeneration.Load() == generation &&
		b.getLMStudioProxy() == p
}

// invalidateLMStudioProxyForRestartLocked invalidates the current logical
// generation and broker-visible handle. Caller holds lmstudioReadyMu.
func (b *Broker) invalidateLMStudioProxyForRestartLocked(generation uint64) bool {
	if b.lmstudioProxyGeneration.Load() != generation {
		return false
	}
	b.lmstudioProxyGeneration.Add(1)
	b.lmstudioProxyPublishedGeneration.Store(0)
	b.setLMStudioProxy(nil)
	return true
}

func (b *Broker) restartLMStudioProxyOrFinish(generation uint64) {
	// Caller holds lmstudioReadyMu. Invalidate before the restart request
	// becomes observable so a ready notification already queued by the failed
	// process cannot complete the gate.
	if !b.invalidateLMStudioProxyForRestartLocked(generation) {
		return
	}
	if b.lmstudioProxySup != nil {
		b.lmstudioProxySup.Restart()
		return
	}
	b.finishLMStudioProxyTerminal()
}

func (b *Broker) reconcileLMStudioProxyAfterEngineManagerReady() {
	if !b.lmstudioPortOwnershipPending() {
		return
	}
	p := b.getLMStudioProxy()
	if p == nil {
		return
	}
	ready, port := p.Status()
	generation := b.lmstudioProxyPublishedGeneration.Load()
	if !ready || port <= 0 || generation != b.lmstudioProxyGeneration.Load() {
		return
	}
	go b.reconcileLMStudioProxyPortOnReadyForGeneration(generation, port)
}

// reconcileLMStudioProxyPortOnReady runs off the proxy reader goroutine because
// both set-port and node/set-local-backend round-trip through that reader.
func (b *Broker) reconcileLMStudioProxyPortOnReady(boundPort int) {
	b.reconcileLMStudioProxyPortOnReadyForGeneration(b.lmstudioProxyGeneration.Load(), boundPort)
}

func (b *Broker) reconcileLMStudioProxyPortOnReadyForGeneration(generation uint64, boundPort int) {
	if b.lmstudioProxyGeneration.Load() != generation {
		return
	}
	// Serialize the ownership transition, including fallback rebind. A set-port
	// emits another ready before its response, so without this guard that second
	// callback could release the gate while the first callback was still moving
	// the proxy. No caller holds another broker lock when entering this method.
	b.lmstudioReadyMu.Lock()
	defer b.lmstudioReadyMu.Unlock()

	p := b.getLMStudioProxy()
	if p == nil || !b.lmstudioProxyGenerationIsCurrent(generation, p) {
		return
	}
	if b.managedLMStudioFacade.Load() && boundPort != managedLMStudioFacadePort {
		if b.rebindLMStudioProxy(p, managedLMStudioFacadePort) {
			boundPort = managedLMStudioFacadePort
		} else {
			fallback, rebound := b.blockManagedLMStudioFacade("the proxy could not bind the compatibility port", p, boundPort)
			if !rebound {
				b.restartLMStudioProxyOrFinish(generation)
				return
			}
			boundPort = fallback
		}
	}

	backend := int(b.lmstudioBackendPort.Load())
	if backend == 0 {
		// Fail closed on the bundled backend default before asking
		// engine-manager for status. Its identity probe is also rejected by the
		// facade, but moving first avoids even transiently probing this proxy.
		if boundPort == managedLMStudioBackendStart {
			fallback := b.setLMStudioProxyFallback(boundPort)
			if !b.rebindLMStudioProxy(p, fallback) {
				b.restartLMStudioProxyOrFinish(generation)
				return
			}
			boundPort = fallback
		}
		backend = defaultLMStudioPort
	}
	avoidBackendCollision := func() bool {
		if backend != boundPort {
			return true
		}
		var fallback int
		var rebound bool
		if b.managedLMStudioFacade.Load() {
			fallback, rebound = b.blockManagedLMStudioFacade("the proxy bound the configured LM Studio backend port", p, boundPort)
		} else {
			fallback = b.setLMStudioProxyFallback(boundPort)
			rebound = b.rebindLMStudioProxy(p, fallback)
		}
		if !rebound {
			b.restartLMStudioProxyOrFinish(generation)
			return false
		}
		boundPort = fallback
		return true
	}
	// Never probe engine-manager while the proxy is sitting on the configured
	// backend: that probe could adopt the proxy as LM Studio. Move the proxy
	// first, then refresh authoritative engine state.
	if !avoidBackendCollision() {
		return
	}

	st, current := b.cacheLMStudioPortStatus()
	if !b.lmstudioProxyGenerationIsCurrent(generation, p) {
		return
	}
	cached := int(b.lmstudioBackendPort.Load())
	if cached <= 0 {
		// A bound proxy plus an unknown configured backend is not a terminal
		// ownership result. Keep restoration gated until engine-manager (or a
		// replacement manager) supplies the authoritative port.
		slog.Warn("LM Studio backend port remains unknown; keeping ownership gate closed")
		return
	}
	backend = cached
	if !avoidBackendCollision() {
		return
	}
	healthy := current && st.Running && st.Port == backend
	if !b.lmstudioProxyGenerationIsCurrent(generation, p) {
		return
	}
	b.setProxyLocalBackend(p, "lmstudio", backend, healthy)
	if !b.lmstudioProxyGenerationIsCurrent(generation, p) {
		return
	}
	b.markLMStudioPortReady()
	b.repushPriority("lmstudio")
}
