// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"

	"nvpair-shared/cors"
	"nvpair-shared/ingressauth"
)

const engineIdentityProbeHeader = "X-NVPAIR-Engine-Identity-Probe"

// localBackend is the explicit loopback engine the cluster mTLS ingress
// forwards to. It is supplied by the broker over node/set-local-backend and is
// deliberately NOT sourced from the discovery overlay: a request that arrived
// over the LAN mTLS ingress can only ever be dumped on this node's own local
// engine, never re-routed to a peer, so the ingress path is strictly terminal
// and cannot recurse or amplify.
type localBackend struct {
	Engine  string `json:"engine"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	Healthy bool   `json:"healthy"`
}

// setLocalBackend records (or, with a zero port / unhealthy flag, effectively
// clears) the local engine the ingress serves.
func (p *Proxy) setLocalBackend(b localBackend) {
	p.backendMu.Lock()
	p.backend = b
	p.backendMu.Unlock()
}

// localBackendTarget returns the loopback URL of the current local engine, and
// false when none is set/healthy (the ingress then answers 503 rather than
// forwarding). The host defaults to 127.0.0.1 and is always loopback.
func (p *Proxy) localBackendTarget() (*url.URL, bool) {
	p.backendMu.RLock()
	b := p.backend
	p.backendMu.RUnlock()
	if b.Port <= 0 || !b.Healthy {
		return nil, false
	}
	host := b.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return &url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(b.Port))}, true
}

// handlePlain is the plaintext personality: it accepts requests from loopback
// and hands them to the full local router (handleHTTP). A non-loopback caller —
// any LAN peer — is refused unless the operator has enabled the API-key gate
// (nvpair-shared/ingressauth) and the caller presents a configured key, in
// which case it is routed exactly like a loopback client. Cluster peers still
// use the mTLS ingress, and loopback is never asked for a key. This is what
// closes the former open-relay exposure: the listener binds all interfaces for
// the TLS personality, but plaintext is loopback-only unless authenticated.
func (p *Proxy) handlePlain(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRemote(r.RemoteAddr) {
		// One Authorize call refreshes the key file and judges the request from
		// that single view, so enablement and the key set cannot change between
		// "is the gate on?" and "is this key good?".
		var d ingressauth.Decision
		if p.lanAuth != nil {
			d = p.lanAuth.Authorize(r)
		}
		// A source outside the operator's allowlist gets nothing — not even the
		// preflight — so the allowlist means what it says for OPTIONS too.
		if d.Enabled && d.Code == ingressauth.CodeSourceNotAllowed {
			slog.Warn("rejected non-loopback plaintext request", "remote", r.RemoteAddr,
				"method", r.Method, "path", r.URL.Path, "code", d.Code)
			writeIngressError(w, d.Status, d.Code, d.Message)
			return
		}
		// Answer a non-loopback preflight ahead of the credential check. It
		// grants no access on its own; the request that follows still receives
		// the real 401/403, and a browser sends no Authorization on a preflight
		// anyway. A loopback preflight continues into handleHTTP so an available
		// engine's exact origin and credentials policy can be preserved.
		if cors.WritePreflight(w, r) {
			return
		}
		if !d.Enabled {
			slog.Warn("rejected non-loopback plaintext request; cluster peers must use mTLS",
				"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
			writeIngressError(w, http.StatusForbidden, "loopback-only",
				"plaintext requests are accepted only from loopback; cluster peers must use the mTLS ingress")
			return
		}
		if !d.Allowed {
			slog.Warn("rejected non-loopback plaintext request", "remote", r.RemoteAddr,
				"method", r.Method, "path", r.URL.Path, "code", d.Code, "key_fp", d.KeyFingerprint)
			if d.Challenge != "" {
				w.Header().Set("WWW-Authenticate", d.Challenge)
			}
			writeIngressError(w, d.Status, d.Code, d.Message)
			return
		}
		// The key is the proxy's credential, not the engine's: never forward it.
		p.lanAuth.StripCredential(r.Header)
		// At Info, not Debug: a production log must show who used a key.
		slog.Info("authenticated non-loopback plaintext request", "remote", r.RemoteAddr,
			"method", r.Method, "path", r.URL.Path, "key_fp", d.KeyFingerprint)
	}
	// Engine-manager marks its private identity/action requests so this
	// compatibility facade can never be mistaken for the local Ollama backend.
	if r.Header.Get(engineIdentityProbeHeader) == "1" {
		writeIngressError(w, http.StatusConflict, "proxy-facade", "the compatibility facade is not an Ollama engine")
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
	target, ok := p.localBackendTarget()
	if !ok {
		writeIngressError(w, http.StatusServiceUnavailable, "no-local-backend",
			"no local inference backend is available on this node")
		return
	}
	slog.Debug("cluster ingress forwarding to local backend",
		"peer", peer, "method", r.Method, "path", r.URL.Path, "target", target.Host)
	p.reverseProxyToLocal(w, r, target)
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
