// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cors is the single source of truth for the CORS policy NVPAIR's
// inference proxies present to browser clients. Both proxies front a
// local-network inference engine for the same kinds of caller (a local web UI,
// an Electron renderer whose origin differs from the proxy's), so they must
// answer a preflight and label a response identically; keeping one
// implementation is what stops the two from drifting apart.
//
// The policy applies to responses a proxy authors itself. A response forwarded
// from an engine that declared its own Access-Control-Allow-Origin keeps that
// engine's policy — including on a preflight, where preserving an exact origin
// and Access-Control-Allow-Credentials is required for credentialed browser
// requests. An engine without a CORS policy gets the proxy's permissive fallback.
package cors

import "net/http"

// Apply writes the permissive CORS policy so browser-based clients can read
// proxy responses — and, crucially, error bodies. Without an
// Access-Control-Allow-Origin a browser surfaces every failure as an opaque
// "CORS error", hiding the real status the proxy returned. The proxies front a
// local-network inference engine, not a credentialed API, so a wildcard origin
// is appropriate and they never reflect credentials.
func Apply(h http.Header) {
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	// A browser rejects a wildcard origin paired with Allow-Credentials: true,
	// so setting the former while inheriting the latter would leave the
	// response unreadable — the failure this policy exists to prevent. Callers
	// reach here only when no engine policy is being preserved, so an
	// Allow-Credentials from an upstream that sent no origin of its own
	// describes a policy this response no longer carries. Drop it.
	h.Del("Access-Control-Allow-Credentials")
	// Uncredentialed wildcard responses may expose every header, so a browser
	// client can read engine metadata outside the CORS-safelisted set.
	h.Set("Access-Control-Expose-Headers", "*")
	h.Set("Access-Control-Max-Age", "86400")
}

func applyPreflight(h http.Header, r *http.Request) {
	Apply(h)
	// Echo the browser's requested headers so an arbitrary client header
	// (e.g. a custom auth header) clears preflight instead of being
	// rejected by our static default.
	if reqHdrs := r.Header.Get("Access-Control-Request-Headers"); reqHdrs != "" {
		h.Set("Access-Control-Allow-Headers", reqHdrs)
	}
}

// WritePreflight answers a browser's OPTIONS preflight locally with 204 plus
// the permissive fallback policy, and reports whether it handled the request.
// The proxies use this when no engine can be consulted (or before rejecting a
// non-loopback plaintext caller). When an engine is available, its preflight is
// forwarded instead so an exact origin and credentials policy can survive.
func WritePreflight(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodOptions {
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
