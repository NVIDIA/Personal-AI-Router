// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// llamaLinuxCUDAGate enables the Linux CUDA selection below. A var so the
// policy tests can exercise it on any host.
var llamaLinuxCUDAGate = runtime.GOOS == "linux"

// prepareLlamaLinuxCUDA is the Linux counterpart of prepareLlamaWindowsCUDA.
//
// The pinned vendor installer picks its CUDA build only when its CUDA probe and
// the CUDA payload download both succeed; on any failure it falls through to
// Vulkan or CPU without saying so, which is how two identical GB10 hosts ended
// up on different backends. When every NVIDIA GPU can run the CUDA build, PAIR
// runs that same pinned installer restricted to CUDA (SKIP_VULKAN, SKIP_ROCM),
// requires a CUDA device before promotion, and retries the transfer once. When
// CUDA does not apply or both attempts fail, it returns no candidate and the
// reason; the caller then runs the unrestricted installer, whose receipt
// records that reason as cuda_not_used. Only cancellation and local write
// failures are errors.
func (e *Executor) prepareLlamaLinuxCUDA(ctx context.Context, st *engineState, stage string) (string, map[string]any, string, error) {
	capabilities, reason, err := e.nvidiaCUDACapabilities(ctx, st)
	if err != nil {
		return "", nil, "", err
	}
	if reason != "" {
		return "", nil, reason, nil
	}
	var lastErr error
	for attempt, dir := range []string{"cuda", "cuda-retry"} {
		candidate, provenance, err := e.stageLlamaInstallerCUDA(ctx, st, filepath.Join(stage, dir), capabilities)
		if err == nil {
			return candidate, provenance, "", nil
		}
		if ctx.Err() != nil {
			return "", nil, "", ctx.Err()
		}
		lastErr = err
		if attempt == 0 {
			e.emitInstallProgress("llamacpp", "retrying", -1)
		}
	}
	return "", nil, fmt.Sprintf("official CUDA build: %v", lastErr), nil
}

// stageLlamaInstallerCUDA runs the pinned installer, restricted to its CUDA
// payload, into a private home and validates the result as a CUDA runtime.
func (e *Executor) stageLlamaInstallerCUDA(ctx context.Context, st *engineState, home string, capabilities []string) (string, map[string]any, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return "", nil, err
	}
	env, err := llamaInstallerEnv(st, home)
	if err != nil {
		return "", nil, err
	}
	env["SKIP_VULKAN"], env["SKIP_ROCM"] = "1", "1"
	script, err := e.download(ctx, "llamacpp", st.plat.Install.Fetch)
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(script)
	e.emitInstallProgress("llamacpp", "installing", -1)
	if err := e.runInstaller(ctx, script, env); err != nil {
		return "", nil, fmt.Errorf("official CUDA installer: %w", transferError(ctx, err))
	}
	candidate := filepath.Join(home, ".llama-app")
	identity, licenses, err := e.validateLlamaCUDA(ctx, st, candidate, 10826)
	if err != nil {
		return "", nil, err
	}
	identity["source"], identity["acceleration_policy"] = "official-installer-cuda", "cuda"
	identity["compute_capabilities"] = capabilities
	identity["installer_url"], identity["installer_sha256"] = st.plat.Install.Fetch.URL, st.plat.Install.Fetch.SHA256
	if err := os.WriteFile(filepath.Join(candidate, "THIRD-PARTY-LICENSES.txt"), []byte(licenses), 0o600); err != nil {
		return "", nil, err
	}
	return candidate, identity, ctx.Err()
}

// runInstaller executes an acquired, checksum-verified vendor installer script.
func (e *Executor) runInstaller(ctx context.Context, script string, env map[string]string) error {
	if e.runLlamaInstaller != nil {
		return e.runLlamaInstaller(ctx, script, env)
	}
	return e.runCommand(ctx, llamaInstallerArgs(runtime.GOOS, script), env)
}
