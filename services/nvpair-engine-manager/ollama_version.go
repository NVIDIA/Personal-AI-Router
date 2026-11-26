// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"golang.org/x/mod/semver"
	"nvpair-shared/engines"
)

type ollamaRelease struct {
	Version string
	Assets  []githubReleaseAsset
}

// CheckOllamaUpdate returns nil for an absent or current installation. A failed
// check is reported separately so clients can retain their last known result.
func (e *Executor) CheckOllamaUpdate(ctx context.Context) (*EngineUpdateInfo, error) {
	return e.checkInstalledEngineUpdate(ctx, engines.NameOllama, e.checkOllamaRelease)
}

func (e *Executor) checkOllamaRelease(ctx context.Context, st *engineState, snapshot engineUpdateSnapshot) (*EngineUpdateInfo, error) {
	// A second installation can become the first detected path while the
	// original is serving. Its version and the updater's target must agree.
	if snapshot.processPath != "" && !isOurEngineImage(snapshot.processPath, snapshot.binPath) {
		return nil, nil
	}
	var current string
	var err error
	if snapshot.running {
		current, err = e.runningOllamaVersion(ctx, snapshot.port)
	} else {
		current, err = ollamaBinaryVersion(ctx, snapshot.binPath)
	}
	if err != nil {
		return nil, err
	}
	release, err := e.latestOllamaRelease(ctx)
	if err != nil {
		return nil, err
	}
	order, err := compareOllamaVersions(current, release.Version)
	if err != nil || order >= 0 {
		return nil, err
	}
	result := &EngineUpdateInfo{
		CurrentVersion: current,
		LatestVersion:  release.Version,
		ReleaseURL:     "https://github.com/ollama/ollama/releases/tag/v" + release.Version,
		InstallType:    "external",
		DownloadURL:    "https://ollama.com/download",
	}
	if !snapshot.adopted && managedOllamaBinary(st, snapshot.binPath) {
		// A release can be public before its archive for this platform is ready.
		if _, err := release.archiveFor(st.bundledInstall); err != nil {
			return nil, nil
		}
		result.InstallType = "managed"
		result.DownloadURL = ""
	}
	return result, nil
}

func (e *Executor) runningOllamaVersion(ctx context.Context, port int) (string, error) {
	var reply struct {
		Version string `json:"version"`
	}
	if err := e.readRunningEngineVersion(ctx, port, "/api/version", &reply); err != nil {
		return "", fmt.Errorf("read running Ollama version: %w", err)
	}
	return normalizedOllamaVersion(reply.Version)
}

func ollamaBinaryVersion(ctx context.Context, binPath string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binPath, "--version")
	// The CLI asks OLLAMA_HOST for its server version before printing its own.
	// Query only the selected binary, even when the user routes to a remote host.
	cmd.Env = processEnvironment(binPath, map[string]string{"OLLAMA_HOST": "http://127.0.0.1:0"})
	configureSysProcAttr(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("read installed Ollama version: %w", err)
	}
	return parseOllamaBinaryVersion(string(out))
}

func parseOllamaBinaryVersion(output string) (string, error) {
	var version string
	for line := range strings.Lines(output) {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "Warning: client version is "); ok {
			return normalizedOllamaVersion(v)
		}
		if v, ok := strings.CutPrefix(line, "ollama version is "); ok {
			version = v
		}
	}
	return normalizedOllamaVersion(version)
}

func (e *Executor) latestOllamaRelease(ctx context.Context) (*ollamaRelease, error) {
	release, err := e.latestGitHubRelease(ctx, "ollama/ollama")
	if err != nil {
		return nil, err
	}
	tag := release.Tag
	version, err := normalizedOllamaVersion(tag)
	if err != nil || !strings.HasPrefix(tag, "v") || semver.Prerelease(tag) != "" {
		return nil, errors.New("Ollama release feed did not return a stable version")
	}
	return &ollamaRelease{Version: version, Assets: release.Assets}, nil
}

func normalizedOllamaVersion(raw string) (string, error) {
	version := strings.TrimPrefix(raw, "v")
	core, _, _ := strings.Cut(version, "-")
	core, _, _ = strings.Cut(core, "+")
	if strings.Count(core, ".") != 2 || !semver.IsValid("v"+version) {
		return "", errors.New("unrecognized Ollama version")
	}
	return version, nil
}

func compareOllamaVersions(current, latest string) (int, error) {
	a, err := normalizedOllamaVersion(current)
	if err != nil {
		return 0, err
	}
	b, err := normalizedOllamaVersion(latest)
	if err != nil {
		return 0, err
	}
	return semver.Compare("v"+a, "v"+b), nil
}
