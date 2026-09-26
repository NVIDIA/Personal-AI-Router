// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// llama.cpp's managed-port choreography, the counterpart of lmstudioport.go.
//
// llama.cpp is a managed engine in the same sense LM Studio is — engine-manager
// installed it and owns its lifecycle, so it may be stopped and repositioned —
// which means it gets the same treatment: move the backend first, verify the
// compatibility port is free, then force the proxy onto it. The shared policy
// in planManagedEnginePorts decides all of that; what is here is the
// orchestration around it.
//
// The one thing llama.cpp has that neither of the others does is a documented
// environment override for its own listen port. Upstream reads LLAMA_ARG_PORT
// for --port (common/arg.cpp), so a user who has set it has told us where their
// llama.cpp listens and therefore which port their clients already point at.
// That is what the facade should claim, so the effective profile resolves it
// rather than assuming the 8080 default. It is the same reasoning that makes an
// inherited OLLAMA_HOST supersede 11434 for Ollama, without any of the alias
// machinery: llama.cpp's variable names a port, not a whole endpoint PAIR has
// to keep answering on.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strconv"

	"nvpair-shared/errors"
)

const llamacppPortOwnershipBlockedID = "llamacpp-proxy:port-ownership-blocked"

// llamacppProxyProfile is the descriptor entry this file's callers plan
// against. See ollamaProxyProfile.
var llamacppProxyProfile = mustEngineProxyProfile("llamacpp")

// llamacppEnvPort reads LLAMA_ARG_PORT, the variable upstream llama.cpp reads
// for --port. Zero means unset or unusable, in which case the table's default
// stands.
func llamacppEnvPort() int {
	raw := os.Getenv("LLAMA_ARG_PORT")
	if raw == "" {
		return 0
	}
	port, err := strconv.Atoi(raw)
	// The ceiling is 65534, not 65535: the backend is relocated to one above
	// whatever the facade claims, so accepting the top port would plan a move
	// onto 65536, which is not a port at all.
	if err != nil || port < 1 || port > 65534 {
		slog.Warn("ignoring unusable LLAMA_ARG_PORT", "value", raw)
		return 0
	}
	return port
}

// llamacppEffectiveProfile is llamacppProxyProfile with the environment's port
// applied, and is what every port decision in this file plans against.
//
// The backend moves to one above whatever the facade claims, so the pair keeps
// the table's relationship rather than stranding the engine on 8081 while the
// facade sits somewhere else entirely.
func llamacppEffectiveProfile() engineProxyProfile {
	profile := llamacppProxyProfile
	if port := llamacppEnvPort(); port > 0 {
		profile.FacadePort = port
		profile.EnginePortBase = port + 1
	}
	return profile
}

// planManagedLlamaCppPorts plans llama.cpp's managed ports under the shared
// policy for an engine the broker may reposition while it runs.
func planManagedLlamaCppPorts(enabled bool, st ollamaPortStatus, available func(int) bool) managedPortPlan {
	return planManagedEnginePorts(llamacppEffectiveProfile(), enabled, st, available)
}

func (b *Broker) markLlamaCppPortReady() {
	if b.llamacppPortReady != nil {
		b.llamacppPortReadyOnce.Do(func() { close(b.llamacppPortReady) })
	}
}

func (b *Broker) llamacppPortOwnershipPending() bool {
	if b.llamacppPortReady == nil {
		return false
	}
	select {
	case <-b.llamacppPortReady:
		return false
	default:
		return true
	}
}

// needsLlamaCppPortGate mirrors needsLMStudioPortGate: the client requests that
// can probe and adopt the configured port, so they must wait while it is
// changing hands.
func needsLlamaCppPortGate(method string, params json.RawMessage) bool {
	if method == "engine:get-installed" {
		return true
	}
	if method != "engine:status" && method != "engine:install" && method != "engine:start" && method != "engine:restart" {
		return false
	}
	var request struct {
		Engine string `json:"engine"`
	}
	return json.Unmarshal(params, &request) == nil && request.Engine == llamacppProxyProfile.Name
}

func (b *Broker) reportLlamaCppPortOwnershipBlocked(reason string) {
	b.forwardErrorsReport(errors.ServiceError{
		ID: llamacppPortOwnershipBlockedID,
		Message: fmt.Sprintf("NVPAIR could not safely reserve llama.cpp port %d: %s. No unknown process was stopped.",
			llamacppEffectiveProfile().FacadePort, reason),
		Timestamp: nowMillis(),
		NodeID:    b.nodeID,
		Severity:  "warning",
		Action:    "none",
	})
}

func (b *Broker) rebindLlamaCppProxy(p *proxyProcess, port int) bool {
	if p == nil || port == 0 {
		return false
	}
	body, _ := json.Marshal(map[string]int{"port": port})
	result, rpcErr, err := p.Call(context.Background(), llamacppProxyProfile.addressed("set-port"), body)
	if err != nil || rpcErr != nil {
		slog.Warn("failed to rebind llama.cpp proxy", "port", port, "err", err, "rpcErr", rpcErr)
		return false
	}
	var ready proxyReadyParams
	return json.Unmarshal(result, &ready) == nil && ready.Port == port
}

// blockManagedLlamaCppFacade records an explicit fallback and optionally
// rebinds a live proxy. It does not open the ownership gate: only a confirmed
// bound proxy generation or an exhausted supervisor may do that.
func (b *Broker) blockManagedLlamaCppFacade(reason string, p *proxyProcess, excludedPorts ...int) (int, bool) {
	b.llamacppState().managedFacade.Store(false)
	fallback := b.setLlamaCppProxyFallback(excludedPorts...)
	b.reportLlamaCppPortOwnershipBlocked(reason)
	return fallback, b.rebindLlamaCppProxy(p, fallback)
}

func (b *Broker) cacheLlamaCppPortStatus() (ollamaPortStatus, bool) {
	em := b.getEngineMgr()
	if em == nil {
		return ollamaPortStatus{}, false
	}
	params, _ := json.Marshal(map[string]string{"engine": llamacppProxyProfile.Name})
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
	b.llamacppState().backendPort.Store(int32(st.Port))
	return st, true
}

func (b *Broker) configureUnmanagedLlamaCppFacade() {
	b.llamacppState().managedFacade.Store(false)
	b.llamacppState().startupPort.Store(0)
	b.forwardErrorsClear(llamacppPortOwnershipBlockedID)
}

// prepareManagedLlamaCppFacade runs after engine-manager starts and before the
// proxy is spawned, exactly as LM Studio's does: engine-manager is the
// authority that may safely stop and reposition an engine it installed, so the
// backend moves first and the compatibility port is judged afterwards.
func (b *Broker) prepareManagedLlamaCppFacade() {
	b.prepareManagedLlamaCppFacadeWithPortCheck(tcpPortAvailable)
}

func (b *Broker) prepareManagedLlamaCppFacadeWithPortCheck(portAvailable func(int) bool) {
	// An explicit settings choice fixes both ports and turns takeover off, for
	// this engine exactly as for the other two.
	if b.prepareExplicitEngineSettings(llamacppProxyProfile.Name) {
		return
	}
	profile := llamacppEffectiveProfile()
	// Ollama's facade is prepared first, so an inherited OLLAMA_HOST alias is
	// already reserved and must stay out of llama.cpp's backend search.
	portAvailable = b.availableOffOllamaHostAlias(portAvailable)

	settings := b.getSettings()
	if settings == nil {
		b.cacheLlamaCppPortStatus()
		_, _ = b.blockManagedLlamaCppFacade("managed-port policy is unavailable", nil)
		return
	}
	result, rpcErr, err := settings.Call(context.Background(), "settings/get-force-ports", nil)
	if err != nil || rpcErr != nil {
		slog.Warn("failed to read managed llama.cpp port setting", "err", err, "rpcErr", rpcErr)
		b.cacheLlamaCppPortStatus()
		_, _ = b.blockManagedLlamaCppFacade("managed-port policy could not be verified", nil)
		return
	}
	var policy struct {
		Value bool `json:"value"`
	}
	if json.Unmarshal(result, &policy) != nil {
		b.cacheLlamaCppPortStatus()
		_, _ = b.blockManagedLlamaCppFacade("managed-port policy could not be decoded", nil)
		return
	}

	em := b.getEngineMgr()
	if em == nil {
		if !policy.Value {
			b.configureUnmanagedLlamaCppFacade()
			return
		}
		_, _ = b.blockManagedLlamaCppFacade("engine manager is unavailable", nil)
		return
	}

	params, _ := json.Marshal(map[string]any{"engine": profile.Name, "port": profile.FacadePort})
	result, rpcErr, err = em.Call(context.Background(), "engine:status", params)
	if err != nil || rpcErr != nil {
		if !policy.Value {
			b.configureUnmanagedLlamaCppFacade()
			return
		}
		_, _ = b.blockManagedLlamaCppFacade("llama.cpp status could not be verified", nil)
		return
	}
	var st ollamaPortStatus
	if json.Unmarshal(result, &st) != nil {
		if !policy.Value {
			b.configureUnmanagedLlamaCppFacade()
			return
		}
		_, _ = b.blockManagedLlamaCppFacade("llama.cpp status could not be decoded", nil)
		return
	}
	if st.Port > 0 {
		b.llamacppState().backendPort.Store(int32(st.Port))
	}
	if !policy.Value {
		b.configureUnmanagedLlamaCppFacade()
		return
	}

	plan := planManagedLlamaCppPorts(true, st, portAvailable)
	if plan.Blocked != "" {
		_, _ = b.blockManagedLlamaCppFacade(plan.Blocked, nil, st.Port)
		return
	}
	if plan.BackendPort != 0 {
		params, _ = json.Marshal(map[string]any{"engine": profile.Name, "port": plan.BackendPort})
		result, rpcErr, err = em.CallNoTimeout(context.Background(), "engine:set-port", params)
		if err != nil || rpcErr != nil {
			b.cacheLlamaCppPortStatus()
			_, _ = b.blockManagedLlamaCppFacade("the llama.cpp backend could not be moved", nil, st.Port, plan.BackendPort)
			return
		}
		b.llamacppState().backendPort.Store(int32(plan.BackendPort))
		var moved ollamaPortStatus
		if json.Unmarshal(result, &moved) == nil && moved.Port > 0 {
			b.llamacppState().backendPort.Store(int32(moved.Port))
		}
	}
	if !portAvailable(profile.FacadePort) {
		_, _ = b.blockManagedLlamaCppFacade("the compatibility port is already in use", nil, st.Port)
		return
	}

	b.llamacppState().managedFacade.Store(plan.Enabled)
	b.llamacppState().startupPort.Store(int32(profile.FacadePort))
	b.forwardErrorsClear(llamacppPortOwnershipBlockedID)
}

func (b *Broker) llamacppProxyGenerationIsCurrent(generation uint64, p *proxyProcess) bool {
	return b.llamaCppProxyGeneration.Load() == generation &&
		b.llamaCppProxyPublishedGeneration.Load() == generation &&
		b.getLlamaCppProxy() == p
}

// rebindLlamaCppFacadeOrFinish moves the facade to another port when the one
// just asked for could not be bound, and gives up on this engine alone if the
// second attempt fails too. See rebindLMStudioFacadeOrFinish for why this
// replaces a process restart.
//
// Caller holds llamacppReadyMu.
func (b *Broker) rebindLlamaCppFacadeOrFinish(p *proxyProcess, generation uint64, avoid ...int) (int, bool) {
	if b.llamaCppProxyGeneration.Load() != generation {
		return 0, false
	}
	fallback := b.setLlamaCppProxyFallback(avoid...)
	if fallback != 0 && b.rebindLlamaCppProxy(p, fallback) {
		return fallback, true
	}
	slog.Warn("llama.cpp facade could not be rebound; releasing its ownership gate",
		"attempted", fallback, "avoided", avoid)
	b.finishLlamaCppProxyTerminal()
	return 0, false
}

func (b *Broker) reconcileLlamaCppProxyAfterEngineManagerReady() {
	if !b.llamacppPortOwnershipPending() {
		return
	}
	p := b.getLlamaCppProxy()
	if p == nil {
		return
	}
	ready, port := p.Status(llamacppProxyProfile.Name)
	generation := b.llamaCppProxyPublishedGeneration.Load()
	if !ready || port <= 0 || generation != b.llamaCppProxyGeneration.Load() {
		return
	}
	go b.reconcileLlamaCppProxyPortOnReadyForGeneration(generation, port)
}

// reconcileLlamaCppProxyPortOnReadyForGeneration runs off the proxy reader
// goroutine because both set-port and node/set-local-backend round-trip through
// that reader.
//
// It holds the node configuration lock throughout, as LM Studio's does:
// automatic port reconciliation and a settings operation both move this facade,
// and the journal decides which of them is in charge.
func (b *Broker) reconcileLlamaCppProxyPortOnReadyForGeneration(generation uint64, boundPort int) {
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()
	if b.settingsGovernFacadeLocked(llamacppProxyProfile) {
		if b.llamaCppProxyGeneration.Load() == generation {
			b.markLlamaCppPortReady()
		}
		return
	}
	if b.llamaCppProxyGeneration.Load() != generation {
		return
	}
	// Serialize the ownership transition, including fallback rebind: a set-port
	// emits another ready before its response, so without this guard that
	// second callback could release the gate while the first was still moving
	// the proxy.
	b.llamacppReadyMu.Lock()
	defer b.llamacppReadyMu.Unlock()

	profile := llamacppEffectiveProfile()
	p := b.getLlamaCppProxy()
	if p == nil || !b.llamacppProxyGenerationIsCurrent(generation, p) {
		return
	}
	if b.llamacppState().managedFacade.Load() && boundPort != profile.FacadePort {
		if b.rebindLlamaCppProxy(p, profile.FacadePort) {
			boundPort = profile.FacadePort
		} else {
			fallback, rebound := b.blockManagedLlamaCppFacade("the proxy could not bind the compatibility port", p, boundPort)
			if !rebound {
				next, ok := b.rebindLlamaCppFacadeOrFinish(p, generation, boundPort, fallback)
				if !ok {
					return
				}
				fallback = next
			}
			boundPort = fallback
		}
	}

	backend := int(b.llamacppState().backendPort.Load())
	if backend == 0 {
		// Fail closed on the relocated default before asking engine-manager
		// for status, so the proxy is never transiently sitting on the port
		// the engine is configured for.
		if boundPort == profile.EnginePortBase {
			fallback := b.setLlamaCppProxyFallback(boundPort)
			if !b.rebindLlamaCppProxy(p, fallback) {
				next, ok := b.rebindLlamaCppFacadeOrFinish(p, generation, boundPort, fallback)
				if !ok {
					return
				}
				fallback = next
			}
			boundPort = fallback
		}
		backend = profile.EnginePortBase
	}
	avoidBackendCollision := func() bool {
		if backend != boundPort {
			return true
		}
		var fallback int
		var rebound bool
		if b.llamacppState().managedFacade.Load() {
			fallback, rebound = b.blockManagedLlamaCppFacade("the proxy bound the configured llama.cpp backend port", p, boundPort)
		} else {
			fallback = b.setLlamaCppProxyFallback(boundPort)
			rebound = b.rebindLlamaCppProxy(p, fallback)
		}
		if !rebound {
			next, ok := b.rebindLlamaCppFacadeOrFinish(p, generation, boundPort, fallback, backend)
			if !ok {
				return false
			}
			fallback = next
		}
		boundPort = fallback
		return true
	}
	if !avoidBackendCollision() {
		return
	}

	st, current := b.cacheLlamaCppPortStatus()
	if !b.llamacppProxyGenerationIsCurrent(generation, p) {
		return
	}
	cached := int(b.llamacppState().backendPort.Load())
	if cached <= 0 {
		// A bound proxy plus an unknown configured backend is not a terminal
		// ownership result. Keep restoration gated until engine-manager
		// supplies the authoritative port.
		slog.Warn("llama.cpp backend port remains unknown; keeping ownership gate closed")
		return
	}
	backend = cached
	if !avoidBackendCollision() {
		return
	}
	healthy := current && st.Running && st.Port == backend
	if !b.llamacppProxyGenerationIsCurrent(generation, p) {
		return
	}
	b.setProxyLocalBackend(p, llamacppProxyProfile.Name, backend, healthy)
	if !b.llamacppProxyGenerationIsCurrent(generation, p) {
		return
	}
	b.markLlamaCppPortReady()
}
