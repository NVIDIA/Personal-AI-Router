// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBufferBodyAndModelLimit covers the body-cap contract: a body over
// maxInferenceBodyBytes is reported too-large with the body dropped (never
// buffered into memory), and a body within the limit is returned with its
// parsed model field.
func TestBufferBodyAndModelLimit(t *testing.T) {
	t.Run("body over the limit is too large", func(t *testing.T) {
		big := bytes.Repeat([]byte("a"), maxInferenceBodyBytes+1)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(big))
		body, model, tooLarge := bufferBodyAndModel(req)
		if !tooLarge {
			t.Fatal("bufferBodyAndModel tooLarge = false, want true past the cap")
		}
		if body != nil {
			t.Error("body should be dropped (nil) when too large, not buffered")
		}
		if model != "" {
			t.Errorf("model = %q, want empty when too large", model)
		}
	})

	t.Run("body at the limit parses", func(t *testing.T) {
		atLimit := bytes.Repeat([]byte("a"), maxInferenceBodyBytes)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(atLimit))
		_, _, tooLarge := bufferBodyAndModel(req)
		if tooLarge {
			t.Fatal("bufferBodyAndModel tooLarge = true at exactly the cap, want false")
		}
	})

	t.Run("model parsed from a small body", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"llama3"}`))
		body, model, tooLarge := bufferBodyAndModel(req)
		if tooLarge || model != "llama3" || string(body) != `{"model":"llama3"}` {
			t.Fatalf("= (%q, %q, %v), want body kept, model llama3, not too large", body, model, tooLarge)
		}
	})
}

// TestHandleHTTPRejectsOversizedBody drives the 413 path end to end: a request
// body past maxInferenceBodyBytes is refused before any candidate resolution
// or upstream forward, with StatusRequestEntityTooLarge.
func TestHandleHTTPRejectsOversizedBody(t *testing.T) {
	p := testProxy(NewDiscovery(), 1235)
	big := bytes.Repeat([]byte("a"), maxInferenceBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(big))
	req.RemoteAddr = "127.0.0.1:40000"
	rec := httptest.NewRecorder()

	p.handleHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d, want %d", rec.Code, http.StatusRequestEntityTooLarge)
	}
}
