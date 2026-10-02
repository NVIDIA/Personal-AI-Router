// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type downloadOutputKey struct{}

// LMS redraws with carriage returns and ANSI controls, not newlines.
// A bounded tail handles split writes without keeping the entire download log.
//
// mu guards every mutable field. The exec copy goroutine writes them while the
// goroutine that asked for the cancellation reads them to decide whether any
// partial file may be deleted, and a process this worker gives up on keeps its
// writer alive past the call that started it — so the reader and the writer
// genuinely overlap rather than merely appearing to.
type lmsDownloadOutput struct {
	mu          sync.Mutex
	text        string
	lastPercent int
	cancelled   bool
	completed   bool
	answered    bool
	// disowned records that this worker stopped waiting on the process behind
	// this writer. Its later output cannot be this cancellation's
	// confirmation, so state reports no cancellation once it is set, and a
	// "Download canceled." arriving afterwards cannot re-arm file deletion.
	disowned bool
	// named holds the model files the CLI's output has mentioned. Without a
	// pinned quantization it is the only evidence of which file `lms get`
	// chose, and so of which partial is this download's. It is collected as
	// the output arrives because text only keeps a bounded tail.
	named    map[string]bool
	stdin    io.Writer
	progress func(int)
}

var lmsPercentPattern = regexp.MustCompile(`\]\s+(\d+(?:\.\d+)?)%`)
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")
var lmsModelFilePattern = regexp.MustCompile(`[^\s/\\"'` + "`" + `]+\.gguf\b`)

// lmsNamedFilesMax bounds named against a CLI that prints a whole catalog.
const lmsNamedFilesMax = 256

func (w *lmsDownloadOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	w.text += string(data)
	clean := ansiPattern.ReplaceAllString(w.text, "")
	answer := strings.Contains(clean, "Continue to download in the background?") && !w.answered
	w.cancelled = w.cancelled || strings.Contains(clean, "Download canceled.")
	w.completed = w.completed || strings.Contains(clean, "Download completed.")
	for _, name := range lmsModelFilePattern.FindAllString(clean, -1) {
		if w.named == nil {
			w.named = make(map[string]bool)
		}
		if len(w.named) < lmsNamedFilesMax {
			w.named[name] = true
		}
	}
	percent := -1
	matches := lmsPercentPattern.FindAllStringSubmatch(clean, -1)
	if len(matches) > 0 {
		value, err := strconv.ParseFloat(matches[len(matches)-1][1], 64)
		if err == nil {
			latest := max(0, min(100, int(value)))
			if latest != w.lastPercent {
				w.lastPercent = latest
				percent = latest
			}
		}
	}
	if len(w.text) > 8192 {
		w.text = w.text[len(w.text)-8192:]
	}
	stdin := w.stdin
	w.mu.Unlock()

	// Declining the prompt and publishing progress both reach outside this
	// writer, and the stdin write can block on the child. Neither runs under
	// the lock the cancelling goroutine needs to read the state above.
	if answer {
		if _, err := io.WriteString(stdin, "n\n"); err != nil {
			return 0, err
		}
		w.mu.Lock()
		w.answered = true
		w.mu.Unlock()
	}
	if percent >= 0 {
		w.progress(percent)
	}
	return len(data), nil
}

// state reports the output as one consistent snapshot, so a caller cannot pair
// a stale completed with a fresh cancelled.
func (w *lmsDownloadOutput) state() (cancelled, completed bool, text string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.cancelled && !w.disowned, w.completed, w.text
}

// namedFiles returns a copy of the model files the output has mentioned.
func (w *lmsDownloadOutput) namedFiles() map[string]bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	named := make(map[string]bool, len(w.named))
	for name := range w.named {
		named[name] = true
	}
	return named
}

// reset clears the previous run's output. It is called before the process
// starts, where nothing is writing yet, but takes the lock anyway: an earlier
// disowned process may still hold this writer.
func (w *lmsDownloadOutput) reset(stdin io.Writer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stdin = stdin
	w.text = ""
	w.cancelled = false
	w.completed = false
	w.answered = false
	w.named = nil
}

// disown withdraws this writer's cancellation confirmation for good, for a
// process the worker could not reclaim. Clearing the flag alone would not
// hold: the process is still running, so its next redraw could set it again
// and delete files on the strength of a stop nothing confirmed.
func (w *lmsDownloadOutput) disown() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.disowned = true
}

func (e *Executor) pullLMSModel(ctx context.Context, st *engineState, engine, model string) (json.RawMessage, error) {
	// A snapshot that could not be taken stays nil, so cancelling this
	// download deletes nothing; the download itself does not depend on it.
	before, err := lmsPartialFiles(lmstudioModelsDir(), model)
	if err != nil {
		slog.Warn("could not snapshot partial downloads; cancelling this download will not remove its files",
			"engine", engine, "model", model, "err", err)
		before = nil
	}
	st.mu.Lock()
	port := st.port
	st.mu.Unlock()
	params, err := json.Marshal(map[string]string{"model": model})
	if err != nil {
		return nil, err
	}
	output := &lmsDownloadOutput{lastPercent: -1, progress: func(percent int) {
		if ctx.Err() == nil {
			e.emitPullProgress(ProgressEvent{Engine: engine, Model: model, Op: "pull", Stage: "downloading", Percent: percent, Message: model})
		}
	}}
	e.emitPullProgress(ProgressEvent{Engine: engine, Model: model, Op: "pull", Stage: "pulling", Message: model})
	result, err := e.runCmdAction(context.WithValue(ctx, downloadOutputKey{}, output), st, st.manifest.Actions[pullModelAction], port, params)
	// Only a cancellation someone asked for may delete partial files. The same
	// context also dies when PAIR quits mid-download, when a remote initiator
	// disconnects, and when the action timeout elapses — and `lms get` is built
	// to resume every one of those on the next attempt.
	cancelled, _, _ := output.state()
	if err != nil && cancelRequested(ctx) && cancelled {
		// Cleanup outlives the cancellation that triggered it, so it runs on a
		// context that keeps this one's values without its deadline.
		if cleanupErr := cleanupLMSAfterCancel(context.WithoutCancel(ctx), lmstudioModelsDir(), model, before, output.namedFiles()); cleanupErr != nil {
			// The download did stop, which is what was asked for. Leftover
			// bytes are the vendor's to resume, so reporting a failed cancel
			// here would deny the one outcome that did happen and leave the
			// row on "Canceling" over a transfer that is gone.
			slog.Warn("partial-file cleanup failed after cancellation",
				"engine", engine, "model", model, "err", cleanupErr)
		}
		return nil, context.Canceled
	}
	return result, err
}

// downloadWaitDelay bounds how long Wait blocks on the stdio pipes after the
// CLI itself has exited. `lms` commonly ships as a shim that execs Node, so the
// process actually writing bytes is a grandchild; one that outlives the signal
// keeps the inherited pipe open and would otherwise park Wait forever, wedging
// every queued pull for that engine. It is set here rather than per platform
// because that parked Wait is not platform-specific.
const downloadWaitDelay = 5 * time.Second

func runLMSDownloadCommand(ctx context.Context, argv []string, output *lmsDownloadOutput) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.WaitDelay = downloadWaitDelay
	if err := configureDownloadProcess(cmd); err != nil {
		return "", err
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	defer stdin.Close()
	output.reset(stdin)
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return "", err
	}
	unpin, err := pinDownloadProcess(cmd)
	if err != nil {
		_ = cmd.Process.Kill()
		go func() { _ = cmd.Wait() }()
		return "", fmt.Errorf("hold download process: %w", err)
	}
	defer unpin()
	finished := make(chan error, 1)
	// reaped is set before the result is published, so a reader that sees it
	// knows the process is gone. cmd.ProcessState cannot serve here: Wait
	// writes it on this goroutine while the cancelling one would be reading
	// it. Wait can return up to downloadWaitDelay after the reap itself, and
	// pinDownloadProcess is what keeps a signal in that gap off a reissued PID.
	var reaped atomic.Bool
	go func() {
		err := cmd.Wait()
		reaped.Store(true)
		finished <- err
	}()
	var runErr error
	select {
	case runErr = <-finished:
	case <-ctx.Done():
		// Both cases can be ready at once — the user cancels exactly as the CLI
		// exits — and select picks between them at random. Prefer the finished
		// run: cmd.Wait has already reaped the process by then, so the OS is
		// free to reissue its PID and an interrupt would reach a stranger.
		select {
		case runErr = <-finished:
		default:
			return stopLMSDownload(lmsCancelGracesFrom(ctx), cmd, finished, &reaped, output)
		}
	}
	_, _, text := output.state()
	if runErr != nil {
		return "", fmt.Errorf("%w: %s", runErr, lmsErrorDetail(text))
	}
	return text, nil
}

// lmsCancelGraces bounds each stage of stopping the CLI. No wait here may be
// unbounded: cmd.Wait does not return until the stdio pipes close, and `lms`
// commonly ships as a shim that execs Node, so a grandchild holding an
// inherited pipe can outlive every signal sent to the group. downloadWaitDelay
// covers that for a process that exited, but a process the kill cannot remove
// at all would park the reap forever — and with it the cancel's JSON-RPC
// response, leaving the row on "Canceling" for good and wedging every queued
// pull for the engine. Giving up on the reap leaks one goroutine holding a
// pipe, which is the cheaper failure.
type lmsCancelGraces struct {
	// interrupt is how long `lms get` has to acknowledge the first interrupt.
	// It answers with the "Continue to download in the background?" prompt,
	// which the output writer declines, then prints "Download canceled."
	interrupt time.Duration
	// retry bounds the second interrupt. The first can land while the CLI is
	// mid-redraw and never reach the handler that prints the prompt.
	retry time.Duration
	// reap bounds the wait for a killed process. Reaching it means the kill did
	// not remove the process, so the cancel is reported as failed.
	reap time.Duration
}

// lmsCancelGracesKey addresses the shortened graces a test installs on a context.
type lmsCancelGracesKey struct{}

func lmsCancelGracesFrom(ctx context.Context) lmsCancelGraces {
	if graces, ok := ctx.Value(lmsCancelGracesKey{}).(lmsCancelGraces); ok {
		return graces
	}
	return lmsCancelGraces{interrupt: 15 * time.Second, retry: 5 * time.Second, reap: 10 * time.Second}
}

// stopLMSDownload interrupts the CLI and reports whether it confirmed the
// cancellation. Confirmation matters beyond the exit code: declining the
// background-download prompt is what aborts the daemon's task, so a client
// killed before it answers can leave the transfer running. The interrupt is
// repeated before the process is reclaimed, and a stop the CLI never
// acknowledged is reported as a failed cancel that deletes nothing.
func stopLMSDownload(
	graces lmsCancelGraces,
	cmd *exec.Cmd,
	finished <-chan error,
	reaped *atomic.Bool,
	output *lmsDownloadOutput,
) (string, error) {
	// Neither signal addresses the CLI alone: on Unix it goes to the whole
	// download process group, and on Windows it becomes a control event for
	// everything sharing the launcher's console. A reaped PID is free for the
	// OS to reissue, so one sent afterwards does not merely miss — it can
	// reach a stranger. runLMSDownloadCommand's select prefers a finished run
	// for this reason, but Wait can also complete while this is running.
	interrupt := func() error {
		if reaped.Load() {
			return os.ErrProcessDone
		}
		return interruptDownload(cmd)
	}
	kill := func() {
		if reaped.Load() {
			return
		}
		_ = killDownload(cmd)
	}
	if err := interrupt(); err != nil {
		kill()
		wasReaped, runErr := waitLMSExit(finished, graces.reap)
		if !wasReaped {
			output.disown()
			return "", fmt.Errorf("could not confirm LM Studio cancellation and could not reclaim its process: %w", err)
		}
		if _, completed, text := output.state(); runErr == nil && completed {
			return text, nil
		}
		return "", fmt.Errorf("could not confirm LM Studio cancellation: %w", err)
	}
	exited, _ := waitLMSExit(finished, graces.interrupt)
	if !exited && interrupt() == nil {
		exited, _ = waitLMSExit(finished, graces.retry)
	}
	if exited {
		cancelled, completed, text := output.state()
		if completed {
			return text, nil
		}
		if !cancelled {
			return "", fmt.Errorf("LM Studio exited without confirming download cancellation")
		}
		return "", context.Canceled
	}
	// Reclaim the process — it holds this worker's stdio pipes — but report the
	// cancel as failed so no partial file is deleted on the strength of a
	// cancellation the CLI never acknowledged. A process that outlasts even the
	// kill is abandoned rather than waited on; see lmsCancelGraces.
	kill()
	wasReaped, _ := waitLMSExit(finished, graces.reap)
	output.disown()
	if !wasReaped {
		// The writer is still attached to a live process, so its output from
		// here belongs to nobody this worker is waiting on.
		return "", fmt.Errorf("LM Studio did not confirm download cancellation and its process could not be reclaimed; the download may still be running in LM Studio")
	}
	return "", fmt.Errorf("LM Studio did not confirm download cancellation; the download may still be running in LM Studio")
}

// waitLMSExit reports whether the CLI was reaped within grace, along with what
// cmd.Wait returned for it.
func waitLMSExit(finished <-chan error, grace time.Duration) (exited bool, runErr error) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case runErr = <-finished:
		return true, runErr
	case <-timer.C:
		return false, nil
	}
}

// lmsErrorDetailMax bounds the CLI output quoted in a returned error. The full
// transcript is kilobytes of redraw frames and local filesystem paths, and it
// travels into the persisted errors pipeline and back to remote initiators.
const lmsErrorDetailMax = 200

func lmsErrorDetail(text string) string {
	clean := strings.TrimSpace(ansiPattern.ReplaceAllString(text, ""))
	runes := []rune(clean)
	if len(runes) <= lmsErrorDetailMax {
		return clean
	}
	return "…" + string(runes[len(runes)-lmsErrorDetailMax:])
}

// cleanupLMSAfterCancel retries until every partial this pull created is
// removed or confirmed to be another client's.
func cleanupLMSAfterCancel(ctx context.Context, root, model string, before map[string]os.FileInfo, named map[string]bool) error {
	return retryPartialCleanup(func(steady partialSteadyPasses) (bool, error) {
		return cleanupLMSPartials(ctx, root, model, before, named, steady)
	})
}

// cleanupLMSPartials runs one pass over the partial files this `lms get`
// created, preserving completed shards. The LM Studio app and any other client
// download into the same repository directory, so a file is deleted only when
// it is this download's file, was absent from the snapshot taken before this
// download started, and has stopped moving. Never delete a model directory or
// follow a symlink outside the cache.
//
// Which file is this download's comes from the pinned quantization when the
// request names one, and otherwise from the files the CLI's output named. An
// unpinned request whose output named none has no candidates, because any
// partial appearing in the repository could be the app fetching another
// quantization beside it.
//
// Presence in the snapshot is disqualifying on its own; what the file has done
// since is not consulted. Bytes gained during this pull look identical whether
// another client is writing them or `lms get` is resuming the file in place,
// and a writer that has merely paused cannot be told from one that finished. A
// resumed download therefore keeps its partial through a cancellation, which
// costs disk the vendor reuses on the next attempt; the alternative costs
// another client its transfer.
func cleanupLMSPartials(
	ctx context.Context,
	root, model string,
	before map[string]os.FileInfo,
	named map[string]bool,
	steady partialSteadyPasses,
) (busy bool, err error) {
	// No snapshot means the inspection failed, not that the directory was
	// empty, and nothing is attributable without one.
	if before == nil {
		return false, nil
	}
	files, err := lmsPartialFiles(root, model)
	if err != nil {
		return false, err
	}
	pinned := lmsModelQuant(model) != ""
	created := make([]string, 0, len(files))
	for path := range files {
		if _, existed := before[path]; existed {
			continue
		}
		if !pinned && !named[lmsPartialTarget(path)] {
			continue
		}
		created = append(created, path)
	}
	return removeSettledPartials(ctx, created, steady)
}

// lmsPartialTarget is the model file a `downloading_<file>.part` partial is
// becoming.
func lmsPartialTarget(path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "downloading_"), ".part")
}

// lmsModelQuant is the quantization a model reference pins with "@quant", or
// empty when it pins none.
func lmsModelQuant(model string) string {
	_, repo, ok := lmsOwnerName(model)
	if !ok {
		return ""
	}
	_, quant, _ := strings.Cut(repo, "@")
	return quant
}

// lmsPartialFiles lists the in-progress downloads in a model's repository
// directory that could belong to the requested quantization.
//
// A nil map means the listing could not be made — the reference names no
// repository — and is distinct from an empty one, which means the repository
// holds no partials. cleanupLMSPartials deletes nothing on the former, so the
// two must not be conflated.
func lmsPartialFiles(root, model string) (map[string]os.FileInfo, error) {
	owner, repo, ok := lmsOwnerName(model)
	if !ok {
		return nil, nil
	}
	repo, quant, _ := strings.Cut(repo, "@")
	for _, part := range []string{owner, repo} {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `\:`) {
			return nil, fmt.Errorf("invalid model repository")
		}
	}
	target := filepath.Join(root, owner, repo)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if os.IsNotExist(err) {
		return map[string]os.FileInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if os.IsNotExist(err) {
		return map[string]os.FileInfo{}, nil
	}
	if err != nil {
		return nil, err
	}
	if !pathWithinRoot(resolvedRoot, resolvedTarget) {
		return nil, fmt.Errorf("partial download path escapes model directory")
	}
	entries, err := os.ReadDir(resolvedTarget)
	if err != nil {
		return nil, err
	}
	files := make(map[string]os.FileInfo)
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.HasPrefix(entry.Name(), "downloading_") || !strings.HasSuffix(entry.Name(), ".part") {
			continue
		}
		// LM Studio names a GGUF after its quantization, so when the request
		// pinned one, only files carrying it can be this download's — pulling
		// @Q4_K_M must not touch a @Q8_0 the LM Studio app is fetching beside
		// it. Without a pinned quantization the CLI picks its own, so every
		// partial is listed here and cleanupLMSPartials narrows them to the
		// files the CLI named.
		if quant != "" && !strings.Contains(strings.ToUpper(entry.Name()), strings.ToUpper(quant)) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		files[filepath.Join(resolvedTarget, entry.Name())] = info
	}
	return files, nil
}
