// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type downloadOutputKey struct{}

// LMS redraws with carriage returns and ANSI controls, not newlines.
// A bounded tail handles split writes without keeping the entire download log.
type lmsDownloadOutput struct {
	text        string
	lastPercent int
	cancelled   bool
	completed   bool
	answered    bool
	stdin       io.Writer
	progress    func(int)
}

var lmsPercentPattern = regexp.MustCompile(`\]\s+(\d+(?:\.\d+)?)%`)
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")

func (w *lmsDownloadOutput) Write(data []byte) (int, error) {
	w.text += string(data)
	clean := ansiPattern.ReplaceAllString(w.text, "")
	if strings.Contains(clean, "Continue to download in the background?") && !w.answered {
		if _, err := io.WriteString(w.stdin, "n\n"); err != nil {
			return 0, err
		}
		w.answered = true
	}
	w.cancelled = w.cancelled || strings.Contains(clean, "Download canceled.")
	w.completed = w.completed || strings.Contains(clean, "Download completed.")
	matches := lmsPercentPattern.FindAllStringSubmatch(clean, -1)
	if len(matches) > 0 {
		value, err := strconv.ParseFloat(matches[len(matches)-1][1], 64)
		if err == nil {
			percent := max(0, min(100, int(value)))
			if percent != w.lastPercent {
				w.lastPercent = percent
				w.progress(percent)
			}
		}
	}
	if len(w.text) > 8192 {
		w.text = w.text[len(w.text)-8192:]
	}
	return len(data), nil
}

func (e *Executor) pullLMSModel(ctx context.Context, st *engineState, engine, model string) (json.RawMessage, error) {
	before, err := lmsPartialFiles(lmstudioModelsDir(), model)
	if err != nil {
		return nil, fmt.Errorf("inspect partial downloads: %w", err)
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
	if err != nil && cancelRequested(ctx) && output.cancelled {
		// Cleanup outlives the cancellation that triggered it, so it runs on a
		// context that keeps this one's values without its deadline.
		if cleanupErr := cleanupLMSPartials(context.WithoutCancel(ctx), lmstudioModelsDir(), model, before); cleanupErr != nil {
			return nil, fmt.Errorf("download stopped, but partial-file cleanup failed: %w", cleanupErr)
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
	output.stdin = stdin
	output.text = ""
	output.cancelled = false
	output.completed = false
	output.answered = false
	cmd.Stdout = output
	cmd.Stderr = output
	if err := cmd.Start(); err != nil {
		return "", err
	}
	finished := make(chan error, 1)
	go func() { finished <- cmd.Wait() }()
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
			return stopLMSDownload(lmsCancelGracesFrom(ctx), cmd, finished, output)
		}
	}
	if runErr != nil {
		return "", fmt.Errorf("%w: %s", runErr, lmsErrorDetail(output.text))
	}
	return output.text, nil
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
	output *lmsDownloadOutput,
) (string, error) {
	if err := interruptDownload(cmd); err != nil {
		_ = killDownload(cmd)
		if reaped, runErr := waitLMSExit(finished, graces.reap); reaped && runErr == nil && output.completed {
			return output.text, nil
		}
		return "", fmt.Errorf("could not confirm LM Studio cancellation: %w", err)
	}
	exited, _ := waitLMSExit(finished, graces.interrupt)
	if !exited && interruptDownload(cmd) == nil {
		exited, _ = waitLMSExit(finished, graces.retry)
	}
	if exited {
		if output.completed {
			return output.text, nil
		}
		if !output.cancelled {
			return "", fmt.Errorf("LM Studio exited without confirming download cancellation")
		}
		return "", context.Canceled
	}
	// Reclaim the process — it holds this worker's stdio pipes — but report the
	// cancel as failed so no partial file is deleted on the strength of a
	// cancellation the CLI never acknowledged. A process that outlasts even the
	// kill is abandoned rather than waited on; see lmsCancelGraces.
	_ = killDownload(cmd)
	waitLMSExit(finished, graces.reap)
	output.cancelled = false
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

// Remove only the partial files this `lms get` produced, preserving completed
// shards. The LM Studio app and any other client download into the same
// repository directory, so a file is deleted only when it carries the requested
// quantization, differs from the snapshot taken before this download started,
// and has stopped moving. Never delete a model directory or follow a symlink
// outside the cache.
func cleanupLMSPartials(ctx context.Context, root, model string, before map[string]os.FileInfo) error {
	files, err := lmsPartialFiles(root, model)
	if err != nil {
		return err
	}
	touched := make([]string, 0, len(files))
	for path, info := range files {
		if old, ok := before[path]; ok && old.Size() == info.Size() && old.ModTime().Equal(info.ModTime()) {
			continue
		}
		touched = append(touched, path)
	}
	stable, _ := quiescentPaths(ctx, touched)
	for _, candidate := range stable {
		if _, err := removeIfUnchanged(candidate); err != nil {
			return err
		}
	}
	return nil
}

// lmsPartialFiles lists the in-progress downloads in a model's repository
// directory that could belong to the requested quantization.
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
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if os.IsNotExist(err) {
		return nil, nil
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
		// it. Without a pinned quantization the CLI picks its own and every
		// partial here stays a candidate, left to the snapshot and settle
		// checks in cleanupLMSPartials.
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
