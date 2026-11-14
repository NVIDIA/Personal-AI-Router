// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
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

// currentLocalBackend snapshots the configured local engine.
func (f *facade) currentLocalBackend() localBackend {
	f.backendMu.RLock()
	defer f.backendMu.RUnlock()
	return f.backend
}

// setLocalBackend records (or, with a zero port / unhealthy flag, effectively
// clears) the local engine this facade's ingress serves.
//
// A non-loopback host is rejected rather than stored. The ingress forwards a
// pin-authenticated peer's request straight here without consulting discovery,
// so an off-box host would turn this node into a relay to an address chosen by
// whoever can reach the control channel. The broker only ever sends 127.0.0.1;
// this is the same defence-in-depth re-validation setLoopbackAlias performs on
// the alias the broker sends it.
func (f *facade) setLocalBackend(b localBackend) error {
	if b.Host != "" && !isLoopbackHost(b.Host) {
		return fmt.Errorf("local backend host %q is not loopback", b.Host)
	}
	f.backendMu.Lock()
	defer f.backendMu.Unlock()
	f.backend = b
	return nil
}

// backendEntry is the wire shape of one endpoint in a Custom facade's backend
// set — the payload that replaces the single localBackend when a node owns
// user-configured OpenAI-compatible endpoints. The key travels only on the
// broker↔proxy stdio link (policy-sanctioned), and never into logs, events,
// or telemetry.
type backendEntry struct {
	URL        string   `json:"url"`
	APIKey     string   `json:"apiKey,omitempty"`
	ModelNames []string `json:"modelNames,omitempty"`
}

// configuredBackend is a parsed backendEntry: the endpoint URL is validated at
// set time so a malformed push fails the control call rather than becoming a
// routing-time surprise.
type configuredBackend struct {
	url    *url.URL
	key    string
	models []string
}

// configuredBackends snapshots this facade's endpoint set.
func (f *facade) configuredBackends() []configuredBackend {
	f.backendMu.RLock()
	defer f.backendMu.RUnlock()
	return f.backends
}

// setLocalBackends replaces the facade's endpoint set wholesale (an empty set
// clears it, restoring the no-backend answer).
//
// The loopback rule on setLocalBackend deliberately does not apply here: a
// Custom endpoint is off-box by definition — that is the feature — and this
// set, like the single backend, arrives only over the broker's own control
// channel, never from discovery. What stays enforced is shape: http(s) scheme
// and a host, so the ingress never dials a malformed target.
func (f *facade) setLocalBackends(set []backendEntry) error {
	parsed := make([]configuredBackend, 0, len(set))
	for _, e := range set {
		u, err := url.Parse(e.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("backend url %q is not a valid http(s) endpoint", e.URL)
		}
		parsed = append(parsed, configuredBackend{url: u, key: e.APIKey, models: e.ModelNames})
	}
	f.backendMu.Lock()
	defer f.backendMu.Unlock()
	f.backends = parsed
	return nil
}

// selectConfiguredBackend picks which endpoint answers a peer request.
//
// A request for a model goes to the first endpoint that declares it, because
// "advertised" is the routing contract peers relied on. Anything else may
// only be served by a lone unattributed endpoint — a server that answers
// whatever it is asked and declared no list — which is the same deal the
// single loopback engine makes. With several configured endpoints and no
// declared owner for the model, the ingress refuses rather than guessing:
// picking one could hand a request (and this node's credential for that
// endpoint) to a server that does not serve the model.
func selectConfiguredBackend(set []configuredBackend, model string) (*configuredBackend, bool) {
	if model == "" {
		if len(set) == 1 {
			return &set[0], true
		}
		return nil, false
	}
	for i := range set {
		for _, declared := range set[i].models {
			if declared == model {
				return &set[i], true
			}
		}
	}
	if len(set) == 1 && len(set[0].models) == 0 {
		return &set[0], true
	}
	return nil, false
}

// handleBackendSetIngress serves a peer request against this node's endpoint
// set. Strictly terminal, like the single-backend path: exactly one
// configured endpoint may be dialed, no failover to peers or other endpoints,
// and the caller's Authorization never leaves the trusted hop.
func (f *facade) handleBackendSetIngress(w http.ResponseWriter, r *http.Request, peer string, set []configuredBackend) {
	bodyBytes, model := bufferBodyAndModel(r, false)
	if bodyBytes != nil {
		// bufferBodyAndModel consumed and closed the original body; replay the
		// exact bytes for the forward.
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	}
	backend, ok := selectConfiguredBackend(set, model)
	if !ok {
		// Actionable 502: the caller (or its proxy) learned from the advertise
		// that this node serves the model and needs to know the advertise is
		// stale, not that the request is malformed.
		writeIngressError(w, http.StatusBadGateway, "no-backend-for-model",
			"no configured endpoint on this node serves the requested model")
		return
	}
	slog.Debug("cluster ingress forwarding to custom endpoint",
		"peer", peer, "method", r.Method, "path", r.URL.Path, "target", backend.url.Host)
	f.forwardToEndpoint(w, r, backend)
}

// forwardToEndpoint streams the request to the chosen endpoint. The transport
// is the plain pool: an endpoint is not a cluster peer, so our node leaf must
// not be presented to it, and https endpoints get system-root TLS (they are
// ordinary public endpoints, not pinned peers).
func (f *facade) forwardToEndpoint(w http.ResponseWriter, r *http.Request, b *configuredBackend) {
	target := b.url
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// The caller's credential is the client's own and is never
			// relayed to a third-party host; only the key configured for
			// this endpoint on this node may travel, and only to it.
			req.Header.Del("Authorization")
			if b.key != "" {
				req.Header.Set("Authorization", "Bearer "+b.key)
			}
		},
		Transport: f.host.plainHTTPTransport(),
		ErrorHandler: func(ew http.ResponseWriter, _ *http.Request, err error) {
			slog.Warn("cluster ingress endpoint error", "target", target.Host, "err", err)
			writeIngressError(ew, http.StatusBadGateway, "backend-error", "custom endpoint error")
		},
	}
	proxy.ServeHTTP(w, r)
}

// localBackendTarget returns the loopback URL of the current local engine, and
// false when none is set/healthy (the ingress then answers 503 rather than
// forwarding). The host defaults to 127.0.0.1, and setLocalBackend refuses to
// store anything that is not loopback, so this is always a loopback target.
func (f *facade) localBackendTarget() (*url.URL, bool) {
	b := f.currentLocalBackend()
	if b.Port <= 0 || !b.Healthy {
		return nil, false
	}
	host := b.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return &url.URL{Scheme: "http", Host: net.JoinHostPort(host, strconv.Itoa(b.Port))}, true
}

// handlePlain is the plaintext personality: it accepts requests only from
// loopback and hands them to the full local router (handleHTTP). A non-loopback
// caller — any LAN peer — is refused; peers must use the mTLS ingress. This is
// what closes the former open-relay exposure (the listener still binds all
// interfaces for the TLS personality, but plaintext is loopback-only).
func (f *facade) handlePlain(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRemote(r.RemoteAddr) {
		slog.Warn("rejected non-loopback plaintext request; cluster peers must use mTLS",
			"remote", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
		writeIngressError(w, http.StatusForbidden, "loopback-only",
			"plaintext requests are accepted only from loopback; cluster peers must use the mTLS ingress")
		return
	}
	// Engine-manager marks its private identity/action requests so this
	// compatibility facade can never be mistaken for the local Ollama backend.
	if r.Header.Get(engineIdentityProbeHeader) == "1" {
		writeIngressError(w, http.StatusConflict, "proxy-facade",
			"the compatibility facade is not the "+f.profile.DisplayName+" engine")
		return
	}
	f.handleHTTP(w, r)
}

// handleClusterIngress is the LAN mTLS personality: it authenticates the caller
// against this node's cluster pins and, once the peer is a trusted cluster
// member, forwards the request straight to the local loopback engine — exactly
// like the local plaintext path, with no route filtering. The mTLS pin is the
// sole authorization boundary (a trusted peer is treated like a local client),
// so the two personalities stay behaviorally identical toward the engine. It
// never calls resolveCandidates, so a peer request cannot be re-routed onward.
func (f *facade) handleClusterIngress(w http.ResponseWriter, r *http.Request) {
	// Re-derive membership and pins per request so a cluster left, or a peer
	// paired or removed, after startup is reflected immediately without a proxy
	// restart — a removed peer must stop being accepted right away, which is the
	// whole point of the gate.
	f.host.mesh.Refresh()
	peer, ok := f.host.mesh.VerifyClientPin(r)
	if !ok {
		writeIngressError(w, http.StatusForbidden, "cluster-auth",
			"client certificate is not a pinned member of this node's cluster")
		return
	}
	// A Custom facade owns a set of user-configured endpoints instead of a
	// loopback engine. Selection happens here, on the owner node, because only
	// here is any endpoint's credential available and only here may a peer's
	// request be released toward a third-party host.
	if set := f.configuredBackends(); len(set) > 0 {
		f.handleBackendSetIngress(w, r, peer, set)
		return
	}
	target, ok := f.localBackendTarget()
	if !ok {
		writeIngressError(w, http.StatusServiceUnavailable, "no-local-backend",
			"no local inference backend is available on this node")
		return
	}
	slog.Debug("cluster ingress forwarding to local backend",
		"peer", peer, "method", r.Method, "path", r.URL.Path, "target", target.Host)
	f.reverseProxyToLocal(w, r, target)
}

// reverseProxyToLocal streams the request to the local engine, preserving
// cancellation (the request context is the proxy's root context, so a client
// disconnect or shutdown tears down the upstream call and stops generation).
func (f *facade) reverseProxyToLocal(w http.ResponseWriter, r *http.Request, target *url.URL) {
	f.newLocalReverseProxy(target).ServeHTTP(w, r)
}

func (f *facade) newLocalReverseProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
		},
		Transport: f.host.plainHTTPTransport(),
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

// writeIngressError returns the actual failure without granting browser permissions.
func writeIngressError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	body, err := json.Marshal(map[string]string{"error": msg, "code": code})
	if err != nil {
		body = []byte(`{"error":"ingress error"}`)
	}
	_, _ = w.Write(body)
}
