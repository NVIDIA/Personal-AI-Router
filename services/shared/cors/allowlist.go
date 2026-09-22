// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cors

import (
	"net/http"
	"os"
	"strings"
)

// The combining helpers in cors.go forward what engines declare; they never
// synthesize a grant. That is necessary but not sufficient at the request
// boundary: a browser page on any origin can reach the proxies' loopback
// listener, and a simple cross-origin POST needs no preflight, so header
// policy alone cannot stop an unlisted page from driving the local inference
// engines (a blind oracle). This file adds the deny-by-default allowlist gate
// that runs before a request is forwarded at all. The two layers compose: the
// gate admits only operator-listed origins, and Combine still intersects what
// the engines themselves permit.

// AllowedOriginsEnv names the operator-controlled origin allowlist. Exact
// origins only; an empty value (the default) admits no browser origins.
const AllowedOriginsEnv = "NVPAIR_PROXY_ALLOWED_ORIGINS"

// allowedOrigins returns the configured allowlist. Read per request so an
// operator change needs a proxy restart at most, not a code change per caller.
func allowedOrigins() []string {
	raw := strings.TrimSpace(os.Getenv(AllowedOriginsEnv))
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
