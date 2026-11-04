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
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// The download itself is done by Hugging Face's own `hf` CLI, which ships with
// the huggingface_hub package vLLM depends on, so it writes the cache exactly as
// vLLM reads it. This file only chooses the files to fetch, reports progress and
// stops a download that stalls.

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

// isConsolidatedWeights reports whether a file holds weights in Mistral's own
// format, consolidated*.safetensors. vLLM matches that pattern on the file name.
func isConsolidatedWeights(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	return strings.HasPrefix(base, "consolidated") && strings.HasSuffix(base, ".safetensors")
}

// selectHFFiles chooses the repository files to download and their total size:
// the weights vLLM will load, plus config and tokenizer files. A repo with
// safetensors does not also need its pickle weights. A repo that also ships
// Mistral-format consolidated*.safetensors is loaded from those (vLLM's
// load_format auto prefers them, with params.json and the Mistral tokenizer), so
// its Hugging Face-format shards are not downloaded. File names that could be
// read as an option or escape the cache are refused.
func selectHFFiles(siblings []hfSibling) (files []string, total int64, err error) {
	hasSafetensors, mistralFormat := false, false
	for _, f := range siblings {
		if strings.HasSuffix(strings.ToLower(f.RFilename), ".safetensors") {
			hasSafetensors = true
		}
		if isConsolidatedWeights(f.RFilename) {
			mistralFormat = true
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
		if mistralFormat && strings.HasSuffix(lower, ".safetensors") && !isConsolidatedWeights(name) {
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
	body, err := e.hfGet(ctx, hfAPIBase+"/"+id+"?blobs=true", maxCatalogBody)
	if err != nil {
		return nil, err
	}
	var repo struct {
		Siblings []hfSibling `json:"siblings"`
	}
	if err := json.Unmarshal(body, &repo); err != nil {
		return nil, fmt.Errorf("decode the Hugging Face reply: %w", err)
	}
	return repo.Siblings, nil
}

// hfGet fetches a Hugging Face URL, with HF_TOKEN when set, and returns at most
// limit bytes of the body. Errors say what the user can do about them.
func (e *Executor) hfGet(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
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
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// hfFileBase serves repository files. A variable so tests can point it at a
// local server.
var hfFileBase = "https://huggingface.co"

const (
	// hfLookupTimeout bounds the model lookup a first start makes, so an
	// unreachable Hugging Face delays the start by at most this much.
	hfLookupTimeout = 10 * time.Second
	// maxHFConfigBody is far above any model's config.json.
	maxHFConfigBody = 1 << 20
)

// hfModelLookup supplies the config and download size of a model that is not in
// the cache, and whether both were found.
type hfModelLookup func(id string) (cfg hfModelConfig, size int64, ok bool)

// lookupHFModel asks Hugging Face for a model's config.json and the size of the
// files a download fetches, so a start that is about to download the model can
// size its launch the same way as for a cached one. It reports false, and the
// start falls back to sizing from free GPU memory, when offline, when the model is
// gated without a token, or when the config lacks what sizing needs.
func (e *Executor) lookupHFModel(ctx context.Context, id string) (hfModelConfig, int64, bool) {
	var cfg hfModelConfig
	if !hfModelIDRe.MatchString(id) || hfOffline() {
		return cfg, 0, false
	}
	ctx, cancel := context.WithTimeout(ctx, hfLookupTimeout)
	defer cancel()
	siblings, err := e.hfRepoFiles(ctx, id)
	if err != nil {
		slog.Info("model size lookup failed; the first start is sized from free GPU memory", "model", id, "err", err)
		return cfg, 0, false
	}
	_, size, err := selectHFFiles(siblings)
	if err != nil {
		slog.Info("model size lookup found no loadable weights", "model", id, "err", err)
		return cfg, 0, false
	}
	raw, err := e.hfGet(ctx, hfFileBase+"/"+id+"/resolve/main/config.json", maxHFConfigBody)
	if err == nil {
		err = json.Unmarshal(raw, &cfg)
	}
	if err != nil {
		slog.Info("model config lookup failed; the first start is sized from free GPU memory", "model", id, "err", err)
		return cfg, 0, false
	}
	if _, _, _, _, ok := cfg.text().kvShape(); !ok {
		return cfg, 0, false
	}
	return cfg, size, true
}

// hfOffline reports whether HF_HUB_OFFLINE asks Hugging Face tools not to use
// the network.
func hfOffline() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HF_HUB_OFFLINE"))) {
	case "1", "on", "yes", "true":
		return true
	}
	return false
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

// hfToolPath finds the engine's Hugging Face CLI, named by a manifest's
// hf_pull or hf_cache action, in the directory that holds the engine binary (a
// vLLM virtualenv ships `hf` beside `vllm`).
func hfToolPath(st *engineState, name string) (string, error) {
	st.mu.Lock()
	binPath := st.binPath
	st.mu.Unlock()
	if binPath == "" {
		bin, err := resolvePlaceholders(st.plat.Runtime.Bin, st.pathVars())
		if err != nil {
			return "", err
		}
		binPath = expandPath(bin)
	}
	tool := filepath.Join(filepath.Dir(binPath), name)
	if _, err := os.Stat(tool); err != nil {
		return "", fmt.Errorf("the Hugging Face tool %q was not found next to %s; install it with `pip install huggingface_hub` or reinstall the engine", name, binPath)
	}
	return tool, nil
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

	tool, err := hfToolPath(st, act.HFPull.Tool)
	if err != nil {
		return nil, fmt.Errorf("pull %q: %w", id, err)
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
