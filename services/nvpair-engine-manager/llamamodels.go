// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

var llamaRepoRE = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*/[A-Za-z0-9_][A-Za-z0-9_.-]*(?::[A-Za-z0-9_]+)?$`)
var llamaTagRE = regexp.MustCompile(`(?i)[-.]([A-Z0-9_]+)$`)
var llamaSplitRE = regexp.MustCompile(`(?i)^(.+)-([0-9]{5})-of-([0-9]{5})$`)

var llamaCommitRE = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

type llamaCacheModel struct {
	ID    string
	Files []string
}

// llamaCacheModels follows llama.cpp b10826 hf-cache.cpp and download.cpp.
// Only the current ref and complete primary GGUF groups are visible.
func llamaCacheModels(root string) ([]llamaCacheModel, error) {
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return []llamaCacheModel{}, nil
	}
	if err != nil {
		return nil, err
	}
	groups := map[string][]string{}
	primary := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "models--") {
			continue
		}
		repo := strings.ReplaceAll(strings.TrimPrefix(entry.Name(), "models--"), "--", "/")
		if !llamaRepoRE.MatchString(repo) || strings.Contains(repo, ":") {
			continue
		}
		base := filepath.Join(root, entry.Name())
		refs, err := os.ReadDir(filepath.Join(base, "refs"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		ref := ""
		for _, r := range refs {
			if !r.IsDir() {
				ref = r.Name()
				break
			}
		}
		for _, r := range refs {
			if r.Name() == "main" {
				ref = "main"
			}
		}
		if ref == "" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(base, "refs", ref))
		if err != nil {
			return nil, err
		}
		commit := strings.TrimSpace(string(body))
		if !llamaCommitRE.MatchString(commit) {
			continue
		}
		snapshot := filepath.Join(base, "snapshots", commit)
		err = filepath.WalkDir(snapshot, func(path string, d fs.DirEntry, walkErr error) error {
			if os.IsNotExist(walkErr) {
				return nil
			}
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".gguf") {
				return nil
			}
			prefix := strings.TrimSuffix(name, ".gguf")
			first := true
			if split := llamaSplitRE.FindStringSubmatch(prefix); split != nil {
				prefix = split[1]
				first = split[2] == "00001"
				count, _ := strconv.Atoi(split[3])
				if count < 1 {
					return nil
				}
				for i := 1; i <= count; i++ {
					part := filepath.Join(filepath.Dir(path), fmt.Sprintf("%s-%05d-of-%05d.gguf", prefix, i, count))
					info, err := os.Stat(part)
					if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
						return nil
					}
					if _, err := llamaOwnedFile(root, part); err != nil {
						return nil
					}
				}
			}
			tag := llamaTagRE.FindStringSubmatch(prefix)
			if tag == nil {
				return nil
			}
			for _, aux := range []string{"mmproj", "mtp-", "eagle3-", "dflash-", "dspark-"} {
				if strings.Contains(prefix, aux) {
					return nil
				}
			}
			if _, err := llamaOwnedFile(root, path); err != nil {
				return nil
			}
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				return nil
			}
			id := repo + ":" + strings.ToUpper(tag[1])
			groups[id] = append(groups[id], path)
			primary[id] = primary[id] || first
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	out := []llamaCacheModel{}
	for id, files := range groups {
		if primary[id] {
			out = append(out, llamaCacheModel{id, files})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func llamaOwnedFile(root, path string) (string, error) {
	root, path = plainWindowsPath(root), plainWindowsPath(path)
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(realRoot, real)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("model is outside managed cache")
	}
	return real, nil
}

// Caller serializes mutations with st.opMu; this function never starts a server.
func (e *Executor) llamaModelAction(ctx context.Context, st *engineState, action string, params json.RawMessage) (json.RawMessage, error) {
	root := llamaModelDir(st)
	if err := validateLlamaPath(root); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var p struct {
		Model string `json:"model"`
		Name  string `json:"name"`
		Path  string `json:"path"`
		File  string `json:"file"`
	}
	if len(params) > 0 {
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, fmt.Errorf("invalid model parameters: %w", err)
		}
	}
	if p.Model == "" {
		p.Model = p.Name
	}
	switch action {
	case "list_downloaded":
		models, err := llamaCacheModels(root)
		if err != nil {
			return nil, err
		}
		data := []map[string]any{}
		for _, m := range models {
			data = append(data, map[string]any{"id": m.ID, "status": map[string]string{"value": "unloaded"}})
		}
		return json.Marshal(map[string]any{"data": data})
	case "pull_model":
		if !llamaRepoRE.MatchString(p.Model) || strings.Contains(p.Model, "..") {
			return nil, fmt.Errorf("model must be owner/repository[:TAG]")
		}
		if err := os.MkdirAll(root, 0700); err != nil {
			return nil, err
		}
		bin := "llama"
		if runtime.GOOS == "windows" {
			bin += ".exe"
		}
		args := []string{"download", "--hf-repo", p.Model}
		if p.File != "" {
			if strings.HasPrefix(p.File, "-") || strings.Contains(p.File, "..") || strings.ContainsAny(p.File, "\\\r\n") {
				return nil, fmt.Errorf("invalid model file")
			}
			args = append(args, "--hf-file", p.File)
		}
		env, err := childEnv(st)
		if err != nil {
			return nil, err
		}
		cachePath, err := llamaCachePath(root)
		if err != nil {
			return nil, err
		}
		env["LLAMA_CACHE"] = cachePath
		env["HF_HUB_CACHE"] = cachePath
		// The transfer is bounded by lack of progress, not wall-clock. The vendor
		// writes its transfer progress to stderr, so every stderr line is progress;
		// the text itself stays private (paths, unstructured diagnostics).
		stall := newStallContext(ctx, llamaStallIdle, llamaTransferMax)
		defer stall.Stop()
		cmd := exec.CommandContext(stall, filepath.Join(st.installDir, "runtime", bin), args...)
		cmd.Env = commandEnv(env)
		configureSysProcAttr(cmd)
		configureCommandCancel(cmd)
		cmd.WaitDelay = 10 * time.Second
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		stderr, err := cmd.StderrPipe()
		if err != nil {
			return nil, err
		}
		e.emitPullProgress(ProgressEvent{Engine: st.manifest.Engine, Op: "pull", Stage: "pulling", Percent: -1, Message: p.Model})
		if err := cmd.Start(); err != nil {
			return nil, llamaDownloadError(err)
		}
		// The downloader is silent on stderr while it transfers (a live run on a
		// 30 KB/s link was cut off as a stall), so the progress signals are the
		// model cache growing under it and, where available, its own I/O
		// counters; stderr bytes still count when they come. Each signal also
		// keeps a waiting UI alive.
		heartbeatEvent := func() {
			e.emitPullProgress(ProgressEvent{Engine: st.manifest.Engine, Op: "pull", Stage: "pulling", Percent: -1, Message: p.Model})
		}
		watching := make(chan struct{})
		go watchTreeGrowth(stall, cachePath, stallPollEvery, func(int64) { heartbeatEvent() }, watching)
		go watchProcessIO(stall, cmd.Process.Pid, stallPollEvery, heartbeatEvent, watching)
		// Diagnostics keep the head and the tail of stderr: an HTTP status
		// arrives early, a full disk late.
		detail := newBoundedCapture(16 * 1024)
		buf := make([]byte, 32*1024)
		heartbeat := time.Now()
		for {
			n, readErr := stderr.Read(buf)
			if n > 0 {
				stall.Touch()
				detail.Write(buf[:n])
				if time.Since(heartbeat) >= 5*time.Second {
					heartbeat = time.Now()
					e.emitPullProgress(ProgressEvent{Engine: st.manifest.Engine, Op: "pull", Stage: "pulling", Percent: -1, Message: p.Model})
				}
			}
			if readErr != nil {
				break
			}
		}
		err = cmd.Wait()
		close(watching)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			if cause := context.Cause(stall); stall.Err() != nil && cause != nil && cause != context.Canceled {
				return nil, fmt.Errorf("llama model download failed: %v", cause)
			}
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				diag := detail.Bytes()
				if len(bytes.TrimSpace(diag)) == 0 { // the vendor reports some failures on stdout only
					diag = tailBytes(stdout.Bytes(), 16*1024)
				}
				exit.Stderr = diag // Output() would have filled this; the pipe consumed it instead.
				// The user-facing message stays classified (no vendor text, paths or
				// URLs leave this process); the local log keeps a bounded excerpt so a
				// failure that matched no class can still be diagnosed.
				slog.Warn("llama download exited unsuccessfully", "model", p.Model, "exit", exit.ExitCode(), "output", string(tailBytes(bytes.TrimSpace(diag), 400)))
			}
			return nil, llamaDownloadError(err)
		}
		if err := validateLlamaDownload(root, stdout.String()); err != nil {
			return nil, err
		}
		return json.Marshal(map[string]string{"status": "success", "model": p.Model})
	case "import_model":
		id, err := llamaImport(ctx, root, p.Path)
		if err != nil {
			return nil, err
		}
		return json.Marshal(map[string]string{"status": "success", "model": id})
	case "delete_model":
		models, err := llamaCacheModels(root)
		if err != nil {
			return nil, err
		}
		for _, m := range models {
			if m.ID != p.Model {
				continue
			}
			for _, path := range m.Files {
				real, err := llamaOwnedFile(root, path)
				if err != nil {
					return nil, err
				}
				if err := os.Remove(path); err != nil {
					return nil, err
				}
				if real != path {
					referenced := false
					err = filepath.WalkDir(root, func(candidate string, d fs.DirEntry, walkErr error) error {
						if walkErr != nil {
							return walkErr
						}
						if d.Type()&os.ModeSymlink != 0 {
							resolved, resolveErr := filepath.EvalSymlinks(candidate)
							if resolveErr == nil && resolved == real {
								referenced = true
							}
						}
						return nil
					})
					if err != nil {
						return nil, err
					}
					if !referenced {
						if err := os.Remove(real); err != nil && !os.IsNotExist(err) {
							return nil, err
						}
					}
				}
			}
			if strings.HasPrefix(m.ID, "local/") {
				// Imports own one flat snapshot; remove only empty scaffolding.
				base := filepath.Join(root, "models--"+strings.ReplaceAll(strings.SplitN(m.ID, ":", 2)[0], "/", "--"))
				if err := os.Remove(filepath.Dir(m.Files[0])); err == nil {
					_ = os.Remove(filepath.Join(base, "snapshots"))
					_ = os.Remove(filepath.Join(base, "refs", "main"))
					_ = os.Remove(filepath.Join(base, "refs"))
					_ = os.Remove(base)
				}
			}
			return json.Marshal(map[string]string{"status": "success", "model": m.ID})
		}
		return nil, fmt.Errorf("model not found in managed cache")
	default:
		return nil, fmt.Errorf("unsupported llama model action %q", action)
	}
}

// The vendor can exit successfully after downloading a preset instead of model
// weights. Validate its returned files, not merely the requested source name.
func validateLlamaDownload(root, output string) error {
	if strings.TrimSpace(output) == "" {
		return fmt.Errorf("download returned no model path")
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		path := strings.TrimSpace(line)
		if !strings.HasSuffix(strings.ToLower(path), ".gguf") {
			return fmt.Errorf("download returned an unsupported preset or configuration; a GGUF model is required")
		}
		real, err := llamaOwnedFile(root, path)
		if err != nil {
			return fmt.Errorf("download did not return a managed model")
		}
		info, err := os.Stat(real)
		if err != nil || !info.Mode().IsRegular() || info.Size() < 24 {
			return fmt.Errorf("download returned an empty or invalid GGUF model")
		}
		file, err := os.Open(real)
		if err != nil {
			return fmt.Errorf("cannot read downloaded model")
		}
		info, statErr := file.Stat()
		var header [24]byte
		_, readErr := io.ReadFull(file, header[:])
		file.Close()
		if statErr != nil || !info.Mode().IsRegular() || info.Size() < 24 || readErr != nil || string(header[:4]) != "GGUF" {
			return fmt.Errorf("download returned an empty or invalid GGUF model")
		}
		version := binary.LittleEndian.Uint32(header[4:8])
		if (version != 2 && version != 3) || binary.LittleEndian.Uint64(header[8:16]) == 0 {
			return fmt.Errorf("download returned an unsupported GGUF header or a file without model tensors")
		}
	}
	return nil
}

func llamaImport(ctx context.Context, root, source string) (string, error) {
	if !filepath.IsAbs(source) {
		return "", fmt.Errorf("absolute GGUF path is required")
	}
	name := filepath.Base(source)
	prefix := strings.TrimSuffix(name, ".gguf")
	tag := llamaTagRE.FindStringSubmatch(prefix)
	if prefix == name || tag == nil || llamaSplitRE.MatchString(prefix) {
		return "", fmt.Errorf("import requires a single GGUF named with its quantization tag")
	}
	for _, aux := range []string{"mmproj", "mtp-", "eagle3-", "dflash-", "dspark-"} {
		if strings.Contains(prefix, aux) {
			return "", fmt.Errorf("import requires a primary model GGUF")
		}
	}
	repo := "local/" + prefix
	if !llamaRepoRE.MatchString(repo) || strings.Contains(repo, "..") || strings.Contains(repo, "--") {
		return "", fmt.Errorf("unsupported GGUF filename")
	}
	src, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("GGUF must be a regular file")
	}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(src, magic); err != nil || string(magic) != "GGUF" {
		return "", fmt.Errorf("invalid GGUF header")
	}
	if _, err := src.Seek(0, 0); err != nil {
		return "", err
	}
	base := filepath.Join(root, "models--local--"+prefix)
	if _, err := os.Lstat(base); err == nil {
		return "", fmt.Errorf("imported model already exists")
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	temp, err := os.MkdirTemp(root, ".import-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	if err := os.MkdirAll(filepath.Join(temp, "snapshots", "local"), 0700); err != nil {
		return "", err
	}
	dst, err := os.Create(filepath.Join(temp, "snapshots", "local", name))
	if err != nil {
		return "", err
	}
	buf := make([]byte, 1024*1024)
	digest := sha256.New()
	writer := io.MultiWriter(dst, digest)
	for {
		if err = ctx.Err(); err != nil {
			break
		}
		var n int
		n, err = src.Read(buf)
		if n > 0 {
			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				err = writeErr
				break
			}
		}
		if err != nil {
			break
		}
	}
	closeErr := dst.Close()
	if err != io.EOF {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	// The vendor accepts only40hex snapshot refs, even for a local cache entry.
	commit := hex.EncodeToString(digest.Sum(nil))[:40]
	if err := os.Rename(filepath.Join(temp, "snapshots", "local"), filepath.Join(temp, "snapshots", commit)); err != nil {
		return "", err
	}
	if err := os.Mkdir(filepath.Join(temp, "refs"), 0700); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(temp, "refs", "main"), []byte(commit), 0600); err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := os.Rename(temp, base); err != nil {
		return "", err
	}
	return repo + ":" + strings.ToUpper(tag[1]), nil
}
