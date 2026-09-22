// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log/slog"
)

// llamacppproxy.go is the broker's llama.cpp counterpart to lmstudioproxy.go:
// the engine-specific head of the shared proxy wiring. The facade lives in the
// same nvpair-proxy process as every other engine's and is brought up with a
// facade/enable after spawn, so there is no separate binary, supervisor, or
// argv here.
//
// llama.cpp is treated exactly as LM Studio is: its stock port is claimed for
// the facade and the engine is relocated above it. The port choreography that
// makes that safe lives in llamacppport.go; this file is the process-facing
// head — the enable spec, the fallback port, and the notification reader.
//
// llamacppProxyProfile is declared in llamacppport.go, next to the effective
// profile that applies LLAMA_ARG_PORT to it.

// llamacppState is shorthand for this engine's runtime row, matching
// ollamaState / lmstudioState.
func (b *Broker) llamacppState() *engineProxyRuntime { return b.engineProxy(llamacppProxyProfile) }

func (b *Broker) setLlamaCppProxy(p *proxyProcess) {
	b.setEngineProxyHandle(llamacppProxyProfile, p)
}

func (b *Broker) getLlamaCppProxy() *proxyProcess {
	return b.engineProxyHandle(llamacppProxyProfile)
}

// llamacppFacadeSpec is the enable request for llama.cpp's facade. Like LM
// Studio's it carries no alias addresses — the alias stands in for an
// inherited host variable, which only Ollama has.
func (b *Broker) llamacppFacadeSpec() enableFacadeRequest {
	spec := enableFacadeRequest{Engine: llamacppProxyProfile.Name}
	if port := int(b.llamacppState().startupPort.Load()); port != 0 {
		spec.Port = port
		spec.IgnorePersistedPort = true
	}
	return spec
}

// setLlamaCppProxyFallback picks a port for this facade to retry on and records
// it, so a later enable asks for the same one.
//
// Mirrors setLMStudioProxyFallback: search from the backend base, and with no
// authoritative backend port yet treat both the compatibility port and the
// relocated default as unsafe rather than as evidence they are free.
func (b *Broker) setLlamaCppProxyFallback(excludedPorts ...int) int {
	profile := llamacppEffectiveProfile()
	if aliasPort := b.currentOllamaHostAlias().Port; aliasPort > 0 {
		excludedPorts = append(excludedPorts, aliasPort)
	}
	for port := range b.siblingEngineProxyPorts(llamacppProxyProfile) {
		excludedPorts = append(excludedPorts, port)
	}
	if backend := int(b.llamacppState().backendPort.Load()); backend > 0 {
		excludedPorts = append(excludedPorts, backend)
	} else {
		excludedPorts = append(excludedPorts, profile.FacadePort, profile.EnginePortBase)
	}
	fallback := nextAvailablePortExcluding(profile.EnginePortBase, excludedPorts, tcpPortAvailable)
	b.llamacppState().startupPort.Store(int32(fallback))
	return fallback
}

// finishLlamaCppProxyTerminal gives up this engine's managed claim and releases
// its startup gate, so client requests stop waiting on an engine that is not
// coming up.
func (b *Broker) finishLlamaCppProxyTerminal() {
	b.llamacppState().managedFacade.Store(false)
	b.markLlamaCppPortReady()
}

// llamacppFallbackPort mirrors lmstudioFallbackPort: prefer the port the
// bind-failed notification already chose, and recompute only if it did not run.
func (b *Broker) llamacppFallbackPort(failed int) int {
	if planned := int(b.llamacppState().startupPort.Load()); planned != 0 && planned != failed {
		return planned
	}
	return b.setLlamaCppProxyFallback(failed)
}

// forwardLlamaCppProxyNotificationForGeneration is the hook spawnProxy invokes
// for this facade's notifications, mirroring its LM Studio counterpart:
// errors:report / errors:clear go into the nvpair-errors pipeline; a lost bind
// chooses a fallback without calling back into this reader goroutine; a ready
// notification starts the ownership reconcile on its own goroutine, because
// that path's set-port and node/set-local-backend both round-trip through here.
func (b *Broker) forwardLlamaCppProxyNotificationForGeneration(generation uint64, method string, params json.RawMessage) {
	if b.llamaCppProxyGeneration.Load() != generation {
		return
	}
	// Strip the facade address before anything dispatches on the method,
	// starting with the errors relay below: it matches bare names.
	method, addressed := facadeMethodFor(llamacppProxyProfile, method)
	if !addressed {
		return
	}
	if b.dispatchErrorsNotif(llamacppProxyProfile.ComponentName(), method, params) {
		return
	}
	// A process can win the compatibility port after preparation's free-port
	// check but before the facade binds. Choose an explicit fallback here; the
	// failing enable retries in-process and llamacppFallbackPort finds it. A
	// port the user chose through settings is left alone, as for every engine.
	if port, recover := b.facadeBindFailure(llamacppProxyProfile, method, params); recover {
		if b.llamacppState().managedFacade.Load() && port == llamacppEffectiveProfile().FacadePort {
			_, _ = b.blockManagedLlamaCppFacade("another process acquired the compatibility port during startup", nil)
		} else {
			fallback := b.setLlamaCppProxyFallback(port)
			slog.Warn("llama.cpp proxy bind failed; retrying on fallback", "port", port, "fallback", fallback)
		}
	}
	if method == "ready" {
		var rp proxyReadyParams
		if err := json.Unmarshal(params, &rp); err == nil && rp.Port > 0 {
			go b.reconcileLlamaCppProxyPortOnReadyForGeneration(generation, rp.Port)
		}
	}
	b.forwardEngineProxyNotification(llamacppProxyProfile, method, params)
}

// cancelLlamaWorkload forwards a headless cancel to the exact request that
// produced it. Only llama.cpp requests that originated on this node are
// accepted: the run id identifies one proxy lifetime, so a stale run or a
// foreign origin cannot cancel an unrelated request that reused an id.
func (b *Broker) cancelLlamaWorkload(msg *Message) {
	var params struct {
		ID     string `json:"id"`
		RunID  string `json:"runId"`
		Engine string `json:"engine"`
		Origin string `json:"originatedFrom"`
	}
	if json.Unmarshal(msg.Params, &params) != nil || params.ID == "" || params.RunID == "" {
		_ = b.codec.RespondError(msg.ID, -32602, "exact workload id and runId are required")
		return
	}
	if params.Engine != llamacppProxyProfile.Name || params.Origin == "" || params.Origin != b.nodeID {
		_ = b.codec.RespondError(msg.ID, -32000, "only llama.cpp requests originating on this node can be cancelled here")
		return
	}
	forward := *msg
	forward.Method = llamacppProxyProfile.ComponentName() + ":workload/cancel"
	b.relayToEngineProxy(llamacppProxyProfile, &forward)
}
