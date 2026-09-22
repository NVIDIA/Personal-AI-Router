// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cors

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAllowRequest(t *testing.T) {
	t.Run("no Origin header is allowed (non-browser callers)", func(t *testing.T) {
		if !AllowRequest(httptest.NewRequest(http.MethodPost, "/api/chat", nil)) {
			t.Fatal("an Origin-less request must be allowed")
		}
	})
	t.Run("empty allowlist denies every browser origin", func(t *testing.T) {
		t.Setenv(AllowedOriginsEnv, "")
		req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
		req.Header.Set("Origin", "https://evil.example")
		if AllowRequest(req) {
			t.Fatal("an unlisted Origin must be denied with an empty allowlist")
		}
	})
	t.Run("exact match required", func(t *testing.T) {
		t.Setenv(AllowedOriginsEnv, " https://ui.example ,http://localhost:5173 ")
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
