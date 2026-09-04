// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"

	"nvpair-shared/cors"
)

const engineIdentityProbeHeader = "X-NVPAIR-Engine-Identity-Probe"

// localBackend is one explicit loopback engine the cluster mTLS ingress
// forwards to. It is supplied by the broker over node/set-local-backend and is
// deliberately NOT sourced from the discovery overlay: a request that arrived
// over the LAN mTLS ingress can only ever be dumped on this node's own local
// engine, never re-routed to a peer, so the ingress path is strictly terminal
// and cannot recurse or amplify.
//
// Engine names which OpenAI engine this is. One node may run LM Studio and vLLM
// at once, so the backends are held per engine and each is set and cleared
// independently — marking one unhealthy must not disturb the other.
type localBackend struct {
	Engine  string `json:"engine"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Healthy bool   `json:"healthy"`
}

// url is the loopback URL for this backend, and false when it is not set or not
// healthy. The host defaults to 127.0.0.1 and is always loopback.
func (b localBackend) url() (*url.URL, bool) {
	if b.Port <= 0 || !b.Healthy {
		return nil, false
	}
	host := b.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return &url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(b.Port))}, true
}

// setLocalBackend records (or, with a zero port / unhealthy flag, effectively
// clears) one engine's local backend. Storing the unhealthy record rather than
// deleting the key keeps "known and down" distinguishable from "never
// advertised" in logs, while localBackendTarget treats both as unavailable.
func (p *Proxy) setLocalBackend(b localBackend) {
	p.backendMu.Lock()
	if p.backends == nil {
		p.backends = make(map[string]localBackend, len(openaiEngines))
	}
	p.backends[b.Engine] = b
	p.backendMu.Unlock()
}

// localBackendTarget returns the loopback URL of one engine's local backend,
// and false when that engine has none set or it is unhealthy (the ingress then
// answers 503 rather than forwarding).
func (p *Proxy) localBackendTarget(engine string) (*url.URL, bool) {
	p.backendMu.RLock()
	b, ok := p.backends[engine]
	p.backendMu.RUnlock()
	if !ok {
		return nil, false
	}
	return b.url()
}

// localBackends returns every healthy local backend in openaiEngines order,
// paired with its engine name. It is the fan-out set for the ingress model list
// and the candidate set for this node's own entry in routing.
func (p *Proxy) localBackends() []localEngineTarget {
	p.backendMu.RLock()
	defer p.backendMu.RUnlock()
	out := make([]localEngineTarget, 0, len(openaiEngines))
	for _, e := range openaiEngines {
		b, ok := p.backends[e.Name]
		if !ok {
			continue
		}
		if u, ok := b.url(); ok {
			out = append(out, localEngineTarget{Engine: e.Name, URL: u})
		}
	}
	return out
}

// localEngineTarget is one healthy local engine: its name and its loopback URL.
type localEngineTarget struct {
	Engine string
	URL    *url.URL
}

// handlePlain is the plaintext personality: it accepts requests only from
// loopback and hands them to the full local router (handleHTTP). A non-loopback
// caller — any LAN peer — is refused; peers must use the mTLS ingress. This is
// what closes the former open-relay exposure (the listener still binds all
// interfaces for the TLS personality, but plaintext is loopback-only).
func (p *Proxy) handlePlain(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRemote(r.RemoteAddr) {
		// Answer a non-loopback preflight ahead of the gate. It grants no access
		// on its own; the request that follows still receives the real 403. A
		// loopback preflight continues into handleHTTP so an available engine's
		// exact origin and credentials policy can be preserved.
		if cors.WritePreflight(w, r) {
			return
		}
		slog.Warn("rejected non-loopback plaintext request; cluster peers must use mTLS",
			"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
		writeIngressError(w, http.StatusForbidden, "loopback-only",
			"plaintext requests are accepted only from loopback; cluster peers must use the mTLS ingress")
		return
	}
	// Engine-manager marks identity/action requests so the federated model-list
	// facade can never satisfy LM Studio's own /v1/models readiness probe.
	if r.Header.Get(engineIdentityProbeHeader) == "1" {
		writeIngressError(w, http.StatusConflict, "proxy-facade", "the compatibility facade is not an LM Studio engine")
		return
	}
	p.handleHTTP(w, r)
}

// handleClusterIngress is the LAN mTLS personality: it authenticates the caller
// against this node's cluster pins and, once the peer is a trusted cluster
// member, forwards the request straight to the local loopback engine — exactly
// like the local plaintext path, with no route filtering. The mTLS pin is the
// sole authorization boundary (a trusted peer is treated like a local client),
// so the two personalities stay behaviorally identical toward the engine. It
// never calls resolveCandidates, so a peer request cannot be re-routed onward.
func (p *Proxy) handleClusterIngress(w http.ResponseWriter, r *http.Request) {
	// Re-derive membership and pins per request so a cluster left, or a peer
	// paired or removed, after startup is reflected immediately without a proxy
	// restart — a removed peer must stop being accepted right away, which is the
	// whole point of the gate.
	p.mesh.Refresh()
	peer, ok := p.mesh.VerifyClientPin(r)
	if !ok {
		writeIngressError(w, http.StatusForbidden, "cluster-auth",
			"client certificate is not a pinned member of this node's cluster")
		return
	}
	backends := p.localBackends()
	if len(backends) == 0 {
		writeIngressError(w, http.StatusServiceUnavailable, "no-local-backend",
			"no local inference backend is available on this node")
		return
	}
	// A peer aggregating the cluster's inventory must see everything this node
	// serves. Forwarding to one engine would hide the other's models and the
	// peer would then never route a request this node could have answered, so
	// the list is merged locally instead. Still terminal: the fan-out is to this
	// node's own loopback engines only.
	if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
		p.serveLocalModelList(w, r, backends)
		return
	}
	target, engine, ok := p.ingressTarget(r, backends)
	if !ok {
		writeIngressError(w, http.StatusServiceUnavailable, "no-model-owner",
			"no local inference engine serves the requested model")
		return
	}
	slog.Debug("cluster ingress forwarding to local backend",
		"peer", peer, "engine", engine, "method", r.Method, "path", r.URL.Path, "target", target.Host)
	p.reverseProxyToLocal(w, r, target)
}

// ingressTarget picks which local engine answers a peer's request. Resolution
// order, top down:
//
//  1. the engine that actually serves the requested model, read live from the
//     local engines themselves (short-TTL cached) rather than from discovery —
//     this node's own entry in the discovery overlay is enriched by the scanner
//     and can lag or, on a freshly started node, be absent entirely, which would
//     misroute every ingress request;
//  2. the only healthy backend, when there is exactly one — the common case, and
//     the answer regardless of what the body names;
//  3. no target, which the caller reports as 503.
//
// The body is buffered and restored, so the reverse proxy still forwards it.
func (p *Proxy) ingressTarget(r *http.Request, backends []localEngineTarget) (*url.URL, string, bool) {
	if len(backends) == 1 {
		return backends[0].URL, backends[0].Engine, true
	}
	body, model := bufferBodyAndModel(r)
	if body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
	}
	if model != "" {
		if engine := engineForModel(p.localModelsByEngine(r.Context(), backends), model); engine != "" {
			for _, b := range backends {
				if b.Engine == engine {
					return b.URL, b.Engine, true
				}
			}
		}
	}
	return nil, "", false
}

// reverseProxyToLocal streams the request to the local engine, preserving
// cancellation (the request context is the proxy's root context, so a client
// disconnect or shutdown tears down the upstream call and stops generation).
func (p *Proxy) reverseProxyToLocal(w http.ResponseWriter, r *http.Request, target *url.URL) {
	p.newLocalReverseProxy(target).ServeHTTP(w, r)
}

func (p *Proxy) newLocalReverseProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
		},
		Transport: p.plainHTTPTransport(),
		ErrorHandler: func(ew http.ResponseWriter, _ *http.Request, err error) {
			slog.Warn("cluster ingress upstream error", "target", target.Host, "err", err)
			writeIngressError(ew, http.StatusBadGateway, "backend-error", "local inference backend error")
		},
	}
}

// isLoopbackRemote reports whether an http.Request RemoteAddr (host:port) is a
// loopback address (127.0.0.0/8 or ::1). An unparseable/empty RemoteAddr is not
// loopback, so it fails closed.
func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// writeIngressError writes a small structured JSON error. It never echoes the
// request body or any generated output. CORS headers are included because these
// are the proxy's own rejections: without them a browser client cannot read the
// status or reason, and every one of them looks like a generic CORS failure.
func writeIngressError(w http.ResponseWriter, status int, code, msg string) {
	cors.Apply(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	body, err := json.Marshal(map[string]string{"error": msg, "code": code})
	if err != nil {
		body = []byte(`{"error":"ingress error"}`)
	}
	_, _ = w.Write(body)
}

// localModelsTTL bounds how stale the ingress engine attribution may be. Long
// enough that a burst of peer requests costs one loopback query per engine,
// short enough that a model swap is picked up within a few seconds.
const localModelsTTL = 5 * time.Second

// localModelsCache memoizes the per-engine model ids read from this node's own
// engines, so ingress routing does not pay a loopback round trip per request.
type localModelsCache struct {
	mu       sync.Mutex
	fetched  time.Time
	byEngine map[string][]string
}

// localModelsByEngine returns which local engine serves which models, asking the
// engines themselves rather than the discovery overlay. Authoritative for this
// node and cheap: loopback, bounded, and cached for localModelsTTL. An engine
// that fails to answer contributes nothing, so a request for its model falls
// through to the caller's next resolution step.
func (p *Proxy) localModelsByEngine(ctx context.Context, backends []localEngineTarget) map[string][]string {
	p.localModels.mu.Lock()
	if time.Since(p.localModels.fetched) < localModelsTTL && p.localModels.byEngine != nil {
		cached := p.localModels.byEngine
		p.localModels.mu.Unlock()
		return cached
	}
	p.localModels.mu.Unlock()

	fresh := make(map[string][]string, len(backends))
	for _, b := range backends {
		ids, err := p.fetchModelIDs(ctx, b.URL)
		if err != nil {
			slog.Debug("local model list unavailable", "engine", b.Engine, "target", b.URL.Host, "err", err)
			continue
		}
		fresh[b.Engine] = ids
	}

	p.localModels.mu.Lock()
	p.localModels.fetched = time.Now()
	p.localModels.byEngine = fresh
	p.localModels.mu.Unlock()
	return fresh
}

// fetchModelIDs reads one loopback engine's /v1/models and returns its ids.
func (p *Proxy) fetchModelIDs(ctx context.Context, target *url.URL) ([]string, error) {
	records, err := p.fetchModelRecords(ctx, target)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(records))
	for _, rec := range records {
		ids = append(ids, rec.key)
	}
	return ids, nil
}

// serveLocalModelList answers a peer's GET /v1/models by merging this node's
// own engines' inventories, first engine's metadata winning on a duplicate id.
func (p *Proxy) serveLocalModelList(w http.ResponseWriter, r *http.Request, backends []localEngineTarget) {
	seen := make(map[string]bool)
	models := make([]json.RawMessage, 0)
	ok := false
	for _, b := range backends {
		records, err := p.fetchModelRecords(r.Context(), b.URL)
		if err != nil {
			slog.Debug("ingress model list candidate unavailable", "engine", b.Engine, "target", b.URL.Host, "err", err)
			continue
		}
		ok = true
		for _, rec := range records {
			if !seen[rec.key] {
				seen[rec.key] = true
				models = append(models, rec.raw)
			}
		}
	}
	if !ok {
		writeIngressError(w, http.StatusServiceUnavailable, "no-local-backend",
			"no local inference backend answered its model list")
		return
	}
	body, err := json.Marshal(struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}{Object: "list", Data: models})
	if err != nil {
		writeIngressError(w, http.StatusInternalServerError, "backend-error", "failed to encode model inventory")
		return
	}
	cors.Apply(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// fetchModelRecords reads one loopback engine's /v1/models over the shared plain
// transport. Loopback only: the caller supplies a target from localBackends.
func (p *Proxy) fetchModelRecords(ctx context.Context, target *url.URL) ([]modelListItem, error) {
	list := *target
	list.Path = "/v1/models"
	client := &http.Client{Timeout: modelListClient.Timeout, Transport: p.plainHTTPTransport()}
	return fetchModelList(ctx, client, &list)
}
