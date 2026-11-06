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
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The download itself is done by Hugging Face's own `hf` CLI, which ships with
// the huggingface_hub package vLLM depends on, so it writes the cache exactly as
// vLLM reads it. This file only chooses the files to fetch, reports progress and
// stops a download that stalls.

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

// isConsolidatedWeights reports whether a file holds weights in Mistral's own
// format, consolidated*.safetensors. vLLM matches that pattern on the file name.
func isConsolidatedWeights(name string) bool {
	base := strings.ToLower(filepath.Base(name))
	return strings.HasPrefix(base, "consolidated") && strings.HasSuffix(base, ".safetensors")
}

// isWeightFile reports whether a repository file holds model weights in a
// format vLLM or PyTorch reads.
func isWeightFile(name string) bool {
	return hasSuffixAny(strings.ToLower(name), []string{".safetensors", ".bin", ".pt", ".pth"})
}

// selectHFFiles chooses the repository files to download, and the size of the
// weights among them: the weights vLLM will load (hfChooseWeights, narrowed to
// shards when the repository's index lists them), plus config and tokenizer
// files. Any other weight file is a copy vLLM would not load, such as pickle
// weights beside safetensors, Hugging Face-format shards beside Mistral-format
// consolidated*.safetensors (with params.json and the Mistral tokenizer), or a
// single-file copy beside indexed shards, and is not downloaded. Launch sizing
// uses the weight size, the bytes vLLM loads into memory. File names that could
// be read as an option or escape the cache are refused.
func selectHFFiles(siblings []hfSibling, shards []string) (files []string, weights int64, err error) {
	names := make([]string, 0, len(siblings))
	for _, f := range siblings {
		names = append(names, f.RFilename)
	}
	chosen, _ := hfChooseWeights(names)
	if shards != nil {
		var ok bool
		if chosen, ok = hfIndexedWeights(chosen, shards); !ok {
			return nil, 0, errors.New("the repository's weight index lists files the repository does not have")
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
		switch {
		case slices.Contains(chosen, name):
			hasWeights = true
			weights += f.Size
		case isWeightFile(name):
			continue
		}
		files = append(files, name)
	}
	if !hasWeights {
		return nil, 0, errors.New("the repository has no safetensors or PyTorch weights this engine can load")
	}
	return files, weights, nil
}

// hfSiblingIndex returns the index that decides which of a repository's weight
// files vLLM loads, when it has one and there is more than one candidate. vLLM
// reads the index in exactly that case before downloading.
func hfSiblingIndex(siblings []hfSibling) (string, bool) {
	names := make([]string, 0, len(siblings))
	for _, f := range siblings {
		names = append(names, f.RFilename)
	}
	weights, index := hfChooseWeights(names)
	return index, len(weights) > 1 && slices.Contains(names, index)
}

func hasSuffixAny(s string, suffixes []string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

// hfRepoFiles lists a repository's files with sizes at a revision.
func (e *Executor) hfRepoFiles(ctx context.Context, id, revision string, env hfEnv) ([]hfSibling, error) {
	ctx, cancel := context.WithTimeout(ctx, catalogHTTPTimeout)
	defer cancel()
	listing := env.endpoint() + "/api/models/" + id
	if revision != "" && revision != "main" {
		listing += "/revision/" + url.PathEscape(revision)
	}
	body, err := e.hfGet(ctx, listing+"?blobs=true", maxCatalogBody, env)
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

// hfGet fetches a Hugging Face URL with the engine's HF_TOKEN, when set, and
// returns at most limit bytes of the body. It makes no request when the engine
// is set to work offline. Errors say what the user can do about them.
func (e *Executor) hfGet(ctx context.Context, rawURL string, limit int64, env hfEnv) ([]byte, error) {
	if env.offline() {
		return nil, errors.New("HF_HUB_OFFLINE or TRANSFORMERS_OFFLINE is set for this engine, so Hugging Face is not contacted")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "PAIR/1.0")
	if token := env.token(); token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("look up the model on Hugging Face: %w", withoutURL(err))
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

// hfEnv is the environment an engine's Hugging Face work runs in: the
// engine's launch environment (the manifest's env and the user's assignments
// in its launch settings) over the service's own, which is what the engine
// process itself sees. PAIR's own requests and hf tool runs use it, so
// HF_HUB_OFFLINE or an HF_TOKEN set for the engine applies to them too.
type hfEnv map[string]string

// engineHFEnv returns the environment an engine's next start would get. A
// manifest value that cannot be resolved without a running launch (a port, for
// instance) is left out; Hugging Face settings never need one.
func engineHFEnv(st *engineState) hfEnv {
	env := hfEnv{}
	if st.plat == nil {
		return env
	}
	st.mu.Lock()
	rt := st.plat.Runtime
	st.mu.Unlock()
	vars := st.pathVars()
	for key, template := range rt.Env {
		if value, err := resolvePlaceholders(template, vars); err == nil {
			env[key] = value
		}
	}
	if rt.LaunchEnv != nil {
		if literal, err := literalEnvironment(rt.LaunchEnv); err == nil {
			for key, value := range literal {
				setEnvironmentValue(env, key, value)
			}
		}
	}
	return env
}

// hfActionVars are the placeholders an hf_cache or hf_pull dir may use: the
// engine's paths and the hub cache its environment selects.
func hfActionVars(st *engineState) map[string]string {
	vars := st.pathVars()
	var args []string
	if st.plat != nil {
		st.mu.Lock()
		rt, port := st.plat.Runtime, st.port
		st.mu.Unlock()
		args = launchArguments(rt, rt.Args, map[string]string{
			"host": effectiveBind(rt.Bind, ""), "port": strconv.Itoa(port),
		})
	}
	vars[hfHubCachePlaceholder] = hfLaunchHub(args, engineHFEnv(st))
	return vars
}

// hfLaunchHub is the hub cache vLLM uses for a launch: its --download-dir when
// the launch sets one, else the cache the engine's environment names. vLLM
// passes --download-dir to huggingface_hub as cache_dir for the weights and
// for the tokenizer and config files alike, in the usual hub layout, so the
// model list, Pull, Delete and launch sizing all read and write there too. The
// directory is read as huggingface_hub reads cache_dir: ~ is the home folder,
// and a relative path is relative to the working directory vLLM shares with
// this service.
func hfLaunchHub(args []string, env hfEnv) string {
	dir, ok := launchFlagValue(args, "--download-dir")
	if !ok || dir == "" {
		return env.hubCache()
	}
	if rest, ok := strings.CutPrefix(dir, "~"); ok && (rest == "" || strings.HasPrefix(rest, "/")) {
		if home, err := os.UserHomeDir(); err == nil {
			dir = home + rest
		}
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir
}

// hfDefaultEndpoint is the Hugging Face Hub, when HF_ENDPOINT names no mirror.
// A variable so tests can point it at a local server.
var hfDefaultEndpoint = "https://huggingface.co"

// home is the Hugging Face home: HF_HOME, else $XDG_CACHE_HOME/huggingface,
// else ~/.cache/huggingface, as huggingface_hub's constants resolve it.
func (env hfEnv) home() string {
	if home := env.get("HF_HOME"); home != "" {
		return env.expand(home)
	}
	if cache := env.get("XDG_CACHE_HOME"); cache != "" {
		return filepath.Join(env.expand(cache), "huggingface")
	}
	return filepath.Join(expandPath("~"), ".cache", "huggingface")
}

// hubCache is the hub cache huggingface_hub reads and writes for the engine:
// HF_HUB_CACHE, else the legacy HUGGINGFACE_HUB_CACHE, else <home>/hub.
func (env hfEnv) hubCache() string {
	for _, key := range []string{"HF_HUB_CACHE", "HUGGINGFACE_HUB_CACHE"} {
		if dir := env.get(key); dir != "" {
			return env.expand(dir)
		}
	}
	return filepath.Join(env.home(), "hub")
}

// endpoint is the Hub the engine downloads from: HF_ENDPOINT, such as a
// mirror, else huggingface.co.
func (env hfEnv) endpoint() string {
	if endpoint := strings.TrimRight(strings.TrimSpace(env.get("HF_ENDPOINT")), "/"); endpoint != "" {
		return endpoint
	}
	return hfDefaultEndpoint
}

// token is the token huggingface_hub would send: none when
// HF_HUB_DISABLE_IMPLICIT_TOKEN is set, else HF_TOKEN, else
// HUGGING_FACE_HUB_TOKEN, else the one a `hf auth login` stored in
// HF_TOKEN_PATH or <home>/token.
func (env hfEnv) token() string {
	if hfTrue(env.get("HF_HUB_DISABLE_IMPLICIT_TOKEN")) {
		return ""
	}
	for _, key := range []string{"HF_TOKEN", "HUGGING_FACE_HUB_TOKEN"} {
		if token := strings.TrimSpace(env.get(key)); token != "" {
			return token
		}
	}
	path := env.get("HF_TOKEN_PATH")
	if path == "" {
		path = filepath.Join(env.home(), "token")
	}
	raw, err := os.ReadFile(env.expand(path))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(raw))
}

// expand expands $VARS, from the engine's environment, and ~ in a Hugging Face
// path setting, as huggingface_hub does with os.path.expandvars and
// os.path.expanduser.
func (env hfEnv) expand(path string) string {
	return expandPath(os.Expand(path, env.get))
}

// get returns the engine's value for key, else the service's.
func (env hfEnv) get(key string) string {
	for name, value := range env {
		if environmentKey(name) == environmentKey(key) {
			return value
		}
	}
	return os.Getenv(key)
}

// offline reports whether HF_HUB_OFFLINE, else TRANSFORMERS_OFFLINE, asks
// Hugging Face tools not to use the network, read the way huggingface_hub
// reads them.
func (env hfEnv) offline() bool {
	value := env.get("HF_HUB_OFFLINE")
	if value == "" {
		value = env.get("TRANSFORMERS_OFFLINE")
	}
	return hfTrue(value)
}

// hfTrue reports whether a Hugging Face setting is on, as huggingface_hub's
// _is_true decides it.
func hfTrue(value string) bool {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "1", "ON", "YES", "TRUE":
		return true
	}
	return false
}

// toolEnv is the environment for one run of the engine's hf tool: the engine's
// own, without progress bars or telemetry.
func (env hfEnv) toolEnv() map[string]string {
	out := map[string]string{
		"HF_HUB_DISABLE_PROGRESS_BARS": "1",
		"HF_HUB_DISABLE_TELEMETRY":     "1",
	}
	for key, value := range env {
		out[key] = value
	}
	return out
}

// maxHFConfigBody is far above any model's config.json.
const maxHFConfigBody = 1 << 20

// hfMetadataTimeout bounds the metadata fetch a first start makes, so an
// unreachable Hugging Face delays the start by at most this much.
const hfMetadataTimeout = 2 * time.Minute

// hfPendingModel is a model a start is about to download: the snapshot that
// already holds its config, tokenizer and chat template, fetched ahead of the
// weights, and the bytes of the weights vLLM will load.
type hfPendingModel struct {
	snapshot string
	weights  int64
}

// hfModelFetcher prepares the model a start serves, at the revision it loads,
// when that model is not in the cache yet, and reports whether it could. A start
// builds one with sync.OnceValues, so sizing, the tool-call parser and the
// weight check share a single fetch.
type hfModelFetcher func() (hfPendingModel, bool)

// fetchHFMetadata downloads a model's small files (config, tokenizer, chat
// template and the like, everything a download fetches except the weights)
// into the hub cache with the engine's hf tool, at the revision vLLM will load,
// and sizes its weights from the repository listing. A start that is about to
// download the model can then size its launch and choose its tool-call parser
// from the same files as for a cached model, and vLLM finds those files
// already there. It reports false, and the start goes ahead without them, when
// the engine is offline, the model is gated without a token, or Hugging Face
// cannot be reached.
func (e *Executor) fetchHFMetadata(ctx context.Context, st *engineState, hubDir, id, revision string, env hfEnv) (hfPendingModel, bool) {
	if !hfModelIDRe.MatchString(id) || env.offline() {
		return hfPendingModel{}, false
	}
	toolName := ""
	for _, act := range st.manifest.Actions {
		if act.HFPull != nil {
			toolName = act.HFPull.Tool
		}
	}
	tool, err := hfToolPath(st, toolName)
	if toolName == "" || err != nil {
		slog.Info("no Hugging Face tool to fetch the model's metadata; the first start is sized from free GPU memory", "model", id, "err", err)
		return hfPendingModel{}, false
	}
	ctx, cancel := context.WithTimeout(ctx, hfMetadataTimeout)
	defer cancel()
	siblings, err := e.hfRepoFiles(ctx, id, revision, env)
	if err != nil {
		slog.Info("model lookup failed; the first start is sized from free GPU memory", "model", id, "err", err)
		return hfPendingModel{}, false
	}
	files, weights, err := selectHFFiles(siblings, nil)
	if err != nil {
		slog.Info("model lookup found no loadable weights", "model", id, "err", err)
		return hfPendingModel{}, false
	}
	metadata := slices.DeleteFunc(files, isWeightFile)
	args := append([]string{"download", id}, metadata...)
	args = append(args, "--cache-dir", hubDir, "--revision", revision)
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Env = os.Environ()
	for key, value := range env.toolEnv() {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		slog.Info("fetching the model's metadata failed; the first start is sized from free GPU memory", "model", id, "err", err, "output", lastLines(string(out), hfErrorTail))
		return hfPendingModel{}, false
	}
	snapshot, ok := hfSnapshotDir(hubDir, id, revision)
	if !ok {
		return hfPendingModel{}, false
	}
	// The index came down with the metadata. Size only the shards it lists, the
	// copy vLLM loads.
	if index, ok := hfSiblingIndex(siblings); ok {
		shards, read := hfReadIndex(filepath.Join(snapshot, index))
		if read {
			_, weights, err = selectHFFiles(siblings, shards)
		}
		if !read || err != nil {
			slog.Info("the model's weight index could not be read; the first start is sized from free GPU memory", "model", id, "err", err)
			return hfPendingModel{}, false
		}
	}
	return hfPendingModel{snapshot: snapshot, weights: weights}, true
}

// fetchHFIndex downloads a repository's weight index ahead of its weights, as
// vLLM's own download does, and returns the shards it lists, so a Pull fetches
// only the copy of the weights vLLM loads.
func (e *Executor) fetchHFIndex(ctx context.Context, tool, hubDir, id, index string, env hfEnv) ([]string, error) {
	cmd := exec.CommandContext(ctx, tool, "download", id, index, "--cache-dir", hubDir)
	cmd.Env = os.Environ()
	for key, value := range env.toolEnv() {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("download the weight index: %s", lastLines(string(out), hfErrorTail))
	}
	snapshot, ok := hfSnapshotDir(hubDir, id, "main")
	if !ok {
		return nil, errors.New("the weight index did not reach the cache")
	}
	shards, ok := hfReadIndex(filepath.Join(snapshot, index))
	if !ok {
		return nil, errors.New("the repository's weight index could not be read")
	}
	return shards, nil
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
		// os.Stat, not entry.Info: huggingface_hub replaces a finished blob
		// with a link to its shared store, and the link's own size is tiny.
		if info, err := os.Stat(filepath.Join(repoDir, "blobs", entry.Name())); err == nil && !info.IsDir() {
			total += info.Size()
		}
	}
	return total
}

// hfToolPath finds the engine's Hugging Face CLI, named by a manifest's
// hf_pull or hf_cache action, in the directory that holds the engine binary,
// after following any link to it (a vLLM virtualenv ships `hf` beside `vllm`).
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
	// A vllm on PATH is often a link into the environment it was installed in
	// (pipx, uv tool), and hf sits beside the real one, as Python does for
	// vllmPython.
	if resolved, err := filepath.EvalSymlinks(binPath); err == nil {
		binPath = resolved
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
	hubDir, err := resolvePlaceholders(act.HFPull.Dir, hfActionVars(st))
	if err != nil {
		return nil, err
	}
	hubDir = expandPath(hubDir)

	tool, err := hfToolPath(st, act.HFPull.Tool)
	if err != nil {
		return nil, fmt.Errorf("pull %q: %w", id, err)
	}

	e.emitPullProgress(ProgressEvent{Engine: engine, Op: "pull", Stage: "pulling", Message: id})
	env := engineHFEnv(st)
	siblings, err := e.hfRepoFiles(ctx, id, "main", env)
	if err != nil {
		return nil, fmt.Errorf("pull %q: %w", id, err)
	}
	files, _, err := selectHFFiles(siblings, nil)
	if index, ok := hfSiblingIndex(siblings); err == nil && ok {
		var shards []string
		if shards, err = e.fetchHFIndex(ctx, tool, hubDir, id, index, env); err == nil {
			files, _, err = selectHFFiles(siblings, shards)
		}
	}
	if err == nil {
		err = checkHFLayout(files)
	}
	if err != nil {
		return nil, fmt.Errorf("pull %q: %w", id, err)
	}
	st.mu.Lock()
	rule := st.plat.Runtime.UnsupportedWeights
	st.mu.Unlock()
	if err := e.checkRepoWeights(ctx, rule, id, files, env); err != nil {
		return nil, fmt.Errorf("pull %q: %w", id, err)
	}
	var total int64
	for _, f := range siblings {
		if slices.Contains(files, f.RFilename) {
			total += f.Size
		}
	}

	repoDir := filepath.Join(hubDir, "models--"+strings.ReplaceAll(id, "/", "--"))
	args := append([]string{"download", id}, files...)
	args = append(args, "--cache-dir", hubDir)
	var tailMu sync.Mutex
	var tail []string
	proc, err := startManagedProc(tool, args, env.toolEnv(), func(_, line string) {
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
			// The revision just downloaded must be complete; another, older
			// complete revision in the cache would not make it so.
			snapshot, ok := hfSnapshotDir(hubDir, id, "main")
			if _, _, complete := completeHFSnapshot(snapshot); !ok || !complete {
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
