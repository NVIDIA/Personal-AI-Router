// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// afterdownload.go runs a listed model's after_download steps: Python scripts
// the engine ships that turn a vendor's checkpoint into a directory the engine
// reads (Kimi K3 publishes tiktoken.model, and Colibri's engines read
// tokenizer.json). The engine's own setup runs the same scripts, with the
// Python its launcher finds, so PAIR finds Python the same way.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"
)

// pythonCandidates are the commands tried for Python 3, in order. On Windows
// the py launcher comes first: it is the one entry point that stays right with
// several versions installed, and Colibri's coli.cmd asks it first too. A var
// so tests can stand in a fake interpreter.
var pythonCandidates = func() [][]string {
	if runtime.GOOS == "windows" {
		return [][]string{{"py", "-3"}, {"python"}, {"python3"}}
	}
	return [][]string{{"python3"}, {"python"}}
}()

// pythonVersionCheck exits zero on the Python the engines' tools require.
const pythonVersionCheck = "import sys; sys.exit(0 if sys.version_info >= (3, 10) else 1)"

const (
	// pythonProbeTimeout bounds one candidate's version check. Windows' Store
	// alias for python opens the Store instead of answering, and must not
	// hold the download up.
	pythonProbeTimeout = 15 * time.Second
	// afterDownloadTimeout bounds one step. Rewriting a tokenizer takes
	// seconds; a step still running after this has hung.
	afterDownloadTimeout = 30 * time.Minute
)

// findPython3 returns the first candidate that is Python 3.10 or newer.
func findPython3(ctx context.Context) ([]string, error) {
	for _, candidate := range pythonCandidates {
		if _, err := exec.LookPath(candidate[0]); err != nil {
			continue
		}
		probeCtx, cancel := context.WithTimeout(ctx, pythonProbeTimeout)
		err := runManifestCommand(probeCtx, append(append([]string(nil), candidate...), "-c", pythonVersionCheck), nil)
		cancel()
		if err == nil {
			return candidate, nil
		}
	}
	return nil, errors.New("Python 3.10 or newer was not found on PATH")
}

// runAfterDownload runs the steps whose file is missing from dir, in order. A
// step needs the engine installed, because the script ships with it.
func (e *Executor) runAfterDownload(ctx context.Context, st *engineState, model string, steps []AfterDownloadStep, dir string) error {
	for _, step := range steps {
		if hasMarker(dir, step.Creates) {
			continue
		}
		script := filepath.Join(st.installDir, filepath.FromSlash(step.Python))
		if fi, err := os.Stat(script); st.installDir == "" || !pathWithinRoot(st.installDir, script) || err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("%s is finished by %s's %s, so install %s and download it again",
				model, st.manifest.DisplayName, step.Python, st.manifest.DisplayName)
		}
		python, err := findPython3(ctx)
		if err != nil {
			return fmt.Errorf("%s is finished by a Python script, and %w", model, err)
		}
		slog.Info("finishing a model download", "engine", st.manifest.Engine, "model", model, "step", step.Python)
		stepCtx, cancel := context.WithTimeout(ctx, afterDownloadTimeout)
		err = runManifestCommand(stepCtx, append(append([]string(nil), python...), script, dir), nil)
		cancel()
		if err != nil {
			return fmt.Errorf("%s could not be finished by %s: %w", model, step.Python, err)
		}
		if !hasMarker(dir, step.Creates) {
			return fmt.Errorf("%s ran without writing %s for %s", step.Python, step.Creates, model)
		}
	}
	return nil
}
