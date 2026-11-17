// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"nvpair-shared/engines"
	"nvpair-shared/httpcon"
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

// runAutoAdvertiseEngine reconciles an engine using the configured port
// recorded in its runtime profile, without compatibility-port reconciliation.
func (b *Broker) runAutoAdvertiseEngine(ctx context.Context, profile engineProxyProfile) {
	client := &http.Client{Timeout: 2 * time.Second}
	ticker := time.NewTicker(autoAdvertiseInterval)
	defer ticker.Stop()

	b.reconcileAdvertiseEngine(profile, client)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.reconcileAdvertiseEngine(profile, client)
		}
	}
}

func (b *Broker) reconcileAdvertiseEngine(profile engineProxyProfile, client *http.Client) {
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()

	enginePort := int(b.engineProxy(profile).backendPort.Load())
	proxyPort := b.engineProxyListenPort(profile)
	up := enginePort > 0 &&
		proxyPort > 0 &&
		enginePort != proxyPort &&
		checkEngineHealth(profile, client, enginePort)
	if up {
		b.registerService(noderec.RegisterParams{Service: profile.DiscoveryService, Port: proxyPort})
		b.setProxyLocalBackend(b.engineProxyHandle(profile), profile.Name, enginePort, true)
		return
	}
	b.unregisterService(profile.DiscoveryService)
	b.setProxyLocalBackend(b.engineProxyHandle(profile), profile.Name, enginePort, false)
}

// runCustomAdvertise reconciles the Custom plane on the same 5 s cadence the
// engine advertisers use. Custom has no local engine to poll: its truth is
// the discovery store's custom slot (what the bridge currently routes) plus
// the custom facade's own readiness. That is also why it cannot ride the
// generic runAutoAdvertiseEngine loop — that loop's up-condition requires a
// local engine port, which Custom by definition never has.
func (b *Broker) runCustomAdvertise(ctx context.Context) {
	ticker := time.NewTicker(autoAdvertiseInterval)
	defer ticker.Stop()

	b.reconcileCustomAdvertise()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.reconcileCustomAdvertise()
		}
	}
}

// reconcileCustomAdvertise brings three things in line with what this node
// can currently serve through the custom facade:
//
//   - the cu service registration, advertised only while at least one routed
//     endpoint exists AND the facade reports ready. A peer that saw cu and
//     routed into a down facade would dial a dead port, so advertisement
//     follows routability, not configuration (D1: nothing to toggle).
//   - the engine-manager model inventory. The push carries the union only
//     while the facade is ready; with the facade down it is empty, which
//     clears ByEngine["custom"] on the next /v1/models fetch. A peer must not
//     keep attributing (or routing toward) models this node cannot serve.
//   - the proxy's ingress backend set (G4). A peer request that reaches this
//     node must land on the endpoint serving its model, and only the broker
//     knows which endpoints this node owns and which credential each needs —
//     the key is injected here, on the owner, and never travels onward. Like
//     the other two it is gated on facade readiness: with no listener there is
//     nothing to receive a set, and resetting the gate while down lets a
//     respawned proxy re-learn the current set instead of inheriting a gate
//     keyed to state it never received.
func (b *Broker) reconcileCustomAdvertise() {
	b.engineConfigMu.Lock()
	defer b.engineConfigMu.Unlock()

	facadePort := b.engineProxyListenPort(customProxyProfile)
	if facadePort == 0 {
		b.unregisterService(noderec.ServiceCustom)
		b.customIngress = customIngressState{}
		b.pushCustomModelsToEngineMgr(nil)
		return
	}

	models := b.store.customModelUnion()
	if len(models) > 0 {
		b.registerService(noderec.RegisterParams{Service: noderec.ServiceCustom, Port: facadePort})
	} else {
		b.unregisterService(noderec.ServiceCustom)
	}
	b.pushCustomBackendSet()
	b.pushCustomModelsToEngineMgr(models)
}

// customSetModelsMethod is the broker→engine-manager notification carrying
// this node's custom model set. Engine-manager publishes it into
// ModelsResult.ByEngine["custom"] without knowing anything about the
// endpoints behind it — least of all their credentials, which never travel
// this hop (or any hop out of the broker's config code).
const customSetModelsMethod = "custom:set-models"

// customSetModelsPayload is the notification envelope. The flat-list shape
// matches every other model-set payload on the wire.
type customSetModelsPayload struct {
	Models []string `json:"models"`
}

// customAdvertiseState is the push change gate, guarded by engineConfigMu
// (every reader/writer reconciles under it). Keying on the model set alone
// would strand a respawned engine-manager on an empty inventory; keying on
// the worker too replays the set to a replacement without an event, which is
// how custom state survives an engine-manager restart.
type customAdvertiseState struct {
	pushKey string
	pushEm  *rpcWorker
}

// pushCustomModelsToEngineMgr writes the current custom inventory to
// engine-manager when it differs from the last thing THAT worker was told.
// A missing worker resets the gate instead (see customAdvertiseState). A
// never-used gate with nothing to push adopts quietly: a fresh
// engine-manager already has an empty inventory, so an empty replay would be
// wire noise on every quiet tick, forever.
func (b *Broker) pushCustomModelsToEngineMgr(models []string) {
	em := b.getEngineMgr()
	if em == nil {
		b.customAdvertise.pushKey = ""
		b.customAdvertise.pushEm = nil
		return
	}
	key := strings.Join(models, "\x1f")
	if key == b.customAdvertise.pushKey && em == b.customAdvertise.pushEm {
		return
	}
	if key == "" && b.customAdvertise.pushKey == "" {
		b.customAdvertise.pushKey = ""
		b.customAdvertise.pushEm = em
		return
	}
	payload := customSetModelsPayload{Models: models}
	if payload.Models == nil {
		payload.Models = []string{}
	}
	if err := em.Notify(customSetModelsMethod, payload); err != nil {
		slog.Warn("custom:set-models push failed; retrying next tick", "err", err)
		return
	}
	b.customAdvertise.pushKey = key
	b.customAdvertise.pushEm = em
}

// proxyBackendEntry is the wire shape of one endpoint in the Custom facade's
// ingress backend set. The key rides this broker↔proxy stdio hop because the
// ingress cannot authenticate to the endpoint without it — the one place a
// key may appear in a JSON-RPC frame, per the custom-plane policy — and it
// is never logged, never emitted as an event, and never leaves this node.
type proxyBackendEntry struct {
	URL        string   `json:"url"`
	APIKey     string   `json:"apiKey,omitempty"`
	ModelNames []string `json:"modelNames,omitempty"`
}

// proxyBackendSetParams is the whole node/set-local-backend payload for a
// Custom facade. Backends is always marshaled as an (possibly empty) array,
// never null: the proxy reads a null/absent key as the single-backend class,
// so a null "clear" would silently fail to retract a stale set and the
// ingress would keep forwarding to removed endpoints.
type proxyBackendSetParams struct {
	Engine   string              `json:"engine"`
	Backends []proxyBackendEntry `json:"backends"`
}

// customIngressState is the change gate for the backend-set push, guarded by
// engineConfigMu like customAdvertise (both are touched only under it, by the
// reconcile). Keying on the serialized set — with the API keys inside, so a
// key-only edit re-pushes rather than leaving the ingress presenting a
// revoked credential — and on the proxy handle identity, so a replaced
// process re-receives the set without any event. Reset whenever the facade
// goes down: a re-enabled facade must not inherit a gate keyed to a backend
// set it never received.
type customIngressState struct {
	pushKey string
	pushP   *proxyProcess
}

// customBackendSet builds the set this node's custom ingress may forward to,
// from the two halves of its truth: the discovery store's custom slot records
// which endpoints currently route (healthy with models at bridge time), and
// the persisted config supplies the URL and credential. A slot whose config
// was deleted stops routing at once — the config is the truth about what may
// be used, the same rule the candidate bridge applies. Entries are ordered by
// endpoint key and each entry's models are sorted, so the same configuration
// produces the same bytes every tick and on every proxy build: the change
// gate and the wire both stay quiet under map-iteration shuffle.
func (b *Broker) customBackendSet() []proxyBackendEntry {
	slots := b.store.customSlots()
	keys := make([]string, 0, len(slots))
	for key := range slots {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	set := make([]proxyBackendEntry, 0, len(keys))
	for _, key := range keys {
		entry, configured := b.findCustomEndpoint(key)
		if !configured {
			continue
		}
		models := append([]string(nil), slots[key]...)
		sort.Strings(models)
		set = append(set, proxyBackendEntry{URL: entry.BaseURL, APIKey: entry.APIKey, ModelNames: models})
	}
	return set
}

// customBackendSetKey serializes a set into the change-gate key. Credentials
// are deliberately inside the key: this string exists only as gate state, is
// never logged, never emitted, never sent anywhere.
func customBackendSetKey(set []proxyBackendEntry) string {
	var sb strings.Builder
	for _, e := range set {
		sb.WriteString(e.URL)
		sb.WriteByte('\x1f')
		sb.WriteString(e.APIKey)
		sb.WriteByte('\x1f')
		sb.WriteString(strings.Join(e.ModelNames, "\x1e"))
		sb.WriteByte('\x1d')
	}
	return sb.String()
}

// pushCustomBackendSet hands the custom facade its ingress endpoint set when
// it differs from what that proxy handle last received. The call is the same
// bounded inline bridge the manual-node path uses: a lost or rejected push
// leaves the gate untouched so the next tick (≤5 s) retries, rather than
// stranding the proxy on a stale set until the configuration changes again.
// An empty set with nothing previously pushed adopts quietly — a fresh proxy
// or facade starts empty, so an empty replay would clear nothing and repaint
// the wire every five seconds forever.
func (b *Broker) pushCustomBackendSet() {
	set := b.customBackendSet()
	key := customBackendSetKey(set)
	p := b.customProxyHandle()
	if p == nil {
		b.customIngress = customIngressState{}
		return
	}
	if key == b.customIngress.pushKey && p == b.customIngress.pushP {
		return
	}
	if key == "" && b.customIngress.pushKey == "" {
		b.customIngress = customIngressState{pushP: p}
		return
	}
	payload := proxyBackendSetParams{Engine: customProxyProfile.Name, Backends: set}
	raw, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("custom ingress set: marshal failed", "err", err)
		return
	}
	method := engines.AddressMethod(customProxyProfile.Name, "node/set-local-backend")
	if _, rpcErr, err := p.Call(context.Background(), method, raw); err != nil {
		slog.Warn("custom ingress set push failed; retrying next tick", "err", err)
		return
	} else if rpcErr != nil {
		slog.Warn("custom ingress set push rejected; retrying next tick", "code", rpcErr.Code, "msg", rpcErr.Message)
		return
	}
	b.customIngress = customIngressState{pushKey: key, pushP: p}
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
	httpcon.DrainAndClose(resp.Body)
	return resp.StatusCode == http.StatusOK
}
