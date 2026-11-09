// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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

func withLMSCancelGraces(ctx context.Context, graces lmsCancelGraces) context.Context {
	return context.WithValue(ctx, lmsCancelGracesKey{}, graces)
}

// fakeDownloadArgv builds the argv for one of the fake engine's download
// subcommands. Each takes the percentage to report before it waits for its
// interrupt.
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

// Without a pinned quantization, the file names `lms get` prints are the only
// evidence of which partial is this download's. A name can arrive split across
// writes and run straight into a redraw.
func TestLMSDownloadOutput_CollectsTheModelFilesTheCLINames(t *testing.T) {
	output := &lmsDownloadOutput{lastPercent: -1, progress: func(int) {}}
	for _, chunk := range []string{
		"Downloading lmstudio-community/Qwen3-8B-GGUF/Qwen3-8B-Q4",
		"_K_M.gguf\r\x1b[2K[====>    ] 12.5%",
	} {
		_, err := output.Write([]byte(chunk))
		require.NoError(t, err)
	}

	assert.Equal(t, map[string]bool{"Qwen3-8B-Q4_K_M.gguf": true}, output.namedFiles())
}

// A transfer that fails transiently is resumed by running `lms get` again, and
// the resumed run can fail before it prints the file name. The name an earlier
// run printed still identifies this download's partial, so starting the next
// run must not forget it.
func TestLMSDownloadOutput_KeepsNamedFilesAcrossResumedRuns(t *testing.T) {
	output := &lmsDownloadOutput{lastPercent: -1, progress: func(int) {}}
	_, err := output.Write([]byte("Downloading lmstudio-community/Qwen3-8B-GGUF/Qwen3-8B-Q4_K_M.gguf"))
	require.NoError(t, err)

	output.reset(io.Discard)

	assert.Equal(t, map[string]bool{"Qwen3-8B-Q4_K_M.gguf": true}, output.namedFiles())
}

// Cancelling the download interrupts the CLI the way Ctrl+C would rather than
// killing it, because only an interrupted `lms get` asks the question whose
// answer stops the daemon's transfer.
func TestRunLMSDownloadCommand_InterruptsTheCLIWhenCancelled(t *testing.T) {
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
		require.Equal(t, fakeDownloadPercent, percent, "the CLI's progress was misread")
	case <-ctx.Done():
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
		finished := make(chan error, 1)
		finished <- cmd.Wait()
		// The process is reaped, which is exactly the state that must not be
		// signalled: its PID is free for the OS to reissue, and both signals
		// reach more than the CLI.
		var reaped atomic.Bool
		reaped.Store(true)
		output := &lmsDownloadOutput{lastPercent: -1, progress: func(int) {}, completed: completed, text: "transcript"}
		return stopLMSDownload(testLMSCancelGraces, cmd, finished, &reaped, output)
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

// `lms get` resumes a transfer that timed out by running again after a pause,
// and no CLI runs during that pause. A cancel landing there found nothing to
// interrupt and reported the timeout instead: the cancel failed, a retry error
// was filed for a download the user had stopped, and its partial stayed behind.
func TestLMSPull_CancelWhileWaitingToResumeStopsTheDownload(t *testing.T) {
	// An hour-long pause guarantees the cancel lands in it, after the first
	// run has exited and before a second one starts.
	defer setResumeBudget(t, 3, time.Hour)()
	waiting := make(chan struct{})
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(&resumeWaitHandler{waiting: waiting}))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })
	modelsDir := t.TempDir()
	partial := filepath.Join(modelsDir, "owner", "repo", "downloading_repo-Q4_K_M.gguf.part")
	manifest := testEngineManifest(fakeEngineBin)
	platformKey := runtime.GOOS + "/" + runtime.GOARCH
	platform := manifest.Platforms[platformKey]
	platform.ModelsDir = modelsDir
	manifest.Platforms[platformKey] = platform
	manifest.Actions[pullModelAction] = Action{
		Cmd:             []string{fakeEngineBin, "downloadstalls", partial, "{model}"},
		ModelResolution: modelResolutionLMSGet,
	}
	ex := newTestExecutor(t, manifest)
	events, unsubscribe := ex.progress.subscribe("fake")
	defer unsubscribe()
	ctx, cancel := context.WithTimeout(settledContext(), 30*time.Second)
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
	// The request pins no quantization, so the file the CLI named is what
	// makes this partial attributable to the download.
	assertRemoved(t, partial)
	frames := publishedFrames(events)
	require.NotEmpty(t, frames, "the pull published no frames")
	assert.Equal(t, "cancelled", frames[len(frames)-1].Stage, "the last frame must report the cancellation")
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

	require.NoError(t, cleanupLMSAfterCancel(settledContext(), root, model, before, nil))

	assertRemoved(t, partial)
	assertPresent(t, completeShard)
}

func TestCleanupLMSAfterCancel_KeepsPartialsOlderThanThePull(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "owner", "repo")
	olderPartial := filepath.Join(target, "downloading_other.gguf.part")
	writePartial(t, olderPartial, "other download")
	before, err := lmsPartialFiles(root, "owner/repo")
	require.NoError(t, err)
	newPartial := filepath.Join(target, "downloading_selected.gguf.part")
	writePartial(t, newPartial, "new download")
	named := map[string]bool{"selected.gguf": true, "other.gguf": true}

	require.NoError(t, cleanupLMSAfterCancel(settledContext(), root, "owner/repo", before, named))

	assertPresent(t, olderPartial)
	assertRemoved(t, newPartial)
}

// An unpinned request lets the CLI choose the quantization, so a partial that
// appeared since the snapshot is only this download's when the CLI named its
// file. One it did not name can be the LM Studio app fetching another
// quantization of the same repository, even though it is new and holding still.
func TestCleanupLMSAfterCancel_UnpinnedRequestRemovesOnlyFilesTheCLINamed(t *testing.T) {
	const model = "lmstudio-community/Qwen3-8B-GGUF"
	test := func(name string, named map[string]bool, wantOursRemoved bool) {
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

			require.NoError(t, cleanupLMSAfterCancel(settledContext(), root, model, before, named))

			if wantOursRemoved {
				assertRemoved(t, ours)
			} else {
				assertPresent(t, ours)
			}
			assertPresent(t, theirs)
		})
	}
	test("removes the named file and keeps the other", map[string]bool{"Qwen3-8B-Q4_K_M.gguf": true}, true)
	test("deletes nothing when the CLI named no file", nil, false)
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
	require.Empty(t, before, "another quantization was captured as this pull's")
	appeared := filepath.Join(target, "downloading_Qwen3-8B-Q6_K.gguf.part")
	ours := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
	writePartial(t, growing, "app download, now longer")
	writePartial(t, appeared, "app started this one after our snapshot")
	writePartial(t, ours, "our download")

	require.NoError(t, cleanupLMSAfterCancel(settledContext(), root, model, before, nil))

	assertPresent(t, growing)
	assertPresent(t, appeared)
	assertRemoved(t, ours)
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
	ctx := withPartialSettle(context.Background(), func() { growFile(t, shared) })

	require.NoError(t, cleanupLMSAfterCancel(ctx, root, model, before, nil))

	assertPresent(t, shared)
}

// Growing since the snapshot is not evidence of ownership. A writer that paused
// looks exactly like one that finished, so a partial already present when this
// pull started stays another client's however many bytes it gained meanwhile.
//
// The model hub pulls unpinned ids, so no quantization filter excludes
// anything — every partial in the repository reaches this rule, which is why it
// has to be the strict one.
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

	require.NoError(t, cleanupLMSAfterCancel(settledContext(), root, model, before, map[string]bool{"Qwen3-8B-Q4_K_M.gguf": true}))

	assertPresent(t, theirs)
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
	resuming := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
	writePartial(t, resuming, "bytes the attempt being resumed left behind")
	before, err := lmsPartialFiles(root, model)
	require.NoError(t, err)
	growFile(t, resuming)
	fresh := filepath.Join(target, "downloading_Qwen3-8B-Q6_K.gguf.part")
	writePartial(t, fresh, "a shard this pull started from nothing")
	named := map[string]bool{"Qwen3-8B-Q4_K_M.gguf": true, "Qwen3-8B-Q6_K.gguf": true}

	require.NoError(t, cleanupLMSAfterCancel(settledContext(), root, model, before, named))

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

	require.NoError(t, cleanupLMSAfterCancel(settledContext(), root, "owner/repo@Q4_K_M", nil, nil))

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
