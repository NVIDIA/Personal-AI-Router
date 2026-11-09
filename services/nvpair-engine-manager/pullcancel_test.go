// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeDownloadPercent is the progress the fake CLI reports before it waits to
// be interrupted. The fake engine is a package main under testdata/, so the
// test binary cannot import it; passing the value in on the command line keeps
// it declared once, here, instead of matching a literal across two files.
const fakeDownloadPercent = 25

// testLMSCancelGraces shrinks the twenty seconds a real `lms get` is given to
// answer an interrupt, so a case that models a CLI ignoring one still runs
// quickly. reap stays generous: it covers a real kill and the pipe closing
// behind it.
var testLMSCancelGraces = lmsCancelGraces{
	interrupt: 500 * time.Millisecond,
	retry:     500 * time.Millisecond,
	reap:      30 * time.Second,
}

// outcome is a pull's return pair, carried off the goroutine that ran it.
type outcome struct {
	result json.RawMessage
	err    error
}

func withLMSCancelGraces(ctx context.Context, graces lmsCancelGraces) context.Context {
	return context.WithValue(ctx, lmsCancelGracesKey{}, graces)
}

// fakeDownloadArgv builds the argv for one of the fake engine's download
// subcommands. Each takes the percentage to report before it waits for its
// interrupt.
func fakeDownloadArgv(subcommand string) []string {
	return []string{fakeEngineBin, subcommand, strconv.Itoa(fakeDownloadPercent)}
}

func assertPullStatus(t *testing.T, result json.RawMessage, want string) {
	t.Helper()
	var terminal struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(result, &terminal); err != nil {
		t.Fatalf("decode pull result %s: %v", result, err)
	}
	if terminal.Status != want {
		t.Errorf("status = %q, want %q", terminal.Status, want)
	}
}

func succeedingPull(context.Context) (json.RawMessage, error) {
	return json.RawMessage(`{"status":"success"}`), nil
}

func TestLMSDownloadProgressChunks(t *testing.T) {
	test := func(name string, chunks []string, want int) {
		t.Run(name, func(t *testing.T) {
			got := -1
			output := &lmsDownloadOutput{lastPercent: -1, progress: func(percent int) { got = percent }}
			for _, chunk := range chunks {
				if _, err := output.Write([]byte(chunk)); err != nil {
					t.Fatal(err)
				}
			}
			if got != want {
				t.Fatalf("progress = %d, want %d", got, want)
			}
		})
	}
	test("split percentage", []string{"\r[==== ] 2", "5.80", "%"}, 25)
	test("ANSI redraw", []string{"\x1b[?25l\x1b[s\r[== ] 30.25%\x1b[u", "\r[=== ] 40.90%"}, 40)
	test("indeterminate output", []string{"Resolving model..."}, -1)
	test("clamps percentage", []string{"\r[=== ] 120.00%"}, 100)
}

func TestLMSDownloadAnswersCancellationPrompt(t *testing.T) {
	var stdin bytes.Buffer
	output := &lmsDownloadOutput{stdin: &stdin, lastPercent: -1, progress: func(int) {}}
	for _, chunk := range []string{"Continue to download ", "in the background? (Y/N): ", "Download canceled."} {
		if _, err := output.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if stdin.String() != "n\n" {
		t.Fatalf("answer = %q, want n followed by newline", stdin.String())
	}
	if cancelled, _, _ := output.state(); !cancelled {
		t.Fatal("cancellation acknowledgement was not recorded")
	}
}

// A process the worker could not reclaim keeps writing into this writer, so
// withdrawing its confirmation has to stick. Clearing the flag alone would not:
// the next redraw carrying "Download canceled." would set it again and re-arm
// deletion of files nothing confirmed had stopped.
func TestLMSDownloadDisownedOutputCannotConfirmACancellation(t *testing.T) {
	output := &lmsDownloadOutput{lastPercent: -1, progress: func(int) {}}
	if _, err := output.Write([]byte("Download canceled.")); err != nil {
		t.Fatal(err)
	}
	if cancelled, _, _ := output.state(); !cancelled {
		t.Fatal("acknowledgement was not recorded before the process was disowned")
	}

	output.disown()
	if cancelled, _, _ := output.state(); cancelled {
		t.Fatal("a disowned process still confirmed the cancellation")
	}
	// The abandoned CLI carries on writing; none of it may re-arm deletion.
	if _, err := output.Write([]byte("\rDownload canceled.")); err != nil {
		t.Fatal(err)
	}
	if cancelled, _, _ := output.state(); cancelled {
		t.Fatal("output written after disowning re-armed partial-file deletion")
	}
}

// The exec copy goroutine writes this struct while the goroutine handling the
// cancellation reads it to decide whether a partial file may be deleted, and a
// disowned process makes them overlap for real rather than in principle. The
// assertion is the race detector's: this fails under -race if the fields are
// touched without the lock.
func TestLMSDownloadOutputToleratesConcurrentAccess(t *testing.T) {
	output := &lmsDownloadOutput{lastPercent: -1, progress: func(int) {}}
	writing := make(chan struct{})
	go func() {
		defer close(writing)
		for i := range 200 {
			if _, err := output.Write([]byte(fmt.Sprintf("\r[=== ] %d.00%%", i%100))); err != nil {
				t.Errorf("write: %v", err)
				return
			}
		}
	}()
	for range 200 {
		output.state()
	}
	output.disown()
	<-writing
}

func TestLMSDownloadInterruptsChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	progress := make(chan int, 1)
	output := &lmsDownloadOutput{lastPercent: -1, progress: func(percent int) { progress <- percent }}
	done := make(chan error, 1)
	go func() {
		_, err := runLMSDownloadCommand(ctx, fakeDownloadArgv("canceldownload"), output)
		done <- err
	}()
	select {
	case percent := <-progress:
		if percent != fakeDownloadPercent {
			t.Fatalf("progress = %d, want %d", percent, fakeDownloadPercent)
		}
	case <-ctx.Done():
		t.Fatal("no progress from child")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("download did not stop")
	}
}

// What the CLI does with its interrupt decides what cancellation may do next,
// so each way stopLMSDownload can end gets a case. Only an acknowledged
// cancellation may delete a partial file: declining the background-download
// prompt is what aborts the daemon's task, so a CLI that exited without
// answering it can have left the transfer running. Every other outcome reports
// a failed cancel and leaves the files alone.
func TestLMSDownloadCancellationOutcomes(t *testing.T) {
	test := func(name, subcommand, wantErrText string, wantCancelled bool) {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(withLMSCancelGraces(context.Background(), testLMSCancelGraces))
			defer cancel()
			progress := make(chan int, 1)
			output := &lmsDownloadOutput{lastPercent: -1, progress: func(percent int) {
				select {
				case progress <- percent:
				default:
				}
			}}
			done := make(chan error, 1)
			go func() {
				_, err := runLMSDownloadCommand(ctx, fakeDownloadArgv(subcommand), output)
				done <- err
			}()
			select {
			case <-progress:
			case <-time.After(30 * time.Second):
				t.Fatal("no progress from the download CLI")
			}
			cancel()

			var err error
			select {
			case err = <-done:
			case <-time.After(60 * time.Second):
				t.Fatal("cancellation never settled")
			}
			switch {
			case wantErrText == "" && err != nil:
				t.Fatalf("cancellation = %v, want the completed run", err)
			case wantErrText == "":
			case err == nil:
				t.Fatalf("cancellation succeeded, want an error containing %q", wantErrText)
			case !strings.Contains(err.Error(), wantErrText):
				t.Fatalf("cancellation = %v, want an error containing %q", err, wantErrText)
			}
			if cancelled, _, _ := output.state(); cancelled != wantCancelled {
				t.Errorf("acknowledged = %v, want %v (this is what gates deleting partial files)", cancelled, wantCancelled)
			}
		})
	}
	// The CLI answers the prompt and confirms: context.Canceled is the
	// cancellation the caller asked for, and cleanup may run.
	test("acknowledged cancellation", "canceldownload", context.Canceled.Error(), true)
	// The download finished as the interrupt landed. There is nothing to cancel
	// and nothing to clean up, so the completed run is reported instead.
	test("download completed first", "downloadcompletes", "", false)
	// Exited without the prompt, so the daemon may still be fetching.
	test("exit without confirmation", "downloadsilent", "exited without confirming download cancellation", false)
	// Never exited at all, so the process is killed and the cancel fails.
	test("interrupt ignored", "downloadignoresinterrupt", "did not confirm download cancellation", false)
}

// An interrupt that cannot be delivered leaves the CLI's state unknown, so the
// only safe report is a failed cancel — unless the run had in fact already
// finished, which its reaped result still proves. A process that has been
// reaped is the reachable way to make delivery fail on every platform.
func TestStopLMSDownloadWhenTheInterruptCannotBeDelivered(t *testing.T) {
	test := func(name string, completed bool, wantText, wantErrText string) {
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(fakeEngineBin, "echo", "done")
			cmd.WaitDelay = downloadWaitDelay
			if err := configureDownloadProcess(cmd); err != nil {
				t.Fatalf("configure download process: %v", err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatalf("start fake CLI: %v", err)
			}
			finished := make(chan error, 1)
			finished <- cmd.Wait()
			// The process is reaped, which is exactly the state that must not
			// be signalled: its PID is free for the OS to reissue, and both
			// signals reach more than the CLI.
			var reaped atomic.Bool
			reaped.Store(true)

			output := &lmsDownloadOutput{
				lastPercent: -1,
				progress:    func(int) {},
				completed:   completed,
				text:        "transcript",
			}
			text, err := stopLMSDownload(testLMSCancelGraces, cmd, finished, &reaped, output)
			if text != wantText {
				t.Errorf("text = %q, want %q", text, wantText)
			}
			if wantErrText == "" {
				if err != nil {
					t.Fatalf("stop = %v, want the completed run", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), wantErrText) {
				t.Fatalf("stop = %v, want an error containing %q", err, wantErrText)
			}
		})
	}
	test("the run had already completed", true, "transcript", "")
	test("the interrupt went nowhere", false, "", "could not confirm LM Studio cancellation")
}

func TestLMSDownloadOutputCollectsNamedModelFiles(t *testing.T) {
	output := &lmsDownloadOutput{lastPercent: -1, progress: func(int) {}}
	for _, chunk := range []string{
		"Downloading lmstudio-community/Qwen3-8B-GGUF/Qwen3-8B-Q4",
		"_K_M.gguf\r\x1b[2K[====>    ] 12.5%",
	} {
		if _, err := output.Write([]byte(chunk)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	named := output.namedFiles()
	if !named["Qwen3-8B-Q4_K_M.gguf"] || len(named) != 1 {
		t.Fatalf("named files = %v, want only Qwen3-8B-Q4_K_M.gguf", named)
	}
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
func TestCancelModelPullWaitsForCleanup(t *testing.T) {
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
		t.Fatalf("cancel acknowledged before cleanup finished: %v", err)
	default:
	}

	close(finishCleanup)
	if err := <-cancelled; err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got := <-pull
	if got.err != nil {
		t.Fatalf("pull: %v", got.err)
	}
	assertPullStatus(t, got.result, "cancelled")
}

// Because the cancel waits for the transfer and its cleanup, its own caller
// giving up — a remote initiator disconnecting, the RPC's budget elapsing — has
// to end that wait. The download still stops; only the acknowledgement is
// abandoned.
func TestCancelModelPullStopsWaitingWhenItsCallerGivesUp(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	downloading := make(chan struct{})
	finishCleanup := make(chan struct{})
	pull := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.trackedPull(context.Background(), "ollama", "demo", func(runCtx context.Context) (json.RawMessage, error) {
			close(downloading)
			<-runCtx.Done()
			<-finishCleanup
			return nil, runCtx.Err()
		})
		pull <- got
	}()
	<-downloading
	// Let the download settle after the assertion, so the executor is not left
	// with a pull in flight when the case ends.
	defer func() {
		close(finishCleanup)
		<-pull
	}()

	abandoned, abandon := context.WithCancel(context.Background())
	waited := make(chan error, 1)
	go func() { waited <- ex.CancelModelPull(abandoned, "ollama", "demo") }()
	abandon()

	select {
	case err := <-waited:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel = %v, want context.Canceled", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("cancel kept waiting after its own caller gave up")
	}
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
// reported "cancelled" while the original went on to download.
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
	assertPullStatus(t, originalResult, "cancelled")
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

// A duplicate request joins the download already in flight and reports its
// outcome. Failing it instead produced a terminal error frame attributed to the
// live pull's own model, which disables that row's Cancel button — and a
// cancellation the user asked for has to settle the duplicate's row too, not
// just the row the Cancel was clicked on.
func TestDuplicatePullJoinsTheActiveDownload(t *testing.T) {
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
	terminal := func(stage string, percent int, message string) ProgressEvent {
		return ProgressEvent{Engine: "ollama", Model: "demo", Op: "pull", Stage: stage, Percent: percent, Message: message}
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

func TestCancelledModelPullAllowsRetry(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ex.trackedPull(ctx, "ollama", "demo", func(context.Context) (json.RawMessage, error) {
		t.Error("cancelled pull ran")
		return nil, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pull on a dead context = %v, want context.Canceled", err)
	}
	result, err := ex.trackedPull(context.Background(), "ollama", "demo", succeedingPull)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertPullStatus(t, result, "success")
}

// newOllamaPullExecutor wires an executor whose ollama pull_model action points
// at a fake /api/pull server.
func newOllamaPullExecutor(t *testing.T, serverURL string, emit func(method string, params any)) *Executor {
	t.Helper()
	_, portText, err := net.SplitHostPort(strings.TrimPrefix(serverURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	manifest := testEngineManifest(fakeEngineBin)
	manifest.Engine = "ollama"
	manifest.Actions[pullModelAction] = Action{HTTP: &ActionHTTP{Method: http.MethodPost, Path: "/api/pull"}}
	reg := NewRegistry()
	reg.engines["ollama"] = manifest
	ex := NewExecutor(reg, NewReporter(nil), emit, t.TempDir())
	st, err := ex.state("ollama")
	if err != nil {
		t.Fatal(err)
	}
	st.running = true
	st.port = port
	return ex
}

// newOllamaBlobsDir points the engine at an empty blobs directory. The partial
// arrives once the download starts, the way a real one does, so the snapshot
// the pull takes beforehand can attribute it.
func newOllamaBlobsDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("OLLAMA_MODELS", root)
	blobs := filepath.Join(root, "blobs")
	if err := os.MkdirAll(blobs, 0700); err != nil {
		t.Fatalf("create blobs dir: %v", err)
	}
	return blobs
}

// The pull's context also dies when PAIR quits mid-download, when a remote
// initiator disconnects, and when the action timeout elapses. None of those
// mean the user gave up on the bytes already on disk, so a resumable transfer
// has to survive them instead of restarting from zero on the next attempt.
func TestUnrequestedCancellationKeepsPartials(t *testing.T) {
	blobs := newOllamaBlobsDir(t)
	partial := filepath.Join(blobs, blobA+"-partial")
	streaming := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writePartial(t, partial, "partial")
		w.Header().Set("Content-Type", "application/x-ndjson")
		if _, err := fmt.Fprintf(w, `{"status":"pulling layer","digest":%q,"total":100,"completed":25}`+"\n", digestA); err != nil {
			t.Error(err)
			return
		}
		w.(http.Flusher).Flush()
		close(streaming)
		<-r.Context().Done()
	}))
	defer server.Close()
	ex := newOllamaPullExecutor(t, server.URL, nil)

	ctx, cancel := context.WithCancel(settledContext())
	defer cancel()
	go func() {
		<-streaming
		cancel()
	}()
	if _, err := ex.PullModelStream(ctx, "ollama", "demo", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("pull error = %v, want context.Canceled", err)
	}
	if data, err := os.ReadFile(partial); err != nil || string(data) != "partial" {
		t.Fatalf("a resumable partial was deleted without a cancel request: data=%q err=%v", data, err)
	}
}

// ollamaTransfer serves an Ollama pull that writes the partial blob a transfer
// would, streams statuses, and then holds the connection open until the client
// lets go of it. The partial appears only once the pull has started, so it is
// one the pull created.
func ollamaTransfer(t *testing.T, partial string, statuses ...string) (server *httptest.Server, disconnected <-chan struct{}) {
	t.Helper()
	closed := make(chan struct{})
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(closed)
		if !assert.NoError(t, os.WriteFile(partial, []byte("partial"), 0o600)) {
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		for _, status := range statuses {
			_, err := fmt.Fprintf(w, `{"status":%q,"digest":%q,"total":100,"completed":25}`+"\n", status, digestA)
			if !assert.NoError(t, err) {
				return
			}
		}
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	return server, closed
}

// A cancel someone asked for stops Ollama's transfer and removes the partial
// blob the pull created.
func TestOllamaCancellation_StopsTheTransferAndRemovesItsPartial(t *testing.T) {
	partial := filepath.Join(newOllamaBlobsDir(t), blobA+"-partial")
	server, disconnected := ollamaTransfer(t, partial, "pulling layer")
	downloading := make(chan struct{})
	var once sync.Once
	ex := newOllamaPullExecutor(t, server.URL, func(method string, _ any) {
		if method == "engine:pull-progress" {
			once.Do(func() { close(downloading) })
		}
	})
	ctx, cancel := context.WithTimeout(settledContext(), 30*time.Second)
	defer cancel()
	done := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.PullModelStream(ctx, "ollama", "demo", nil)
		done <- got
	}()
	select {
	case <-downloading:
	case <-ctx.Done():
		require.FailNow(t, "the pull did not start")
	}

	require.NoError(t, ex.CancelModelPull(ctx, "ollama", "demo"))
	got := <-done

	require.NoError(t, got.err)
	assertPullStatus(t, got.result, "cancelled")
	select {
	case <-disconnected:
	case <-ctx.Done():
		require.FailNow(t, "the HTTP transfer did not disconnect")
	}
	assertRemoved(t, partial)
}

// Ollama's success record is its last: the model is installed. The pull
// reports success on it even while the connection stays open, and a cancel
// that lands afterwards finds nothing to stop and leaves the files alone.
func TestOllamaPull_SucceedsOnOllamasSuccessRecord(t *testing.T) {
	test := func(name string, statuses ...string) {
		t.Run(name, func(t *testing.T) {
			partial := filepath.Join(newOllamaBlobsDir(t), blobA+"-partial")
			server, _ := ollamaTransfer(t, partial, statuses...)
			ex := newOllamaPullExecutor(t, server.URL, nil)
			ctx, cancel := context.WithTimeout(settledContext(), 30*time.Second)
			defer cancel()

			result, err := ex.PullModelStream(ctx, "ollama", "demo", nil)
			require.NoError(t, err)
			require.NoError(t, ex.CancelModelPull(ctx, "ollama", "demo"))

			assertPullStatus(t, result, "success")
			assertPresent(t, partial)
		})
	}
	test("success is the last record", "pulling layer", "success")
	test("a record follows success", "pulling layer", "success", "")
}

// Cleanup that cannot finish is not a failed cancellation. The transfer has
// already stopped, which is what was asked for, and the bytes left behind are
// the vendor's to resume. Reporting the cleanup error as the pull's outcome
// denied the one thing that did happen and left the row on "Canceling" over a
// download that was already gone.
func TestOllamaCancellationSucceedsWhenCleanupCannotFinish(t *testing.T) {
	blobs := newOllamaBlobsDir(t)
	partial := filepath.Join(blobs, blobA+"-partial")
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writePartial(t, partial, "partial")
		w.Header().Set("Content-Type", "application/x-ndjson")
		if _, err := fmt.Fprintf(w, `{"status":"pulling layer","digest":%q,"total":100,"completed":25}`+"\n", digestA); err != nil {
			t.Error(err)
			return
		}
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	ex := newOllamaPullExecutor(t, server.URL, nil)

	ctx, cancel := context.WithTimeout(settledContext(), 60*time.Second)
	defer cancel()
	done := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.PullModelStream(ctx, "ollama", "demo", nil)
		done <- got
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("pull did not start")
	}
	// The partial exists and is attributable to this pull by now, so this is
	// the point its removal can be made to fail.
	blockPartialRemoval(t, partial)

	if err := ex.CancelModelPull(ctx, "ollama", "demo"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("pull = %v, want the cancellation to be reported as a cancellation", got.err)
	}
	assertPullStatus(t, got.result, "cancelled")
	// Cleanup could not remove it, and saying so is the warning's job, not the
	// cancel's. The file stays for the next attempt to resume.
	assertPresent(t, partial)
}

// A predecessor here is another of this engine's downloads that registered
// first. trackedPull serializes pulls per engine — the vendor caches share
// partial files across models, so one pull's cleanup must not run while another
// PAIR pull is writing — which leaves a later request queued behind the ones
// already in flight.
//
// Cancelling a queued download has to settle on its own, without waiting for a
// turn it will never take: the row already says "Canceling", and a predecessor
// can be a multi-gigabyte transfer. The channels below stay open for the whole
// case to hold the predecessors in flight and prove that.
func TestQueuedModelPullCancelsBeforePredecessorsFinish(t *testing.T) {
	test := func(name string, predecessorCount int) {
		t.Run(name, func(t *testing.T) {
			queued := make(chan struct{}, 1)
			ex := NewExecutor(NewRegistry(), NewReporter(nil), func(method string, _ any) {
				if method == "engine:pull-progress" {
					queued <- struct{}{}
				}
			}, t.TempDir())
			ex.pulls = make(map[pullKey]*activePull)
			for i := 0; i < predecessorCount; i++ {
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
				t.Fatal("pull was not queued")
			}
			if err := ex.CancelModelPull(ctx, "ollama", "queued"); err != nil {
				t.Fatalf("cancel queued pull: %v", err)
			}
			select {
			case got := <-finished:
				if got.err != nil {
					t.Fatalf("queued pull: %v", got.err)
				}
				if ran {
					t.Error("canceled queued transfer started")
				}
				assertPullStatus(t, got.result, "cancelled")
			case <-ctx.Done():
				t.Fatal("queued pull did not finish after cancellation")
			}
			ex.pullMu.Lock()
			remaining := len(ex.pulls)
			_, stillQueued := ex.pulls[pullKey{engine: "ollama", model: "queued"}]
			ex.pullMu.Unlock()
			if stillQueued || remaining != predecessorCount {
				t.Fatalf("registry after cancellation: queued=%v, remaining=%d", stillQueued, remaining)
			}
		})
	}
	test("one active predecessor", 1)
	test("multiple blocked predecessors", 3)
}
