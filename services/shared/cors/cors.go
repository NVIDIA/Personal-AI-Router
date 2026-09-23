// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cors combines upstream HTTP permissions without defining engine policy.
// Forwarded responses must not be supplemented with default CORS permissions.
package cors

import (
	"net/http"
	"strconv"
	"strings"
)

// preflightMaxAgeSeconds is how long a browser may reuse a combined preflight
// result. The combination is an intersection over a changing set of engines,
// so it is deliberately short.
const preflightMaxAgeSeconds = 60

func IsPreflight(r *http.Request) bool {
	return r.Method == http.MethodOptions && r.Header.Get("Origin") != "" && r.Header.Get("Access-Control-Request-Method") != ""
}

// AllowsOrigin checks uncredentialed sharing. Credentialed sharing is separately
// intersected by Combine because a request's credentials mode is not observable.
func AllowsOrigin(h http.Header, origin string) bool {
	values := h.Values("Access-Control-Allow-Origin")
	return origin != "" && len(values) == 1 && (values[0] == origin || values[0] == "*")
}
func credentials(h http.Header, origin string) bool {
	values := h.Values("Access-Control-Allow-Credentials")
	return h.Get("Access-Control-Allow-Origin") == origin && len(values) == 1 && values[0] == "true"
}
func tokens(values []string) ([]string, bool) {
	var out []string
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			token = strings.TrimSpace(token)
			if token == "" {
				continue
			}
			if !validToken(token) {
				return nil, false
			}
			out = append(out, token)
		}
	}
	return out, true
}
func permits(list []string, token string, fold, wildcard bool) bool {
	for _, allowed := range list {
		if allowed == token || (fold && strings.EqualFold(allowed, token)) || (wildcard && allowed == "*") {
			return true
		}
	}
	return false
}
func permitsPreflight(h http.Header, method string, headers []string, credentialed bool) bool {
	methods, ok := tokens(h.Values("Access-Control-Allow-Methods"))
	if !ok {
		return false
	}
	allowedHeaders, ok := tokens(h.Values("Access-Control-Allow-Headers"))
	if !ok {
		return false
	}
	if method != "GET" && method != "HEAD" && method != "POST" && !permits(methods, method, false, !credentialed) {
		return false
	}
	for _, header := range headers {
		if !permits(allowedHeaders, header, true, !credentialed && !strings.EqualFold(header, "Authorization")) {
			return false
		}
	}
	return true
}

// Combine grants only permissions supported by every response. Callers separately
// validate statuses and bodies. Single forwarded responses are relayed unchanged.
func Combine(r *http.Request, responses []http.Header) (http.Header, bool) {
	origin := r.Header.Get("Origin")
	if origin == "" || len(responses) == 0 {
		return nil, false
	}
	preflight := IsPreflight(r)
	method := r.Header.Get("Access-Control-Request-Method")
	requested, ok := tokens(r.Header.Values("Access-Control-Request-Headers"))
	if preflight && (!ok || !validToken(method)) {
		return nil, false
	}
	out := make(http.Header)
	credentialed := true
	for _, h := range responses {
		if !AllowsOrigin(h, origin) || (preflight && !permitsPreflight(h, method, requested, false)) {
			return nil, false
		}
		credentialed = credentialed && credentials(h, origin) && (!preflight || permitsPreflight(h, method, requested, true))
		for _, vary := range h.Values("Vary") {
			out.Add("Vary", vary)
		}
	}
	out.Add("Vary", "Origin")
	out.Set("Access-Control-Allow-Origin", origin)
	if credentialed {
		out.Set("Access-Control-Allow-Credentials", "true")
	}
	if preflight {
		out.Add("Vary", "Access-Control-Request-Method, Access-Control-Request-Headers")
		out.Set("Access-Control-Allow-Methods", method)
		if len(requested) > 0 {
			out.Set("Access-Control-Allow-Headers", strings.Join(requested, ", "))
		}
		// Bound rather than forbid preflight caching. Zero made a browser
		// preflight every request, and each preflight fans out to every
		// candidate engine. A short window keeps that cost off the hot path
		// while limiting how long a stale intersection can be reused; the
		// actual response's headers are still computed live, so a permission
		// withdrawn mid-window still blocks the request.
		out.Set("Access-Control-Max-Age", strconv.Itoa(preflightMaxAgeSeconds))
	}
	return out, true
}

// EndToEndHeaders retains headers for forwarded requests and responses,
// excluding connection-specific fields as a reverse proxy does.
func EndToEndHeaders(h http.Header) http.Header {
	out := h.Clone()
	for _, value := range out.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			out.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range []string{"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade"} {
		out.Del(name)
	}
	return out
}

// FanoutHeaders preserves CORS inputs without sharing caller credentials across
// engines. Credentials for a proxy endpoint do not authorize every peer.
func FanoutHeaders(h http.Header) http.Header {
	out := EndToEndHeaders(h)
	out.Del("Authorization")
	out.Del("Cookie")
	return out
}

// tokenSpecials is the non-alphanumeric half of RFC 9110's `tchar` set. The
// backtick is an ordinary character in an interpreted string literal.
const tokenSpecials = "!#$%&'*+-.^_`|~"

// validToken implements the HTTP token grammar for field names and methods.
func validToken(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' {
			continue
		}
		if !strings.ContainsRune(tokenSpecials, rune(c)) {
			return false
		}
	}
	return true
}
