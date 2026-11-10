// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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
	// announced holds the quantizations the CLI's download plan said it would
	// fetch. Without a pinned quantization it is the only evidence of which
	// files `lms get` chose, and so of which partial is this download's. It is
	// collected as the output arrives because text only keeps a bounded tail,
	// and kept across the request's attempts because a resumed run can fail
	// before it prints the plan an earlier run started.
	announced []string
	// line is the output since the last line break. A plan line is read only
	// once the CLI has finished it, since a pipe can split one partway through
	// a quantization's name.
	line     string
	stdin    io.Writer
	progress func(int)
}

var lmsPercentPattern = regexp.MustCompile(`\]\s+(\d+(?:\.\d+)?)%`)
var ansiPattern = regexp.MustCompile("\x1b\\[[0-9;?]*[a-zA-Z]")

// lmsPlanQuantPattern matches the GGUF quantization names LM Studio shows,
// such as Q4_K_M, IQ4_XS, BF16 and MXFP4.
var lmsPlanQuantPattern = regexp.MustCompile(`(?i)^(?:UD-)?(?:I?Q\d+(?:_[0-9A-Z]+)*|TQ\d+_\d+|B?F16|F32|MXFP4)$`)

// lmsOutputTail bounds both the transcript kept for errors and a line the CLI
// has not finished.
const lmsOutputTail = 8192

func (w *lmsDownloadOutput) Write(data []byte) (int, error) {
	w.mu.Lock()
	w.text += string(data)
	clean := ansiPattern.ReplaceAllString(w.text, "")
	answer := strings.Contains(clean, "Continue to download in the background?") && !w.answered
	w.cancelled = w.cancelled || strings.Contains(clean, "Download canceled.")
	w.completed = w.completed || strings.Contains(clean, "Download completed.")
	w.line += string(data)
	if end := strings.LastIndexAny(w.line, "\r\n"); end >= 0 {
		for _, line := range strings.FieldsFunc(w.line[:end], func(r rune) bool { return r == '\r' || r == '\n' }) {
			quant := lmsAnnouncedQuant(ansiPattern.ReplaceAllString(line, ""))
			if quant != "" && !slices.Contains(w.announced, quant) {
				w.announced = append(w.announced, quant)
			}
		}
		w.line = w.line[end+1:]
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
	if len(w.text) > lmsOutputTail {
		w.text = w.text[len(w.text)-lmsOutputTail:]
	}
	if len(w.line) > lmsOutputTail {
		w.line = w.line[len(w.line)-lmsOutputTail:]
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

// announcedQuants returns a copy of the quantizations the CLI's plan announced.
func (w *lmsDownloadOutput) announcedQuants() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.announced)
}

// lmsAnnouncedQuant returns the quantization a line of `lms get`'s download
// plan says it will fetch, or "" for any other line. The plan prints each model
// it will download as "↓ To download: <name> <quantization> [GGUF] - <size>".
// LM Studio leaves the quantization out when it does not know it, so the word
// before the format tag counts only when it has a quantization's shape; any
// other format names files that do not carry one.
func lmsAnnouncedQuant(line string) string {
	_, plan, ok := strings.Cut(line, "To download:")
	if !ok {
		return ""
	}
	end := strings.LastIndex(plan, " - ")
	if end < 0 {
		return ""
	}
	words := strings.Fields(plan[:end])
	if len(words) < 2 || words[len(words)-1] != "[GGUF]" || !lmsPlanQuantPattern.MatchString(words[len(words)-2]) {
		return ""
	}
	return words[len(words)-2]
}

// reset clears the previous run's output, keeping the quantizations it
// announced. It is called before the process starts, where nothing is writing
// yet, but takes the lock anyway: an earlier disowned process may still hold
// this writer.
func (w *lmsDownloadOutput) reset(stdin io.Writer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stdin = stdin
	w.text = ""
	w.line = ""
	w.cancelled = false
	w.completed = false
	w.answered = false
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
	before, err := lmsPartialFiles(st.modelsDir, model)
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
	//
	// The cancellation stopped the download when the CLI confirmed it, or when
	// it found no CLI running — before the first started, between candidate
	// sources, or while a failed transfer waited to be resumed. Only those
	// cases come back as context.Canceled: a running CLI that did not confirm
	// is reported as its own failure, since its download may still be going.
	cancelled, _, _ := output.state()
	if err != nil && cancelRequested(ctx) && (cancelled || errors.Is(err, context.Canceled)) {
		// Cleanup outlives the cancellation that triggered it, so it runs on a
		// context that keeps this one's values without its deadline.
		if cleanupErr := cleanupLMSAfterCancel(context.WithoutCancel(ctx), st.modelsDir, model, before, output.announcedQuants()); cleanupErr != nil {
			// The download did stop, which is what was asked for. Leftover
			// bytes are the vendor's to resume, so reporting a failed cancel
			// here would deny the one outcome that did happen and leave the
			// row on "Canceling" over a transfer that is gone.
			slog.Warn("partial-file cleanup failed after cancellation",
				"engine", engine, "model", model, "err", cleanupErr)
		}
		return nil, context.Canceled
	}
	// A context that died on its own reports why — above all the action
	// timeout — rather than the bare cancellation the CLI was stopped with. A
	// stop the CLI did not confirm keeps its own report.
	if err != nil && ctx.Err() != nil && !cancelRequested(ctx) && !errors.As(err, new(stopUnconfirmedError)) {
		return nil, pullStopped(ctx, model)
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
	// exited closes once Wait has returned, so a stop that sees it knows the
	// process is gone and runErr is final. cmd.ProcessState cannot serve here:
	// Wait writes it on this goroutine while the cancelling one would be
	// reading it. Wait can return up to downloadWaitDelay after the reap
	// itself, and pinDownloadProcess is what keeps a signal in that gap off a
	// reissued PID.
	exited := make(chan struct{})
	var runErr error
	go func() {
		runErr = cmd.Wait()
		close(exited)
	}()
	select {
	case <-exited:
	case <-ctx.Done():
		if !processExited(exited) {
			return stopLMSDownload(downloadStopGracesFrom(ctx, lmsStopGraces), lmsDownloadProcess(cmd, exited), func() error { return runErr }, output)
		}
	}
	_, _, text := output.state()
	if runErr != nil {
		return "", fmt.Errorf("%w: %s", runErr, lmsErrorDetail(text))
	}
	return text, nil
}

// lmsStopGraces are the graces `lms get` is given to stop. It answers its
// first interrupt with the "Continue to download in the background?" prompt,
// which the output writer declines, then prints "Download canceled." The retry
// is there because the first interrupt can land while the CLI is mid-redraw
// and never reach the handler that prints the prompt.
var lmsStopGraces = downloadStopGraces{interrupt: 15 * time.Second, retry: 5 * time.Second, reap: 10 * time.Second}

// lmsDownloadProcess is the `lms get` CLI as stopDownload sees it. Neither
// signal addresses the CLI alone: on Unix each goes to the whole download
// process group, and on Windows it becomes a control event for everything
// sharing the launcher's console.
func lmsDownloadProcess(cmd *exec.Cmd, exited <-chan struct{}) downloadProcess {
	return downloadProcess{
		exited:    exited,
		interrupt: func() error { return interruptDownload(cmd) },
		kill:      func() { _ = killDownload(cmd) },
	}
}

// stopLMSDownload stops the CLI and reports whether it confirmed the
// cancellation. Confirmation matters beyond the exit code: declining the
// background-download prompt is what aborts the daemon's task, so a client
// killed before it answers can leave the transfer running. A stop the CLI
// never acknowledged is reported as a failed cancel that deletes nothing,
// marked with stopUnconfirmed. runErr is what Wait returned, and is read only
// once the process has been reaped.
func stopLMSDownload(graces downloadStopGraces, process downloadProcess, runErr func() error, output *lmsDownloadOutput) (string, error) {
	stop := stopDownload(process, graces)
	if !stop.exited {
		// The writer is still attached to a live process, so its output from
		// here belongs to nobody this worker is waiting on.
		output.disown()
		if stop.interruptErr != nil {
			return "", stopUnconfirmed(fmt.Errorf("could not confirm LM Studio cancellation and could not reclaim its process: %w", stop.interruptErr))
		}
		return "", stopUnconfirmed(errors.New("LM Studio did not confirm download cancellation and its process could not be reclaimed; the download may still be running in LM Studio"))
	}
	cancelled, completed, text := output.state()
	switch {
	case stop.interruptErr != nil:
		// Nothing asked the CLI to stop, so only a run that had already
		// finished proves anything.
		if completed && runErr() == nil {
			return text, nil
		}
		return "", stopUnconfirmed(fmt.Errorf("could not confirm LM Studio cancellation: %w", stop.interruptErr))
	case stop.killed:
		// Reclaimed, but no partial file may be deleted on the strength of a
		// cancellation the CLI never acknowledged.
		output.disown()
		return "", stopUnconfirmed(errors.New("LM Studio did not confirm download cancellation; the download may still be running in LM Studio"))
	case completed:
		return text, nil
	case !cancelled:
		return "", stopUnconfirmed(errors.New("LM Studio exited without confirming download cancellation"))
	}
	return "", context.Canceled
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

// cleanupLMSAfterCancel removes the partial files this `lms get` created once
// nothing is writing them; see removeQuiescentPartials. No snapshot means the
// inspection failed, not that the directory was empty, and nothing is
// attributable without one.
func cleanupLMSAfterCancel(ctx context.Context, root, model string, before map[string]os.FileInfo, announced []string) error {
	if before == nil {
		return nil
	}
	return removeQuiescentPartials(ctx, func() ([]string, error) {
		return lmsCreatedPartials(root, model, before, announced)
	})
}

// lmsCreatedPartials lists the partial files this `lms get` created, leaving
// out completed shards. The LM Studio app and any other client download into
// the same repository directory, so a file is a candidate only when it is this
// download's file and was absent from the snapshot taken before this download
// started. Never list a model directory or a symlink out of the cache.
//
// LM Studio names a GGUF after its quantization, so which file is this
// download's comes from the quantization: the one the request pins, or else
// the ones the CLI's plan announced. Pulling @Q4_K_M must not touch a @Q8_0
// the LM Studio app is fetching beside it. An unpinned request whose CLI
// announced none has no candidates, because any partial appearing in the
// repository could be the app fetching another quantization beside it.
//
// Presence in the snapshot is disqualifying on its own; what the file has done
// since is not consulted. Bytes gained during this pull look identical whether
// another client is writing them or `lms get` is resuming the file in place,
// and a writer that has merely paused cannot be told from one that finished. A
// resumed download therefore keeps its partial through a cancellation, which
// costs disk the vendor reuses on the next attempt; the alternative costs
// another client its transfer.
func lmsCreatedPartials(root, model string, before map[string]os.FileInfo, announced []string) ([]string, error) {
	files, err := lmsPartialFiles(root, model)
	if err != nil {
		return nil, err
	}
	quants := announced
	if quant := lmsModelQuant(model); quant != "" {
		quants = []string{quant}
	}
	created := make([]string, 0, len(files))
	for path := range files {
		if _, existed := before[path]; existed {
			continue
		}
		target := lmsPartialTarget(path)
		if !slices.ContainsFunc(quants, func(quant string) bool { return lmsNameCarriesQuant(target, quant) }) {
			continue
		}
		created = append(created, path)
	}
	return created, nil
}

// lmsNameCarriesQuant reports whether a model file's name carries quant as a
// whole token, in either case. Quantization names nest — F16 inside BF16, Q6_K
// inside Q6_K_L — so a letter, digit or underscore on either side of a match
// makes it part of a longer name rather than this one.
func lmsNameCarriesQuant(name, quant string) bool {
	if quant == "" {
		return false
	}
	name, quant = strings.ToUpper(name), strings.ToUpper(quant)
	for from := 0; ; {
		i := strings.Index(name[from:], quant)
		if i < 0 {
			return false
		}
		start := from + i
		if !lmsNameByte(name, start-1) && !lmsNameByte(name, start+len(quant)) {
			return true
		}
		from = start + 1
	}
}

// lmsNameByte reports whether name[i] continues a quantization name: a letter,
// digit or underscore. Positions outside name do not.
func lmsNameByte(name string, i int) bool {
	if i < 0 || i >= len(name) {
		return false
	}
	c := name[i]
	return c == '_' || '0' <= c && c <= '9' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z'
}

// lmsPartialTarget is the model file a `downloading_<file>.part` partial is
// becoming.
func lmsPartialTarget(path string) string {
	return strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "downloading_"), ".part")
}

// lmsModelQuant is the quantization a model reference pins with "@quant", or
// empty when it pins none. A pin that is not a quantization name pins none:
// cleanup matches the pin against file names as a whole token, and "@GGUF" or
// "@8B" would match one every quantization's file carries.
func lmsModelQuant(model string) string {
	_, repo, ok := lmsOwnerName(model)
	if !ok {
		return ""
	}
	_, quant, _ := strings.Cut(repo, "@")
	if !lmsPlanQuantPattern.MatchString(quant) {
		return ""
	}
	return quant
}

// lmsPartialFiles lists the in-progress downloads in a model's repository
// directory, whichever quantization each is fetching.
//
// A nil map means the listing could not be made — the reference names no
// repository — and is distinct from an empty one, which means the repository
// holds no partials. cleanupLMSAfterCancel deletes nothing on the former, so
// the two must not be conflated.
func lmsPartialFiles(root, model string) (map[string]os.FileInfo, error) {
	if root == "" {
		return nil, errors.New("engine declares no models directory")
	}
	owner, repo, ok := lmsOwnerName(model)
	if !ok {
		return nil, nil
	}
	repo, _, _ = strings.Cut(repo, "@")
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
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		files[filepath.Join(resolvedTarget, entry.Name())] = info
	}
	return files, nil
}
