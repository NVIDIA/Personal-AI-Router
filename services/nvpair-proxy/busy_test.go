// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The busy hold (spec §5.1, "A saturated owner") is the proxy's answer to a
// flood observed in production. A local client sent about a hundred inference
// requests a second for a model that only this node's own Ollama advertised;
// Ollama's queue filled and it answered 503 within milliseconds; and the proxy
// admitted every one of those requests as a queued workload and re-dispatched
// it into the same full queue five times over fifteen seconds. The Jobs view
// showed thousands of queued cards for minutes, and the engine saw five times
// the load. The hold keeps the per-request retry policy intact and refuses
// only NEW admissions while every eligible owner has just declared itself busy.

const busyBody = `{"error":"server busy, please try again"}`

// TestHandleHTTP_BusyHoldRefusesNewAdmissions: once the only owner has answered
// busy, a new request is refused at admission — a local 503 with Retry-After,
// no dispatch, and no workload — while the request that was already admitted
// still spent its whole budget on that owner, as §5.1 requires.
func TestHandleHTTP_BusyHoldRefusesNewAdmissions(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		// A minute-long hold makes the assertions immune to a stalled runner:
		// nothing in this body waits for the hold to lapse.
		setForTest(t, &busyHold, time.Minute)
		busyURL, hits := newCountingServer(t, http.StatusServiceUnavailable, busyBody)

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "saturated", busyURL, tc.advertisedModel))
		events := &recRW{}
		p := newTestProxy(tc.profile, NewCodec(events), disc, tc.profile.FacadePort)
		f := p.soleFacade()

		first := httptest.NewRecorder()
		f.handleHTTP(first, tc.inferenceRequest())
		if first.Code != http.StatusServiceUnavailable {
			t.Fatalf("admitted request status = %d, want the owner's own 503 passed through", first.Code)
		}
		if got := hits(); got != maxDispatchAttempts {
			t.Fatalf("owner saw %d dispatches for the admitted request, want %d: the hold must not shorten an admitted request's budget", got, maxDispatchAttempts)
		}
		submitted := events.count("workload:submitted")
		if submitted == 0 {
			t.Fatal("the admitted request never became a workload")
		}

		second := httptest.NewRecorder()
		f.handleHTTP(second, tc.inferenceRequest())
		if second.Code != http.StatusServiceUnavailable {
			t.Fatalf("refused request status = %d, want 503", second.Code)
		}
		if second.Header().Get("Retry-After") == "" {
			t.Fatal("refused request carries no Retry-After")
		}
		if msg, _ := decodeJSONBody(t, second.Body.String())["error"].(string); !strings.Contains(msg, "busy") {
			t.Fatalf("refused request body = %q, want an error naming the busy owners", second.Body.String())
		}
		if got := hits(); got != maxDispatchAttempts {
			t.Fatalf("owner saw %d dispatches after the refused request, want still %d: a refusal contacts no engine", got, maxDispatchAttempts)
		}
		if got := events.count("workload:submitted"); got != submitted {
			t.Fatalf("refused request emitted %d workload event(s); a request never admitted is not a job", got-submitted)
		}
		if !events.has(`"error":"every node advertising the requested model is busy"`) {
			t.Fatalf("refused request left no proxy/request record: %s", events.b)
		}
	})
}

// TestHandleHTTP_BusyHoldLapses: the hold is a pause, not a verdict. Once it
// lapses with no fresh busy answer, the next request is admitted and dispatched
// again, so an owner that recovered quietly is found without anyone probing it.
func TestHandleHTTP_BusyHoldLapses(t *testing.T) {
	tc := anyCase(t)
	setForTest(t, &busyHold, 20*time.Millisecond)
	busyURL, hits := newCountingServer(t, http.StatusServiceUnavailable, busyBody)

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "saturated", busyURL, tc.advertisedModel))
	p := testProxy(tc.profile, disc, tc.profile.FacadePort)
	f := p.soleFacade()

	f.handleHTTP(httptest.NewRecorder(), tc.inferenceRequest())
	if got := hits(); got != maxDispatchAttempts {
		t.Fatalf("owner saw %d dispatches, want %d", got, maxDispatchAttempts)
	}
	waitForCond(t, 2*time.Second, "the busy hold to lapse", func() bool {
		allBusy, _ := f.busyFor([]candidate{{id: "saturated"}}, time.Now())
		return !allBusy
	})

	f.handleHTTP(httptest.NewRecorder(), tc.inferenceRequest())
	if got := hits(); got != 2*maxDispatchAttempts {
		t.Fatalf("owner saw %d dispatches after the hold lapsed, want %d: the request must be admitted again", got, 2*maxDispatchAttempts)
	}
}

// TestHandleHTTP_BusyHoldClearedByCommit: a node that commits a response is
// accepting work, whatever it said a moment ago, so its hold is dropped at once
// rather than left to lapse. Without the clear, the minute-long hold armed by
// the first answer would refuse the second request.
func TestHandleHTTP_BusyHoldClearedByCommit(t *testing.T) {
	tc := anyCase(t)
	setForTest(t, &busyHold, time.Minute)
	var hits atomic.Int32
	recovering := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			io.WriteString(w, busyBody)
			return
		}
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	t.Cleanup(recovering.Close)

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "recovering", recovering.URL, tc.advertisedModel))
	p := testProxy(tc.profile, disc, tc.profile.FacadePort)
	f := p.soleFacade()

	first := httptest.NewRecorder()
	f.handleHTTP(first, tc.inferenceRequest())
	if first.Code != http.StatusOK || hits.Load() != 2 {
		t.Fatalf("first request: status %d after %d dispatches, want 200 after 2", first.Code, hits.Load())
	}
	second := httptest.NewRecorder()
	f.handleHTTP(second, tc.inferenceRequest())
	if second.Code != http.StatusOK || hits.Load() != 3 {
		t.Fatalf("second request: status %d after %d dispatches, want 200 after 3 (the commit must have cleared the hold)", second.Code, hits.Load())
	}
}

// TestHandleHTTP_BusyHoldNeedsEveryOwner: the hold refuses admission only when
// no eligible owner is free. One busy node among two changes nothing for a new
// request, which is admitted and served by the other after the usual failover.
func TestHandleHTTP_BusyHoldNeedsEveryOwner(t *testing.T) {
	tc := anyCase(t)
	setForTest(t, &busyHold, time.Minute)
	busyURL, busyHits := newCountingServer(t, http.StatusServiceUnavailable, busyBody)
	freeURL, freeHits := newCountingServer(t, http.StatusOK, `{"done":true}`)

	disc := NewDiscovery()
	// IDs sort the busy node first, so a request meets it before failing over.
	disc.AddManual(nodeForModel(t, "a-busy", busyURL, tc.advertisedModel))
	disc.AddManual(nodeForModel(t, "b-free", freeURL, tc.advertisedModel))
	p := testProxy(tc.profile, disc, tc.profile.FacadePort)
	f := p.soleFacade()

	for i := 1; i <= 2; i++ {
		rec := httptest.NewRecorder()
		f.handleHTTP(rec, tc.inferenceRequest())
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 via the free owner", i, rec.Code)
		}
	}
	if busyHits() == 0 {
		t.Fatal("the busy owner was never dispatched to, so the hold under test was never armed")
	}
	if allBusy, _ := f.busyFor([]candidate{{id: "a-busy"}}, time.Now()); !allBusy {
		t.Fatal("the busy owner is not held after answering 503")
	}
	if got := freeHits(); got != 2 {
		t.Fatalf("free owner served %d requests, want 2: one held owner must not refuse admission while another is free", got)
	}
}

// TestHandleHTTP_BusyHoldSparesControlRoutes: only model-bearing inference is
// gated. A control call to a held node is forwarded as before, because the
// hold reflects the engine's inference queue and says nothing about those.
func TestHandleHTTP_BusyHoldSparesControlRoutes(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		setForTest(t, &busyHold, time.Minute)
		busyURL, hits := newCountingServer(t, http.StatusServiceUnavailable, busyBody)

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "saturated", busyURL, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)
		f := p.soleFacade()

		f.handleHTTP(httptest.NewRecorder(), tc.inferenceRequest())
		before := hits()
		if allBusy, _ := f.busyFor([]candidate{{id: "saturated"}}, time.Now()); !allBusy {
			t.Fatal("the owner is not held after answering 503")
		}

		rec := httptest.NewRecorder()
		f.handleHTTP(rec, httptest.NewRequest(http.MethodGet, tc.nonInferencePath, nil))
		if got := hits(); got != before+1 {
			t.Fatalf("held owner saw %d control-route dispatch(es), want 1: the hold gates inference only", got-before)
		}
	})
}

// TestBusyFor_EmptyIsNotBusy: an empty candidate list is the no-owner
// rejection's case, decided earlier; the gate must not claim it.
func TestBusyFor_EmptyIsNotBusy(t *testing.T) {
	f := testProxy(anyProfile(t), NewDiscovery(), 0).soleFacade()
	f.markBusy("n", time.Now())
	if allBusy, _ := f.busyFor(nil, time.Now()); allBusy {
		t.Fatal("no candidates reported as all busy")
	}
}

// TestRetryAfterSeconds: whole seconds, rounded up, never zero.
func TestRetryAfterSeconds(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want int
	}{
		{0, 1},
		{time.Millisecond, 1},
		{time.Second, 1},
		{1001 * time.Millisecond, 2},
		{2500 * time.Millisecond, 3},
	} {
		if got := retryAfterSeconds(tc.d); got != tc.want {
			t.Errorf("retryAfterSeconds(%v) = %d, want %d", tc.d, got, tc.want)
		}
	}
}
