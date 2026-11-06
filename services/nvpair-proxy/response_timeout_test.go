// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The configured timeout governs both waits of an attempt. Raising only the
// header wait would leave an engine that sends headers early and then queues
// (LM Studio) cut off by the first-content wait at the old value.
func TestSetResponseTimeout_GovernsBothWaits(t *testing.T) {
	if responseHeaderTimeout != defaultResponseTimeout || firstBodyTimeout != defaultResponseTimeout {
		t.Fatalf("defaults = header %v, first body %v; want both %v",
			responseHeaderTimeout, firstBodyTimeout, defaultResponseTimeout)
	}
	setForTest(t, &responseHeaderTimeout, responseHeaderTimeout)
	setForTest(t, &firstBodyTimeout, firstBodyTimeout)

	setResponseTimeout(45 * time.Minute)
	if responseHeaderTimeout != 45*time.Minute || firstBodyTimeout != 45*time.Minute {
		t.Fatalf("after setResponseTimeout(45m): header %v, first body %v", responseHeaderTimeout, firstBodyTimeout)
	}
}

// The header timeout must reach every inference transport, and 0 must reach
// them as "no deadline".
func TestResponseHeaderTimeout_ReachesEveryTransport(t *testing.T) {
	for _, want := range []time.Duration{45 * time.Minute, 0} {
		setForTest(t, &responseHeaderTimeout, want)
		p := testProxy(anyProfile(t), NewDiscovery(), anyProfile(t).FacadePort)
		if got := p.plainHTTPTransport().ResponseHeaderTimeout; got != want {
			t.Errorf("plain transport ResponseHeaderTimeout = %v, want %v", got, want)
		}
		// Unclustered, a peer lookup falls back to a plain transport built the
		// same way.
		if got := p.peerHTTPTransport("no-such-peer").ResponseHeaderTimeout; got != want {
			t.Errorf("peer transport ResponseHeaderTimeout = %v, want %v", got, want)
		}
	}
}

// A non-streaming completion sends no header until generation ends. With a
// deadline shorter than that silence the request must fail; with a longer one,
// or none, the same request must succeed. This is what the old 120s broke for
// queued and long-running jobs.
func TestResponseHeaderTimeout_BoundsSilentUpstream(t *testing.T) {
	const silence = 300 * time.Millisecond
	tc := anyCase(t)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(silence):
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	defer upstream.Close()

	for _, c := range []struct {
		name    string
		timeout time.Duration
		ok      bool
	}{
		{"shorter than the silence", silence / 4, false},
		{"longer than the silence", 10 * silence, true},
		{"disabled", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			setForTest(t, &responseHeaderTimeout, c.timeout)
			setForTest(t, &jobDeadline, 2*time.Second)
			disc := NewDiscovery()
			disc.AddManual(nodeForModel(t, "serving-node", upstream.URL, tc.advertisedModel))
			p := testProxy(tc.profile, disc, tc.profile.FacadePort)

			rec := httptest.NewRecorder()
			p.soleFacade().handleHTTP(rec, tc.inferenceRequest())
			if got := rec.Code == http.StatusOK; got != c.ok {
				t.Fatalf("status = %d (%s), want success=%v", rec.Code, rec.Body.String(), c.ok)
			}
		})
	}
}

// A zero first-content budget waits for the engine rather than expiring at
// once: time.NewTimer(0) would fire immediately and fail every request.
func TestFirstBodyTimeout_ZeroWaitsForContent(t *testing.T) {
	setForTest(t, &firstBodyTimeout, 0)
	tc := anyCase(t)
	stalled := newStalledEngine(t)
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "slow", stalled.url, tc.advertisedModel))
	p := testProxy(tc.profile, disc, tc.profile.FacadePort)

	time.AfterFunc(300*time.Millisecond, stalled.release)
	rec := httptest.NewRecorder()
	p.soleFacade().handleHTTP(rec, tc.inferenceRequest())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d (%s), want 200 once the engine answers", rec.Code, rec.Body.String())
	}
	if got := stalled.hits(); got != 1 {
		t.Errorf("upstream saw %d dispatches, want 1: the wait must not expire and retry", got)
	}
}
