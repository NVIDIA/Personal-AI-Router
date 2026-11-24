// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"time"
)

const (
	lmstudioHostReadyTimeout  = 5 * time.Minute
	lmstudioHostPollInterval  = 250 * time.Millisecond
	lmstudioHostStatusTimeout = 10 * time.Second
)

var versionNumberRe = regexp.MustCompile(`\d+`)

// startLMStudioHost starts LM Studio's llmster daemon with a hidden console
// before `lms server start` runs. On Windows `lms` launches llmster detached,
// so llmster has no console and each node.exe worker it spawns opens a visible
// one. Started from here, llmster inherits a hidden console and so do its
// workers. It is best effort: when llmster cannot be found or started,
// `lms server start` still brings the daemon up, only with the visible windows.
func (e *Executor) startLMStudioHost(ctx context.Context, cli string) {
	if runtime.GOOS != "windows" {
		return
	}
	llmster := latestLLMSter(cli)
	if llmster == "" {
		slog.Warn("llmster not found next to the LM Studio CLI; its worker consoles will be visible", "cli", cli)
		return
	}
	running, err := e.lmstudioHostRunning(ctx, cli)
	if err != nil {
		slog.Warn("cannot tell whether llmster is running; leaving startup to lms", "err", err)
		return
	}
	if running {
		return
	}
	cmd := exec.Command(llmster)
	configureSysProcAttr(cmd)
	if err := cmd.Start(); err != nil {
		slog.Warn("start llmster hidden failed; leaving startup to lms", "err", err)
		return
	}
	// llmster outlives this call and the engine, which stops it with lms.
	if err := cmd.Process.Release(); err != nil {
		slog.Warn("release llmster process handle failed", "err", err)
	}
	deadline := time.Now().Add(lmstudioHostReadyTimeout)
	for time.Now().Before(deadline) {
		if running, err := e.lmstudioHostRunning(ctx, cli); err == nil && running {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(lmstudioHostPollInterval):
		}
	}
	slog.Warn("llmster did not report running in time; continuing with lms server start", "timeout", lmstudioHostReadyTimeout)
}

// lmstudioHostRunning reports whether `lms daemon status --json` shows a
// running host, daemon or GUI.
func (e *Executor) lmstudioHostRunning(ctx context.Context, cli string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, lmstudioHostStatusTimeout)
	defer cancel()
	out, err := e.runCommandOutput(ctx, []string{cli, "daemon", "status", "--json"})
	if err != nil {
		return false, err
	}
	var status struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		return false, err
	}
	return status.Status == "running", nil
}

// latestLLMSter returns the llmster executable of the newest version installed
// beside the LM Studio CLI (<root>/bin/lms -> <root>/llmster/<version>/llmster),
// or "" when none is installed.
func latestLLMSter(cli string) string {
	root := filepath.Dir(filepath.Dir(cli))
	versions, err := os.ReadDir(filepath.Join(root, "llmster"))
	if err != nil {
		return ""
	}
	best, bestParts := "", []int(nil)
	for _, v := range versions {
		if !v.IsDir() {
			continue
		}
		path := filepath.Join(root, "llmster", v.Name(), "llmster.exe")
		if _, err := os.Stat(path); err != nil {
			continue
		}
		parts := versionParts(v.Name())
		if best == "" || compareVersionParts(parts, bestParts) > 0 {
			best, bestParts = path, parts
		}
	}
	return best
}

// versionParts splits a version such as "0.0.27-1" into its numbers.
func versionParts(version string) []int {
	var parts []int
	for _, digits := range versionNumberRe.FindAllString(version, -1) {
		n, err := strconv.Atoi(digits)
		if err != nil {
			continue
		}
		parts = append(parts, n)
	}
	return parts
}

func compareVersionParts(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		var x, y int
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x - y
		}
	}
	return 0
}
