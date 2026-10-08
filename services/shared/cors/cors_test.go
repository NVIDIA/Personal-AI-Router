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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			assert.Equal(t, tc.allowed, ok, "allowed")
			if !ok {
				return
			}
			assert.Equal(t, tc.creds, (h.Get("Access-Control-Allow-Credentials") == "true"), "credentials widened")
			assert.Equal(t, "http://app.test", h.Get("Access-Control-Allow-Origin"), "bad combined headers")
			assert.Equal(t, "PUT", h.Get("Access-Control-Allow-Methods"), "bad combined headers")
			// Bounded, not disabled: zero made every browser request pay for a
			// preflight fan-out to every candidate engine.
			maxAge, err := strconv.Atoi(h.Get("Access-Control-Max-Age"))
			require.NoError(t, err, "preflight max-age is not a short positive window")
			assert.Positive(t, maxAge, "preflight max-age is not a short positive window")
			assert.LessOrEqual(t, maxAge, 600, "preflight max-age is not a short positive window")
			assert.Contains(t, strings.Join(h.Values("Vary"), ","), "Accept-Encoding", "lost Vary")
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
	{
		_, ok := Combine(r, []http.Header{h})
		require.True(t, ok, "safelisted method needs no explicit grant")
	}
	r.Method = "GET"
	{
		_, ok := Combine(r, []http.Header{h})
		require.True(t, ok, "ordinary response")
	}
	{
		_, ok := Combine(r, nil)
		require.False(t, ok, "empty set allowed")
	}
	r.Header.Del("Origin")
	{
		_, ok := Combine(r, []http.Header{h})
		require.False(t, ok, "invented origin")
	}
	assert.False(t, IsPreflight(r), "GET is not preflight")
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
	assert.Equal(t, "", got.Get("X-Hop"), "forwarded hop headers")
	assert.Equal(t, "", got.Get("Keep-Alive"), "forwarded hop headers")
	assert.Equal(t, "", got.Get("Connection"), "forwarded hop headers")
	assert.Equal(t, h.Get("Origin"), got.Get("Origin"), "lost end-to-end headers")
	assert.Equal(t, h.Get("Authorization"), got.Get("Authorization"), "lost end-to-end headers")
	assert.Equal(t, h.Get("Cookie"), got.Get("Cookie"), "lost end-to-end headers")
	assert.NotEqual(t, "", h.Get("X-Hop"), "mutated caller")
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
		assert.False(t, r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "", "fan-out shared caller credentials")
		assert.False(t, r.Header.Get("Origin") != "http://app.test" || r.Header.Get("Access-Control-Request-Headers") != "Content-Type, Authorization", "fan-out lost CORS inputs")
		for key, values := range policy() {
			w.Header()[key] = values
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	rec := httptest.NewRecorder()
	ServePreflight(rec, request, []Target{target(server), target(server)})
	assert.Equal(t, http.StatusNoContent, rec.Code)
	assert.Equal(t, "Bearer private", request.Header.Get("Authorization"), "mutated original request")
	assert.Equal(t, "session=private", request.Header.Get("Cookie"), "mutated original request")
}
func TestPreflightResponses(t *testing.T) {
	test := func(name string, status int, withPolicy bool, combinedStatus int) {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.False(t, r.Header.Get("Origin") != "http://app.test" || r.URL.RawQuery != "test=1" || r.Header.Get("Access-Control-Request-Method") != "PUT", "request changed")
				if withPolicy {
					for k, v := range policy() {
						w.Header()[k] = v
					}
				}
				w.Header().Set("Location", "/redirect-target")
				w.WriteHeader(status)
				if status != http.StatusNoContent {
					{
						_, err := io.WriteString(w, "engine response")
						assert.NoError(t, err)
					}
				}
			}))
			defer server.Close()
			rec := httptest.NewRecorder()
			ServePreflight(rec, preflight(), []Target{target(server)})
			assert.Equal(t, status, rec.Code, "single status")
			assert.Equal(t, withPolicy, (rec.Header().Get("Access-Control-Allow-Origin") != ""), "single policy changed")
			assert.False(t, status != http.StatusNoContent && rec.Body.String() != "engine response", "single body changed")
			assert.Equal(t, int32(1), calls.Load(), "followed redirect")
			rec = httptest.NewRecorder()
			ServePreflight(rec, preflight(), []Target{target(server), target(server)})
			want := combinedStatus
			assert.Equal(t, want, rec.Code, "combined status")
			assert.False(t, want != http.StatusNoContent && rec.Header().Get("Access-Control-Allow-Origin") != "", "permission on failure")
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
	assert.Equal(t, http.StatusBadGateway, rec.Code, "no engines granted preflight")
	assert.Equal(t, "", rec.Header().Get("Access-Control-Allow-Origin"), "no engines granted preflight")
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
			require.FailNow(t, "test expectation failed", "fanout did not start")
		}
	}
	assert.Equal(t, int32(8), max.Load(), "active maximum")
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		require.FailNow(t, "test expectation failed", "cancellation blocked")
	}
	assert.LessOrEqual(t, max.Load(), int32(8))
	assert.Equal(t, http.StatusBadGateway, rec.Code)
}
