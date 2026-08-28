// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"strings"

	"nvpair-shared/applog"
	"nvpair-shared/noderec"
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
	b.workersMu.Lock()
	b.lmstudioProxy = p
	b.workersMu.Unlock()
}

func (b *Broker) getLMStudioProxy() *proxyProcess {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.lmstudioProxy
}

func (b *Broker) finishLMStudioProxyTerminal() {
	b.managedLMStudioFacade.Store(false)
	b.markLMStudioPortReady()
}

func (b *Broker) configureLMStudioProxySupervisorCallbacks(sup *supervisor) {
	sup.onCrash, sup.onRecovered = b.supervisedWorkerCallbacks("lmstudio-proxy", func() { b.setLMStudioProxy(nil) })
	sup.onExhausted = func(attempt int) {
		slog.Warn("lmstudio-proxy is terminally unavailable; releasing ownership gate", "attempt", attempt)
		b.finishLMStudioProxyTerminal()
	}
}

func (b *Broker) lmstudioProxyArgs() []string {
	var args []string
	if port := int(b.lmstudioProxyStartupPort.Load()); port != 0 {
		args = []string{"--port", fmt.Sprintf("%d", port), "--ignore-persisted-port"}
	}
	return append(args, b.clusterDirArgs()...)
}

// spawnLMStudioProxy is the lmstudio-proxy supervisor's spawn closure,
// mirroring spawnProxy. It reuses startProxy because the two proxies share a
// binary protocol.
func (b *Broker) spawnLMStudioProxy() (supervisedHandle, error) {
	b.lmstudioReadyMu.Lock()
	generation := b.lmstudioProxyGeneration.Add(1)
	b.lmstudioReadyMu.Unlock()
	// Thread the cluster dir so the LM Studio proxy brings up its pin-gated LAN
	// mTLS ingress (and dials peers over mTLS) once this node is clustered.
	pp, err := startProxy(
		"lmstudio-proxy",
		b.lmstudioProxyPath,
		applog.LevelString(),
		b.relayDir,
		func(method string, params json.RawMessage) {
			b.forwardLMStudioProxyNotificationForGeneration(generation, method, params)
		},
		b.lmstudioProxyArgs()...,
	)
	if err != nil {
		return nil, err
	}
	b.setLMStudioProxy(pp)
	b.lmstudioProxyPublishedGeneration.Store(generation)
	// A fast child can announce ready before its handle is published. Replay
	// reconciliation after publication; the gate's sync.Once makes this safe
	// when the notification goroutine already handled it.
	if ready, port := pp.Status(); ready && port > 0 {
		go b.reconcileLMStudioProxyPortOnReadyForGeneration(generation, port)
	}
	slog.Info("lmstudio-proxy started", "path", b.lmstudioProxyPath, "pid", pp.cmd.Process.Pid)
	return pp, nil
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
	if b.dispatchErrorsNotif("lmstudio-proxy", method, params) {
		return
	}
	// A process can win :1234 after preparation's free-port check but before
	// the proxy binds. The failed process is exiting, so set the next spawn to
	// an explicit fallback without calling back into this reader goroutine.
	if method == "error" {
		var ep struct {
			Code string `json:"code"`
			Port int    `json:"port"`
		}
		if json.Unmarshal(params, &ep) == nil && ep.Code == "bind-failed" {
			if b.managedLMStudioFacade.Load() && ep.Port == managedLMStudioFacadePort {
				_, _ = b.blockManagedLMStudioFacade("another process acquired the compatibility port during startup", nil)
			} else {
				fallback := b.setLMStudioProxyFallback(ep.Port)
				slog.Warn("LM Studio proxy bind failed; retrying on fallback", "port", ep.Port, "fallback", fallback)
			}
		}
	}
	if proxyWorkloadMethods[method] {
		b.routeProxyWorkload(method, params)
		return
	}
	if method == noderec.NotifyNodeActivity {
		b.routeNodeActivity(params)
		return
	}
	if method == "ready" {
		var rp proxyReadyParams
		if err := json.Unmarshal(params, &rp); err == nil && rp.Port > 0 {
			go b.reconcileLMStudioProxyPortOnReadyForGeneration(generation, rp.Port)
		}
	}
	b.proxyMu.Lock()
	subscribed := b.lmstudioProxySubscribed
	b.proxyMu.Unlock()
	if !subscribed {
		return
	}
	if err := b.codec.Notify("lmstudio-proxy:"+method, params); err != nil {
		slog.Warn("forward lmstudio-proxy notification failed", "method", method, "err", err)
	}
}

// relayToLMStudioProxy forwards an lmstudio-proxy:<method> request to
// lmstudio-proxy as <method> (prefix stripped) and maps its response straight
// back, mirroring relayToProxy. lmstudio-proxy:shutdown is refused — the broker
// owns the proxy's lifecycle.
func (b *Broker) relayToLMStudioProxy(msg *Message) {
	method := strings.TrimPrefix(msg.Method, "lmstudio-proxy:")
	if method == "shutdown" {
		if err := b.codec.RespondError(msg.ID, -32601, "lmstudio-proxy:shutdown is not allowed; the broker owns the proxy lifecycle"); err != nil {
			log.Printf("failed to respond to lmstudio-proxy:shutdown: %v", err)
		}
		return
	}

	p := b.getLMStudioProxy()
	if p == nil {
		if err := b.codec.RespondError(msg.ID, -32000, "lmstudio-proxy not available"); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
		return
	}

	result, rpcErr, err := p.Call(context.Background(), method, msg.Params)
	switch {
	case err != nil:
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("lmstudio-proxy call failed: %v", err)); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
	case rpcErr != nil:
		if err := b.codec.RespondError(msg.ID, rpcErr.Code, rpcErr.Message); err != nil {
			log.Printf("failed to relay lmstudio-proxy error for %s: %v", msg.Method, err)
		}
	default:
		if err := b.codec.Respond(msg.ID, result); err != nil {
			log.Printf("failed to relay lmstudio-proxy result for %s: %v", msg.Method, err)
		}
	}
}
