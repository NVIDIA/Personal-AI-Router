// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cors

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRejectBrowserRequest_Allow(t *testing.T) {
	test := func(name, header, value string) {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
			req.Header[header] = []string{value}

			if RejectBrowserRequest(rec, req) {
				t.Fatal("RejectBrowserRequest = true, want request allowed")
			}
			if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
				t.Errorf("response was modified: status=%d body=%q", rec.Code, rec.Body.String())
			}
		})
	}

	test("authorization", "Authorization", "Bearer token")
	test("content type", "Content-Type", "application/json")
	test("user agent", "User-Agent", "native-client")
}

func TestRejectBrowserRequest_Reject(t *testing.T) {
	test := func(name, header, value string) {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			rec.Header().Set("Access-Control-Allow-Origin", "*")
			req := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
			req.Header[header] = []string{value}

			if !RejectBrowserRequest(rec, req) {
				t.Fatal("RejectBrowserRequest = false, want request rejected")
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want %d", rec.Code, http.StatusForbidden)
			}
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
				t.Errorf("Access-Control-Allow-Origin = %q, want absent", got)
			}
			if body := rec.Body.String(); !strings.Contains(body, `"code":"browser-origin"`) {
				t.Errorf("body = %q, want browser-origin code", body)
			}
		})
	}

	test("origin", "Origin", "https://attacker.example")
	test("null origin", "Origin", "null")
	test("empty origin", "Origin", "")
	test("fetch metadata", "Sec-Fetch-Site", "cross-site")
	test("preflight method", "Access-Control-Request-Method", http.MethodPost)
	test("preflight headers", "Access-Control-Request-Headers", "Authorization")
}

func TestStripAllowOrigin(t *testing.T) {
	h := http.Header{
		"Access-Control-Allow-Origin": []string{"https://app.example"},
		"X-Engine-Metadata":           []string{"preserved"},
	}

	StripAllowOrigin(h)

	if got := h.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("Access-Control-Allow-Origin = %q, want absent", got)
	}
	if got := h.Get("X-Engine-Metadata"); got != "preserved" {
		t.Errorf("X-Engine-Metadata = %q, want preserved", got)
	}
}
