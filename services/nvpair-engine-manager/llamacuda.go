// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The CUDA 13 llama build carries kernels for compute capability 7.5 (Turing)
// and newer. An older GPU still enumerates as a CUDA device, so the staged
// build's device check alone cannot reject it.
const llamaMinCUDACapability = 7.5

// llamaCUDACapabilities parses nvidia-smi's one-row-per-GPU compute capability output.
func llamaCUDACapabilities(out string) ([]string, error) {
	var capabilities []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if _, err := strconv.ParseFloat(line, 64); err != nil {
			return nil, fmt.Errorf("nvidia-smi reported an unreadable compute capability %q", line)
		}
		capabilities = append(capabilities, line)
	}
	if len(capabilities) == 0 {
		return nil, errors.New("nvidia-smi reported no NVIDIA GPU")
	}
	return capabilities, nil
}

// prepareLlamaWindowsCUDA stages and validates the pinned official CUDA build
// when every NVIDIA GPU can run it. When CUDA does not apply, or the staged
// build reports no CUDA device, it returns no candidate and the reason so the
// caller runs the pinned vendor installer instead. Only cancellation and local
// write failures are errors.
// nvidiaCUDACapabilities reads one compute capability per NVIDIA GPU and
// decides whether the pinned CUDA build applies to every one of them. A
// non-empty reason means CUDA is not used and says why; only cancellation
// and local failures are errors. Shared by the Windows x64 and Linux gates.
func (e *Executor) nvidiaCUDACapabilities(ctx context.Context, st *engineState) ([]string, string, error) {
	env, err := childEnv(st)
	if err != nil {
		return nil, "", err
	}
	query := e.nvidiaComputeQuery
	if query == nil {
		query = func(ctx context.Context, env map[string]string) (string, error) {
			return e.runCommandOutput(ctx, []string{"nvidia-smi", "--query-gpu=compute_cap", "--format=csv,noheader"}, env)
		}
	}
	queryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	raw, err := query(queryCtx, env)
	cancel()
	if ctx.Err() != nil {
		return nil, "", ctx.Err()
	}
	if err != nil {
		return nil, fmt.Sprintf("no NVIDIA GPU reported by nvidia-smi: %v", err), nil
	}
	capabilities, err := llamaCUDACapabilities(raw)
	if err != nil {
		return nil, err.Error(), nil
	}
	for _, capability := range capabilities {
		if value, _ := strconv.ParseFloat(capability, 64); value < llamaMinCUDACapability {
			return nil, fmt.Sprintf("NVIDIA compute capability %s is below the 7.5 the CUDA build requires", capability), nil
		}
	}
	return capabilities, "", nil
}

func (e *Executor) prepareLlamaWindowsCUDA(ctx context.Context, st *engineState, stage string) (string, map[string]any, string, error) {
	capabilities, reason, err := e.nvidiaCUDACapabilities(ctx, st)
	if err != nil {
		return "", nil, "", err
	}
	if reason != "" {
		return "", nil, reason, nil
	}
	candidate := filepath.Join(stage, "cuda")
	if err := e.stageLlamaArchives(ctx, st, candidate, st.plat.Install.CUDAArchives); err != nil {
		if ctx.Err() != nil {
			return "", nil, "", context.Cause(ctx)
		}
		return "", nil, fmt.Sprintf("official CUDA archives: %v", err), nil
	}
	identity, licenses, err := e.validateLlamaCUDA(ctx, st, candidate, 10826)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, "", context.Cause(ctx)
		}
		return "", nil, fmt.Sprintf("official CUDA build: %v", err), nil
	}
	identity["source"], identity["acceleration_policy"] = "pinned-cuda-archives", "cuda"
	identity["compute_capabilities"] = capabilities
	identity["archives"], identity["recipe_sha256"] = st.plat.Install.CUDAArchives, llamaArchiveRecipeHash(st.plat.Install.CUDAArchives)
	if err := os.WriteFile(filepath.Join(candidate, "THIRD-PARTY-LICENSES.txt"), []byte(licenses), 0600); err != nil {
		return "", nil, "", err
	}
	return candidate, identity, "", ctx.Err()
}
