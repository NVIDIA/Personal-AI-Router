// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cors

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestApplySetsPolicy(t *testing.T) {
	h := http.Header{}
	Apply(h)
	for header, want := range map[string]string{
		"Access-Control-Allow-Origin":   "*",
		"Access-Control-Expose-Headers": "*",
	} {
		if got := h.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if h.Get("Access-Control-Allow-Methods") == "" {
		t.Error("missing Access-Control-Allow-Methods")
	}
	// A wildcard origin is only valid for uncredentialed responses, so the
	// policy must never claim to allow credentials.
	if got := h.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want unset alongside a wildcard origin", got)
	}
}

// TestApplyClearsInheritedCredentials: Apply runs on forwarded responses too,
// where the header map is whatever the upstream sent. An upstream that emits
// Allow-Credentials without an origin of its own would otherwise leave the
// invalid wildcard + credentials pair, which a browser fails closed.
func TestApplyClearsInheritedCredentials(t *testing.T) {
	h := http.Header{"Access-Control-Allow-Credentials": []string{"true"}}
	Apply(h)

	if got := h.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := h.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want cleared alongside the wildcard origin", got)
	}
}

func TestWritePreflightEchoesRequestedHeaders(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	req.Header.Set("Access-Control-Request-Headers", "X-Custom-Token")

	if !WritePreflight(rec, req) {
		t.Fatal("WritePreflight(OPTIONS) = false, want the preflight handled")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusNoContent)
	}
	if got := rec.Header().Get("Access-Control-Allow-Headers"); got != "X-Custom-Token" {
		t.Errorf("Access-Control-Allow-Headers = %q, want echoed X-Custom-Token", got)
	}
}

// TestWritePreflightIgnoresOtherMethods: a real request must fall through to the
// caller's own handling untouched, with no response written.
func TestWritePreflightIgnoresOtherMethods(t *testing.T) {
	rec := httptest.NewRecorder()
	if WritePreflight(rec, httptest.NewRequest(http.MethodPost, "/api/chat", nil)) {
		t.Fatal("WritePreflight(POST) = true, want the request left to the caller")
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want no headers written", got)
	}
}

func TestCompletePreflightFallbackPreservesEnginePolicy(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/api/chat", nil)
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
	req := httptest.NewRequest(http.MethodOptions, "/api/chat", nil)
	req.Header.Set("Access-Control-Request-Headers", "X-Custom-Token")
	resp := &http.Response{
		StatusCode:    http.StatusNotFound,
		Status:        "404 Not Found",
		Header:        http.Header{"Content-Type": []string{"text/plain"}},
		Body:          io.NopCloser(strings.NewReader("not found")),
		ContentLength: 9,
		Request:       req,
	}

	if !CompletePreflightFallback(resp) {
		t.Fatal("CompletePreflightFallback = false, want the local fallback applied")
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Headers"); got != "X-Custom-Token" {
		t.Errorf("Access-Control-Allow-Headers = %q, want echoed X-Custom-Token", got)
	}
	if resp.Body != http.NoBody || resp.ContentLength != 0 {
		t.Errorf("fallback body = %#v with length %d, want http.NoBody with length 0", resp.Body, resp.ContentLength)
	}
	if got := resp.Header.Get("Content-Type"); got != "" {
		t.Errorf("Content-Type = %q, want removed from the empty 204", got)
	}
}

// TestCompletePreflightFallbackDropsCredentialsWithoutOrigin: an upstream
// preflight carrying Allow-Credentials but no origin has no policy to preserve,
// so the fallback applies — and must not leave the credentials header behind to
// invalidate the wildcard origin it just wrote.
func TestCompletePreflightFallbackDropsCredentialsWithoutOrigin(t *testing.T) {
	req := httptest.NewRequest(http.MethodOptions, "/api/chat", nil)
	resp := &http.Response{
		StatusCode: http.StatusNoContent,
		Status:     "204 No Content",
		Header:     http.Header{"Access-Control-Allow-Credentials": []string{"true"}},
		Body:       http.NoBody,
		Request:    req,
	}

	if !CompletePreflightFallback(resp) {
		t.Fatal("CompletePreflightFallback = false, want the local fallback applied")
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
	if got := resp.Header.Get("Access-Control-Allow-Credentials"); got != "" {
		t.Errorf("Access-Control-Allow-Credentials = %q, want cleared alongside the wildcard origin", got)
	}
}
