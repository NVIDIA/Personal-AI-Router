// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
var testLMSCancelGraces = downloadStopGraces{
	interrupt: 500 * time.Millisecond,
	retry:     500 * time.Millisecond,
	reap:      30 * time.Second,
}

// fakeDownloadArgv builds the argv for one of the fake engine's download
// subcommands. Each takes the percentage to report before it waits for its
// interrupt, and optionally the partial file to write first.
func fakeDownloadArgv(subcommand string) []string {
	return []string{fakeEngineBin, subcommand, strconv.Itoa(fakeDownloadPercent)}
}

// `lms get` redraws its progress bar with carriage returns and ANSI controls
// rather than lines, and a pipe can split one redraw across writes, so the
// percentage is read from the output accumulated so far.
func TestLMSDownloadOutput_ReadsProgressFromTheCLIsRedraws(t *testing.T) {
	test := func(name string, chunks []string, want []int) {
		t.Run(name, func(t *testing.T) {
			var reported []int
			output := &lmsDownloadOutput{lastPercent: -1, progress: func(percent int) { reported = append(reported, percent) }}
			for _, chunk := range chunks {
				_, err := output.Write([]byte(chunk))
				require.NoError(t, err)
			}

			assert.Equal(t, want, reported)
		})
	}
	test("a percentage split across writes", []string{"\r[==== ] 2", "5.80", "%"}, []int{25})
	test("each new percentage of an ANSI redraw", []string{"\x1b[?25l\x1b[s\r[== ] 30.25%\x1b[u", "\r[=== ] 40.90%"}, []int{30, 40})
	test("output that reports no percentage", []string{"Resolving model..."}, nil)
	test("a percentage past 100, clamped", []string{"\r[=== ] 120.00%"}, []int{100})
}

// Interrupted, `lms get` asks whether to go on downloading in the background,
// and only declining stops the daemon's transfer. The question can arrive split
// across writes like any other output.
func TestLMSDownloadOutput_DeclinesToContinueInTheBackground(t *testing.T) {
	var stdin bytes.Buffer
	output := &lmsDownloadOutput{stdin: &stdin, lastPercent: -1, progress: func(int) {}}
	for _, chunk := range []string{"Continue to download ", "in the background? (Y/N): ", "Download canceled."} {
		_, err := output.Write([]byte(chunk))
		require.NoError(t, err)
	}

	assert.Equal(t, "n\n", stdin.String())
	cancelled, _, _ := output.state()
	assert.True(t, cancelled, "the CLI's confirmation of the cancellation was not recorded")
}

// A process the worker could not reclaim keeps writing into this writer, so
// withdrawing its confirmation has to stick. Clearing the flag alone would not:
// the next redraw carrying "Download canceled." would set it again and re-arm
// deletion of files nothing confirmed had stopped.
func TestLMSDownloadOutput_DisownedProcessCannotConfirmACancellation(t *testing.T) {
	output := &lmsDownloadOutput{lastPercent: -1, progress: func(int) {}}
	_, err := output.Write([]byte("Download canceled."))
	require.NoError(t, err)
	cancelled, _, _ := output.state()
	require.True(t, cancelled, "the confirmation was not recorded before the process was disowned")

	output.disown()
	cancelled, _, _ = output.state()
	assert.False(t, cancelled, "a disowned process still confirmed the cancellation")

	// The abandoned CLI carries on writing; none of it may re-arm deletion.
	_, err = output.Write([]byte("\rDownload canceled."))
	require.NoError(t, err)
	cancelled, _, _ = output.state()
	assert.False(t, cancelled, "output written after disowning re-armed partial-file deletion")
}

// The exec copy goroutine writes this struct while the goroutine handling the
// cancellation reads it to decide whether a partial file may be deleted, and a
// disowned process makes them overlap for real rather than in principle. The
// assertion is the race detector's: this fails under -race if the fields are
// touched without the lock.
func TestLMSDownloadOutput_ToleratesConcurrentAccess(t *testing.T) {
	output := &lmsDownloadOutput{lastPercent: -1, progress: func(int) {}}
	writing := make(chan struct{})
	go func() {
		defer close(writing)
		for i := range 200 {
			_, err := output.Write([]byte(fmt.Sprintf("\r[=== ] %d.00%%", i%100)))
			if !assert.NoError(t, err) {
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

// Without a pinned quantization, the download plan `lms get` prints is the only
// evidence of which partial is this download's: it names the quantization the
// CLI chose, and never the file. A pipe can split a plan line partway through
// that name, so only a finished line counts, and only a GGUF line whose
// quantization LM Studio knew names one at all.
func TestLMSDownloadOutput_ReadsTheQuantizationItsPlanAnnounces(t *testing.T) {
	test := func(name string, chunks []string, want []string) {
		t.Run(name, func(t *testing.T) {
			output := &lmsDownloadOutput{lastPercent: -1, progress: func(int) {}}
			for _, chunk := range chunks {
				_, err := output.Write([]byte(chunk))
				require.NoError(t, err)
			}

			assert.Equal(t, want, output.announcedQuants())
		})
	}
	test("a plan line split across writes", []string{
		"\r ↓ To download: Qwen3 8B Q4",
		"_K_M [GGUF] - 5.03 GB\x1b[0K\n",
	}, []string{"Q4_K_M"})
	test("the model a hub artifact depends on", []string{
		"\r ↓ To download: model qwen/qwen3-8b - 5.03 GB\x1b[0K\n",
		"\r └─ ↓ To download: Qwen3 8B Q8_0 [GGUF] - 8.71 GB\x1b[0K\n",
	}, []string{"Q8_0"})
	test("a plan redrawn while it resolves", []string{
		"\r ↓ To download: Qwen3 8B Q4_K_M [GGUF] - 5.03 GB\x1b[0K\n",
		"\r ↓ To download: Qwen3 8B Q4_K_M [GGUF] - 5.03 GB\x1b[0K\n",
	}, []string{"Q4_K_M"})
	test("a plan line the CLI has not finished", []string{"\r ↓ To download: Qwen3 8B Q4"}, nil)
	test("an MLX model, whose files carry no quantization", []string{
		"\r ↓ To download: Qwen3 8B 4bit [MLX] - 4.62 GB\x1b[0K\n",
	}, nil)
	test("a plan line without a quantization", []string{
		"\r ↓ To download: Qwen3 8B [GGUF] - 5.03 GB\x1b[0K\n",
	}, nil)
	test("a file name outside the plan", []string{
		"lmstudio-community/Qwen3-8B-GGUF/Qwen3-8B-Q4_K_M.gguf\n",
	}, nil)
}

// A transfer that fails transiently is resumed by running `lms get` again, and
// the resumed run can fail before it prints its plan. The quantization an
// earlier run announced still identifies this download's partial, so starting
// the next run must not forget it.
func TestLMSDownloadOutput_KeepsAnnouncedQuantizationsAcrossResumedRuns(t *testing.T) {
	output := &lmsDownloadOutput{lastPercent: -1, progress: func(int) {}}
	_, err := output.Write([]byte("\r ↓ To download: Qwen3 8B Q4_K_M [GGUF] - 5.03 GB\x1b[0K\n"))
	require.NoError(t, err)

	output.reset(io.Discard)

	assert.Equal(t, []string{"Q4_K_M"}, output.announcedQuants())
}

// Cancelling the download interrupts the CLI the way Ctrl+C would rather than
// killing it, because only an interrupted `lms get` asks the question whose
// answer stops the daemon's transfer.
func TestRunLMSDownloadCommand_InterruptsTheCLIWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
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
		require.Equal(t, fakeDownloadPercent, percent, "the CLI's progress was misread")
	case err := <-done:
		require.FailNow(t, "the download CLI ended before reporting progress", "%v", err)
	// The broker-flags test runs this in a child that has just rebuilt the
	// fake CLI, and starting it there under load has taken over ten seconds.
	case <-time.After(30 * time.Second):
		require.FailNow(t, "no progress from the download CLI")
	}

	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(20 * time.Second):
		require.FailNow(t, "the download CLI did not stop")
	}
}

// What the CLI does with its interrupt decides what cancellation may do next,
// and only a cancellation the CLI confirmed may delete a partial file:
// declining the background-download prompt is what aborts the daemon's task,
// so a CLI that exited without answering it can have left the transfer
// running. Every other outcome reports a failed cancel and leaves the files
// alone.
func TestRunLMSDownloadCommand_ConfirmsACancellationOnlyWhenTheCLIDoes(t *testing.T) {
	// interrupt runs a fake CLI until it reports progress, cancels it, and
	// returns whether the CLI confirmed the cancellation and how the run ended.
	interrupt := func(t *testing.T, subcommand string) (bool, error) {
		t.Helper()
		ctx, cancel := context.WithCancel(withDownloadStopGraces(t.Context(), testLMSCancelGraces))
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
			require.FailNow(t, "no progress from the download CLI")
		}

		cancel()
		select {
		case err := <-done:
			confirmed, _, _ := output.state()
			return confirmed, err
		case <-time.After(60 * time.Second):
			require.FailNow(t, "the cancellation never settled")
			return false, nil
		}
	}

	// The CLI declines to continue in the background and confirms: the run
	// ends as the cancellation the caller asked for, and cleanup may run.
	t.Run("the CLI confirms the cancellation", func(t *testing.T) {
		confirmed, err := interrupt(t, "canceldownload")
		assert.ErrorIs(t, err, context.Canceled)
		assert.True(t, confirmed)
	})
	// The download finished as the interrupt landed, so there is nothing to
	// cancel or clean up and the completed run is reported instead.
	t.Run("the download completes first", func(t *testing.T) {
		confirmed, err := interrupt(t, "downloadcompletes")
		assert.NoError(t, err)
		assert.False(t, confirmed, "a completed download was taken for a cancelled one")
	})
	fails := func(name, subcommand, wantErr string) {
		t.Run(name, func(t *testing.T) {
			confirmed, err := interrupt(t, subcommand)
			assert.ErrorContains(t, err, wantErr)
			assert.False(t, confirmed, "a stop the CLI never confirmed would allow its partial files to be deleted")
		})
	}
	// Exited without the prompt, so the daemon may still be fetching.
	fails("the CLI exits without confirming", "downloadsilent", "exited without confirming download cancellation")
	// Never exited at all, so the process is killed and the cancel fails.
	fails("the CLI ignores the interrupt", "downloadignoresinterrupt", "did not confirm download cancellation")
}

// A process `lms get` starts can outlive it still holding the output it
// inherited, so Wait stops reading that output downloadWaitDelay after the CLI
// exits. The CLI's own exit decides the run: failing a download it finished
// would have the user fetch the model again.
func TestRunLMSDownloadCommand_SucceedsWhenTheCLILeavesAHelperBehind(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "helper.pid")
	t.Cleanup(func() { killFakeHelper(pidFile) })
	output := &lmsDownloadOutput{lastPercent: -1, progress: func(int) {}}

	text, err := runLMSDownloadCommand(t.Context(), append(fakeDownloadArgv("downloadleavesahelper"), pidFile), output)

	require.NoError(t, err, "the helper failed a download the CLI completed")
	assert.Contains(t, text, "Download completed.")
}

// An interrupt that cannot be delivered leaves the CLI's state unknown, so the
// only safe report is a failed cancel — unless the run had in fact already
// finished, which its reaped result still proves. A process that has been
// reaped is the reachable way to make delivery fail on every platform.
func TestStopLMSDownload_FailsAnUndeliverableInterruptUnlessTheRunCompleted(t *testing.T) {
	// stop runs stopLMSDownload against a fake CLI that has already been
	// reaped, with output that did or did not see the download complete.
	stop := func(t *testing.T, completed bool) (string, error) {
		t.Helper()
		cmd := exec.Command(fakeEngineBin, "echo", "done")
		cmd.WaitDelay = downloadWaitDelay
		require.NoError(t, configureDownloadProcess(cmd))
		require.NoError(t, cmd.Start(), "start the fake CLI")
		runErr := cmd.Wait()
		// The process is reaped, which is exactly the state that must not be
		// signalled: its PID is free for the OS to reissue, and both signals
		// reach more than the CLI.
		exited := make(chan struct{})
		close(exited)
		output := &lmsDownloadOutput{lastPercent: -1, progress: func(int) {}, completed: completed, text: "transcript"}
		return stopLMSDownload(testLMSCancelGraces, lmsDownloadProcess(cmd, exited), func() error { return runErr }, output)
	}

	t.Run("the run had already completed", func(t *testing.T) {
		text, err := stop(t, true)
		require.NoError(t, err)
		assert.Equal(t, "transcript", text)
	})
	t.Run("the interrupt went nowhere", func(t *testing.T) {
		text, err := stop(t, false)
		assert.ErrorContains(t, err, "could not confirm LM Studio cancellation")
		assert.Empty(t, text)
	})
}

// resumeWaitHandler reports when the LM Studio runner logs that it is waiting
// to resume a failed transfer, a point at which no CLI is running.
type resumeWaitHandler struct {
	waiting chan struct{}
	once    sync.Once
}

func (*resumeWaitHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *resumeWaitHandler) Handle(_ context.Context, record slog.Record) error {
	if record.Message == "lms get download failed transiently; resuming" {
		h.once.Do(func() { close(h.waiting) })
	}
	return nil
}

func (h *resumeWaitHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *resumeWaitHandler) WithGroup(string) slog.Handler      { return h }

// newLMSPullExecutor returns an executor whose fake engine downloads models
// into modelsDir by running cmd in place of `lms get`.
func newLMSPullExecutor(t *testing.T, modelsDir string, cmd ...string) *Executor {
	t.Helper()
	manifest := testEngineManifest(fakeEngineBin)
	platformKey := runtime.GOOS + "/" + runtime.GOARCH
	platform := manifest.Platforms[platformKey]
	platform.ModelsDir = modelsDir
	manifest.Platforms[platformKey] = platform
	manifest.Actions[pullModelAction] = Action{Cmd: cmd, ModelResolution: modelResolutionLMSGet}
	return newTestExecutor(t, manifest)
}

// `lms get` resumes a transfer that timed out by running again after a pause,
// and no CLI runs during that pause. A cancel landing there found nothing to
// interrupt and reported the timeout instead: the cancel failed, a retry error
// was filed for a download the user had stopped, and its partial stayed behind.
func TestLMSPull_CancelWhileWaitingToResumeStopsTheDownload(t *testing.T) {
	// An hour-long pause guarantees the cancel lands in it, after the first
	// run has exited and before a second one starts.
	defer setResumeBudget(t, 3, time.Hour)()
	waiting := make(chan struct{})
	previousLogger, previousOutput, previousFlags := slog.Default(), log.Writer(), log.Flags()
	slog.SetDefault(slog.New(&resumeWaitHandler{waiting: waiting}))
	// Installing a handler also redirects the log package, and restoring the
	// default logger does not point it back.
	t.Cleanup(func() {
		slog.SetDefault(previousLogger)
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
	})
	modelsDir := t.TempDir()
	partial := filepath.Join(modelsDir, "owner", "repo", "downloading_repo-Q4_K_M.gguf.part")
	ex := newLMSPullExecutor(t, modelsDir, fakeEngineBin, "downloadstalls", partial, "{model}")
	events, unsubscribe := ex.progress.subscribe("fake")
	defer unsubscribe()
	ctx, cancel := context.WithTimeout(settledContext(t), 30*time.Second)
	defer cancel()
	pull := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.PullModelStream(ctx, "fake", "owner/repo", nil)
		pull <- got
	}()
	select {
	case <-waiting:
	case <-ctx.Done():
		require.FailNow(t, "the download never waited to resume")
	}

	require.NoError(t, ex.CancelModelPull(ctx, "fake", "owner/repo"))
	got := <-pull

	require.NoError(t, got.err, "the cancellation should settle the pull as cancelled")
	assertPullStatus(t, got.result, "cancelled")
	// The request pins no quantization, so the one the CLI's plan announced is
	// what makes this partial attributable to the download.
	assertRemoved(t, partial)
	frames := publishedFrames(events)
	require.NotEmpty(t, frames, "the pull published no frames")
	assert.Equal(t, "cancelled", frames[len(frames)-1].Stage, "the last frame must report the cancellation")
}

// startLMSDownload pulls model, a bare owner/repo reference, through a fake
// `lms get` running subcommand, which writes a partial file in the repository's
// directory and announces its quantization before it waits to be interrupted.
// It returns once the CLI has reported progress.
func startLMSDownload(t *testing.T, ctx context.Context, subcommand, model string) (*Executor, string, <-chan outcome) {
	t.Helper()
	modelsDir := t.TempDir()
	owner, repo, _ := strings.Cut(model, "/")
	partial := filepath.Join(modelsDir, owner, repo, "downloading_"+repo+"-Q4_K_M.gguf.part")
	ex := newLMSPullExecutor(t, modelsDir, append(fakeDownloadArgv(subcommand), partial, "{model}")...)
	events, unsubscribe := ex.progress.subscribe("fake")
	defer unsubscribe()
	done := make(chan outcome, 1)
	go func() {
		var got outcome
		got.result, got.err = ex.PullModelStream(ctx, "fake", model, nil)
		done <- got
	}()
	for {
		select {
		case ev := <-events:
			if ev.Stage == "downloading" {
				return ex, partial, done
			}
		case got := <-done:
			require.FailNow(t, "the download ended before reporting progress", "%v", got.err)
		case <-ctx.Done():
			require.FailNow(t, "no progress from the download CLI")
		}
	}
}

// A cancel someone asked for interrupts the running CLI, which declines to
// continue in the background and confirms. That is the stop that may delete
// the partial, and the pull settles as the cancellation that was asked for.
// The LM Studio Hub does not have owner/repo, so the CLI ran from the
// repository's Hugging Face URL, which downloads into the repository's
// directory.
func TestLMSCancellation_StopsTheDownloadAndRemovesItsPartial(t *testing.T) {
	ctx, cancel := context.WithTimeout(settledContext(t), 30*time.Second)
	defer cancel()
	ex, partial, done := startLMSDownload(t, ctx, "canceldownload", "owner/repo")

	require.NoError(t, ex.CancelModelPull(ctx, "fake", "owner/repo"))
	got := awaitOutcome(t, ctx, done)

	require.NoError(t, got.err)
	assertPullStatus(t, got.result, "cancelled")
	assertRemoved(t, partial)
}

// A model the LM Studio Hub resolves is stored under the owner and repository
// of the source its definition names, such as lmstudio-community/Qwen3-8B-GGUF
// for qwen/qwen3-8b, and `lms get` never prints which. The directory the
// request names then says nothing about which files are this download's, so
// cancelling removes nothing, whatever appears there.
func TestLMSCancellation_RemovesNothingForAModelTheHubResolved(t *testing.T) {
	ctx, cancel := context.WithTimeout(settledContext(t), 30*time.Second)
	defer cancel()
	ex, partial, done := startLMSDownload(t, ctx, "canceldownload", "hub/model")

	require.NoError(t, ex.CancelModelPull(ctx, "fake", "hub/model"))
	got := awaitOutcome(t, ctx, done)

	require.NoError(t, got.err)
	assertPullStatus(t, got.result, "cancelled")
	assertPresent(t, partial)
}

// The pull's context also ends when PAIR quits mid-download, when a remote
// initiator disconnects, and when the action timeout elapses. The CLI confirms
// that interrupt like any other, but nobody gave up on the bytes already on
// disk, and `lms get` resumes them on the next attempt.
func TestLMSPull_KeepsItsPartialWhenStoppedWithoutACancelRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(settledContext(t), 30*time.Second)
	defer cancel()
	pullCtx, stopPull := context.WithCancel(ctx)
	defer stopPull()
	_, partial, done := startLMSDownload(t, pullCtx, "canceldownload", "owner/repo")

	stopPull()
	got := awaitOutcome(t, ctx, done)

	assert.ErrorIs(t, got.err, context.Canceled)
	assertPresent(t, partial)
}

// A download the action timeout stopped says so. The CLI confirms that
// interrupt as it does a cancel's, and the frame the download ended on used to
// give the bare "context canceled" it was stopped with as the reason.
func TestLMSPull_ReportsTheActionTimeoutThatStoppedIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(settledContext(t), 30*time.Second)
	defer cancel()
	modelsDir := t.TempDir()
	partial := filepath.Join(modelsDir, "owner", "repo", "downloading_repo-Q4_K_M.gguf.part")
	ex := newLMSPullExecutor(t, modelsDir, append(fakeDownloadArgv("canceldownload"), partial, "{model}")...)
	ex.actionTimeout = 2 * time.Second
	events, unsubscribe := ex.progress.subscribe("fake")
	defer unsubscribe()

	_, err := ex.PullModelStream(ctx, "fake", "owner/repo", nil)

	assert.ErrorContains(t, err, "the download did not finish within 2s")
	assert.NotErrorIs(t, err, context.Canceled)
	frames := publishedFrames(events)
	require.NotEmpty(t, frames, "the pull published no frames")
	assert.Equal(t, "error", frames[len(frames)-1].Stage)
	assert.Contains(t, frames[len(frames)-1].Message, "the download did not finish within 2s")
}

// Only a cancellation the CLI confirmed is known to have stopped LM Studio's
// transfer: a CLI that exited without answering the background-download prompt
// may have left the daemon fetching. Reporting that as "cancelled" would tell
// the user the download stopped, so the cancel fails, and the partial stays for
// whatever is still writing it.
func TestLMSPull_FailsARequestedCancellationTheCLIDidNotConfirm(t *testing.T) {
	ctx, cancel := context.WithTimeout(settledContext(t), 30*time.Second)
	defer cancel()
	ex, partial, done := startLMSDownload(t, ctx, "downloadsilent", "owner/repo")

	assert.ErrorContains(t, ex.CancelModelPull(ctx, "fake", "owner/repo"), "exited without confirming download cancellation")
	got := awaitOutcome(t, ctx, done)

	assert.ErrorContains(t, got.err, "exited without confirming download cancellation")
	assertPresent(t, partial)
	// The daemon may still be fetching after the pull settled, so a cancel
	// asking again cannot be answered as though the download had stopped.
	assert.ErrorContains(t, ex.CancelModelPull(ctx, "fake", "owner/repo"), "exited without confirming download cancellation",
		"a cancel asking after the pull settled")
}

// fromHuggingFace is the `lms get` argument a pull of model runs when the LM
// Studio Hub does not have it: the repository's huggingface.co URL, the only
// run whose partials cleanup can attribute.
func fromHuggingFace(model string) string {
	if isHTTPURL(model) {
		return model
	}
	return "https://huggingface.co/" + model
}

// Only a run from a huggingface.co URL downloads into the repository directory
// the request names. Whatever else `lms get` resolved, a Hub artifact by id or
// by lmstudio.ai URL, stores its files under its source's owner and repository
// instead, so cancelling it removes nothing; and a cancel that landed before
// any run started has nothing to attribute either.
func TestCleanupLMSAfterCancel_RemovesNothingUnlessTheCLIRanFromAHuggingFaceURL(t *testing.T) {
	const model = "owner/repo@Q4_K_M"
	test := func(name, source string, wantRemoved bool) {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "owner", "repo")
			require.NoError(t, os.MkdirAll(target, 0700))
			before, err := lmsPartialFiles(root, model)
			require.NoError(t, err)
			partial := filepath.Join(target, "downloading_repo-Q4_K_M.gguf.part")
			writePartial(t, partial, "data")

			require.NoError(t, cleanupLMSAfterCancel(settledContext(t), root, model, source, before, nil))

			if wantRemoved {
				assertRemoved(t, partial)
			} else {
				assertPresent(t, partial)
			}
		})
	}
	test("a Hugging Face URL", "https://huggingface.co/owner/repo@Q4_K_M", true)
	test("a Hub artifact id", "owner/repo@Q4_K_M", false)
	test("an lmstudio.ai URL", "https://lmstudio.ai/models/owner/repo@Q4_K_M", false)
	test("no run", "", false)
}

func TestCleanupLMSAfterCancel_RemovesThePartialButKeepsCompletedShards(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "owner", "repo")
	completeShard := filepath.Join(target, "model-00001-of-00002.gguf")
	writePartial(t, completeShard, "data")
	const model = "https://huggingface.co/owner/repo@Q4_K_M"
	before, err := lmsPartialFiles(root, model)
	require.NoError(t, err)
	partial := filepath.Join(target, "downloading_model-Q4_K_M.gguf.part")
	writePartial(t, partial, "data")

	require.NoError(t, cleanupLMSAfterCancel(settledContext(t), root, model, fromHuggingFace(model), before, nil))

	assertRemoved(t, partial)
	assertPresent(t, completeShard)
}

// An unpinned request lets the CLI choose the quantization, so a partial that
// appeared since the snapshot is only this download's when it carries the one
// the CLI announced. Another can be the LM Studio app fetching a different
// quantization of the same repository, even though it is new and holding still.
func TestCleanupLMSAfterCancel_UnpinnedRequestRemovesOnlyTheAnnouncedQuantization(t *testing.T) {
	const model = "lmstudio-community/Qwen3-8B-GGUF"
	test := func(name string, announced []string, wantOursRemoved bool) {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
			require.NoError(t, os.MkdirAll(target, 0700))
			before, err := lmsPartialFiles(root, model)
			require.NoError(t, err)
			ours := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
			theirs := filepath.Join(target, "downloading_Qwen3-8B-Q8_0.gguf.part")
			writePartial(t, ours, "our download")
			writePartial(t, theirs, "the app's download, stalled")

			require.NoError(t, cleanupLMSAfterCancel(settledContext(t), root, model, fromHuggingFace(model), before, announced))

			if wantOursRemoved {
				assertRemoved(t, ours)
			} else {
				assertPresent(t, ours)
			}
			assertPresent(t, theirs)
		})
	}
	test("removes the announced quantization and keeps the other", []string{"Q4_K_M"}, true)
	test("deletes nothing when the CLI announced none", nil, false)
}

// The LM Studio app downloads into the same repository directory PAIR does, so
// cancelling a @Q4_K_M pull must not remove the @Q8_0 the app is fetching —
// neither the copy that grew since the snapshot nor one created after it.
func TestCleanupLMSAfterCancel_KeepsOtherQuantizations(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
	growing := filepath.Join(target, "downloading_Qwen3-8B-Q8_0.gguf.part")
	writePartial(t, growing, "app download")
	const model = "lmstudio-community/Qwen3-8B-GGUF@Q4_K_M"
	before, err := lmsPartialFiles(root, model)
	require.NoError(t, err)
	appeared := filepath.Join(target, "downloading_Qwen3-8B-Q6_K.gguf.part")
	ours := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
	writePartial(t, growing, "app download, now longer")
	writePartial(t, appeared, "app started this one after our snapshot")
	writePartial(t, ours, "our download")

	require.NoError(t, cleanupLMSAfterCancel(settledContext(t), root, model, fromHuggingFace(model), before, nil))

	assertPresent(t, growing)
	assertPresent(t, appeared)
	assertRemoved(t, ours)
}

// Quantization names nest: F16 is part of BF16, and Q6_K part of Q6_K_L. A
// partial is this download's only when it carries the requested name whole, or
// cancelling one quantization removes another that the LM Studio app is
// fetching beside it.
func TestCleanupLMSAfterCancel_KeepsQuantizationsWhoseNamesContainTheRequestedOne(t *testing.T) {
	test := func(name, quant, ours, theirs string) {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
			require.NoError(t, os.MkdirAll(target, 0700))
			model := "lmstudio-community/Qwen3-8B-GGUF@" + quant
			before, err := lmsPartialFiles(root, model)
			require.NoError(t, err)
			oursPath := filepath.Join(target, "downloading_"+ours+".part")
			theirsPath := filepath.Join(target, "downloading_"+theirs+".part")
			writePartial(t, oursPath, "our download")
			writePartial(t, theirsPath, "the app's download")

			require.NoError(t, cleanupLMSAfterCancel(settledContext(t), root, model, fromHuggingFace(model), before, nil))

			assertRemoved(t, oursPath)
			assertPresent(t, theirsPath)
		})
	}
	test("F16 beside BF16", "F16", "Qwen3-8B-F16.gguf", "Qwen3-8B-BF16.gguf")
	test("Q6_K beside Q6_K_L", "Q6_K", "Qwen3-8B-Q6_K.gguf", "Qwen3-8B-Q6_K_L.gguf")
	test("Q4_0 beside Q4_0_4_4", "Q4_0", "Qwen3-8B-Q4_0.gguf", "Qwen3-8B-Q4_0_4_4.gguf")
	test("a pin in lower case", "q6_k", "Qwen3-8B-Q6_K.gguf", "Qwen3-8B-Q6_K_L.gguf")
	test("shards of split downloads", "Q4_K", "Qwen3-8B-Q4_K-00001-of-00002.gguf", "Qwen3-8B-Q4_K_M-00001-of-00002.gguf")
}

// The TUI's download prompt sends whatever follows "@" as typed. Matched as a
// token, a pin that names no quantization — "@GGUF", "@8B" — is one every
// quantization's file in the repository carries, so it pins none, and a partial
// is this download's only when it carries the quantization the CLI announced.
func TestCleanupLMSAfterCancel_TreatsAPinThatNamesNoQuantizationAsNone(t *testing.T) {
	test := func(name, pin string, announced []string, wantOursRemoved bool) {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "lmstudio-community", "Meta-Llama-3.1-8B-Instruct-GGUF")
			require.NoError(t, os.MkdirAll(target, 0700))
			model := "lmstudio-community/Meta-Llama-3.1-8B-Instruct-GGUF@" + pin
			before, err := lmsPartialFiles(root, model)
			require.NoError(t, err)
			ours := filepath.Join(target, "downloading_Meta-Llama-3.1-8B-Instruct-Q4_K_M.gguf.part")
			theirs := filepath.Join(target, "downloading_Meta-Llama-3.1-8B-Instruct-Q8_0.gguf.part")
			writePartial(t, ours, "our download")
			writePartial(t, theirs, "the app's download")

			require.NoError(t, cleanupLMSAfterCancel(settledContext(t), root, model, fromHuggingFace(model), before, announced))

			if wantOursRemoved {
				assertRemoved(t, ours)
			} else {
				assertPresent(t, ours)
			}
			assertPresent(t, theirs)
		})
	}
	test("@GGUF removes the announced quantization", "GGUF", []string{"Q4_K_M"}, true)
	test("@8B removes the announced quantization", "8B", []string{"Q4_K_M"}, true)
	test("@8B deletes nothing when the CLI announced none", "8B", nil, false)
}

// The app can also be fetching the very quantization PAIR asked for, into the
// same file. Carrying the requested quantization in its name is what makes a
// partial a candidate, not what makes it this download's, so one still growing
// after this transfer stopped has to survive too.
func TestCleanupLMSAfterCancel_KeepsTheRequestedQuantizationWhileAnotherClientWritesIt(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
	const model = "lmstudio-community/Qwen3-8B-GGUF@Q4_K_M"
	require.NoError(t, os.MkdirAll(target, 0700))
	before, err := lmsPartialFiles(root, model)
	require.NoError(t, err)
	shared := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
	writePartial(t, shared, "shared download")
	ctx := withPartialSettle(t.Context(), func() { growFile(t, shared) })

	require.NoError(t, cleanupLMSAfterCancel(ctx, root, model, fromHuggingFace(model), before, nil))

	assertPresent(t, shared)
}

// Growing since the snapshot is not evidence of ownership. A writer that paused
// looks exactly like one that finished, so a partial already present when this
// pull started stays another client's however many bytes it gained meanwhile.
//
// The model hub pulls unpinned ids, and the app fetching the quantization the
// CLI then announces writes the very file this pull would — so nothing but the
// snapshot excludes it, which is why that rule has to be the strict one.
func TestCleanupLMSAfterCancel_KeepsAPreexistingPartialThatGrew(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
	const model = "lmstudio-community/Qwen3-8B-GGUF"
	theirs := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
	writePartial(t, theirs, "app download")
	before, err := lmsPartialFiles(root, model)
	require.NoError(t, err)
	require.Len(t, before, 1, "the snapshot should hold the partial already present")
	// The app writes more while our download runs, then stalls, so the file is
	// perfectly still by the time cleanup observes it.
	growFile(t, theirs)

	require.NoError(t, cleanupLMSAfterCancel(settledContext(t), root, model, fromHuggingFace(model), before, []string{"Q4_K_M"}))

	assertPresent(t, theirs)
}

// `lms get` prints the quantization it chose, never the file, and a repository
// can hold more than one file of a quantization: variants of a model, or every
// file of a repository named for the quantization. Which of two such partials
// is this download's is then a guess, so cancelling removes neither.
//
// That holds when one of them predates the pull too. `lms get` may be resuming
// that one, and the partial that appeared since is then another client's. A
// single file and a split one are different files, and so are two splits of
// one name into different numbers of shards.
func TestCleanupLMSAfterCancel_KeepsEveryFileWhenTheQuantizationNamesMoreThanOne(t *testing.T) {
	const model = "BobbyLLM/Qwen3.5-4B-EU-Tool-Q4_K_M-GGUF@Q4_K_M"
	test := func(name, first, second string, firstPredatesThePull bool) {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "BobbyLLM", "Qwen3.5-4B-EU-Tool-Q4_K_M-GGUF")
			require.NoError(t, os.MkdirAll(target, 0700))
			firstPath := filepath.Join(target, "downloading_"+first+".part")
			secondPath := filepath.Join(target, "downloading_"+second+".part")
			if firstPredatesThePull {
				writePartial(t, firstPath, "a download this pull may be resuming")
			}
			before, err := lmsPartialFiles(root, model)
			require.NoError(t, err)
			if !firstPredatesThePull {
				writePartial(t, firstPath, "one download")
			}
			writePartial(t, secondPath, "another")

			require.NoError(t, cleanupLMSAfterCancel(settledContext(t), root, model, fromHuggingFace(model), before, nil))

			assertPresent(t, firstPath)
			assertPresent(t, secondPath)
		})
	}
	test("two variants", "Qwen3.5-4B-EU-Tool-Q4_K_M.gguf", "Qwen3.5-4B-EU-Tool-v2-Q4_K_M.gguf", false)
	test("a variant that predates the pull", "Qwen3.5-4B-EU-Tool-Q4_K_M.gguf", "Qwen3.5-4B-EU-Tool-v2-Q4_K_M.gguf", true)
	test("a single file and a split one", "Qwen3.5-4B-EU-Tool-Q4_K_M.gguf", "Qwen3.5-4B-EU-Tool-Q4_K_M-00001-of-00002.gguf", false)
	test("splits into different numbers of shards", "Qwen3.5-4B-EU-Tool-Q4_K_M-00001-of-00002.gguf", "Qwen3.5-4B-EU-Tool-Q4_K_M-00001-of-00003.gguf", false)
}

// A GGUF too large for one file is split into shards named
// "<name>-00001-of-00003.gguf", which one `lms get` downloads. They count as
// one file, so cancelling removes every shard the download started.
func TestCleanupLMSAfterCancel_RemovesEveryShardOfTheFileItWasFetching(t *testing.T) {
	root := t.TempDir()
	const model = "lmstudio-community/Qwen3-235B-A22B-GGUF@Q4_K_M"
	target := filepath.Join(root, "lmstudio-community", "Qwen3-235B-A22B-GGUF")
	require.NoError(t, os.MkdirAll(target, 0700))
	before, err := lmsPartialFiles(root, model)
	require.NoError(t, err)
	first := filepath.Join(target, "downloading_Qwen3-235B-A22B-Q4_K_M-00001-of-00003.gguf.part")
	second := filepath.Join(target, "downloading_Qwen3-235B-A22B-Q4_K_M-00002-of-00003.gguf.part")
	writePartial(t, first, "data")
	writePartial(t, second, "data")

	require.NoError(t, cleanupLMSAfterCancel(settledContext(t), root, model, fromHuggingFace(model), before, nil))

	assertRemoved(t, first)
	assertRemoved(t, second)
}

// `lms get` resumes in place, so the same rule means a cancelled resume leaves
// its own partial behind. That is deliberate: nothing on disk tells a resume
// target apart from another client's file, the bytes stay useful to the next
// attempt, and the alternative is deleting a live download whenever the guess
// goes the other way. Files this pull actually created are still removed.
func TestCleanupLMSAfterCancel_LeavesAResumedDownloadsPartial(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
	const model = "lmstudio-community/Qwen3-8B-GGUF"
	resuming := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M-00001-of-00002.gguf.part")
	writePartial(t, resuming, "bytes the attempt being resumed left behind")
	before, err := lmsPartialFiles(root, model)
	require.NoError(t, err)
	growFile(t, resuming)
	fresh := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M-00002-of-00002.gguf.part")
	writePartial(t, fresh, "a shard this pull started from nothing")

	require.NoError(t, cleanupLMSAfterCancel(settledContext(t), root, model, fromHuggingFace(model), before, []string{"Q4_K_M"}))

	assertPresent(t, resuming)
	assertRemoved(t, fresh)
}

// A nil snapshot means the inspection failed, not that the directory was empty.
// Attribution is impossible without one, so cleanup deletes nothing — the rule
// cleanupOllamaAfterCancel applies too.
func TestCleanupLMSAfterCancel_DeletesNothingWithoutASnapshot(t *testing.T) {
	root := t.TempDir()
	partial := filepath.Join(root, "owner", "repo", "downloading_model-Q4_K_M.gguf.part")
	writePartial(t, partial, "data")

	require.NoError(t, cleanupLMSAfterCancel(settledContext(t), root, "owner/repo@Q4_K_M", fromHuggingFace("owner/repo@Q4_K_M"), nil, nil))

	assertPresent(t, partial)
}

// The models directory is declared by the engine's manifest. Without one there
// is no cache to attribute files in, and resolving the repository anyway would
// look for partials relative to the engine manager's working directory.
func TestLMSPartialFiles_RefusesAnUndeclaredModelsDirectory(t *testing.T) {
	files, err := lmsPartialFiles("", "owner/repo@Q4_K_M")

	assert.ErrorContains(t, err, "no models directory")
	assert.Nil(t, files, "a listing that could not be made must stay distinct from an empty one")
}
