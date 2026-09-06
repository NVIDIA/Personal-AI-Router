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

// llamacppproxy.go is the broker's llama.cpp counterpart to its lmstudio-proxy
// wiring. llamacpp-proxy speaks the same JSON-RPC control plane as the other
// engine proxies (a "ready" port notification, node/add-manual/remove-manual,
// nodes/list, node/select, the workload:* lifecycle stream), so it reuses the
// proxyProcess client type; only the namespace differs — the broker relays it
// under llamacpp-proxy:. There is no managed facade and PAIR never binds the
// engine's stock :8082; the proxy listens on :8084 (or a persisted port).

func (b *Broker) setLlamaCppProxy(p *proxyProcess) {
	b.workersMu.Lock()
	b.llamaCppProxy = p
	b.workersMu.Unlock()
}

func (b *Broker) getLlamaCppProxy() *proxyProcess {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.llamaCppProxy
}

func (b *Broker) configureLlamaCppProxySupervisorCallbacks(sup *supervisor) {
	sup.onCrash, sup.onRecovered = b.supervisedWorkerCallbacks("llamacpp-proxy", func() { b.setLlamaCppProxy(nil) })
}

func (b *Broker) llamaCppProxyArgs() []string {
	var args []string
	if port := int(b.llamaCppProxyStartupPort.Load()); port != 0 {
		args = []string{"--port", fmt.Sprintf("%d", port), "--ignore-persisted-port"}
	}
	return append(args, b.clusterDirArgs()...)
}

// spawnLlamaCppProxy is the llamacpp-proxy supervisor's spawn closure,
// mirroring spawnLMStudioProxy without the managed-facade port reconcile.
func (b *Broker) spawnLlamaCppProxy() (supervisedHandle, error) {
	generation := b.llamaCppProxyGeneration.Add(1)
	pp, err := startProxy(
		"llamacpp-proxy",
		b.llamaCppProxyPath,
		applog.LevelString(),
		b.relayDir,
		func(method string, params json.RawMessage) {
			b.forwardLlamaCppProxyNotificationForGeneration(generation, method, params)
		},
		b.llamaCppProxyArgs()...,
	)
	if err != nil {
		return nil, err
	}
	b.setLlamaCppProxy(pp)
	slog.Info("llamacpp-proxy started", "path", b.llamaCppProxyPath, "pid", pp.cmd.Process.Pid)
	return pp, nil
}

// forwardLlamaCppProxyNotification is the hook startProxy invokes on the
// llamacpp-proxy reader goroutine. It mirrors forwardLMStudioProxyNotification
// without the managed-facade bind-failed dance: errors:report / errors:clear
// go into the nvpair-errors pipeline; workload lifecycle events are stamped
// and forwarded to the workload-manager (llamacpp-proxy tags its workloads
// "llamacpp"); everything else is re-emitted to llamacpp-proxy:subscribe'd
// clients as llamacpp-proxy:<method>.
func (b *Broker) forwardLlamaCppProxyNotification(method string, params json.RawMessage) {
	b.forwardLlamaCppProxyNotificationForGeneration(b.llamaCppProxyGeneration.Load(), method, params)
}

func (b *Broker) forwardLlamaCppProxyNotificationForGeneration(generation uint64, method string, params json.RawMessage) {
	if b.llamaCppProxyGeneration.Load() != generation {
		return
	}
	if b.dispatchErrorsNotif("llamacpp-proxy", method, params) {
		return
	}
	if method == "error" {
		var ep struct {
			Code string `json:"code"`
			Port int    `json:"port"`
		}
		if json.Unmarshal(params, &ep) == nil && ep.Code == "bind-failed" {
			slog.Warn("llama.cpp proxy bind failed", "port", ep.Port)
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
	b.proxyMu.Lock()
	subscribed := b.llamaCppProxySubscribed
	b.proxyMu.Unlock()
	if !subscribed {
		return
	}
	if err := b.codec.Notify("llamacpp-proxy:"+method, params); err != nil {
		slog.Warn("forward llamacpp-proxy notification failed", "method", method, "err", err)
	}
}

// relayToLlamaCppProxy forwards an llamacpp-proxy:<method> request to
// llamacpp-proxy as <method> (prefix stripped) and maps its response straight
// back, mirroring relayToLMStudioProxy. llamacpp-proxy:shutdown is refused —
// the broker owns the proxy's lifecycle.
func (b *Broker) relayToLlamaCppProxy(msg *Message) {
	method := strings.TrimPrefix(msg.Method, "llamacpp-proxy:")
	if method == "shutdown" {
		if err := b.codec.RespondError(msg.ID, -32601, "llamacpp-proxy:shutdown is not allowed; the broker owns the proxy lifecycle"); err != nil {
			log.Printf("failed to respond to llamacpp-proxy:shutdown: %v", err)
		}
		return
	}

	p := b.getLlamaCppProxy()
	if p == nil {
		if err := b.codec.RespondError(msg.ID, -32000, "llamacpp-proxy not available"); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
		return
	}

	result, rpcErr, err := p.Call(context.Background(), method, msg.Params)
	switch {
	case err != nil:
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("llamacpp-proxy call failed: %v", err)); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
	case rpcErr != nil:
		if err := b.codec.RespondError(msg.ID, rpcErr.Code, rpcErr.Message); err != nil {
			log.Printf("failed to relay llamacpp-proxy error for %s: %v", msg.Method, err)
		}
	default:
		if err := b.codec.Respond(msg.ID, result); err != nil {
			log.Printf("failed to relay llamacpp-proxy result for %s: %v", msg.Method, err)
		}
	}
}
