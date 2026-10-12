// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log/slog"
)

// lmstudioproxy.go is the broker's LM Studio counterpart to its ollama-proxy
// wiring (proxy.go / the proxy:* handlers in broker.go). lmstudio-proxy speaks
// the exact same JSON-RPC control plane as ollama-proxy (a "ready" port
// notification, node/add-manual/remove-manual, nodes/list, node/select, the
// workload:* lifecycle stream), so it reuses the proxyProcess client type;
// only the namespace differs — the broker relays it under lmstudio-proxy:
// instead of proxy:. Owning it here is what lets the broker bridge a reachable
// manual LM Studio node into routing, the same way it does for Ollama.

func (b *Broker) setLMStudioProxy(p *proxyProcess) {
	b.setEngineProxyHandle(lmstudioProxyProfile, p)
}

func (b *Broker) getLMStudioProxy() *proxyProcess {
	return b.engineProxyHandle(lmstudioProxyProfile)
}

func (b *Broker) finishLMStudioProxyTerminal() {
	b.lmstudioState().managedFacade.Store(false)
	b.markLMStudioPortReady()
}

// lmstudioFacadeSpec is the enable request for LM Studio's facade, mirroring
// ollamaFacadeSpec. It has no alias addresses: the alias stands in for an
// inherited host variable, which only Ollama has.
func (b *Broker) lmstudioFacadeSpec() enableFacadeRequest {
	spec := enableFacadeRequest{Engine: lmstudioProxyProfile.Name}
	if port := int(b.lmstudioState().startupPort.Load()); port != 0 {
		spec.Port = port
		spec.IgnorePersistedPort = true
	}
	return spec
}

// lmstudioFallbackPort mirrors ollamaFallbackPort: prefer the port the
// bind-failed notification already chose, since that path also decides whether
// the managed facade stays in play, and recompute only if it did not run.
func (b *Broker) lmstudioFallbackPort(failed int) int {
	if planned := int(b.lmstudioState().startupPort.Load()); planned != 0 && planned != failed {
		return planned
	}
	return b.setLMStudioProxyFallback(failed)
}

// forwardLMStudioProxyNotification is the hook startProxy invokes on the
// lmstudio-proxy reader goroutine. It mirrors forwardProxyNotification:
// errors:report / errors:clear go into the nvpair-errors pipeline; workload
// lifecycle events are stamped and forwarded to the workload-manager for
// cluster broadcast (lmstudio-proxy tags its workloads "lmstudio"); everything
// else is re-emitted to lmstudio-proxy:subscribe'd clients as
// lmstudio-proxy:<method>. Readiness reconciliation runs on its own goroutine
// because its set-port/local-backend calls round-trip through this reader.
func (b *Broker) forwardLMStudioProxyNotification(method string, params json.RawMessage) {
	b.forwardLMStudioProxyNotificationForGeneration(b.lmstudioProxyGeneration.Load(), method, params)
}

func (b *Broker) forwardLMStudioProxyNotificationForGeneration(generation uint64, method string, params json.RawMessage) {
	if b.lmstudioProxyGeneration.Load() != generation {
		return
	}
	// Strip the facade address before anything dispatches on the method,
	// starting with the errors relay directly below: it matches bare names.
	method, addressed := facadeMethodFor(lmstudioProxyProfile, method)
	if !addressed {
		return
	}
	if b.dispatchErrorsNotif(lmstudioProxyProfile.ComponentName(), method, params) {
		return
	}
	// A process can win :1234 after preparation's free-port check but before
	// the facade binds. Choose an explicit fallback here, without calling back
	// into this reader goroutine.
	//
	// The failed process is not exiting any more: this notification precedes
	// the failing enable's response, so the port chosen here is what
	// lmstudioFallbackPort finds when that enable retries in-process.
	if method == "error" {
		var ep struct {
			Code string `json:"code"`
			Port int    `json:"port"`
		}
		if json.Unmarshal(params, &ep) == nil && ep.Code == "bind-failed" && !b.lmstudioState().explicitSettings.Load() {
			if b.lmstudioState().managedFacade.Load() && ep.Port == managedLMStudioFacadePort {
				_, _ = b.blockManagedLMStudioFacade("another process acquired the compatibility port during startup", nil)
			} else {
				fallback := b.setLMStudioProxyFallback(ep.Port)
				slog.Warn("LM Studio proxy bind failed; retrying on fallback", "port", ep.Port, "fallback", fallback)
			}
		}
	}
	if method == "ready" {
		var rp proxyReadyParams
		if err := json.Unmarshal(params, &rp); err == nil && rp.Port > 0 {
			go b.reconcileLMStudioProxyPortOnReadyForGeneration(generation, rp.Port)
		}
	}
	b.forwardEngineProxyNotification(lmstudioProxyProfile, method, params)
}
