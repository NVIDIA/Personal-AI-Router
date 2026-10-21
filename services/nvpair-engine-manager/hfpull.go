// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// hfAPIBase is the Hugging Face model API. A variable so tests can point it at
// a local server.
var hfAPIBase = hfModelsAPI

const (
	hfPollInterval = time.Second
	// hfStopGrace is how long a cancelled download gets to exit before it is killed.
	hfStopGrace = 3 * time.Second
	// hfErrorTail is how many trailing output lines a failed download reports.
	hfErrorTail = 4
)

// hfDroppedSuffixes are file types a vLLM-served model never reads. Skipping
// them keeps a repo that ships several formats from downloading each of them.
var hfDroppedSuffixes = []string{".gguf", ".onnx", ".h5", ".msgpack", ".ot", ".tflite"}

// hfAlternateWeights are dropped when the repo also has safetensors.
var hfAlternateWeights = []string{".bin", ".pt", ".pth"}

// selectHFFiles chooses the repository files to download and their total size.
// A repo with safetensors does not also need its pickle weights, nor the
// consolidated copy some publishers add beside the sharded one. File names that
// could be read as an option or escape the cache are refused.
func selectHFFiles(siblings []hfSibling) (files []string, total int64, err error) {
	hasSafetensors, hasSharded := false, false
	for _, f := range siblings {
		name := strings.ToLower(f.RFilename)
		if strings.HasSuffix(name, ".safetensors") {
			hasSafetensors = true
			if !strings.HasPrefix(filepath.Base(name), "consolidated") {
				hasSharded = true
			}
		}
	}
	hasWeights := false
	for _, f := range siblings {
		name := f.RFilename
		lower := strings.ToLower(name)
		if name == "" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") || strings.Contains(name, "..") {
			continue
		}
		if strings.HasPrefix(lower, "original/") || hasSuffixAny(lower, hfDroppedSuffixes) {
			continue
		}
		if hasSafetensors && hasSuffixAny(lower, hfAlternateWeights) {
			continue
		}
		if hasSharded && strings.HasSuffix(lower, ".safetensors") && strings.HasPrefix(filepath.Base(lower), "consolidated") {
			continue
		}
		if strings.HasSuffix(lower, ".safetensors") || hasSuffixAny(lower, hfAlternateWeights) {
			hasWeights = true
		}
		files = append(files, name)
		total += f.Size
	}
	if !hasWeights {
		return nil, 0, errors.New("the repository has no safetensors or PyTorch weights this engine can load")
	}
	return files, total, nil
}

func hasSuffixAny(s string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

// hfRepoFiles lists a repository's files with sizes.
func (e *Executor) hfRepoFiles(ctx context.Context, id string) ([]hfSibling, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hfAPIBase+"/"+id+"?blobs=true", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "PAIR/1.0")
	if token := os.Getenv("HF_TOKEN"); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err
		}
		return nil, fmt.Errorf("look up the model on Hugging Face: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, errors.New("no such model on Hugging Face")
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, errors.New("this model needs a Hugging Face login: accept its terms on huggingface.co and set HF_TOKEN")
	default:
		return nil, fmt.Errorf("Hugging Face returned %s", resp.Status)
	}
	var repo struct {
		Siblings []hfSibling `json:"siblings"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxCatalogBody)).Decode(&repo); err != nil {
		return nil, fmt.Errorf("decode the Hugging Face reply: %w", err)
	}
	return repo.Siblings, nil
}

// hfBlobBytes totals what is in a repository's blobs directory, which holds
// finished files and the .incomplete ones still being written.
func hfBlobBytes(repoDir string) int64 {
	entries, err := os.ReadDir(filepath.Join(repoDir, "blobs"))
	if err != nil {
		return 0
	}
	var total int64
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && !info.IsDir() {
			total += info.Size()
		}
	}
	return total
}

// pullHFModel downloads a Hugging Face repository into the hub cache with the
// engine's `hf` tool. Progress is the share of the repository's bytes that has
// landed in the cache, which stays honest across resumed and parallel files.
// A download that makes no progress for the pull timeout is abandoned.
func (e *Executor) pullHFModel(ctx context.Context, st *engineState, engine, model string, act Action) (json.RawMessage, error) {
	id := strings.TrimSpace(model)
	if !hfModelIDRe.MatchString(id) {
		return nil, fmt.Errorf("pull %q: not a valid Hugging Face model id (use org/name)", model)
	}
	vars := st.pathVars()
	hubDir, err := resolvePlaceholders(act.HFPull.Dir, vars)
	if err != nil {
		return nil, err
	}
	hubDir = expandPath(hubDir)

	st.mu.Lock()
	binPath := st.binPath
	st.mu.Unlock()
	if binPath == "" {
		bin, err := resolvePlaceholders(st.plat.Runtime.Bin, vars)
		if err != nil {
			return nil, err
		}
		binPath = expandPath(bin)
	}
	tool := filepath.Join(filepath.Dir(binPath), act.HFPull.Tool)
	if _, err := os.Stat(tool); err != nil {
		return nil, fmt.Errorf("pull %q: the Hugging Face tool %q was not found next to %s; install it with `pip install huggingface_hub` or reinstall the engine", id, act.HFPull.Tool, binPath)
	}

	e.emitPullProgress(ProgressEvent{Engine: engine, Op: "pull", Stage: "pulling", Message: id})
	siblings, err := e.hfRepoFiles(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("pull %q: %w", id, err)
	}
	files, total, err := selectHFFiles(siblings)
	if err != nil {
		return nil, fmt.Errorf("pull %q: %w", id, err)
	}

	repoDir := filepath.Join(hubDir, "models--"+strings.ReplaceAll(id, "/", "--"))
	args := append([]string{"download", id}, files...)
	args = append(args, "--cache-dir", hubDir)
	var tailMu sync.Mutex
	var tail []string
	proc, err := startManagedProc(tool, args, map[string]string{
		"HF_HUB_DISABLE_PROGRESS_BARS": "1",
		"HF_HUB_DISABLE_TELEMETRY":     "1",
	}, func(_, line string) {
		if line = strings.TrimSpace(line); line != "" {
			tailMu.Lock()
			defer tailMu.Unlock()
			tail = append(tail, line)
			if len(tail) > hfErrorTail {
				tail = tail[1:]
			}
		}
	})
	if err != nil {
		return nil, fmt.Errorf("pull %q: start the download: %w", id, err)
	}

	pullCtx, watchdog := newPullProgressWatchdog(ctx, e.pullProgressTimeout)
	defer watchdog.stop()
	ticker := time.NewTicker(hfPollInterval)
	defer ticker.Stop()
	lastPct := -1
	for {
		select {
		case <-proc.done:
			if proc.cmd.ProcessState == nil || !proc.cmd.ProcessState.Success() {
				tailMu.Lock()
				reason := strings.Join(tail, "; ")
				tailMu.Unlock()
				return nil, fmt.Errorf("pull %q: download failed: %s", id, reason)
			}
			if _, complete := completeHFModel(repoDir); !complete {
				return nil, fmt.Errorf("pull %q: the download finished but the model in the cache is incomplete", id)
			}
			e.emitPullProgress(ProgressEvent{Engine: engine, Op: "pull", Stage: "success", Percent: 100, Message: id})
			return json.Marshal(map[string]any{"model": id, "bytes": total})
		case <-pullCtx.Done():
			proc.stop(hfStopGrace)
			return nil, fmt.Errorf("pull %q: %w", id, watchdog.resolveError(pullCtx, context.Cause(pullCtx)))
		case <-ticker.C:
			done := hfBlobBytes(repoDir)
			watchdog.recordProgress("bytes", done)
			if total <= 0 {
				continue
			}
			pct := int(done * 100 / total)
			if pct > 99 {
				pct = 99
			}
			if pct != lastPct && pct > 0 {
				lastPct = pct
				e.emitPullProgress(ProgressEvent{Engine: engine, Op: "pull", Stage: "downloading", Percent: pct, Message: id})
			}
		}
	}
}
