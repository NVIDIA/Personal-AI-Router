// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"nvpair-shared/noderec"
)

// An engine's stock port is its FacadePort in the engine table, used ONLY as a
// fallback when engine-manager cannot report the real one. Never hardcode the
// advertise or health port: the product-default proxy takes Ollama's 11434, so
// a fixed 11434 would advertise the proxy as Ollama and make the proxy — and
// peers — self-forward into a loop. The real port is resolved per poll via
// localEnginePort.
var (
	defaultOllamaPort   = ollamaProxyProfile.FacadePort
	defaultLMStudioPort = lmstudioProxyProfile.FacadePort
)

const (
	// engineManagerHTTPPort is the fixed LAN port the broker tells
	// nvpair-engine-manager to serve its HTTP surface (/v1/models) on, and the port
	// it registers as the em service so peers' daemons can fetch this node's
	// model list. Fixed like node-info's :14318 (next free in the 143xx range);
	// the broker knows it, so no dynamic port handshake is needed.
	engineManagerHTTPPort = 14322

	// engineControlPort is the fixed LAN port the broker tells
	// nvpair-engine-manager to serve its cluster-scoped mTLS remote-control surface
	// (the ec service: remote install/pull/start/stop + engine status) on. Unlike
	// em (plain, model list) it's pin-based mTLS and only binds when this node is
	// clustered. Next free after em in the 143xx range.
	engineControlPort = 14323

	// autoAdvertiseInterval is how often the broker polls a local engine to
	// decide whether to register it with (or unregister it from) the discovery
	// daemon (a 5s cadence).
	autoAdvertiseInterval = 5 * time.Second
)

// runAutoAdvertise is the broker's ollama engine-registration loop. It polls
// the local ollama server on a fixed cadence and reconciles this node's ol
// service registration in the discovery daemon against it: register (with the
// served model list) when ollama is up, unregister when it goes away. The
// daemon folds the registration into this node's single _nvpair-node record, so
// peers discover the engine through the shared channel. Runs until ctx is
// cancelled (broker shutdown).
//
// (Pre-cutover this loop also spawned a nvpair-advertiser subprocess to publish an
// _nvpair-ollama record; that per-service advertisement was retired when the
// discovery consolidation landed and the binary was deleted.)
func (b *Broker) runAutoAdvertise(ctx context.Context) {
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(autoAdvertiseInterval)
	defer ticker.Stop()

	b.reconcileAdvertise(client)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.reconcileAdvertise(client)
		}
	}
}

// reconcileAdvertise checks local ollama and brings this node's ol registration
// into line with it. Post-secure-inference the advertised ol endpoint is the
// promoted PROXY port, never the engine port: peers dial the proxy over cluster
// mTLS and it forwards to the loopback engine. The engine's real (loopback) port
// is a private detail handed only to the local proxy via node/set-local-backend.
//
//   - engine healthy + proxy up -> register {ol, PROXY port} + set-local-backend{enginePort, healthy}
//   - otherwise                  -> unregister ol + clear the proxy's local backend
//
// The model list is not carried here — it lives on engine-manager's em
// /v1/models endpoint (registered separately), which peers fetch during
// enrichment.
func (b *Broker) reconcileAdvertise(client *http.Client) {
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()
	// During the managed bind -> backend-move transition, engine:status would
	// probe :11434 and could mistake the proxy (or a remote response forwarded
	// through it) for an externally started Ollama. Do not query liveness until
	// the backend has moved or managed setup has safely fallen back.
	if b.ollamaFacadeIsPendingBackend() {
		b.unregisterService(noderec.ServiceOllama)
		b.setProxyLocalBackend(b.getProxy(), "ollama", 0, false)
		return
	}
	enginePort, probe := b.localEnginePort("ollama", defaultOllamaPort)
	proxyPort := b.proxyListenPort()
	// Advertise only when the engine is healthy AND the proxy is up AND the two
	// ports differ. Equal ports mean we can't tell the engine from the proxy
	// (or there is no separate engine), and setting the local backend to the
	// proxy's own port would make the ingress forward to itself.
	up := probe && proxyPort != 0 && enginePort != proxyPort && checkEngineHealth(ollamaProxyProfile, client, enginePort)
	if up {
		b.registerService(noderec.RegisterParams{Service: noderec.ServiceOllama, Port: proxyPort})
		b.setProxyLocalBackend(b.getProxy(), "ollama", enginePort, true)
	} else {
		b.unregisterService(noderec.ServiceOllama)
		b.setProxyLocalBackend(b.getProxy(), "ollama", enginePort, false)
	}
}

func (b *Broker) ollamaFacadeIsPendingBackend() bool {
	if b.managedOllamaBackend.Load() != 0 || b.ollamaMoveInFlight.Load() {
		return true
	}
	if int(b.ollamaState().backendPort.Load()) != managedOllamaFacadePort {
		return false
	}
	// Recovery flips managed mode off before it live-rebinds the proxy away
	// from :11434. Keep probes gated through that interval (and indefinitely if
	// the rebind fails) so the proxy can never be adopted as Ollama.
	return b.ollamaState().managedFacade.Load() || b.proxyListenPort() == managedOllamaFacadePort
}

// runAutoAdvertiseLMStudio is the LM Studio sibling of runAutoAdvertise: it
// polls the local LM Studio server and reconciles this node's lm service
// registration against it, so an LM Studio host appears on the cluster the same
// way an Ollama host does. Kept parallel to the Ollama path rather than folded
// into it: the two are a deliberate temporary pair, to be unified when the
// proxies are.
func (b *Broker) runAutoAdvertiseLMStudio(ctx context.Context) {
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(autoAdvertiseInterval)
	defer ticker.Stop()

	b.reconcileAdvertiseLMStudio(client)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.reconcileAdvertiseLMStudio(client)
		}
	}
}

// reconcileAdvertiseLMStudio brings this node's lm registration into line with
// the local LM Studio server, mirroring reconcileAdvertise: it advertises the
// promoted proxy port (never the engine) and hands the engine's loopback port to
// the LM Studio proxy via node/set-local-backend.
func (b *Broker) reconcileAdvertiseLMStudio(client *http.Client) {
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()
	enginePort, probe := b.localEnginePort("lmstudio", defaultLMStudioPort)
	proxyPort := b.lmstudioProxyListenPort()
	if proxyPort != 0 && enginePort == proxyPort {
		// engine-manager may be temporarily unavailable after managed setup.
		// Prefer the last confirmed backend, but never hand the proxy its own
		// listener as a local destination.
		if cached := int(b.lmstudioState().backendPort.Load()); cached > 0 && cached != proxyPort {
			enginePort = cached
		} else {
			enginePort = 0
			probe = false
		}
	}
	// enginePort may be a stock-port fallback: localEnginePort returns one when
	// engine-manager is unavailable, and it is indistinguishable from a real
	// status here. Use it to advertise this tick only; never write it to
	// lmstudioBackendPort. That cache's authoritative owners are the managed
	// facade setup and live engine:status. Promoting the fallback poisons the
	// cache while the proxy and engine restart together (as on the first invite),
	// which later makes the compatibility proxy on the facade port look like the
	// backend and wrongly disables managed mode.
	up := probe && proxyPort != 0 && enginePort != proxyPort && checkEngineHealth(lmstudioProxyProfile, client, enginePort)
	if up {
		b.registerService(noderec.RegisterParams{Service: noderec.ServiceLMStudio, Port: proxyPort})
		b.setProxyLocalBackend(b.getLMStudioProxy(), "lmstudio", enginePort, true)
	} else {
		b.unregisterService(noderec.ServiceLMStudio)
		b.setProxyLocalBackend(b.getLMStudioProxy(), "lmstudio", enginePort, false)
	}
}

// proxyLocalBackend is the node/set-local-backend payload: the loopback engine
// the proxy's cluster mTLS ingress forwards to, and the proxy's own self
// candidate on the local routing path.
type proxyLocalBackend struct {
	Engine  string `json:"engine"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Healthy bool   `json:"healthy"`
}

// setProxyLocalBackend hands the proxy its local (loopback) engine endpoint, or
// clears it (healthy=false) when the engine is down / unresolved. Best-effort
// and idempotent — re-sent on every reconcile so a freshly (re)spawned proxy
// re-learns its backend within one poll interval. A nil proxy is a no-op.
func (b *Broker) setProxyLocalBackend(p *proxyProcess, engine string, port int, healthy bool) {
	if p == nil {
		return
	}
	b.callProxyManual(p, engine, "node/set-local-backend", proxyLocalBackend{
		Engine:  engine,
		Host:    "127.0.0.1",
		Port:    port,
		Healthy: healthy,
	}, "local-backend")
}

// localEnginePort asks engine-manager for the port the named engine is actually
// serving on. The bool says whether there is a port worth probing. A valid
// running:false response is authoritative and returns false; only an unavailable
// manager/RPC retains the legacy stock-port fallback.
func (b *Broker) localEnginePort(engine string, fallback int) (int, bool) {
	em := b.getEngineMgr()
	if em == nil {
		return fallback, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	params, _ := json.Marshal(map[string]string{"engine": engine})
	result, rpcErr, err := em.Call(ctx, "engine:status", params)
	if err != nil || rpcErr != nil {
		return fallback, true
	}
	return runningEnginePort(result)
}

func runningEnginePort(result json.RawMessage) (int, bool) {
	var st struct {
		Running bool `json:"running"`
		Port    int  `json:"port"`
	}
	if json.Unmarshal(result, &st) != nil || !st.Running || st.Port <= 0 {
		return 0, false
	}
	return st.Port, true
}

// engineProxyListenPort returns an engine proxy's current listen port, or 0 if
// it is not supervised or has not reported ready. Used to refuse advertising an
// engine at its own proxy's port, which would be a self-forward loop, and to
// keep the compatibility fallback from mistaking a proxy that moved onto the
// facade port for the engine itself.
func (b *Broker) engineProxyListenPort(profile engineProxyProfile) int {
	if p := b.engineProxyHandle(profile); p != nil {
		if ready, port := p.Status(profile.Name); ready {
			return port
		}
	}
	return 0
}

func (b *Broker) proxyListenPort() int {
	return b.engineProxyListenPort(ollamaProxyProfile)
}

func (b *Broker) lmstudioProxyListenPort() int {
	return b.engineProxyListenPort(lmstudioProxyProfile)
}

// checkEngineHealth reports whether a local engine is answering on the given
// port, by probing the path its own liveness convention uses. The port is
// resolved per poll (see localEnginePort) rather than hardcoded, so the proxy
// is never mistaken for the engine it fronts.
func checkEngineHealth(profile engineProxyProfile, client *http.Client, port int) bool {
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d%s", port, profile.HealthProbePath))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
