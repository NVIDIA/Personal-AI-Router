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
	"testing"
	"time"
)

// Digests and blob names are written out in full rather than built with
// strings.Repeat, so a failure message can be grepped straight back to the case
// that produced it.
const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	blobA   = "sha256-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	blobB   = "sha256-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	digestC = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	blobC   = "sha256-cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
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

// withPartialSettle replaces the real wait between the stat passes in
// quiescentPaths. A case where nothing is writing installs a no-op so the
// window costs nothing; one that models another client still writing appends
// from the hook, which lands inside the window on every run where a background
// goroutine racing a real sleep only usually does.
func withPartialSettle(ctx context.Context, settle func()) context.Context {
	return context.WithValue(ctx, partialSettleKey{}, settle)
}

// settledContext is the context for cleanup that has nothing to wait for.
func settledContext() context.Context {
	return withPartialSettle(context.Background(), func() {})
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

func writeBlob(t *testing.T, root, name string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, []byte("data"), 0600); err != nil {
		t.Fatalf("seed blob %s: %v", name, err)
	}
	return path
}

func writePartial(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("create %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatalf("seed partial %s: %v", path, err)
	}
}

// growFile appends to path, standing in for another client still writing it.
func growFile(t *testing.T, path string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatalf("open %s for append: %v", path, err)
	}
	if _, err := file.WriteString("more"); err != nil {
		t.Errorf("append to %s: %v", path, err)
	}
	if err := file.Close(); err != nil {
		t.Errorf("close %s: %v", path, err)
	}
}

func assertRemoved(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s was not removed: %v", filepath.Base(path), err)
	}
}

func assertPresent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Errorf("%s was removed: %v", filepath.Base(path), err)
	}
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
	if !output.cancelled {
		t.Fatal("cancellation acknowledgement was not recorded")
	}
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
			if output.cancelled != wantCancelled {
				t.Errorf("acknowledged = %v, want %v (this is what gates deleting partial files)", output.cancelled, wantCancelled)
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

			output := &lmsDownloadOutput{
				lastPercent: -1,
				progress:    func(int) {},
				completed:   completed,
				text:        "transcript",
			}
			text, err := stopLMSDownload(testLMSCancelGraces, cmd, finished, output)
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

func TestLMSPartialCleanupPreservesCompletedShards(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "owner", "repo")
	complete := filepath.Join(target, "model-00001-of-00002.gguf")
	writePartial(t, complete, "data")

	const model = "https://huggingface.co/owner/repo@Q4_K_M"
	before, err := lmsPartialFiles(root, model)
	if err != nil {
		t.Fatalf("snapshot partials: %v", err)
	}
	partial := filepath.Join(target, "downloading_model-Q4_K_M.gguf.part")
	writePartial(t, partial, "data")

	if err := cleanupLMSPartials(settledContext(), root, model, before); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	assertRemoved(t, partial)
	assertPresent(t, complete)
}

func TestLMSPartialCleanupPreservesUntouchedDownloads(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "owner", "repo")
	oldPartial := filepath.Join(target, "downloading_other.gguf.part")
	writePartial(t, oldPartial, "other download")

	before, err := lmsPartialFiles(root, "owner/repo")
	if err != nil {
		t.Fatalf("snapshot partials: %v", err)
	}
	newPartial := filepath.Join(target, "downloading_selected.gguf.part")
	writePartial(t, newPartial, "new download")

	if err := cleanupLMSPartials(settledContext(), root, "owner/repo", before); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	assertPresent(t, oldPartial)
	assertRemoved(t, newPartial)
}

// The LM Studio app downloads into the same repository directory PAIR does, so
// cancelling a @Q4_K_M pull must not remove the @Q8_0 the app is fetching —
// neither the copy that grew since the snapshot nor one created after it.
func TestLMSPartialCleanupPreservesOtherQuantizations(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
	growing := filepath.Join(target, "downloading_Qwen3-8B-Q8_0.gguf.part")
	writePartial(t, growing, "app download")

	const model = "lmstudio-community/Qwen3-8B-GGUF@Q4_K_M"
	before, err := lmsPartialFiles(root, model)
	if err != nil {
		t.Fatalf("snapshot partials: %v", err)
	}
	if len(before) != 0 {
		t.Fatalf("another quantization was captured as this pull's: %v", before)
	}
	appeared := filepath.Join(target, "downloading_Qwen3-8B-Q6_K.gguf.part")
	mine := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
	writePartial(t, growing, "app download, now longer")
	writePartial(t, appeared, "app started this one after our snapshot")
	writePartial(t, mine, "our download")

	if err := cleanupLMSPartials(settledContext(), root, model, before); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	assertPresent(t, growing)
	assertPresent(t, appeared)
	assertRemoved(t, mine)
}

// The app can also be fetching the very quantization PAIR asked for, into the
// same file. Carrying the requested quantization in its name is what makes a
// partial a candidate, not what makes it this download's, so one still growing
// after this transfer stopped has to survive too.
func TestLMSPartialCleanupPreservesTheRequestedQuantizationWhileItGrows(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
	const model = "lmstudio-community/Qwen3-8B-GGUF@Q4_K_M"
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatalf("create %s: %v", target, err)
	}
	before, err := lmsPartialFiles(root, model)
	if err != nil {
		t.Fatalf("snapshot partials: %v", err)
	}
	shared := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
	writePartial(t, shared, "shared download")
	ctx := withPartialSettle(context.Background(), func() { growFile(t, shared) })

	if err := cleanupLMSPartials(ctx, root, model, before); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	assertPresent(t, shared)
}

// Growing since the snapshot is not evidence of ownership. A writer that paused
// looks exactly like one that finished, so a partial already present when this
// pull started stays another client's however many bytes it gained meanwhile.
//
// The model hub pulls unpinned ids, so quant is empty and the quantization
// filter excludes nothing — every partial in the repository reaches this rule,
// which is why it has to be the strict one.
func TestLMSPartialCleanupPreservesAPreexistingPartialThatGrew(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
	const model = "lmstudio-community/Qwen3-8B-GGUF"
	theirs := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
	writePartial(t, theirs, "app download")

	before, err := lmsPartialFiles(root, model)
	if err != nil {
		t.Fatalf("snapshot partials: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("snapshot missed the pre-existing partial: %v", before)
	}
	// The app writes more while our download runs, then stalls, so the file is
	// perfectly still by the time cleanup observes it.
	growFile(t, theirs)

	if err := cleanupLMSPartials(settledContext(), root, model, before); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	assertPresent(t, theirs)
}

// `lms get` resumes in place, so the same rule means a cancelled resume leaves
// its own partial behind. That is deliberate: nothing on disk tells a resume
// target apart from another client's file, the bytes stay useful to the next
// attempt, and the alternative is deleting a live download whenever the guess
// goes the other way. Files this pull actually created are still removed.
func TestLMSPartialCleanupLeavesAResumedDownloadsPartial(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "lmstudio-community", "Qwen3-8B-GGUF")
	const model = "lmstudio-community/Qwen3-8B-GGUF"
	resuming := filepath.Join(target, "downloading_Qwen3-8B-Q4_K_M.gguf.part")
	writePartial(t, resuming, "bytes the attempt being resumed left behind")

	before, err := lmsPartialFiles(root, model)
	if err != nil {
		t.Fatalf("snapshot partials: %v", err)
	}
	growFile(t, resuming)
	fresh := filepath.Join(target, "downloading_Qwen3-8B-Q6_K.gguf.part")
	writePartial(t, fresh, "a shard this pull started from nothing")

	if err := cleanupLMSPartials(settledContext(), root, model, before); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	assertPresent(t, resuming)
	assertRemoved(t, fresh)
}

// A nil snapshot means the inspection failed, not that the directory was empty.
// Attribution is impossible without one, so cleanup deletes nothing — the rule
// cleanupOllamaPartials already applies.
func TestLMSPartialCleanupDeletesNothingWithoutASnapshot(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "owner", "repo")
	partial := filepath.Join(target, "downloading_model-Q4_K_M.gguf.part")
	writePartial(t, partial, "data")

	if err := cleanupLMSPartials(settledContext(), root, "owner/repo@Q4_K_M", nil); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	assertPresent(t, partial)
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

// A cancel dispatched before its pull registers must stop the download rather
// than report success while it runs on under a UI stuck on "Canceling". The
// pull and the cancel each run on their own goroutine, so the read loop's
// ordering does not settle which reaches the registry first.
func TestCancelBeforePullRegistersStopsIt(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	// The read loop claims a pull before dispatching it, so this is the window
	// in which the cancel's goroutine can reach the executor first.
	release := ex.claimPull("ollama", "demo", true)
	defer release()
	if err := ex.CancelModelPull(context.Background(), "ollama", "demo"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	result, err := ex.trackedPull(context.Background(), "ollama", "demo", func(context.Context) (json.RawMessage, error) {
		t.Error("cancelled pull started its download")
		return nil, nil
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	assertPullStatus(t, result, "cancelled")

	// The tombstone is consumed, so the same click cannot cancel a retry.
	ran := false
	if _, err := ex.trackedPull(context.Background(), "ollama", "demo", func(context.Context) (json.RawMessage, error) {
		ran = true
		return succeedingPull(context.Background())
	}); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !ran {
		t.Fatal("retry after a consumed cancellation did not start")
	}
}

// A cancel can also land just after the pull it targets finished, and that one
// has nothing to stop. Holding it for the next pull of the same model made a
// retry report "cancelled" without downloading anything.
func TestCancelAfterPullCompletesLeavesTheRetryAlone(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	release := ex.claimPull("ollama", "demo", true)
	if _, err := ex.trackedPull(context.Background(), "ollama", "demo", succeedingPull); err != nil {
		t.Fatalf("first pull: %v", err)
	}
	release()

	if err := ex.CancelModelPull(context.Background(), "ollama", "demo"); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	ran := false
	result, err := ex.trackedPull(context.Background(), "ollama", "demo", func(context.Context) (json.RawMessage, error) {
		ran = true
		return succeedingPull(context.Background())
	})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if !ran {
		t.Fatal("retry inherited the cancellation of a download that had already finished")
	}
	assertPullStatus(t, result, "success")
}

// A pull can also end without ever consuming the cancel held for it — it
// failed before registering, or never registered at all. That tombstone has to
// go with the attempt rather than sit out its window.
func TestPendingCancelDoesNotOutliveTheAttemptItWasHeldFor(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	release := ex.claimPull("ollama", "demo", true)
	if err := ex.CancelModelPull(context.Background(), "ollama", "demo"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	release()

	ran := false
	result, err := ex.trackedPull(context.Background(), "ollama", "demo", func(context.Context) (json.RawMessage, error) {
		ran = true
		return succeedingPull(context.Background())
	})
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if !ran {
		t.Fatal("a cancellation held for an abandoned attempt was inherited by the next pull")
	}
	assertPullStatus(t, result, "success")
}

// An unclaimed engine and model has no download to stop, so the cancel is a
// no-op rather than a tombstone.
func TestCancelWithNoDownloadRequestedIsANoOp(t *testing.T) {
	ex := NewExecutor(NewRegistry(), NewReporter(nil), nil, t.TempDir())
	if err := ex.CancelModelPull(context.Background(), "ollama", "demo"); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	ex.pullMu.Lock()
	pending := len(ex.pendingCancels)
	ex.pullMu.Unlock()
	if pending != 0 {
		t.Fatalf("armed %d cancellation(s) for a download nobody requested", pending)
	}
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
			active := &activePull{cancel: func() {}, done: make(chan struct{})}
			ex.pulls = map[string]*activePull{pullKey("ollama", "demo"): active}

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
			if got.err != nil {
				t.Fatalf("joined request: %v", got.err)
			}
			assertPullStatus(t, got.result, wantStatus)
		})
	}
	test("completed download", json.RawMessage(`{"status":"success"}`), "success")
	test("cancelled download", cancelledPull(), "cancelled")
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

func TestOllamaCancellationHandlesCompletion(t *testing.T) {
	test := func(name string, statuses []string, wantStatus string) {
		t.Run(name, func(t *testing.T) {
			blobs := newOllamaBlobsDir(t)
			partial := filepath.Join(blobs, blobA+"-partial")
			started := make(chan struct{})
			disconnected := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writePartial(t, partial, "partial")
				w.Header().Set("Content-Type", "application/x-ndjson")
				for _, status := range statuses {
					if _, err := fmt.Fprintf(w, `{"status":%q,"digest":%q,"total":100,"completed":25}`+"\n", status, digestA); err != nil {
						t.Error(err)
						return
					}
				}
				w.(http.Flusher).Flush()
				<-r.Context().Done()
				close(disconnected)
			}))
			defer server.Close()
			progressFrames := 0
			ex := newOllamaPullExecutor(t, server.URL, func(method string, _ any) {
				if method == "engine:pull-progress" {
					progressFrames++
					if progressFrames == len(statuses) {
						close(started)
					}
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
			case <-started:
			case <-ctx.Done():
				t.Fatal("pull did not start")
			}
			if err := ex.CancelModelPull(ctx, "ollama", "demo"); err != nil {
				t.Fatalf("cancel: %v", err)
			}
			got := <-done
			if got.err != nil {
				t.Fatalf("pull: %v", got.err)
			}
			assertPullStatus(t, got.result, wantStatus)
			select {
			case <-disconnected:
			case <-ctx.Done():
				t.Fatal("HTTP transfer did not disconnect")
			}
			if wantStatus == "success" {
				data, err := os.ReadFile(partial)
				if err != nil || string(data) != "partial" {
					t.Fatalf("late cancellation changed partial file after completion: data=%q, err=%v", data, err)
				}
				return
			}
			assertRemoved(t, partial)
		})
	}
	test("active transfer is canceled and cleaned", []string{"pulling layer"}, "cancelled")
	test("success survives late cancellation", []string{"pulling layer", "success"}, "success")
	test("success survives later frame and cancellation", []string{"pulling layer", "success", ""}, "success")
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
			ex.pulls = make(map[string]*activePull)
			for i := 0; i < predecessorCount; i++ {
				ex.pulls[pullKey("ollama", fmt.Sprintf("predecessor-%d", i))] = &activePull{done: make(chan struct{})}
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
			_, stillQueued := ex.pulls[pullKey("ollama", "queued")]
			ex.pullMu.Unlock()
			if stillQueued || remaining != predecessorCount {
				t.Fatalf("registry after cancellation: queued=%v, remaining=%d", stillQueued, remaining)
			}
		})
	}
	test("one active predecessor", 1)
	test("multiple blocked predecessors", 3)
}
