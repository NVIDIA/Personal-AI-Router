// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Commit is the first byte of response body, not the response headers, because
// headers can mean nothing more than "accepted" (spec §5.2 for the engine
// measurements behind that). These bodies pin the boundary: what fails over,
// what commits, what terminates, and what is left alone.
//
// firstBodyTimeout stands in at 200ms for its production 120s throughout, so a
// body that drives a node to its five-dispatch budget costs a second of wall
// clock rather than ten minutes.
const testFirstBodyTimeout = 200 * time.Millisecond

// stalledEngine is an upstream that sends 200 and headers immediately and then
// produces no body until released, which is the shape an engine presents for a
// streaming request queued behind other work.
type stalledEngine struct {
	url string
	// hits counts dispatches, so a test can show the retry budget was spent
	// rather than inferring it from timing.
	hits    func() int
	release func()
}

func newStalledEngine(t *testing.T) stalledEngine {
	t.Helper()
	var n atomic.Int32
	releaseCh := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(releaseCh) }) }

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // headers on the wire, body still empty
		}
		select {
		case <-releaseCh:
		case <-r.Context().Done():
		}
	}))
	// Cleanup is LIFO, so release runs before Close. Close waits for the
	// handler to return and the handler is parked on release, so the other
	// order deadlocks the end of the test.
	t.Cleanup(srv.Close)
	t.Cleanup(release)

	return stalledEngine{
		url:     srv.URL,
		hits:    func() int { return int(n.Load()) },
		release: release,
	}
}

// TestHandleHTTP_HeadersWithoutContentFailsOver is the regression for the
// engine that accepts and stalls. The first owner sends headers and no content;
// the request must move to the next owner rather than binding itself to a node
// that has not started generating.
func TestHandleHTTP_HeadersWithoutContentFailsOver(t *testing.T) {
	setForTest(t, &firstBodyTimeout, testFirstBodyTimeout)

	forEachEngine(t, func(t *testing.T, tc engineCase) {
		stalled := newStalledEngine(t)

		var servedBody string
		good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			servedBody = string(b)
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"done":true}`)
		}))
		defer good.Close()

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "stalled", stalled.url, tc.advertisedModel))
		disc.AddManual(nodeForModel(t, "good", good.URL, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)
		p.soleFacade().SetSelected("stalled") // deterministic: the staller is tried first

		rec := httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (should have failed over past the stalled engine)", rec.Code)
		}
		if got := decodeJSONBody(t, rec.Body.String()); got["done"] != true {
			t.Fatalf(`body has done = %v, want true: the response came from the wrong node`, got["done"])
		}
		if servedBody != tc.inferenceBody() {
			t.Errorf("failover node got body %q, want the original request body", servedBody)
		}
	})
}

// TestHandleHTTP_NoContentOnFinalAttemptTerminates covers the exhausted case,
// which is the one that could hang: a first-content timeout must not commit
// even with no attempt left, because the peek reader is still blocked on the
// upstream body (spec §5.2).
//
// 504 rather than 502: the node answered, it just never produced anything.
func TestHandleHTTP_NoContentOnFinalAttemptTerminates(t *testing.T) {
	setForTest(t, &firstBodyTimeout, testFirstBodyTimeout)

	forEachEngine(t, func(t *testing.T, tc engineCase) {
		// The only owner, so every attempt in the budget goes to it and the
		// last one has nowhere to fail over to.
		stalled := newStalledEngine(t)

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "silent", stalled.url, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)

		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			p.soleFacade().handleHTTP(rec, tc.inferenceRequest())
		}()

		select {
		case <-done:
		// Five attempts at the shortened budget is about a second. Any finite
		// guard catches the regression this test exists for, since a committed
		// silent upstream blocks forever.
		case <-time.After(5 * time.Second):
			t.Fatal("handleHTTP never returned: a silent upstream committed and hung the request")
		}

		if got := stalled.hits(); got != maxDispatchAttempts {
			t.Errorf("upstream saw %d dispatches, want the full budget of %d spent on the only owner", got, maxDispatchAttempts)
		}
		if rec.Code != http.StatusGatewayTimeout {
			t.Fatalf("status = %d, want 504 for an upstream that answered but produced no content", rec.Code)
		}
		want := "upstream error: " + errFirstBodyTimeout.Error()
		if got := decodeJSONBody(t, rec.Body.String()); got["error"] != want {
			t.Fatalf("body error = %v, want %q", got["error"], want)
		}
	})
}

// TestHandleHTTP_FirstBytePreservedOnCommit guards the splice: the byte
// awaitFirstBody consumes to detect the commit has to be handed back, or every
// committed response would be served a byte short. Compared bytewise rather
// than as JSON, since losing the first byte is exactly what would still parse
// as something.
func TestHandleHTTP_FirstBytePreservedOnCommit(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		const payload = `{"response":"whole body intact","done":true}`
		good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, payload)
		}))
		defer good.Close()

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "good", good.URL, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)

		rec := httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Body.String(); got != payload {
			t.Fatalf("body = %q, want %q (the peeked first byte was dropped)", got, payload)
		}
	})
}

// TestHandleHTTP_EmptyBodyCommits: a 200 with no body at all commits rather
// than burning the budget waiting for content that is never coming (spec §5.2).
func TestHandleHTTP_EmptyBodyCommits(t *testing.T) {
	// Long, so a wait would be unmistakable: if the empty body were treated as
	// a stall, the guard below would trip well before this expires.
	setForTest(t, &firstBodyTimeout, 30*time.Second)

	forEachEngine(t, func(t *testing.T, tc engineCase) {
		empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer empty.Close()

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "empty", empty.URL, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)

		done := make(chan int, 1)
		go func() {
			rec := httptest.NewRecorder()
			p.soleFacade().handleHTTP(rec, tc.inferenceRequest())
			done <- rec.Code
		}()

		select {
		case code := <-done:
			if code != http.StatusOK {
				t.Fatalf("status = %d, want 200", code)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("an empty 200 body blocked on the first-byte wait instead of committing")
		}
	})
}

// TestHandleHTTP_NonInferenceCommitsOnHeaders: the gate is scoped to inference.
// A control endpoint's headers mean what they say, and some of them (a pull's
// progress stream) legitimately produce their first byte late, so waiting there
// would add latency for no benefit.
func TestHandleHTTP_NonInferenceCommitsOnHeaders(t *testing.T) {
	setForTest(t, &firstBodyTimeout, testFirstBodyTimeout)

	forEachEngine(t, func(t *testing.T, tc engineCase) {
		stalled := newStalledEngine(t)

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "stalled", stalled.url, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)

		rec := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			req := httptest.NewRequest(http.MethodGet, tc.nonInferencePath, nil)
			p.soleFacade().handleHTTP(rec, req)
			close(done)
		}()

		// Still blocked on the upstream body well past the first-content
		// budget, which is the whole assertion: if the gate applied here the
		// handler would have given up at firstBodyTimeout and returned 504,
		// and simply waiting for it to finish would pass either way.
		blocked := 2 * testFirstBodyTimeout
		time.Sleep(blocked)
		select {
		case <-done:
			t.Fatalf("handler returned after %v with status %d: the first-content gate was applied to a non-inference route",
				blocked, rec.Code)
		default:
		}

		stalled.release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("a non-inference request did not complete after the upstream released")
		}
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: the request committed on headers and streamed normally", rec.Code)
		}
	})
}
