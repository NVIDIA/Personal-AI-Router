// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The configured header timeout must reach every inference transport, and 0
// must mean "no deadline" rather than Go's default of none-set-yet.
func TestResponseHeaderTimeoutReachesEveryTransport(t *testing.T) {
	p := testProxy(NewDiscovery(), 11434)
	if p.responseHeaderTimeout != defaultResponseHeaderTimeout {
		t.Fatalf("default = %v, want %v", p.responseHeaderTimeout, defaultResponseHeaderTimeout)
	}
	for _, want := range []time.Duration{45 * time.Minute, 0} {
		p := testProxy(NewDiscovery(), 11434)
		p.responseHeaderTimeout = want
		if got := p.plainHTTPTransport().ResponseHeaderTimeout; got != want {
			t.Errorf("plain transport ResponseHeaderTimeout = %v, want %v", got, want)
		}
		// Unclustered, a peer lookup falls back to a plain transport built the same way.
		if got := p.peerHTTPTransport("no-such-peer").ResponseHeaderTimeout; got != want {
			t.Errorf("peer transport ResponseHeaderTimeout = %v, want %v", got, want)
		}
	}
}

// A non-streaming completion sends no header until generation ends. With a
// deadline shorter than that silence the request must fail as an upstream
// error; with a longer one the same request must succeed. This is the
// behaviour the 120s default broke for queued or long-running jobs.
func TestResponseHeaderTimeoutBoundsSilentUpstream(t *testing.T) {
	const silence = 500 * time.Millisecond
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(silence)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer upstream.Close()

	for _, tc := range []struct {
		name    string
		timeout time.Duration
		status  int
	}{
		{"shorter than the silence", silence / 3, http.StatusBadGateway},
		{"longer than the silence", 10 * silence, http.StatusOK},
		{"disabled", 0, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disc := NewDiscovery()
			disc.AddManual(nodeForModel(t, "serving-node", upstream.URL, "llama"))
			p := testProxy(disc, 11434)
			p.responseHeaderTimeout = tc.timeout

			rec := httptest.NewRecorder()
			p.handleHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/chat", strings.NewReader(`{"model":"llama"}`)))
			if rec.Code != tc.status {
				t.Fatalf("status = %d (%s), want %d", rec.Code, strings.TrimSpace(rec.Body.String()), tc.status)
			}
		})
	}
}
