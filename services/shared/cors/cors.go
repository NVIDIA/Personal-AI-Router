// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cors is the single source of truth for the CORS policy NVPAIR's
// inference proxies present to browser clients. Both proxies front a
// local-network inference engine for the same kinds of caller (a local web UI,
// an Electron renderer whose origin differs from the proxy's), so they must
// answer a preflight and label a response identically; keeping one
// implementation is what stops the two from drifting apart.
//
// The policy is deny-by-default. A browser page on any origin can reach the
// proxies' loopback listener (browsers connect from loopback, so a bind-scoped
// gate does not exclude them); with an "allow every origin" policy any website
// could drive and read the local inference engines — exfiltrating responses
// and using the models as a free oracle. Browser callers are therefore
// admitted only from exact origins the operator lists in
// NVPAIR_PROXY_ALLOWED_ORIGINS (comma-separated, scheme+host[:port], compared
// exactly). Non-browser callers (the Electron main process, CLI tools, health
// probes) send no Origin header and are unaffected.
//
// The policy applies to responses a proxy authors itself. A response forwarded
// from an engine that declared its own Access-Control-Allow-Origin keeps that
// engine's policy — including on a preflight, where preserving an exact origin
// and Access-Control-Allow-Credentials is required for credentialed browser
// requests. An engine without a CORS policy gets the proxy's deny-by-default
// fallback.
package cors

import (
	"net/http"
	"os"
	"strings"
)

// allowedOriginsEnv names the operator-controlled origin allowlist. Exact
// origins only; an empty value (the default) admits no browser origins.
const allowedOriginsEnv = "NVPAIR_PROXY_ALLOWED_ORIGINS"

// allowedOrigins returns the configured allowlist. Read per request so an
// operator change needs a proxy restart at most, not a code change per caller.
func allowedOrigins() []string {
	raw := strings.TrimSpace(os.Getenv(allowedOriginsEnv))
	if raw == "" {
		return nil
	}
	var out []string
	for _, o := range strings.Split(raw, ",") {
		if o = strings.TrimSpace(o); o != "" {
			out = append(out, o)
		}
	}
	return out
}

// originAllowed reports whether the request's Origin header is absent (a
// non-browser or same-origin-with-no-origin caller — allowed) or exactly
// matches one configured origin (allowed). A present-but-unlisted Origin is
// the cross-origin browser case: denied.
func originAllowed(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	for _, want := range allowedOrigins() {
		if origin == want {
			return true
		}
	}
	return false
}

// AllowRequest gates a browser-capable request: anything carrying an Origin
// not on the operator allowlist is rejected before it can drive an engine,
// closing the cross-origin blind-oracle path (a simple cross-origin POST needs
// no preflight, so header/preflight policy alone cannot provide this).
func AllowRequest(r *http.Request) bool { return originAllowed(r) }

// RejectOrigin writes the 403 for a disallowed Origin. The body is static so
// nothing about the proxy's internals is reflected.
func RejectOrigin(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":"cross-origin browser requests are not allowed","code":"origin-not-allowed"}`))
}

// Apply writes the proxy's own CORS policy for an allowlisted (or
// Origin-less) caller. With no allowlist entry matching the request origin the
// recommended posture is to write no CORS headers at all — a browser then
// cannot read the response cross-origin, while non-browser callers are
// untouched because they ignore CORS entirely.
//
// Without an Access-Control-Allow-Origin a browser surfaces every failure as
// an opaque "CORS error", but that opacity is the intended cost of deny-by-
// default: the operator lists the origins that may read responses, and every
// other origin gets nothing.
func Apply(h http.Header, origin string) {
	// The proxies never allow credentials; make that explicit against any
	// inherited header so the response can't be misread as credentialed —
	// regardless of whether an origin grant is being written.
	h.Del("Access-Control-Allow-Credentials")
	if origin == "" {
		return
	}
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Vary", "Origin")
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
}

// applyPreflight answers an allowlisted origin's preflight with the static
// method/header grant. Arbitrary request headers are deliberately NOT echoed:
// a preflight naming a header outside the static grant simply fails, which is
// deny-by-default doing its job.
func applyPreflight(h http.Header, r *http.Request) {
	Apply(h, r.Header.Get("Origin"))
}

// WritePreflight answers a browser's OPTIONS preflight locally when the origin
// is allowlisted, and reports whether it handled the request. A disallowed
// origin's preflight is left unhandled so the caller's own gate answers 403.
// When an engine is available and allowlisted, its preflight is forwarded
// instead so an exact origin and credentials policy can survive.
func WritePreflight(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodOptions {
		return false
	}
	if !originAllowed(r) {
		return false
	}
	applyPreflight(w.Header(), r)
	w.WriteHeader(http.StatusNoContent)
	return true
}

// CompletePreflightFallback replaces an upstream OPTIONS response that has no
// CORS policy with the proxy's local 204 fallback. Engines that do declare an
// Access-Control-Allow-Origin are left completely untouched; in particular,
// an exact origin plus Access-Control-Allow-Credentials must reach the browser
// unchanged for a credentialed preflight to pass.
func CompletePreflightFallback(resp *http.Response) bool {
	if resp == nil || resp.Request == nil || resp.Request.Method != http.MethodOptions ||
		resp.Header.Get("Access-Control-Allow-Origin") != "" {
		return false
	}
	if !originAllowed(resp.Request) {
		return false
	}
	if resp.Body != nil {
		_ = resp.Body.Close()
	}
	applyPreflight(resp.Header, resp.Request)
	resp.StatusCode = http.StatusNoContent
	resp.Status = "204 No Content"
	resp.Body = http.NoBody
	resp.ContentLength = 0
	resp.TransferEncoding = nil
	resp.Header.Del("Content-Length")
	resp.Header.Del("Content-Type")
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Transfer-Encoding")
	return true
}
