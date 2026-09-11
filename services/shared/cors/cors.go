// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cors enforces the inference proxies' browser-origin boundary.
// PAIR does not expose a browser API: supported local clients are native
// processes, including Electron's main process. Browser-marked requests are
// rejected before routing, and an Access-Control-Allow-Origin supplied by an
// engine is removed before a response crosses the proxy boundary.
package cors

import (
	"net/http"
	"strings"
)

const browserRequestError = `{"error":"browser-originated requests are not supported","code":"browser-origin"}`

// RejectBrowserRequest rejects requests carrying browser-controlled CORS or
// Fetch Metadata headers. It returns true after writing the response.
func RejectBrowserRequest(w http.ResponseWriter, r *http.Request) bool {
	// TODO: If PAIR adds a supported browser client, replace blanket rejection
	// here with an exact, user-configured origin allowlist (scheme, host, and
	// port), and return that origin from the response path below. Localhost
	// origins should be allowed only when the user deliberately enables them.
	if !isBrowserRequest(r.Header) {
		return false
	}

	StripAllowOrigin(w.Header())
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(browserRequestError))
	return true
}

// isBrowserRequest identifies headers added by web browsers. If we see one, the
// request came from a browser or a client choosing to look like one; the policy
// rejects both.
// See:
//   - https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Origin
//   - https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Sec-Fetch-Site
func isBrowserRequest(h http.Header) bool {
	for name := range h {
		canonical := http.CanonicalHeaderKey(name)
		if canonical == "Origin" || strings.HasPrefix(canonical, "Sec-Fetch-") ||
			strings.HasPrefix(canonical, "Access-Control-Request-") {
			return true
		}
	}
	return false
}

// StripAllowOrigin prevents an engine from widening PAIR's browser-origin
// boundary while preserving every other part of the engine response.
func StripAllowOrigin(h http.Header) {
	h.Del("Access-Control-Allow-Origin")
}
