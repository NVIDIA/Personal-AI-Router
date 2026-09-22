// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The version endpoint is the only unpinned llama source. The manifest cannot
// supply a latest URL or relax the ordinary fetch pin rules.
//
// The installer is pinned by commit and verified against llamaInstallerSHA256
// before it runs. A branch ref would be mutable, and this script is executed:
// upstream could change the bytes between review and any user's install, and
// recording the hash afterwards gates nothing, because by then it has already
// run as the engine-manager user. Verifying first means the worst an upstream
// change can do is fail the download, which falls back to the pinned CUDA
// archives below.
//
// Pinning the script does not pin the engine: the build installed is whatever
// llamaLatestVersionURL resolves to, passed in as LLAMA_VERSION. Bumping the
// pin is a deliberate edit of both constants together.
const llamaInstallerCommit = "4ee224e8b16ad6c48be85609e74dd8b1e8d740ae"
const llamaLatestInstallerURL = "https://raw.githubusercontent.com/ggml-org/llama-install.sh/" +
	llamaInstallerCommit + "/install.ps1"
const llamaLatestVersionURL = "https://huggingface.co/buckets/ggml-org/install.sh/resolve/latest"
const maxLlamaInstallerBytes = 1 << 20

// Digest of the pinned commit's install.ps1. A var only so the fixtures can
// substitute their own script, which stands in for an installer they cannot
// run; nothing in the shipped binary assigns to it.
var llamaInstallerSHA256 = "455084203db0c864f4eb218bc82792b4304458a96211c385275d8337a5049851"

// Overridable by serial socket-free tests; covers resolution through validation.
var llamaUpstreamAttemptTimeout = 3 * time.Minute

var llamaBuildTag = regexp.MustCompile(`^b([1-9][0-9]*)$`)
var llamaBuildOutput = regexp.MustCompile(`(?m)^(?:b([1-9][0-9]*)-[a-zA-Z0-9]+|version: [^\r\n]*\(build ([1-9][0-9]*), commit [a-zA-Z0-9]+\))`)
var llamaCUDADevice = regexp.MustCompile(`(?m)^\s*CUDA[0-9]+:\s*\S`)

func llamaBuildNumber(version string) int64 {
	m := llamaBuildOutput.FindStringSubmatch(version)
	if m == nil {
		return 0
	}
	n, _ := strconv.ParseInt(m[1]+m[2], 10, 64)
	return n
}

func (e *Executor) latestLlamaBuild(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, llamaLatestVersionURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("resolve latest official llama build: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("resolve latest official llama build: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 65))
	if err != nil {
		return "", err
	}
	build := strings.TrimSpace(string(data))
	if len(data) > 64 || !llamaBuildTag.MatchString(build) {
		return "", errors.New("latest official llama build response is invalid")
	}
	if n, err := strconv.ParseInt(build[1:], 10, 64); err != nil || n == 0 {
		return "", errors.New("latest official llama build number is invalid")
	}
	return build, ctx.Err()
}

// Checks a staged candidate without a server or model. The candidate must
// report the exact numeric build that was selected for it.
func (e *Executor) validateLlamaCUDA(ctx context.Context, st *engineState, candidate string, expected int64) (map[string]any, string, error) {
	return e.validateLlamaARMApp(ctx, st, candidate, expected, true)
}

func (e *Executor) validateLlamaARMApp(ctx context.Context, st *engineState, candidate string, expected int64, requireCUDA bool) (map[string]any, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	env, err := childEnv(st)
	if err != nil {
		return nil, "", err
	}
	bin := filepath.Join(candidate, llamaExecutable())
	version, err := e.runCommandOutput(ctx, []string{bin, "version"}, env)
	if err != nil {
		return nil, "", fmt.Errorf("validate llama version: %w", err)
	}
	build := llamaBuildNumber(version)
	if build == 0 || expected != build {
		return nil, "", errors.New("downloaded llama version did not match the selected build")
	}
	identity := map[string]any{"version": strings.TrimSpace(version), "selected_build": fmt.Sprintf("b%d", build), "platform": "windows/arm64"}
	licenses, err := e.runCommandOutput(ctx, []string{bin, "licenses"}, env)
	if err != nil || strings.TrimSpace(licenses) == "" {
		return identity, "", errors.New("llama third-party licenses could not be read")
	}
	if requireCUDA {
		devices, err := e.runCommandOutput(ctx, []string{bin, "cli", "--list-devices"}, env)
		if err != nil || !llamaCUDADevice.MatchString(devices) {
			return identity, "", errors.New("llama did not report a CUDA device; check the NVIDIA driver and retry")
		}
	}
	f, err := os.Open(bin)
	if err != nil {
		return identity, "", err
	}
	h := sha256.New()
	_, err = io.Copy(llamaArchiveWriter{ctx, h}, f)
	f.Close()
	if err != nil {
		return identity, "", err
	}
	identity["binary_sha256"], identity["cuda_device_verified"] = hex.EncodeToString(h.Sum(nil)), requireCUDA
	return identity, licenses, ctx.Err()
}

func (e *Executor) stageLlamaUpstream(ctx context.Context, st *engineState, stage string) (candidate string, provenance map[string]any, err error) {
	ctx, cancel := context.WithTimeout(ctx, llamaUpstreamAttemptTimeout)
	defer cancel()
	// Command helpers include exit diagnostics but do not preserve cancellation.
	defer func() {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	provenance = map[string]any{"installer_url": llamaLatestInstallerURL, "version_url": llamaLatestVersionURL, "source": "official-upstream", "installer_provenance": "HTTPS official source, pinned by commit and verified against a prequalified SHA256 before execution"}
	build, err := e.latestLlamaBuild(ctx)
	if err != nil {
		return "", provenance, err
	}
	provenance["selected_build"] = build
	selectedBuild, _ := strconv.ParseInt(build[1:], 10, 64)
	if selectedBuild < 10826 {
		return "", provenance, errors.New("latest upstream build is older than supported CUDA fallback b10826")
	}
	env, err := llamaInstallerEnv(st, stage)
	if err != nil {
		return "", provenance, err
	}
	env["LLAMA_VERSION"], env["SKIP_CUDA"], env["SKIP_VULKAN"] = build, "", "1"
	if err = os.MkdirAll(stage, 0700); err != nil {
		return "", provenance, err
	}
	// SHA256 makes downloadLimited reject a mismatch before the script reaches
	// disk, so the verification happens ahead of runCommand below.
	script, err := e.downloadLimited(ctx, "llamacpp", &Fetch{URL: llamaLatestInstallerURL, SHA256: llamaInstallerSHA256}, maxLlamaInstallerBytes)
	if err != nil {
		return "", provenance, err
	}
	defer os.Remove(script)
	provenance["installer_sha256"] = llamaInstallerSHA256
	e.emitInstallProgress("llamacpp", "installing", -1)
	if err = e.runCommand(ctx, llamaInstallerArgs("windows", script), env); err != nil {
		return "", provenance, fmt.Errorf("official latest llama installer: %w", err)
	}
	candidate = filepath.Join(stage, "llama-app")
	identity, licenses, err := e.validateLlamaCUDA(ctx, st, candidate, selectedBuild)
	if err != nil {
		return "", provenance, err
	}
	for k, v := range identity {
		provenance[k] = v
	}
	err = os.WriteFile(filepath.Join(candidate, "THIRD-PARTY-LICENSES.txt"), []byte(licenses), 0600)
	return candidate, provenance, err
}

// prepareLlamaUpstream stages the latest official CUDA build first and, when
// that attempt fails for any reason other than the caller's own cancellation,
// stages the checksum-pinned CUDA archives instead. The candidate it returns is
// always a complete, validated runtime; the caller promotes it.
func (e *Executor) prepareLlamaUpstream(ctx context.Context, st *engineState, stage string) (string, map[string]any, error) {
	candidate, provenance, primaryErr := e.stageLlamaUpstream(ctx, st, filepath.Join(stage, "upstream"))
	if err := ctx.Err(); err != nil {
		return "", nil, err // A parent cancellation never authorizes a fallback.
	}
	if primaryErr == nil {
		return candidate, provenance, nil
	}
	e.emitInstallProgress("llamacpp", "fallback", -1)
	candidate = filepath.Join(stage, "fallback") // Never mix failed script bytes with the ZIPs.
	if err := e.stageLlamaArchives(ctx, st, candidate); err != nil {
		return "", nil, fmt.Errorf("upstream attempt failed (%v); official CUDA fallback failed: %w", primaryErr, err)
	}
	fallback, licenses, err := e.validateLlamaCUDA(ctx, st, candidate, 10826)
	if err != nil {
		return "", nil, fmt.Errorf("upstream attempt failed (%v); official CUDA fallback validation failed: %w", primaryErr, err)
	}
	fallback["source"], fallback["fallback_reason"], fallback["upstream_attempt"] = "pinned-cuda-archives", primaryErr.Error(), provenance
	fallback["archives"], fallback["recipe_sha256"] = st.plat.Install.Archives, llamaArchiveRecipeHash(st.plat.Install.Archives)
	if err = os.WriteFile(filepath.Join(candidate, "THIRD-PARTY-LICENSES.txt"), []byte(licenses), 0600); err != nil {
		return "", nil, err
	}
	return candidate, fallback, ctx.Err()
}
