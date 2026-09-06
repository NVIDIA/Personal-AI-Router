// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"nvpair-shared/ingressauth"
)

const (
	lanKey    = "0123456789abcdef0123456789abcdef"
	lanRemote = "192.0.2.50:40000"
)

// authEngine is an httptest engine that records the headers of the last request
// it served, so a test can prove what the proxy did and did not forward.
func authEngine(t *testing.T) (*httptest.Server, *atomic.Pointer[http.Header]) {
	t.Helper()
	var seen atomic.Pointer[http.Header]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Clone()
		seen.Store(&h)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// lanProxy is a proxy with one routable engine and the API-key gate enabled for
// lanKey, optionally restricted to cidrs.
func lanProxy(t *testing.T, cidrs ...netip.Prefix) (*Proxy, *atomic.Pointer[http.Header]) {
	t.Helper()
	engine, seen := authEngine(t)
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "engine", engine.URL, "llama"))
	p := testProxy(disc, 11435)
	p.lanAuth = ingressauth.New([]string{lanKey}, cidrs)
	return p, seen
}

func inferenceRequest(remote string, hdr ...string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"llama","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.RemoteAddr = remote
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	return req
}

func ingressCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("ingress error body %q is not JSON: %v", rec.Body.String(), err)
	}
	return body.Code
}

// TestHandlePlainAuthenticatedNonLoopbackIsRouted: with the gate enabled, a LAN
// caller presenting a configured key is routed through the full local router
// like a loopback client, and the key is stripped before the engine sees it.
func TestHandlePlainAuthenticatedNonLoopbackIsRouted(t *testing.T) {
	p, seen := lanProxy(t)
	rec := httptest.NewRecorder()
	p.handlePlain(rec, inferenceRequest(lanRemote, "Authorization", "Bearer "+lanKey))

	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated LAN status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	h := seen.Load()
	if h == nil {
		t.Fatal("engine never received the routed request")
	}
	if got := h.Get("Authorization"); got != "" {
		t.Errorf("engine received Authorization = %q, want the proxy's key stripped", got)
	}
	if got := h.Get("X-Api-Key"); got != "" {
		t.Errorf("engine received X-Api-Key = %q, want stripped", got)
	}
}

func TestHandlePlainXApiKeyAccepted(t *testing.T) {
	p, seen := lanProxy(t)
	rec := httptest.NewRecorder()
	p.handlePlain(rec, inferenceRequest(lanRemote, "X-Api-Key", lanKey))

	if rec.Code != http.StatusOK {
		t.Fatalf("X-Api-Key status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if h := seen.Load(); h == nil || h.Get("X-Api-Key") != "" {
		t.Fatalf("engine headers = %v, want the request forwarded without X-Api-Key", h)
	}
}

// TestHandlePlainNonLoopbackWithoutKeyIs401: an enabled gate turns the LAN
// refusal from 403 loopback-only into 401 with a challenge, still carrying CORS
// so a browser can read it, and nothing is forwarded.
func TestHandlePlainNonLoopbackWithoutKeyIs401(t *testing.T) {
	p, seen := lanProxy(t)
	rec := httptest.NewRecorder()
	p.handlePlain(rec, inferenceRequest(lanRemote))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-key LAN status = %d, want 401", rec.Code)
	}
	if got := ingressCode(t, rec); got != ingressauth.CodeUnauthorized {
		t.Errorf("code = %q, want %q", got, ingressauth.CodeUnauthorized)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != `Bearer realm="nvpair-proxy"` {
		t.Errorf("WWW-Authenticate = %q, want a Bearer challenge", got)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want * on the refusal", got)
	}
	if strings.Contains(rec.Body.String(), lanKey) {
		t.Error("refusal body echoes a key")
	}
	if seen.Load() != nil {
		t.Fatal("an unauthenticated LAN request reached the engine")
	}
}

func TestHandlePlainNonLoopbackWrongKeyIs401(t *testing.T) {
	p, seen := lanProxy(t)
	rec := httptest.NewRecorder()
	p.handlePlain(rec, inferenceRequest(lanRemote, "Authorization", "Bearer "+lanKey+"-not"))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-key LAN status = %d, want 401", rec.Code)
	}
	if got := ingressCode(t, rec); got != ingressauth.CodeUnauthorized {
		t.Errorf("code = %q, want %q", got, ingressauth.CodeUnauthorized)
	}
	if seen.Load() != nil {
		t.Fatal("a request with a wrong key reached the engine")
	}
}

// TestHandlePlainLoopbackNeedsNoKeyWhenEnabled: enabling the gate changes
// nothing for loopback — no key is required, and a client's own Authorization
// header (an SDK placeholder, say) is forwarded untouched as it is today.
func TestHandlePlainLoopbackNeedsNoKeyWhenEnabled(t *testing.T) {
	p, seen := lanProxy(t)
	rec := httptest.NewRecorder()
	p.handlePlain(rec, inferenceRequest("127.0.0.1:40000", "Authorization", "Bearer lm-studio"))

	if rec.Code != http.StatusOK {
		t.Fatalf("loopback status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	h := seen.Load()
	if h == nil {
		t.Fatal("engine never received the loopback request")
	}
	if got := h.Get("Authorization"); got != "Bearer lm-studio" {
		t.Errorf("loopback Authorization forwarded as %q, want it untouched", got)
	}
}

// TestHandlePlainOutsideAllowedCIDRIs403: with an allowlist configured, a
// caller outside it is refused before its key is examined — a valid key does not
// help, and no Bearer challenge is issued.
func TestHandlePlainOutsideAllowedCIDRIs403(t *testing.T) {
	p, seen := lanProxy(t, netip.MustParsePrefix("10.0.0.0/8"))
	rec := httptest.NewRecorder()
	p.handlePlain(rec, inferenceRequest(lanRemote, "Authorization", "Bearer "+lanKey))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("out-of-allowlist status = %d, want 403", rec.Code)
	}
	if got := ingressCode(t, rec); got != ingressauth.CodeSourceNotAllowed {
		t.Errorf("code = %q, want %q", got, ingressauth.CodeSourceNotAllowed)
	}
	if got := rec.Header().Get("WWW-Authenticate"); got != "" {
		t.Errorf("WWW-Authenticate = %q, want none on a source refusal", got)
	}
	if seen.Load() != nil {
		t.Fatal("a request from outside the allowlist reached the engine")
	}
}

func TestHandlePlainInsideAllowedCIDRIsRouted(t *testing.T) {
	p, seen := lanProxy(t, netip.MustParsePrefix("192.0.2.0/24"))
	rec := httptest.NewRecorder()
	p.handlePlain(rec, inferenceRequest(lanRemote, "Authorization", "Bearer "+lanKey))

	if rec.Code != http.StatusOK {
		t.Fatalf("in-allowlist status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if seen.Load() == nil {
		t.Fatal("engine never received the request")
	}
}

// TestHandlePlainPreflightStillAnsweredWhenEnabled: a browser sends no
// Authorization on a preflight, so the 204 must keep preceding the gate.
func TestHandlePlainPreflightStillAnsweredWhenEnabled(t *testing.T) {
	p, seen := lanProxy(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.RemoteAddr = lanRemote
	req.Header.Set("Access-Control-Request-Headers", "Authorization")
	p.handlePlain(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("LAN preflight status = %d, want 204", rec.Code)
	}
	if seen.Load() != nil {
		t.Fatal("a preflight reached the engine")
	}
}

// TestHandlePlainGateWithoutKeysKeepsLoopbackOnly: a gate that exists but has no
// keys is the default: the LAN refusal stays the original 403 loopback-only.
func TestHandlePlainGateWithoutKeysKeepsLoopbackOnly(t *testing.T) {
	p, seen := lanProxy(t)
	p.lanAuth = ingressauth.New(nil, nil)
	rec := httptest.NewRecorder()
	p.handlePlain(rec, inferenceRequest(lanRemote, "Authorization", "Bearer "+lanKey))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if got := ingressCode(t, rec); got != "loopback-only" {
		t.Errorf("code = %q, want loopback-only", got)
	}
	if seen.Load() != nil {
		t.Fatal("a LAN request reached the engine with no keys configured")
	}
}
