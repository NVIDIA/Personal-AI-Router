// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"nvpair-shared/applog"
	"nvpair-shared/noderec"
)

// mlxproxy.go is the broker's MLX wiring, the third sibling of proxy.go
// (ollama-proxy) and lmstudioproxy.go (lmstudio-proxy). mlx-proxy speaks the
// same JSON-RPC control plane as both, so it reuses proxyProcess; only the
// namespace differs — relayed under mlx-proxy: rather than proxy: or
// lmstudio-proxy:.
//
// What is deliberately absent is the managed-facade machinery in
// lmstudioport.go. That exists because LM Studio's server and its proxy both
// want :1234, so the broker has to move a running engine off the compatibility
// port before the proxy can take it. MLX has no such collision: PAIR starts
// mlx_lm.server with an explicit --port from the manifest (8081), leaving the
// proxy the conventional :8080 that clients already point at. No port to
// reclaim means no ownership gate, no fallback ladder, and no rebind dance.

// defaultMLXPort is mlx_lm.server's stock port. It is only the fallback used
// when engine-manager cannot say where the engine actually is; the manifest's
// runtime.port (8081) is the real answer, and the proxy owns 8080.
const defaultMLXPort = 8081

// mlxProxyFallbackStart is where the search for a replacement listener begins
// when mlx-proxy's preferred port is taken. mlx-proxy defaults to :8080 because
// that is mlx_lm.server's documented port, so a client already pointed at a
// local MLX server is routed with no reconfiguration -- but :8080 is also the
// most contended port on a developer's machine (Docker Desktop and any number
// of dev servers claim it), so the fallback is not an edge case, it is the
// common case. Starting at 8090 keeps the replacement recognisably in the MLX
// range rather than scattering it into ephemeral territory.
const mlxProxyFallbackStart = 8090

func (b *Broker) setMLXProxy(p *proxyProcess) {
	b.workersMu.Lock()
	b.mlxProxy = p
	b.workersMu.Unlock()
}

func (b *Broker) getMLXProxy() *proxyProcess {
	b.workersMu.Lock()
	defer b.workersMu.Unlock()
	return b.mlxProxy
}

func (b *Broker) configureMLXProxySupervisorCallbacks(sup *supervisor) {
	report, recovered := b.supervisedWorkerCallbacks("mlx-proxy", func() { b.setMLXProxy(nil) })
	// A process that exits after reporting bind-failed did not crash: it told us
	// why it was leaving, and the next spawn already has a free port. Reporting
	// that as "subprocess mlx-proxy exited unexpectedly" puts a red error in the
	// UI on every launch, because :8080 being taken is the normal case for this
	// engine rather than a fault. The error does clear itself after the
	// supervisor's healthy-reset window, but a minute of looking broken on first
	// launch is exactly the wrong first impression.
	//
	// Only the one exit we were told about is swallowed -- the flag is consumed
	// here, so a genuine crash on the next spawn reports normally.
	sup.onCrash = func(attempt int) {
		if b.mlxProxyRebinding.Swap(false) {
			b.setMLXProxy(nil)
			slog.Info("mlx-proxy exited to rebind after a port conflict; not reporting a crash", "attempt", attempt)
			return
		}
		report(attempt)
	}
	sup.onRecovered = recovered
	sup.onExhausted = func(attempt int) {
		slog.Warn("mlx-proxy is terminally unavailable", "attempt", attempt)
	}
}

// spawnMLXProxy is the mlx-proxy supervisor's spawn closure. Unlike its two
// siblings it passes no startup port: with no facade to reserve there is
// nothing to override, so the proxy uses its own persisted-or-default port.
func (b *Broker) spawnMLXProxy() (supervisedHandle, error) {
	generation := b.mlxProxyGeneration.Add(1)
	pp, err := startProxy(
		"mlx-proxy",
		b.mlxProxyPath,
		applog.LevelString(),
		b.relayDir,
		func(method string, params json.RawMessage) {
			b.forwardMLXProxyNotificationForGeneration(generation, method, params)
		},
		b.mlxProxyArgs()...,
	)
	if err != nil {
		return nil, err
	}
	b.setMLXProxy(pp)
	slog.Info("mlx-proxy started", "path", b.mlxProxyPath, "pid", pp.cmd.Process.Pid)
	return pp, nil
}

// mlxProxyArgs passes an explicit port only after a bind failure has forced one.
// On the first spawn the proxy chooses for itself (its persisted port, else
// :8080), which is what makes the default the compatibility value rather than
// something the broker imposes.
func (b *Broker) mlxProxyArgs() []string {
	var args []string
	if port := int(b.mlxProxyStartupPort.Load()); port != 0 {
		args = []string{"--port", fmt.Sprintf("%d", port), "--ignore-persisted-port"}
	}
	return append(args, b.clusterDirArgs()...)
}

// setMLXProxyFallback picks the next free port for the following spawn after a
// bind failure. The failed port is excluded explicitly: a process that owns
// :8080 is unlikely to release it between one spawn and the next, and without
// the exclusion the search would hand back the same port and crash-loop.
func (b *Broker) setMLXProxyFallback(excludedPorts ...int) int {
	fallback := nextAvailablePortExcluding(mlxProxyFallbackStart, excludedPorts, tcpPortAvailable)
	b.mlxProxyStartupPort.Store(int32(fallback))
	return fallback
}

// forwardMLXProxyNotification mirrors forwardLMStudioProxyNotification:
// errors:report / errors:clear go into the nvpair-errors pipeline; workload
// lifecycle events are stamped and forwarded to the workload-manager for
// cluster broadcast (mlx-proxy tags its workloads "mlx"); everything else is
// re-emitted to mlx-proxy:subscribe'd clients as mlx-proxy:<method>.
func (b *Broker) forwardMLXProxyNotification(method string, params json.RawMessage) {
	b.forwardMLXProxyNotificationForGeneration(b.mlxProxyGeneration.Load(), method, params)
}

func (b *Broker) forwardMLXProxyNotificationForGeneration(generation uint64, method string, params json.RawMessage) {
	if b.mlxProxyGeneration.Load() != generation {
		return
	}
	if b.dispatchErrorsNotif("mlx-proxy", method, params) {
		return
	}
	// A bind failure is reported by the exiting process, so the fix has to land
	// on the NEXT spawn rather than on this one. Without it the supervisor
	// restarts the proxy onto the same taken port forever.
	if method == "error" {
		var ep struct {
			Code string `json:"code"`
			Port int    `json:"port"`
		}
		if json.Unmarshal(params, &ep) == nil && ep.Code == "bind-failed" {
			fallback := b.setMLXProxyFallback(ep.Port)
			// Consumed by the supervisor's onCrash: this exit is expected.
			b.mlxProxyRebinding.Store(true)
			slog.Warn("MLX proxy bind failed; retrying on fallback", "port", ep.Port, "fallback", fallback)
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
	subscribed := b.mlxProxySubscribed
	b.proxyMu.Unlock()
	if !subscribed {
		return
	}
	if err := b.codec.Notify("mlx-proxy:"+method, params); err != nil {
		slog.Warn("forward mlx-proxy notification failed", "method", method, "err", err)
	}
}

// relayToMLXProxy forwards an mlx-proxy:<method> request to mlx-proxy as
// <method> (prefix stripped) and maps its response straight back, mirroring
// relayToLMStudioProxy. mlx-proxy:shutdown is refused — the broker owns the
// proxy's lifecycle.
func (b *Broker) relayToMLXProxy(msg *Message) {
	method := strings.TrimPrefix(msg.Method, "mlx-proxy:")
	if method == "shutdown" {
		if err := b.codec.RespondError(msg.ID, -32601, "mlx-proxy:shutdown is not allowed; the broker owns the proxy lifecycle"); err != nil {
			log.Printf("failed to respond to mlx-proxy:shutdown: %v", err)
		}
		return
	}

	p := b.getMLXProxy()
	if p == nil {
		if err := b.codec.RespondError(msg.ID, -32000, "mlx-proxy not available"); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
		return
	}

	result, rpcErr, err := p.Call(context.Background(), method, msg.Params)
	switch {
	case err != nil:
		if err := b.codec.RespondError(msg.ID, -32000, fmt.Sprintf("mlx-proxy call failed: %v", err)); err != nil {
			log.Printf("failed to respond to %s: %v", msg.Method, err)
		}
	case rpcErr != nil:
		if err := b.codec.RespondError(msg.ID, rpcErr.Code, rpcErr.Message); err != nil {
			log.Printf("failed to relay mlx-proxy error for %s: %v", msg.Method, err)
		}
	default:
		if err := b.codec.Respond(msg.ID, result); err != nil {
			log.Printf("failed to relay mlx-proxy result for %s: %v", msg.Method, err)
		}
	}
}

// runAutoAdvertiseMLX is the MLX sibling of runAutoAdvertise / …LMStudio: it
// polls the local mlx_lm.server and reconciles this node's mx registration
// against it, so an MLX host appears on the cluster the same way the other two
// do.
func (b *Broker) runAutoAdvertiseMLX(ctx context.Context) {
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(autoAdvertiseInterval)
	defer ticker.Stop()

	b.reconcileAdvertiseMLX(client)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.reconcileAdvertiseMLX(client)
		}
	}
}

// reconcileAdvertiseMLX advertises the proxy port (never the engine) as this
// node's mx service and hands the engine's loopback port to mlx-proxy via
// node/set-local-backend. Simpler than the LM Studio reconciler by exactly the
// facade cache it does not need: there is no managed port to reclaim, so a
// stock-port fallback here can never poison a backend cache, and the only guard
// left is the one that matters — never hand the proxy its own listener.
func (b *Broker) reconcileAdvertiseMLX(client *http.Client) {
	enginePort, probe := b.localEnginePort("mlx", defaultMLXPort)
	proxyPort := b.mlxProxyListenPort()
	up := probe && proxyPort != 0 && enginePort != proxyPort && checkMLXHealth(client, enginePort)
	if up {
		b.registerService(noderec.RegisterParams{Service: noderec.ServiceMLX, Port: proxyPort})
		b.setProxyLocalBackend(b.getMLXProxy(), "mlx", enginePort, true)
		return
	}
	b.unregisterService(noderec.ServiceMLX)
	b.setProxyLocalBackend(b.getMLXProxy(), "mlx", enginePort, false)
}

func (b *Broker) mlxProxyListenPort() int {
	if p := b.getMLXProxy(); p != nil {
		if ready, port := p.Status(); ready {
			return port
		}
	}
	return 0
}

// checkMLXHealth reports whether a local mlx_lm.server is answering. /health is
// used rather than /v1/models because it is the endpoint that also reports
// which model is resident, and because /v1/models walks the whole Hugging Face
// cache on every call — too expensive for a liveness poll on a machine holding
// tens of gigabytes of weights.
func checkMLXHealth(client *http.Client, port int) bool {
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/health", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
