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

const llamaCppProxyFacadePort = 8081

func (b *Broker) setLlamaCppProxy(p *proxyProcess) {
	b.workersMu.Lock()
	b.llamacppProxy = p
	b.workersMu.Unlock()
}

func (b *Broker) getLlamaCppProxy() *proxyProcess {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.llamacppProxy
}

func (b *Broker) configureLlamaCppProxySupervisorCallbacks(sup *supervisor) {
	sup.onCrash, sup.onRecovered = b.supervisedWorkerCallbacks(
		"llamacpp-proxy",
		func() {
			b.setLlamaCppProxy(nil)
			b.unregisterService(noderec.ServiceLlamaCpp)
		},
	)
	sup.onExhausted = func(attempt int) {
		slog.Warn(
			"llamacpp-proxy is terminally unavailable",
			"attempt", attempt,
		)
		b.unregisterService(noderec.ServiceLlamaCpp)
	}
}

func (b *Broker) llamacppProxyArgs() []string {
	args := []string{
		"--port",
		fmt.Sprintf("%d", llamaCppProxyFacadePort),
		"--ignore-persisted-port",
	}
	return append(args, b.clusterDirArgs()...)
}

func (b *Broker) spawnLlamaCppProxy() (supervisedHandle, error) {
	pp, err := startProxy(
		"llamacpp-proxy",
		b.llamacppProxyPath,
		applog.LevelString(),
		b.relayDir,
		b.forwardLlamaCppProxyNotification,
		b.llamacppProxyArgs()...,
	)
	if err != nil {
		return nil, err
	}

	b.setLlamaCppProxy(pp)

	if ready, _ := pp.Status(); ready {
		go b.repushPriority("llamacpp")
	}

	slog.Info(
		"llamacpp-proxy started",
		"path", b.llamacppProxyPath,
		"pid", pp.cmd.Process.Pid,
	)
	return pp, nil
}

func (b *Broker) forwardLlamaCppProxyNotification(
	method string,
	params json.RawMessage,
) {
	if b.dispatchErrorsNotif("llamacpp-proxy", method, params) {
		return
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
		go b.repushPriority("llamacpp")
	}

	b.proxyMu.Lock()
	subscribed := b.llamacppProxySubscribed
	b.proxyMu.Unlock()
	if !subscribed {
		return
	}

	if err := b.codec.Notify("llamacpp-proxy:"+method, params); err != nil {
		slog.Warn(
			"forward llamacpp-proxy notification failed",
			"method", method,
			"err", err,
		)
	}
}

func (b *Broker) relayToLlamaCppProxy(msg *Message) {
	method := strings.TrimPrefix(msg.Method, "llamacpp-proxy:")
	if method == "shutdown" {
		if err := b.codec.RespondError(
			msg.ID,
			-32601,
			"llamacpp-proxy:shutdown is not allowed; the broker owns the proxy lifecycle",
		); err != nil {
			log.Printf(
				"failed to respond to llamacpp-proxy:shutdown: %v",
				err,
			)
		}
		return
	}

	p := b.getLlamaCppProxy()
	if p == nil {
		if err := b.codec.RespondError(
			msg.ID,
			-32000,
			"llamacpp-proxy not available",
		); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
		return
	}

	result, rpcErr, err := p.Call(context.Background(), method, msg.Params)
	switch {
	case err != nil:
		if responseErr := b.codec.RespondError(
			msg.ID,
			-32000,
			fmt.Sprintf("llamacpp-proxy call failed: %v", err),
		); responseErr != nil {
			log.Printf(
				"failed to respond to %s: %v",
				msg.Method,
				responseErr,
			)
		}
	case rpcErr != nil:
		if responseErr := b.codec.RespondError(
			msg.ID,
			rpcErr.Code,
			rpcErr.Message,
		); responseErr != nil {
			log.Printf(
				"failed to relay llamacpp-proxy error for %s: %v",
				msg.Method,
				responseErr,
			)
		}
	default:
		if responseErr := b.codec.Respond(msg.ID, result); responseErr != nil {
			log.Printf(
				"failed to relay llamacpp-proxy result for %s: %v",
				msg.Method,
				responseErr,
			)
		}
	}
}
