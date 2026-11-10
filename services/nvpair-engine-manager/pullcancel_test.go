// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// outcome is a pull's return pair, carried off the goroutine that ran it.
type outcome struct {
	result json.RawMessage
	err    error
}

// awaitOutcome waits for a pull running on another goroutine to return, and
// fails the test rather than hanging if ctx ends first.
func awaitOutcome(t *testing.T, ctx context.Context, done <-chan outcome) outcome {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-ctx.Done():
		require.FailNow(t, "the pull did not return before the test's deadline")
		return outcome{}
	}
}

func assertPullStatus(t *testing.T, result json.RawMessage, want string) {
	t.Helper()
	var terminal struct {
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(result, &terminal), "decode pull result %s", result)
	assert.Equal(t, want, terminal.Status)
}

func succeedingPull(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"status":"success"}`), nil
}

// The cancel may only be acknowledged once the partial files are gone, because
// the UI reads that acknowledgement as "it is safe to download this again".
//
// The case drives that order explicitly:
//
//  1. the download registers and blocks, so there is something to cancel;
//  2. the cancel arrives and closes the download's context;
//  3. the download observes the cancellation and enters its cleanup, where it
//     blocks. Reaching this point proves the cancel is already past
//     CancelModelPull's own call to p.cancel, so it must still be waiting;
//  4. cleanup finishes, and only then does the cancel return.
func TestCancelModelPull_AnswersOnlyOnceCleanupHasSettled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	downloading := make(chan struct{})
	cleaningUp := make(chan struct{})
	finishCleanup := make(chan struct{})
	pull := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.trackedPull(ctx, "ollama", "demo", func(runCtx context.Context) (json.RawMessage, error) {
			close(downloading)
			<-runCtx.Done()
			close(cleaningUp)
			<-finishCleanup
			return nil, runCtx.Err()
		})
		pull <- got
	}()
	<-downloading

	cancelled := make(chan error, 1)
	go func() { cancelled <- ex.CancelModelPull(ctx, "ollama", "demo") }()
	<-cleaningUp
	select {
	case err := <-cancelled:
		require.FailNow(t, "the cancel was acknowledged before cleanup finished", "cancel returned %v", err)
	default:
	}

	close(finishCleanup)
	require.NoError(t, <-cancelled)
	got := <-pull
	require.NoError(t, got.err)
	assertPullStatus(t, got.result, "cancelled")
}

// Because the cancel waits for the transfer and its cleanup, its own caller
// giving up — a remote initiator disconnecting, the RPC's budget elapsing — has
// to end that wait. The download still stops; only the acknowledgement is
// abandoned.
func TestCancelModelPull_StopsWaitingWhenItsCallerGivesUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	downloading := make(chan struct{})
	finishCleanup := make(chan struct{})
	pull := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.trackedPull(ctx, "ollama", "demo", func(runCtx context.Context) (json.RawMessage, error) {
			close(downloading)
			<-runCtx.Done()
			<-finishCleanup
			return nil, runCtx.Err()
		})
		pull <- got
	}()
	<-downloading

	abandoned, abandon := context.WithCancel(ctx)
	waited := make(chan error, 1)
	go func() { waited <- ex.CancelModelPull(abandoned, "ollama", "demo") }()
	abandon()

	select {
	case err := <-waited:
		assert.ErrorIs(t, err, context.Canceled)
	case <-ctx.Done():
		require.FailNow(t, "the cancel kept waiting after its own caller gave up")
	}
	close(finishCleanup)
	got := awaitOutcome(t, ctx, pull)
	require.NoError(t, got.err)
	assertPullStatus(t, got.result, "cancelled")
}

// stalledTransfer is a download endpoint that sends nothing until the request
// is abandoned, so only a cancellation can end a pull made against it.
func stalledTransfer(t *testing.T) *httptest.Server {
	t.Helper()
	stop := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-stop:
		}
	}))
	t.Cleanup(func() {
		close(stop)
		server.Close()
	})
	return server
}

// claimedPull runs the download for a request claimed the way the read loop
// claims one, reporting whether the download itself started.
func claimedPull(t *testing.T, ex *Executor, request context.Context) (result json.RawMessage, ran bool) {
	t.Helper()
	result, err := ex.trackedPull(request, "ollama", "demo", func(ctx context.Context) (json.RawMessage, error) {
		ran = true
		return succeedingPull(ctx)
	})
	require.NoError(t, err)
	return result, ran
}

// A cancel dispatched before its pull registers must stop the download rather
// than report success while it runs on under a UI stuck on "Canceling". The
// pull and the cancel each run on their own goroutine, so the read loop's
// ordering does not settle which reaches the executor first.
func TestCancelModelPull_StopsARequestThatHasNotRegisteredYet(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	// The read loop claims a pull before dispatching it, so this is the window
	// in which the cancel's goroutine can reach the executor first.
	request, release := ex.claimPull(context.Background(), "ollama", "demo")
	defer release()

	require.NoError(t, ex.CancelModelPull(context.Background(), "ollama", "demo"))
	result, ran := claimedPull(t, ex, request)

	assert.False(t, ran, "the cancelled request started its download")
	assertPullStatus(t, result, "cancelled")
}

// Two requests for one model can be outstanding at once: an original the user
// cancelled before it registered, and a retry accepted after the cancel. The
// cancel belongs to the original whichever of the two reaches the executor
// first. Keyed by engine and model alone, it was consumed by the retry, which
// reported "cancelled" while the original went on to download. Here the
// original arrives once the retry has installed the model, so it settles on
// that success rather than a "cancelled" that says the model is not there.
func TestCancelModelPull_StopsTheOriginalRequestEvenWhenItsRetryRegistersFirst(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	original, releaseOriginal := ex.claimPull(context.Background(), "ollama", "demo")
	defer releaseOriginal()
	require.NoError(t, ex.CancelModelPull(context.Background(), "ollama", "demo"))
	retry, releaseRetry := ex.claimPull(context.Background(), "ollama", "demo")
	defer releaseRetry()

	retryResult, retryRan := claimedPull(t, ex, retry)
	originalResult, originalRan := claimedPull(t, ex, original)

	assert.True(t, retryRan, "the retry inherited the original request's cancellation")
	assertPullStatus(t, retryResult, "success")
	assert.False(t, originalRan, "the original request started after its cancellation was acknowledged")
	assertPullStatus(t, originalResult, "success")
}

// A cancel can find nothing outstanding: the download it targets already
// finished, or none was ever requested. There is nothing to stop, and holding
// the cancel for the next pull of the same model made a retry report
// "cancelled" without downloading anything.
func TestCancelModelPull_WithNothingOutstandingLeavesTheNextRequestAlone(t *testing.T) {
	test := func(name string, before func(t *testing.T, ex *Executor)) {
		t.Run(name, func(t *testing.T) {
			ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
			before(t, ex)

			require.NoError(t, ex.CancelModelPull(context.Background(), "ollama", "demo"))
			next, release := ex.claimPull(context.Background(), "ollama", "demo")
			defer release()
			result, ran := claimedPull(t, ex, next)

			assert.True(t, ran, "the next request inherited a cancellation that had nothing to stop")
			assertPullStatus(t, result, "success")
		})
	}
	test("the download already finished", func(t *testing.T, ex *Executor) {
		first, release := ex.claimPull(context.Background(), "ollama", "demo")
		defer release()
		_, ran := claimedPull(t, ex, first)
		require.True(t, ran)
	})
	test("no download was ever requested", func(*testing.T, *Executor) {})
}

// A retry can be accepted while the download it replaces is still stopping,
// which takes seconds when cleanup has partial files to wait on. The cancel
// came before the retry, so it is not the retry's: joined to the cancelled
// download, the retry reported "cancelled" without downloading anything. It
// waits for that download to stop instead — its transfer must not run under
// that download's cleanup — and then downloads, its frames after the
// cancelled download's outcome.
func TestTrackedPull_DownloadsARetryOnceTheCancelledDownloadHasStopped(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	events, unsubscribe := ex.progress.subscribe("ollama")
	defer unsubscribe()
	downloading := make(chan struct{})
	cleaningUp := make(chan struct{})
	finishCleanup := make(chan struct{})
	original := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.trackedPull(ctx, "ollama", "demo", func(runCtx context.Context) (json.RawMessage, error) {
			close(downloading)
			<-runCtx.Done()
			close(cleaningUp)
			<-finishCleanup
			return nil, runCtx.Err()
		})
		original <- got
	}()
	<-downloading
	cancelled := make(chan error, 1)
	go func() { cancelled <- ex.CancelModelPull(ctx, "ollama", "demo") }()
	<-cleaningUp
	retry, release := ex.claimPull(ctx, "ollama", "demo")
	defer release()
	retryDownloading := make(chan struct{})
	retryDone := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.trackedPull(retry, "ollama", "demo", func(runCtx context.Context) (json.RawMessage, error) {
			close(retryDownloading)
			return succeedingPull(runCtx)
		})
		retryDone <- got
	}()
	select {
	case <-retryDownloading:
		require.FailNow(t, "the retry downloaded under the cancelled download's cleanup")
	case got := <-retryDone:
		require.FailNow(t, "the retry settled before the cancelled download had stopped", "result %s, error %v", got.result, got.err)
	case <-time.After(100 * time.Millisecond):
	}

	close(finishCleanup)
	require.NoError(t, <-cancelled)
	originalGot := awaitOutcome(t, ctx, original)
	retryGot := awaitOutcome(t, ctx, retryDone)

	require.NoError(t, originalGot.err)
	assertPullStatus(t, originalGot.result, "cancelled")
	require.NoError(t, retryGot.err)
	assertPullStatus(t, retryGot.result, "success")
	select {
	case <-retryDownloading:
	default:
		assert.Fail(t, "the retry never downloaded")
	}
	assert.Equal(t, []ProgressEvent{
		{Engine: "ollama", Model: "demo", Op: "pull", Stage: "cancelled", Message: "demo", Download: 1},
		{Engine: "ollama", Model: "demo", Op: "pull", Stage: "success", Percent: 100, Message: "demo", Download: 2},
	}, publishedFrames(events), "the retry's frames must follow the cancelled download's outcome, as a download of its own")
}

// A stop the engine could not confirm leaves a download that may still be
// running. Asked about once the pull has settled, a cancel would find nothing
// registered and answer as though the download had stopped, which the UI reads
// as safe to download again. It reports the failure instead, until the model's
// next download replaces it.
func TestCancelModelPull_ReportsAStopNeverConfirmedUntilTheNextDownload(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	unconfirmed := stopUnconfirmed(errors.New("the engine did not confirm the download stopped"))
	cancelled := make(chan error, 1)
	_, err := ex.trackedPull(context.Background(), "ollama", "demo", func(ctx context.Context) (json.RawMessage, error) {
		go func() { cancelled <- ex.CancelModelPull(context.Background(), "ollama", "demo") }()
		<-ctx.Done()
		return nil, unconfirmed
	})
	require.ErrorIs(t, err, unconfirmed)
	require.ErrorIs(t, <-cancelled, unconfirmed, "the cancel that asked for the stop")

	assert.ErrorIs(t, ex.CancelModelPull(context.Background(), "ollama", "demo"), unconfirmed, "a cancel asking after the pull settled")

	result, err := ex.trackedPull(context.Background(), "ollama", "demo", succeedingPull)
	require.NoError(t, err)
	assertPullStatus(t, result, "success")
	assert.NoError(t, ex.CancelModelPull(context.Background(), "ollama", "demo"), "a cancel after the next download completed")
}

// A request can end without ever registering its download — it failed
// validation, or its context died first. The cancel that found it outstanding
// stopped that request, and must not carry over to the next one.
func TestCancelModelPull_DoesNotCarryOverToALaterRequest(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	_, releaseAbandoned := ex.claimPull(context.Background(), "ollama", "demo")
	require.NoError(t, ex.CancelModelPull(context.Background(), "ollama", "demo"))
	releaseAbandoned()

	next, release := ex.claimPull(context.Background(), "ollama", "demo")
	defer release()
	result, ran := claimedPull(t, ex, next)

	assert.True(t, ran, "a cancellation of an abandoned request was inherited by the next one")
	assertPullStatus(t, result, "success")
}

// A predecessor here is another of this engine's downloads that registered
// first. trackedPull serializes pulls per engine — the vendor caches share
// partial files across models, so one pull's cleanup must not run while another
// PAIR pull is writing — which leaves a later request queued behind the ones
// already in flight.
//
// Cancelling a queued download has to settle on its own, without waiting for a
// turn it will never take: the row already says "Canceling", and a predecessor
// can be a multi-gigabyte transfer. The predecessors stay in flight for the
// whole case to prove that.
func TestCancelModelPull_SettlesAQueuedDownloadWithoutWaitingForItsTurn(t *testing.T) {
	test := func(name string, predecessorCount int) {
		t.Run(name, func(t *testing.T) {
			queued := make(chan struct{}, 1)
			ex := NewExecutor(NewRegistry(), NewReporter(nil), func(method string, _ any) {
				if method == "engine:pull-progress" {
					select {
					case queued <- struct{}{}:
					default:
					}
				}
			}, t.TempDir())
			ex.pulls = make(map[pullKey]*activePull)
			for i := range predecessorCount {
				ex.pulls[pullKey{engine: "ollama", model: fmt.Sprintf("predecessor-%d", i)}] = &activePull{done: make(chan struct{})}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			finished := make(chan outcome, 1)
			ran := false
			go func() {
				var got outcome
				got.result, got.err = ex.trackedPull(ctx, "ollama", "queued", func(context.Context) (json.RawMessage, error) {
					ran = true
					return nil, nil
				})
				finished <- got
			}()
			select {
			case <-queued:
			case <-ctx.Done():
				require.FailNow(t, "the pull was not queued")
			}

			require.NoError(t, ex.CancelModelPull(ctx, "ollama", "queued"))
			var got outcome
			select {
			case got = <-finished:
			case <-ctx.Done():
				require.FailNow(t, "the queued pull did not settle after its cancellation")
			}

			require.NoError(t, got.err)
			assertPullStatus(t, got.result, "cancelled")
			assert.False(t, ran, "the cancelled download started")
			ex.pullMu.Lock()
			defer ex.pullMu.Unlock()
			assert.NotContains(t, ex.pulls, pullKey{engine: "ollama", model: "queued"}, "the cancelled download is still registered")
			assert.Len(t, ex.pulls, predecessorCount, "the cancellation disturbed the downloads ahead of it")
		})
	}
	test("behind one active download", 1)
	test("behind several downloads", 3)
}

// A duplicate request joins the download already in flight and reports its
// outcome. Failing it instead produced a terminal error frame attributed to the
// live pull's own model, which disables that row's Cancel button — and a
// cancellation the user asked for has to settle the duplicate's row too, not
// just the row the Cancel was clicked on.
func TestTrackedPull_JoinsADuplicateRequestOntoTheActiveDownload(t *testing.T) {
	test := func(name string, activeResult json.RawMessage, wantStatus string) {
		t.Run(name, func(t *testing.T) {
			ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
			events, unsubscribe := ex.progress.subscribe("ollama")
			defer unsubscribe()
			active := &activePull{cancel: func() {}, done: make(chan struct{})}
			ex.pulls = map[pullKey]*activePull{{engine: "ollama", model: "demo"}: active}

			joined := make(chan outcome, 1)
			go func() {
				var got outcome
				got.result, got.err = ex.trackedPull(context.Background(), "ollama", "demo", func(context.Context) (json.RawMessage, error) {
					return nil, fmt.Errorf("duplicate request started a second download")
				})
				joined <- got
			}()
			// Settling the live pull is what releases the joiner, so the result
			// is published before the close that hands it over. The order holds
			// however the two goroutines are scheduled.
			active.result = activeResult
			close(active.done)
			got := <-joined

			require.NoError(t, got.err)
			assertPullStatus(t, got.result, wantStatus)
			// The download's owner reports how it ended to every subscriber,
			// so a frame from the joiner would announce that outcome twice.
			assert.Empty(t, events, "the joined request reported an outcome of its own")
		})
	}
	test("completed download", json.RawMessage(`{"status":"success"}`), "success")
	test("cancelled download", cancelledPull(), "cancelled")
}

// Every download ends with exactly one terminal frame, whichever engine ran it
// and however it ended, because a UI that stopped waiting on the RPC converges
// on that frame alone. LM Studio's downloads reported their progress but never
// their end.
func TestTrackedPull_EndsEachDownloadWithOneTerminalFrame(t *testing.T) {
	test := func(name string, pull func(t *testing.T, ex *Executor), want ProgressEvent) {
		t.Run(name, func(t *testing.T) {
			ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
			events, unsubscribe := ex.progress.subscribe("ollama")
			defer unsubscribe()

			pull(t, ex)
			var frames []ProgressEvent
			for len(events) > 0 {
				frames = append(frames, <-events)
			}

			assert.Equal(t, []ProgressEvent{want}, frames)
		})
	}
	// Each case's executor runs one download, its first.
	terminal := func(stage string, percent int, message string) ProgressEvent {
		return ProgressEvent{Engine: "ollama", Model: "demo", Op: "pull", Stage: stage, Percent: percent, Message: message, Download: 1}
	}
	test("a download that completes", func(t *testing.T, ex *Executor) {
		_, err := ex.trackedPull(context.Background(), "ollama", "demo", succeedingPull)
		require.NoError(t, err)
	}, terminal("success", 100, "demo"))
	test("a download that fails", func(t *testing.T, ex *Executor) {
		_, err := ex.trackedPull(context.Background(), "ollama", "demo", func(context.Context) (json.RawMessage, error) {
			return nil, errors.New("disk full")
		})
		require.Error(t, err)
	}, terminal("error", -1, "ollama experienced an error while downloading a model: disk full"))
	test("a download someone cancelled", func(t *testing.T, ex *Executor) {
		cancelled := make(chan error, 1)
		_, err := ex.trackedPull(context.Background(), "ollama", "demo", func(ctx context.Context) (json.RawMessage, error) {
			go func() { cancelled <- ex.CancelModelPull(context.Background(), "ollama", "demo") }()
			<-ctx.Done()
			return nil, ctx.Err()
		})
		require.NoError(t, err)
		require.NoError(t, <-cancelled)
	}, terminal("cancelled", 0, "demo"))
	test("a request cancelled before its download started", func(t *testing.T, ex *Executor) {
		request, release := ex.claimPull(context.Background(), "ollama", "demo")
		defer release()
		require.NoError(t, ex.CancelModelPull(context.Background(), "ollama", "demo"))
		result, ran := claimedPull(t, ex, request)
		require.False(t, ran)
		assertPullStatus(t, result, "cancelled")
	}, terminal("cancelled", 0, "demo"))
}

// publishedFrames drains the frames already published to a subscriber.
func publishedFrames(events <-chan ProgressEvent) []ProgressEvent {
	var frames []ProgressEvent
	for len(events) > 0 {
		frames = append(frames, <-events)
	}
	return frames
}

// A request the user cancelled before it registered can reach the executor
// while a retry accepted after the cancel is downloading. Frames name only the
// engine and model, so a "cancelled" frame from the original request reads as
// the retry ending: UIs retired the row and disabled its Cancel button while
// the retry's bytes were still arriving.
func TestTrackedPull_CancelledRequestSendsNoFrameWhileARetryDownloads(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	events, unsubscribe := ex.progress.subscribe("ollama")
	defer unsubscribe()
	original, releaseOriginal := ex.claimPull(ctx, "ollama", "demo")
	defer releaseOriginal()
	require.NoError(t, ex.CancelModelPull(ctx, "ollama", "demo"))
	retry, releaseRetry := ex.claimPull(ctx, "ollama", "demo")
	defer releaseRetry()
	retryDownloading := make(chan struct{})
	finishRetry := make(chan struct{})
	retryDone := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.trackedPull(retry, "ollama", "demo", func(runCtx context.Context) (json.RawMessage, error) {
			close(retryDownloading)
			select {
			case <-finishRetry:
				return succeedingPull(runCtx)
			case <-runCtx.Done():
				return nil, runCtx.Err()
			}
		})
		retryDone <- got
	}()
	select {
	case <-retryDownloading:
	case got := <-retryDone:
		require.FailNow(t, "the retry ended before its download started", "result %s, error %v", got.result, got.err)
	case <-ctx.Done():
		require.FailNow(t, "the retry's download did not start")
	}

	originalResult, originalRan := claimedPull(t, ex, original)
	close(finishRetry)
	retryGot := awaitOutcome(t, ctx, retryDone)

	assert.False(t, originalRan, "the cancelled request started its download")
	assertPullStatus(t, originalResult, "cancelled")
	require.NoError(t, retryGot.err)
	assertPullStatus(t, retryGot.result, "success")
	assert.Equal(t, []ProgressEvent{
		{Engine: "ollama", Model: "demo", Op: "pull", Stage: "success", Percent: 100, Message: "demo", Download: 1},
	}, publishedFrames(events), "only the retry may report how the model's download ended")
}

// A duplicate request still on its way to the executor when the user cancels
// is stopped along with the download it would have joined. That download
// reports the cancellation itself, so the duplicate adds no second terminal
// frame.
func TestTrackedPull_CancelledDuplicateAddsNoSecondTerminalFrame(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	events, unsubscribe := ex.progress.subscribe("ollama")
	defer unsubscribe()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	downloading := make(chan struct{})
	cleaningUp := make(chan struct{})
	finishCleanup := make(chan struct{})
	download := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.trackedPull(ctx, "ollama", "demo", func(runCtx context.Context) (json.RawMessage, error) {
			close(downloading)
			<-runCtx.Done()
			close(cleaningUp)
			<-finishCleanup
			return nil, runCtx.Err()
		})
		download <- got
	}()
	<-downloading
	duplicate, release := ex.claimPull(ctx, "ollama", "demo")
	defer release()
	cancelled := make(chan error, 1)
	go func() { cancelled <- ex.CancelModelPull(ctx, "ollama", "demo") }()
	// The cancel marks the outstanding requests before it stops the download,
	// so once cleanup starts the duplicate's request is already cancelled.
	<-cleaningUp

	duplicateResult, duplicateRan := claimedPull(t, ex, duplicate)
	close(finishCleanup)
	require.NoError(t, <-cancelled)
	downloadGot := <-download

	assert.False(t, duplicateRan, "the cancelled duplicate started a download")
	assertPullStatus(t, duplicateResult, "cancelled")
	require.NoError(t, downloadGot.err)
	assertPullStatus(t, downloadGot.result, "cancelled")
	assert.Equal(t, []ProgressEvent{
		{Engine: "ollama", Model: "demo", Op: "pull", Stage: "cancelled", Message: "demo", Download: 1},
	}, publishedFrames(events), "a cancelled download ends with exactly one terminal frame")
}

// Requests for one model that a cancel stops before any of them registers
// started no download between them, and say so once: the first to reach the
// executor sends the "cancelled" frame, and the rest add none.
func TestTrackedPull_RequestsCancelledBeforeAnyRegistersSendOneTerminalFrame(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	events, unsubscribe := ex.progress.subscribe("ollama")
	defer unsubscribe()
	first, releaseFirst := ex.claimPull(context.Background(), "ollama", "demo")
	defer releaseFirst()
	second, releaseSecond := ex.claimPull(context.Background(), "ollama", "demo")
	defer releaseSecond()
	require.NoError(t, ex.CancelModelPull(context.Background(), "ollama", "demo"))

	firstResult, firstRan := claimedPull(t, ex, first)
	secondResult, secondRan := claimedPull(t, ex, second)

	assert.False(t, firstRan || secondRan, "a cancelled request started its download")
	assertPullStatus(t, firstResult, "cancelled")
	assertPullStatus(t, secondResult, "cancelled")
	assert.Equal(t, []ProgressEvent{
		{Engine: "ollama", Model: "demo", Op: "pull", Stage: "cancelled", Message: "demo", Download: 1},
	}, publishedFrames(events), "the model's download ended once")
}

// A duplicate request can still be on its way to the executor when the
// download it would have joined finishes, and a cancel sent after that stops
// only the duplicate. Its client has heard "success" and the model is
// installed: a "cancelled" frame or result from the duplicate said otherwise,
// and the terminal interface showed the download as cancelled.
func TestTrackedPull_CancelledDuplicateOfAFinishedDownloadSettlesOnItsSuccess(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	events, unsubscribe := ex.progress.subscribe("ollama")
	defer unsubscribe()
	original, releaseOriginal := ex.claimPull(context.Background(), "ollama", "demo")
	duplicate, releaseDuplicate := ex.claimPull(context.Background(), "ollama", "demo")
	defer releaseDuplicate()
	originalResult, originalRan := claimedPull(t, ex, original)
	releaseOriginal()
	require.True(t, originalRan)
	assertPullStatus(t, originalResult, "success")
	require.NoError(t, ex.CancelModelPull(context.Background(), "ollama", "demo"))

	duplicateResult, duplicateRan := claimedPull(t, ex, duplicate)

	assert.False(t, duplicateRan, "the cancelled duplicate started a download")
	assertPullStatus(t, duplicateResult, "success")
	assert.Equal(t, []ProgressEvent{
		{Engine: "ollama", Model: "demo", Op: "pull", Stage: "success", Percent: 100, Message: "demo", Download: 1},
	}, publishedFrames(events), "the model's download ended once")
}

// A retry waiting for the cancelled download it replaces to stop can be
// cancelled in turn. It never starts a download, and the cancelled download
// reports the model's outcome while it waits, so the retry adds no frame of
// its own.
func TestTrackedPull_RetryCancelledWhileItWaitsAddsNoSecondTerminalFrame(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	events, unsubscribe := ex.progress.subscribe("ollama")
	defer unsubscribe()
	downloading := make(chan struct{})
	cleaningUp := make(chan struct{})
	finishCleanup := make(chan struct{})
	original := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.trackedPull(ctx, "ollama", "demo", func(runCtx context.Context) (json.RawMessage, error) {
			close(downloading)
			<-runCtx.Done()
			close(cleaningUp)
			<-finishCleanup
			return nil, runCtx.Err()
		})
		original <- got
	}()
	<-downloading
	firstCancel := make(chan error, 1)
	go func() { firstCancel <- ex.CancelModelPull(ctx, "ollama", "demo") }()
	<-cleaningUp
	retry, release := ex.claimPull(ctx, "ollama", "demo")
	defer release()
	retryDownloading := make(chan struct{})
	retryDone := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.trackedPull(retry, "ollama", "demo", func(runCtx context.Context) (json.RawMessage, error) {
			close(retryDownloading)
			return succeedingPull(runCtx)
		})
		retryDone <- got
	}()
	select {
	case got := <-retryDone:
		require.FailNow(t, "the retry settled before the cancelled download had stopped", "result %s, error %v", got.result, got.err)
	case <-time.After(100 * time.Millisecond):
	}
	secondCancel := make(chan error, 1)
	go func() { secondCancel <- ex.CancelModelPull(ctx, "ollama", "demo") }()
	claim := retry.Value(pullClaimKey{}).(*pullClaim)
	require.Eventually(t, func() bool {
		ex.pullMu.Lock()
		defer ex.pullMu.Unlock()
		return claim.cancelled
	}, 10*time.Second, time.Millisecond, "the second cancel never reached the waiting retry")

	close(finishCleanup)
	require.NoError(t, <-firstCancel)
	require.NoError(t, <-secondCancel)
	originalGot := awaitOutcome(t, ctx, original)
	retryGot := awaitOutcome(t, ctx, retryDone)

	require.NoError(t, originalGot.err)
	assertPullStatus(t, originalGot.result, "cancelled")
	require.NoError(t, retryGot.err)
	assertPullStatus(t, retryGot.result, "cancelled")
	select {
	case <-retryDownloading:
		assert.Fail(t, "the cancelled retry started a download")
	default:
	}
	assert.Equal(t, []ProgressEvent{
		{Engine: "ollama", Model: "demo", Op: "pull", Stage: "cancelled", Message: "demo", Download: 1},
	}, publishedFrames(events), "the model's download ended once")
}

// slowCancelledFrame stands in for an executor's client. It records the pull
// frames sent to it as "model stage", and holds the first "cancelled" frame
// before recording it, as a slow write to the client would, until release is
// closed. holding closes once that frame is held.
type slowCancelledFrame struct {
	holding chan struct{}
	release chan struct{}
	once    sync.Once
	mu      sync.Mutex
	frames  []string
}

func newSlowCancelledFrame() *slowCancelledFrame {
	return &slowCancelledFrame{holding: make(chan struct{}), release: make(chan struct{})}
}

func (c *slowCancelledFrame) emit(method string, params any) {
	frame, ok := params.(map[string]any)
	if !ok || method != "engine:pull-progress" {
		return
	}
	model, _ := frame["model"].(string)
	stage, _ := frame["stage"].(string)
	if stage == "cancelled" {
		held := false
		c.once.Do(func() { held = true })
		if held {
			close(c.holding)
			<-c.release
		}
	}
	c.mu.Lock()
	c.frames = append(c.frames, model+" "+stage)
	c.mu.Unlock()
}

func (c *slowCancelledFrame) sent() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.frames)
}

// awaitHeld waits for the cancelled request to start reporting its
// cancellation, failing the test if ctx ends first.
func (c *slowCancelledFrame) awaitHeld(t *testing.T, ctx context.Context) {
	t.Helper()
	select {
	case <-c.holding:
	case <-ctx.Done():
		require.FailNow(t, "the cancelled request never reported its cancellation")
	}
}

// startCancelledBeforeItRegisters runs a request for demo that a cancel stopped
// before it reached the executor, so it reports the model's "cancelled" itself.
func startCancelledBeforeItRegisters(t *testing.T, ctx context.Context, ex *Executor) <-chan outcome {
	t.Helper()
	request, release := ex.claimPull(ctx, "ollama", "demo")
	t.Cleanup(release)
	require.NoError(t, ex.CancelModelPull(ctx, "ollama", "demo"))
	done := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.trackedPull(request, "ollama", "demo", func(context.Context) (json.RawMessage, error) {
			return nil, errors.New("the cancelled request started its download")
		})
		done <- got
	}()
	return done
}

// A request a cancel stopped before it started reports the model's "cancelled"
// itself when no download of the model has. Frames name only the engine and
// model, so a retry accepted after the cancel waits until that frame is out:
// a "cancelled" landing after the retry's first frame would tell every UI that
// the retry had ended.
func TestTrackedPull_RetryWaitsForTheCancelledFrameOfARequestThatNeverStarted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := newSlowCancelledFrame()
	ex := NewExecutor(NewRegistry(), NewReporter(nil), client.emit, t.TempDir())
	original := startCancelledBeforeItRegisters(t, ctx, ex)
	retry, releaseRetry := ex.claimPull(ctx, "ollama", "demo")
	defer releaseRetry()
	client.awaitHeld(t, ctx)

	retryStarted := make(chan struct{})
	retryDone := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.trackedPull(retry, "ollama", "demo", func(runCtx context.Context) (json.RawMessage, error) {
			close(retryStarted)
			return succeedingPull(runCtx)
		})
		retryDone <- got
	}()
	select {
	case <-retryStarted:
		assert.Fail(t, "the retry started while the cancelled request was still reporting")
	case <-time.After(100 * time.Millisecond):
	}
	close(client.release)
	originalGot := awaitOutcome(t, ctx, original)
	retryGot := awaitOutcome(t, ctx, retryDone)

	require.NoError(t, originalGot.err)
	assertPullStatus(t, originalGot.result, "cancelled")
	require.NoError(t, retryGot.err)
	assertPullStatus(t, retryGot.result, "success")
	assert.Equal(t, []string{"demo cancelled", "demo success"}, client.sent())
}

// A request reporting a cancellation that stopped it before it started holds
// its model only while that frame goes out, and transfers nothing, so another
// model's download on the engine has nothing to queue behind.
func TestTrackedPull_StartsAnotherModelAtOnceWhileACancelledRequestReports(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := newSlowCancelledFrame()
	ex := NewExecutor(NewRegistry(), NewReporter(nil), client.emit, t.TempDir())
	original := startCancelledBeforeItRegisters(t, ctx, ex)
	client.awaitHeld(t, ctx)

	// Bounded, so a download wrongly queued behind the held report fails here
	// rather than at the test's deadline.
	otherCtx, cancelOther := context.WithTimeout(ctx, 5*time.Second)
	defer cancelOther()
	result, err := ex.trackedPull(otherCtx, "ollama", "other", succeedingPull)
	close(client.release)
	awaitOutcome(t, ctx, original)

	require.NoError(t, err)
	assertPullStatus(t, result, "success")
	assert.Equal(t, []string{"other success", "demo cancelled"}, client.sent())
}

// A request reporting a cancellation that stopped it before it started
// transferred nothing, so a cancel that finds it answers as one finding nothing
// would: with the failure of a stop the engine never confirmed, if the model's
// last download ended on one, because that download may still be running.
func TestCancelModelPull_ReportsAnUnconfirmedStopWhileACancelledRequestReports(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := newSlowCancelledFrame()
	ex := NewExecutor(NewRegistry(), NewReporter(nil), client.emit, t.TempDir())
	unconfirmed := stopUnconfirmed(errors.New("the router did not confirm the download stopped"))
	_, err := ex.trackedPull(ctx, "llamacpp", "demo", func(context.Context) (json.RawMessage, error) {
		return nil, unconfirmed
	})
	require.ErrorIs(t, err, unconfirmed, "setup: the download did not end on an unconfirmed stop")
	request, release := ex.claimPull(ctx, "llamacpp", "demo")
	defer release()
	require.ErrorIs(t, ex.CancelModelPull(ctx, "llamacpp", "demo"), unconfirmed, "setup")
	done := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.trackedPull(request, "llamacpp", "demo", func(context.Context) (json.RawMessage, error) {
			return nil, errors.New("the cancelled request started its download")
		})
		done <- got
	}()
	client.awaitHeld(t, ctx)

	wait := ex.latchPullCancel("llamacpp", "demo")
	close(client.release)

	assert.ErrorIs(t, wait(ctx), unconfirmed)
	got := awaitOutcome(t, ctx, done)
	require.NoError(t, got.err)
	assertPullStatus(t, got.result, "cancelled")
}

// A request whose context has already ended — its caller gave up, or PAIR is
// shutting down — must not start its download, and must leave nothing behind
// that stops the next request for the same model.
func TestTrackedPull_RunsARetryAfterARequestWhoseContextEnded(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	ended, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := ex.trackedPull(ended, "ollama", "demo", func(context.Context) (json.RawMessage, error) {
		return nil, errors.New("a request whose context had ended started its download")
	})
	require.ErrorIs(t, err, context.Canceled)

	result, err := ex.trackedPull(context.Background(), "ollama", "demo", succeedingPull)
	require.NoError(t, err)
	assertPullStatus(t, result, "success")
}
