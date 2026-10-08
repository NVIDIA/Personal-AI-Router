// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain shortens the retry backoff for the whole package. Production waits
// seconds between attempts, and a body that drives a node to its dispatch
// budget would otherwise pay all of it in wall clock — one all-nodes-down test
// alone went from under a second to nineteen. The schedule itself is asserted
// directly in TestBackoffFor, so shortening it here costs no coverage.
func TestMain(m *testing.M) {
	retryBackoff = []time.Duration{time.Millisecond}
	os.Exit(m.Run())
}

// newCountingServer is an upstream that answers every request the same way and
// counts how many it saw, which is the quantity the retry tests assert on: not
// what a node replied, but how many times it was contacted.
//
// The count comes back as a func so a caller cannot reset it, and reading it
// while requests are still in flight is safe.
func newCountingServer(t *testing.T, status int, body string) (serverURL string, hits func() int) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() int { return int(n.Load()) }
}

// waitForCond polls until cond holds, failing the test if it never does.
//
// The retry tests need to act at a point in a request's life — "the dispatch
// has reached the upstream", not "50ms have passed". On a loaded runner those
// are different statements, and a fixed sleep that loses the race usually makes
// the test pass while proving nothing.
func waitForCond(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	require.FailNowf(t, "timed out waiting for condition", "waited %v for %s", timeout, what)
}

// decodeJSONBody parses a response body as a JSON object so an assertion can
// name the field it cares about, instead of substring-matching a serialization
// that a reordered marshal or a changed quote style would break.
func decodeJSONBody(t *testing.T, body string) map[string]any {
	t.Helper()
	var got map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &got), "response body is not a JSON object: %q", body)
	return got
}

// setForTest overrides a package-level knob for one test and restores it after.
//
// The retry bounds are vars precisely so tests can shorten them. Registering
// the restore with t.Cleanup keeps it on the same line as the override and runs
// it even when the body ends in t.Fatal, which a hand-written defer pair in
// each test does not.
func setForTest[A any](t *testing.T, target *A, value A) {
	t.Helper()
	orig := *target
	t.Cleanup(func() { *target = orig })
	*target = value
}

// TestBackoffFor covers the production schedule, which TestMain has replaced
// for every other body in the package, so it puts the real one back first.
func TestBackoffFor(t *testing.T) {
	setForTest(t, &retryBackoff, defaultRetryBackoff)

	// Indexed by dispatches already made, so the wait after the first dispatch
	// is the first entry. A deadline an hour out keeps the remaining-time clamp
	// out of it. Jitter is up to ±25%, so assert a band.
	for _, tc := range []struct {
		dispatches int
		want       time.Duration
	}{
		{dispatches: 1, want: time.Second},
		{dispatches: 2, want: 2 * time.Second},
		{dispatches: 3, want: 4 * time.Second},
		{dispatches: 4, want: 8 * time.Second},
		{dispatches: 9, want: 8 * time.Second}, // clamped to the last entry
	} {
		got := backoffFor(tc.dispatches, time.Hour)
		lo := tc.want - tc.want/4
		hi := tc.want + tc.want/4
		assert.GreaterOrEqual(t, got, lo, "backoffFor")
		assert.LessOrEqual(t, got, hi, "backoffFor")
	}
}

// TestBackoffFor_NeverOutlastsTheDeadline: a wait longer than the time left
// would guarantee the bounds check after it fails, so the delay would buy
// latency and no attempt.
func TestBackoffFor_NeverOutlastsTheDeadline(t *testing.T) {
	setForTest(t, &retryBackoff, defaultRetryBackoff)

	// Dispatch 3 alone would wait 4s.
	assert.LessOrEqual(t, backoffFor(3, 100*time.Millisecond), 100*time.Millisecond, "backoffFor with 100ms left")
	assert.Equal(t, time.Duration(0), backoffFor(1, 0), "backoffFor with no time left")
}

// Committing an attempt and abandoning it because its target left discovery can
// become true at once, so the outcome is a single claim rather than a pair of
// signals (spec §5.3). These cover the claim directly, because the race window
// in the handler is not reachable deterministically.

func TestAttemptClaim_CommitWinsATie(t *testing.T) {
	var claim attemptClaim

	require.True(t, claim.commit(), "commit on a pending claim should win")
	require.False(t, claim.abandon(), "abandon after commit must lose: a delivered first byte is evidence the node is serving, and cancelling would truncate a working stream")
	require.False(t, claim.abandoned(), "a committed claim must not report as abandoned")
}

func TestAttemptClaim_AbandonBlocksALaterCommit(t *testing.T) {
	var claim attemptClaim

	require.True(t, claim.abandon(), "abandon on a pending claim should win")
	require.False(t, claim.commit(), "commit after abandon must lose: the attempt is already being cancelled, so serving it would stream through a dying context")
	require.True(t, claim.abandoned(), "an abandoned claim must report as abandoned so the caller can name the real reason")
}

func TestAttemptClaim_CommitIsIdempotent(t *testing.T) {
	var claim attemptClaim

	require.True(t, claim.commit(), "a second commit from the same committed attempt should still report true")
	require.True(t, claim.commit(), "a second commit from the same committed attempt should still report true")
}

func TestAttemptClaim_SettledOnlyAfterAClaim(t *testing.T) {
	var pending, committed, abandoned attemptClaim
	committed.commit()
	abandoned.abandon()

	assert.False(t, pending.settled(), "an unclaimed attempt must read as unsettled, or the watcher would stop watching it")
	assert.True(t, committed.settled(), "a claimed attempt must read as settled so the watcher stops")
	assert.True(t, abandoned.settled(), "a claimed attempt must read as settled so the watcher stops")
}

// TestAttemptClaim_ExactlyOneWinnerUnderContention is the property the type
// exists for: with both sides racing, one and only one succeeds.
func TestAttemptClaim_ExactlyOneWinnerUnderContention(t *testing.T) {
	for range 500 {
		var claim attemptClaim
		var commits, abandons atomic.Int32

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			if claim.commit() {
				commits.Add(1)
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			if claim.abandon() {
				abandons.Add(1)
			}
		}()
		close(start)
		wg.Wait()

		require.Equal(t, int32(1), commits.Load()+abandons.Load(), "winners")
	}
}

// TestHandleHTTP_RetriesTheOnlyOwner is the case the old loop could not serve.
// Bounded by the candidate count, a single-owner cluster got exactly one
// attempt and then a terminal error — which is the common single-desktop
// shape. The budget is a dispatch count now, so the same node is tried again.
func TestHandleHTTP_RetriesTheOnlyOwner(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		var hits atomic.Int32
		flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if hits.Add(1) <= 2 {
				w.WriteHeader(http.StatusServiceUnavailable)
				io.WriteString(w, `{"error":"loading model"}`)
				return
			}
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"done":true}`)
		}))
		defer flaky.Close()

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "only", flaky.URL, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)

		rec := httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

		require.Equal(t, http.StatusOK, rec.Code, "status")
		require.Equal(t, int32(3), hits.Load(), "upstream saw")
	})
}

// TestHandleHTTP_StopsAtDispatchBudget: retrying is bounded. An owner that
// always fails is dispatched to exactly maxDispatchAttempts times, and the
// final attempt's own response is what the client gets — the proxy does not
// substitute a generated status when the budget is what ran out.
//
// The upstream answers 502 rather than 503 so those two outcomes are
// distinguishable: 503 is what the proxy writes when the loop ends between
// attempts (spec §5.4), so an upstream 503 here would match either behavior.
func TestHandleHTTP_StopsAtDispatchBudget(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		const upstreamBody = `{"error":"still loading"}`
		brokenURL, hits := newCountingServer(t, http.StatusBadGateway, upstreamBody)

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "broken", brokenURL, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)

		rec := httptest.NewRecorder()
		p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

		require.Equal(t, maxDispatchAttempts, hits(), "upstream should consume the full dispatch budget")
		require.Equal(t, http.StatusBadGateway, rec.Code, "status")
		require.Equal(t, upstreamBody, rec.Body.String(), "upstream response must pass through unchanged")
	})
}

// TestHandleHTTP_NonInferenceIsNotRetriedBeyondItsCandidates keeps the retry
// budget off state-changing control-plane routes (spec §5.1). A 5xx is
// retryable regardless of route, so without the bound a failed POST /api/pull
// would be replayed into repeated model downloads. One owner means one
// dispatch.
func TestHandleHTTP_NonInferenceIsNotRetriedBeyondItsCandidates(t *testing.T) {
	forEachEngine(t, func(t *testing.T, tc engineCase) {
		failingURL, hits := newCountingServer(t, http.StatusInternalServerError, `{"error":"pull failed"}`)

		disc := NewDiscovery()
		disc.AddManual(nodeForModel(t, "only", failingURL, tc.advertisedModel))
		p := testProxy(tc.profile, disc, tc.profile.FacadePort)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, tc.nonInferencePath, strings.NewReader(`{"name":"m"}`))
		p.soleFacade().handleHTTP(rec, req)

		require.Equal(t, 1, hits(), "upstream saw")
		require.Equal(t, http.StatusInternalServerError, rec.Code, "status")
	})
}

// TestHandleHTTP_ReservationReleasedBetweenAttempts is the shared-scheduler
// half of the retry policy, and the reason it is not just a loop.
//
// The reservation map is process-wide: both engine facades estimate load from
// the same counts. A retry that kept its claim while sitting in a backoff would
// leave a node looking busy when nothing is running on it, and would push the
// OTHER engine's traffic away from a node that is in fact free. So the claim is
// dropped before every wait and re-taken on the next dispatch, which is the
// same reasoning that clears scheduledOn — applied to the proxy's own estimate
// rather than the scheduler's.
//
// Two failing owners, so the claim has to move as well as clear. A leak on the
// first node would make it look permanently loaded, which is observable twice:
// the count it is left holding, and the skew in how the five dispatches were
// split between two nodes that are equally broken.
func TestHandleHTTP_ReservationReleasedBetweenAttempts(t *testing.T) {
	tc := anyCase(t)

	firstURL, firstHits := newCountingServer(t, http.StatusServiceUnavailable, "")
	secondURL, secondHits := newCountingServer(t, http.StatusServiceUnavailable, "")

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "broken-a", firstURL, tc.advertisedModel))
	disc.AddManual(nodeForModel(t, "broken-b", secondURL, tc.advertisedModel))
	p := testProxy(tc.profile, disc, tc.profile.FacadePort)
	// A snapshot so both nodes are scheduler-listed and therefore reservable.
	p.SetPriority([]string{"broken-a", "broken-b"})

	rec := httptest.NewRecorder()
	p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

	require.Equal(t, maxDispatchAttempts, firstHits()+secondHits(), "dispatches must consume the full budget")
	require.NotEqual(t, 0, firstHits(), "dispatches = broken-a")
	require.NotEqual(t, 0, secondHits(), "dispatches = broken-a")

	p.priorityMu.Lock()
	leftover := map[string]int{
		"broken-a": p.priorityReservations["broken-a"],
		"broken-b": p.priorityReservations["broken-b"],
	}
	p.priorityMu.Unlock()
	for id, n := range leftover {
		assert.Equal(t, 0, n, "reservations left on (%v, %v)", id, n)
	}
}

// TestHandleHTTP_WaitsForAnOwnerWithoutSpendingAttempts pins the split between
// the two bounds. When every owner disappears mid-flight the request waits for
// one to come back, and that wait consumes no dispatch attempt — so the only
// thing that ends it is the deadline. Read against dispatch failures alone the
// deadline looks unreachable, which is exactly why this path needs a test: it
// is the reason the deadline is not dead configuration.
func TestHandleHTTP_WaitsForAnOwnerWithoutSpendingAttempts(t *testing.T) {
	setForTest(t, &jobDeadline, 1500*time.Millisecond)
	setForTest(t, &retryBackoff, []time.Duration{150 * time.Millisecond})

	tc := anyCase(t)

	// Present at admission so the request is accepted, but its server is
	// already closed, so the one dispatch it gets fails at the transport.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "gone", deadURL, tc.advertisedModel))
	p := testProxy(tc.profile, disc, tc.profile.FacadePort)

	rec := httptest.NewRecorder()
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.soleFacade().handleHTTP(rec, tc.inferenceRequest())
	}()

	// Drop the owner once that first dispatch has failed, so every later
	// re-resolution finds nothing at all.
	time.Sleep(75 * time.Millisecond)
	disc.RemoveManual("gone")

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "handleHTTP never returned")
	}
	elapsed := time.Since(start)

	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "status")
	// The distinguishing evidence: the budget was never spent, so the reason is
	// the missing owner rather than exhausted dispatches.
	const want = "no node advertising the requested model became available before the retry deadline"
	require.Equal(t, want, decodeJSONBody(t, rec.Body.String())["error"], "body error (%v)", want)
	// It waited rather than giving up at once, which is the point of the wait.
	require.GreaterOrEqual(t, elapsed, jobDeadline/2, "returned after (%v, %v)", elapsed, jobDeadline)
}

// TestHandleHTTP_DeadlineWithAnOwnerPresentSaysSo is the other way to run out
// of time, and the one whose reason is easy to get wrong.
//
// A slow attempt can spend 240s — the header cap and the first-content cap back
// to back — so a job can reach the 10-minute deadline before it reaches five
// dispatches. The owner is still right there, so reporting that no node became
// available would blame a missing node for attempts that were made and failed.
func TestHandleHTTP_DeadlineWithAnOwnerPresentSaysSo(t *testing.T) {
	// A deadline the first slow attempt outlives, so the loop breaks on time
	// rather than on the dispatch budget, with the owner still advertising.
	setForTest(t, &firstBodyTimeout, 300*time.Millisecond)
	setForTest(t, &jobDeadline, 150*time.Millisecond)

	tc := anyCase(t)
	stalled := newStalledEngine(t)

	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "slow", stalled.url, tc.advertisedModel))
	p := testProxy(tc.profile, disc, tc.profile.FacadePort)

	rec := httptest.NewRecorder()
	p.soleFacade().handleHTTP(rec, tc.inferenceRequest())

	got := stalled.hits()
	require.NotEqual(t, 0, got, "upstream saw")
	require.Less(t, got, maxDispatchAttempts, "upstream saw")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "status")
	const want = "the retry deadline passed while dispatch attempts were still failing"
	require.Equal(t, want, decodeJSONBody(t, rec.Body.String())["error"], "body error (%v)", want)
}

// TestHandleHTTP_TargetLeavingDiscoveryAbortsAttempt covers the in-flight
// abort, and with it the distinction the whole mechanism rests on.
//
// Without it, a node that vanished mid-request was noticed only when the
// first-content budget expired: the attempt sat for the full 120s while the
// broker's own node-loss sweep had already stamped the record failed, so the UI
// reported a failure the client was still waiting on. Here the budget is left
// long on purpose, so finishing quickly is only possible via the discovery
// watcher.
//
// The second assertion is the subtle half. Aborting an attempt cancels a
// context, and so does a client hanging up; if the two were confused, either a
// retarget would be reported as cancelled — losing a job that went on to
// succeed — or a real disconnect would keep burning attempts. The attempt
// context is a child, and both the disconnect watcher and the terminal
// classification read the parent, which is what keeps them apart.
func TestHandleHTTP_TargetLeavingDiscoveryAbortsAttempt(t *testing.T) {
	// Left long on purpose: if the watcher does not end the attempt, nothing
	// else will inside the test's patience.
	setForTest(t, &firstBodyTimeout, 30*time.Second)
	setForTest(t, &targetWatchInterval, 25*time.Millisecond)

	tc := anyCase(t)

	stalled := newStalledEngine(t)

	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"done":true}`)
	}))
	defer good.Close()

	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "vanishing", stalled.url, tc.advertisedModel))
	disc.AddManual(nodeForModel(t, "survivor", good.URL, tc.advertisedModel))
	p := newTestProxy(tc.profile, NewCodec(rec), disc, tc.profile.FacadePort)
	p.soleFacade().SetSelected("vanishing") // deterministic: the doomed node first

	resp := httptest.NewRecorder()
	start := time.Now()
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.soleFacade().handleHTTP(resp, tc.inferenceRequest())
	}()

	// Take the node away only once the attempt has actually reached the stalled
	// engine. Removing it first would leave the survivor as the only owner at
	// resolution time, and the request would succeed through it without the
	// watcher ever being involved — passing while proving nothing.
	waitForCond(t, 5*time.Second, "the first dispatch to reach the stalled engine",
		func() bool { return stalled.hits() > 0 })
	disc.RemoveManual("vanishing")

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		require.FailNow(t, "handleHTTP never returned: the target leaving discovery did not abort the attempt")
	}
	elapsed := time.Since(start)

	require.LessOrEqual(t, elapsed, 10*time.Second, "took")
	require.Equal(t, http.StatusOK, resp.Code, "status")
	require.Contains(t, resp.Body.String(), `"done":true`, "body came from the wrong node")
	require.Contains(t, rec.String(), `"state":"completed"`, "the job must complete: aborting an attempt is not the client disconnecting")
	require.NotContains(t, rec.String(), `"state":"cancelled"`, "a retarget was misreported as cancelled: the attempt cancel was mistaken for the client leaving")
}

// TestHandleHTTP_AbandonedRequestStopsRetrying: a client that has gone away
// should not keep a retry budget alive. Nobody is left to receive the answer,
// so the remaining attempts belong to work someone is waiting for.
func TestHandleHTTP_AbandonedRequestStopsRetrying(t *testing.T) {
	// Long enough that the cancel below lands inside the wait rather than
	// between two dispatches.
	setForTest(t, &retryBackoff, []time.Duration{300 * time.Millisecond})

	tc := anyCase(t)

	brokenURL, hits := newCountingServer(t, http.StatusServiceUnavailable, "")

	rec := &recRW{}
	disc := NewDiscovery()
	disc.AddManual(nodeForModel(t, "broken", brokenURL, tc.advertisedModel))
	p := newTestProxy(tc.profile, NewCodec(rec), disc, tc.profile.FacadePort)

	ctx, cancel := context.WithCancel(context.Background())
	req := tc.inferenceRequest().WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.soleFacade().handleHTTP(httptest.NewRecorder(), req)
	}()

	// Abandon the request once the first dispatch has been made, which puts the
	// cancel inside the backoff before the second. Waiting on the count rather
	// than the clock is what makes the assertion below mean something: with no
	// dispatch yet observed there would be nothing for a retry to exceed.
	waitForCond(t, 5*time.Second, "the first dispatch to reach the upstream",
		func() bool { return hits() > 0 })
	afterFirst := hits()
	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "handleHTTP did not return after the client abandoned the request")
	}

	require.LessOrEqual(t, hits(), afterFirst, "upstream saw")
	require.Contains(t, rec.String(), `"state":"cancelled"`, "an abandoned request must terminate as cancelled")
}
