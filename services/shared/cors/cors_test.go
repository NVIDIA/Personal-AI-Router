// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package cors

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func preflight() *http.Request {
	r := httptest.NewRequest("OPTIONS", "/api/chat?test=1", nil)
	r.Header.Set("Origin", "http://app.test")
	r.Header.Set("Access-Control-Request-Method", "PUT")
	r.Header.Set("Access-Control-Request-Headers", "Content-Type, Authorization")
	return r
}
func policy() http.Header {
	h := make(http.Header)
	h.Set("Access-Control-Allow-Origin", "http://app.test")
	h.Set("Access-Control-Allow-Methods", "GET, PUT")
	h.Set("Access-Control-Allow-Headers", "content-type, authorization")
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Add("Vary", "Accept-Encoding")
	return h
}
func TestCombine(t *testing.T) {
	for _, tc := range []struct {
		name           string
		change         func(http.Header)
		allowed, creds bool
	}{
		{"exact", func(h http.Header) {}, true, true},
		{"missing origin", func(h http.Header) { h.Del("Access-Control-Allow-Origin") }, false, false},
		{"wrong origin", func(h http.Header) { h.Set("Access-Control-Allow-Origin", "http://wrong.test") }, false, false},
		{"duplicate origin", func(h http.Header) { h.Add("Access-Control-Allow-Origin", "http://app.test") }, false, false},
		{"wildcard origin", func(h http.Header) { h.Set("Access-Control-Allow-Origin", "*") }, true, false},
		{"no credentials", func(h http.Header) { h.Del("Access-Control-Allow-Credentials") }, true, false},
		{"missing method", func(h http.Header) { h.Del("Access-Control-Allow-Methods") }, false, false},
		{"case sensitive method", func(h http.Header) { h.Set("Access-Control-Allow-Methods", "put") }, false, false},
		{"missing header", func(h http.Header) { h.Set("Access-Control-Allow-Headers", "Content-Type") }, false, false},
		{"wildcard does not grant authorization", func(h http.Header) { h.Set("Access-Control-Allow-Headers", "*") }, false, false},
		{"wildcard plus authorization", func(h http.Header) { h.Set("Access-Control-Allow-Headers", "*, Authorization") }, true, false},
		{"wildcard method", func(h http.Header) { h.Set("Access-Control-Allow-Methods", "*") }, true, false},
		{"invalid list", func(h http.Header) { h.Set("Access-Control-Allow-Headers", "Content-Type, bad header") }, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			second := policy()
			tc.change(second)
			h, ok := Combine(preflight(), []http.Header{policy(), second})
			if ok != tc.allowed {
				t.Fatalf("allowed=%v want %v", ok, tc.allowed)
			}
			if !ok {
				return
			}
			if (h.Get("Access-Control-Allow-Credentials") == "true") != tc.creds {
				t.Fatal("credentials widened")
			}
			if h.Get("Access-Control-Allow-Origin") != "http://app.test" || h.Get("Access-Control-Allow-Methods") != "PUT" {
				t.Fatalf("bad combined headers: %v", h)
			}
			// Bounded, not disabled: zero made every browser request pay for a
			// preflight fan-out to every candidate engine.
			maxAge, err := strconv.Atoi(h.Get("Access-Control-Max-Age"))
			if err != nil || maxAge <= 0 || maxAge > 600 {
				t.Fatalf("preflight max-age is not a short positive window: %q", h.Get("Access-Control-Max-Age"))
			}
			if !strings.Contains(strings.Join(h.Values("Vary"), ","), "Accept-Encoding") {
				t.Fatal("lost Vary")
			}
		})
	}
}

func TestCombineSafelistedAndOrdinaryRequests(t *testing.T) {
	r := preflight()
	r.Header.Set("Access-Control-Request-Method", "GET")
	r.Header.Del("Access-Control-Request-Headers")
	h := policy()
	h.Del("Access-Control-Allow-Methods")
	h.Del("Access-Control-Allow-Headers")
	if _, ok := Combine(r, []http.Header{h}); !ok {
		t.Fatal("safelisted method needs no explicit grant")
	}
	r.Method = "GET"
	if _, ok := Combine(r, []http.Header{h}); !ok {
		t.Fatal("ordinary response")
	}
	if _, ok := Combine(r, nil); ok {
		t.Fatal("empty set allowed")
	}
	r.Header.Del("Origin")
	if _, ok := Combine(r, []http.Header{h}); ok {
		t.Fatal("invented origin")
	}
	if IsPreflight(r) {
		t.Fatal("GET is not preflight")
	}
}
func TestEndToEndHeaders(t *testing.T) {
	h := http.Header{
		"Origin":        {"http://app.test"},
		"Authorization": {"Bearer test"},
		"Cookie":        {"test=1"},
		"Connection":    {"X-Hop, keep-alive"},
		"X-Hop":         {"private"},
		"Keep-Alive":    {"yes"},
	}
	got := EndToEndHeaders(h)
	if got.Get("X-Hop") != "" || got.Get("Keep-Alive") != "" || got.Get("Connection") != "" {
		t.Fatal("forwarded hop headers")
	}
	if got.Get("Origin") != h.Get("Origin") || got.Get("Authorization") != h.Get("Authorization") || got.Get("Cookie") != h.Get("Cookie") {
		t.Fatal("lost end-to-end headers")
	}
	if h.Get("X-Hop") == "" {
		t.Fatal("mutated caller")
	}
}
func target(s *httptest.Server) Target {
	u, _ := url.Parse(s.URL)
	return Target{URL: u, Transport: s.Client().Transport}
}
func TestFanoutStripsCredentials(t *testing.T) {
	request := preflight()
	request.Header.Set("Authorization", "Bearer private")
	request.Header.Set("Cookie", "session=private")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Error("fan-out shared caller credentials")
		}
		if r.Header.Get("Origin") != "http://app.test" || r.Header.Get("Access-Control-Request-Headers") != "Content-Type, Authorization" {
			t.Error("fan-out lost CORS inputs")
		}
		for key, values := range policy() {
			w.Header()[key] = values
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	rec := httptest.NewRecorder()
	ServePreflight(rec, request, []Target{target(server), target(server)})
	if rec.Code != http.StatusNoContent {
		t.Fatalf("status=%d", rec.Code)
	}
	if request.Header.Get("Authorization") != "Bearer private" || request.Header.Get("Cookie") != "session=private" {
		t.Fatal("mutated original request")
	}
}
func TestPreflightResponses(t *testing.T) {
	test := func(name string, status int, withPolicy bool, combinedStatus int) {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Header.Get("Origin") != "http://app.test" || r.URL.RawQuery != "test=1" || r.Header.Get("Access-Control-Request-Method") != "PUT" {
					t.Error("request changed")
				}
				if withPolicy {
					for k, v := range policy() {
						w.Header()[k] = v
					}
				}
				w.Header().Set("Location", "/redirect-target")
				w.WriteHeader(status)
				if status != http.StatusNoContent {
					if _, err := io.WriteString(w, "engine response"); err != nil {
						t.Error(err)
					}
				}
			}))
			defer server.Close()
			rec := httptest.NewRecorder()
			ServePreflight(rec, preflight(), []Target{target(server)})
			if rec.Code != status {
				t.Fatalf("single status %d", rec.Code)
			}
			if (rec.Header().Get("Access-Control-Allow-Origin") != "") != withPolicy {
				t.Fatal("single policy changed")
			}
			if status != http.StatusNoContent && rec.Body.String() != "engine response" {
				t.Fatal("single body changed")
			}
			if calls.Load() != 1 {
				t.Fatal("followed redirect")
			}
			rec = httptest.NewRecorder()
			ServePreflight(rec, preflight(), []Target{target(server), target(server)})
			want := combinedStatus
			if rec.Code != want {
				t.Fatalf("combined status=%d want=%d", rec.Code, want)
			}
			if want != http.StatusNoContent && rec.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("permission on failure")
			}
		})
	}
	test("allow", http.StatusNoContent, true, http.StatusNoContent)
	test("denial", http.StatusForbidden, false, http.StatusForbidden)
	test("no policy", http.StatusOK, false, http.StatusForbidden)
	test("redirect", http.StatusTemporaryRedirect, true, http.StatusForbidden)
	test("failure", http.StatusServiceUnavailable, false, http.StatusBadGateway)
}

func TestPreflightWithoutTargets(t *testing.T) {
	rec := httptest.NewRecorder()
	ServePreflight(rec, preflight(), nil)
	if rec.Code != http.StatusBadGateway || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatal("no engines granted preflight")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestBoundedFanoutAndCancellation(t *testing.T) {
	var active, max atomic.Int32
	started := make(chan struct{}, 20)
	transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := max.Load(); n > old; old = max.Load() {
			if max.CompareAndSwap(old, n) {
				break
			}
		}
		started <- struct{}{}
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	u, _ := url.Parse("http://engine.test")
	targets := make([]Target, 20)
	for i := range targets {
		targets[i] = Target{URL: u, Transport: transport}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { ServePreflight(rec, preflight().WithContext(ctx), targets); close(done) }()
	for i := 0; i < 8; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("fanout did not start")
		}
	}
	if max.Load() != 8 {
		t.Fatalf("active maximum %d", max.Load())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked")
	}
	if max.Load() > 8 || rec.Code != http.StatusBadGateway {
		t.Fatalf("max=%d status=%d", max.Load(), rec.Code)
	}
}
