// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestApplyWithNoOriginWritesNothing(t *testing.T) {
	h := http.Header{}
	Apply(h, "")
	if got := h.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want nothing for an Origin-less caller", got)
	}
}

func TestApplyWithAllowlistedOrigin(t *testing.T) {
	h := http.Header{}
	Apply(h, "https://ui.example")
	if got := h.Get("Access-Control-Allow-Origin"); got != "https://ui.example" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the exact allowlisted origin", got)
	}
	if got := h.Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin so caches key on it", got)
	}
	if got := h.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want unset", got)
	}
	if got := h.Get("Access-Control-Expose-Headers"); got != "" {
		t.Errorf("Access-Control-Expose-Headers = %q, want unset (no wildcard header exposure)", got)
	}
}

func TestAllowRequest(t *testing.T) {
	t.Run("no Origin header is allowed (non-browser callers)", func(t *testing.T) {
		if !AllowRequest(httptest.NewRequest(http.MethodPost, "/api/chat", nil)) {
			t.Fatal("an Origin-less request must be allowed")
		}
	})
	t.Run("empty allowlist denies every browser origin", func(t *testing.T) {
		t.Setenv(allowedOriginsEnv, "")
		req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
		req.Header.Set("Origin", "https://evil.example")
		if AllowRequest(req) {
			t.Fatal("an unlisted Origin must be denied with an empty allowlist")
		}
	})
	t.Run("exact match required", func(t *testing.T) {
		t.Setenv(allowedOriginsEnv, " https://ui.example ,http://localhost:5173 ")
		allowed := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
		allowed.Header.Set("Origin", "https://ui.example")
		if !AllowRequest(allowed) {
			t.Fatal("an exactly-allowlisted Origin must be allowed")
		}
		prefix := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
		prefix.Header.Set("Origin", "https://ui.example.evil.example")
		if AllowRequest(prefix) {
			t.Fatal("a same-suffix origin must not match an allowlist entry as a prefix")
		}
		scheme := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
		scheme.Header.Set("Origin", "http://ui.example")
		if AllowRequest(scheme) {
			t.Fatal("a scheme-swapped origin must not match")
		}
	})
}

func TestRejectOriginShape(t *testing.T) {
	rec := httptest.NewRecorder()
	RejectOrigin(rec)
	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestWritePreflightGatesOnOrigin(t *testing.T) {
	t.Run("disallowed origin falls through unhandled", func(t *testing.T) {
		t.Setenv(allowedOriginsEnv, "")
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
		req.Header.Set("Origin", "https://evil.example")
		if WritePreflight(rec, req) {
			t.Fatal("WritePreflight = true for an unlisted origin, want fall-through")
		}
	})
	t.Run("allowlisted origin gets a 204 with the static grant", func(t *testing.T) {
		t.Setenv(allowedOriginsEnv, "https://ui.example")
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
		req.Header.Set("Origin", "https://ui.example")
		req.Header.Set("Access-Control-Request-Headers", "X-Custom-Token")
		if !WritePreflight(rec, req) {
			t.Fatal("WritePreflight = false for an allowlisted origin")
		}
		if rec.Code != http.StatusNoContent {
			t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://ui.example" {
			t.Errorf("Access-Control-Allow-Origin = %q, want the exact origin", got)
		}
		// Arbitrary request headers are deliberately NOT echoed: a header
		// outside the static grant must fail preflight.
		if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "Content-Type, Authorization" {
			t.Errorf("Access-Control-Allow-Headers = %q, want the static grant (no echo)", got)
		}
	})
	t.Run("non-OPTIONS falls through", func(t *testing.T) {
		rec := httptest.NewRecorder()
		if WritePreflight(rec, httptest.NewRequest(http.MethodPost, "/api/chat", nil)) {
			t.Fatal("WritePreflight(POST) = true, want the request left to the caller")
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q, want no headers written", got)
		}
	})
}

func TestCompletePreflightFallbackPreservesEnginePolicy(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/api/chat", nil)
	req.Header.Set("Origin", "https://app.example")
	resp := &http.Response{
		StatusCode: http.StatusNoContent,
		Status:     "204 No Content",
		Header: http.Header{
			"Access-Control-Allow-Origin":      []string{"https://app.example"},
			"Access-Control-Allow-Credentials": []string{"true"},
		},
		Body:    http.NoBody,
		Request: req,
	}

	if CompletePreflightFallback(resp) {
		t.Fatal("CompletePreflightFallback = true, want the engine policy preserved")
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the engine's exact origin", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "true" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want the engine's true", got)
	}
}

func TestCompletePreflightFallbackReplacesMissingEnginePolicy(t *testing.T) {
	t.Setenv(allowedOriginsEnv, "https://app.example")
	req := httptest.NewRequest(http.MethodOptions, "/api/chat", nil)
	req.Header.Set("Origin", "https://app.example")
	req.Header.Set("Access-Control-Request-Headers", "X-Custom-Token")
	resp := &http.Response{
		StatusCode:    http.StatusNotFound,
		Status:        "404 Not Found",
		Header:        http.Header{"Content-Type": []string{"text/plain"}},
		Body:          http.NoBody,
		ContentLength: 0,
		Request:       req,
	}

	if !CompletePreflightFallback(resp) {
		t.Fatal("CompletePreflightFallback = false, want the local fallback applied")
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://app.example" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the exact origin", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "Content-Type, Authorization" {
		t.Errorf("Access-Control-Allow-Headers = %q, want the static grant", got)
	}
	if resp.Body != http.NoBody || resp.ContentLength != 0 {
		t.Errorf("fallback body = %#v with length %d, want http.NoBody with length 0", resp.Body, resp.ContentLength)
	}
	if got := resp.Header.Get("Content-Type"); got != "" {
		t.Errorf("Content-Type = %q, want removed from the empty 204", got)
	}
}

func TestCompletePreflightFallbackDropsCredentialsWithoutOrigin(t *testing.T) {
	t.Setenv(allowedOriginsEnv, "")
	req := httptest.NewRequest(http.MethodOptions, "/api/chat", nil)
	resp := &http.Response{
		StatusCode: http.StatusNoContent,
		Status:     "204 No Content",
		Header:     http.Header{"Access-Control-Allow-Credentials": []string{"true"}},
		Body:       http.NoBody,
		Request:    req,
	}

	// No Origin on the request and an empty allowlist: originAllowed is true
	// (the Origin-absent branch), so the fallback still applies and must not
	// leave the credentials header behind to invalidate the exact-origin
	// response it just wrote.
	if !CompletePreflightFallback(resp) {
		t.Fatal("CompletePreflightFallback = false, want the local fallback applied")
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want nothing for an Origin-less caller", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want cleared", got)
	}
}
