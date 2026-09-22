// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// childEnv gives installer, control CLI and serving processes the same cache
// and runtime settings. Caller action parameters cannot change this environment.
func childEnv(st *engineState, overrides ...map[string]string) (map[string]string, error) {
	st.mu.Lock()
	vars := map[string]string{"install_dir": st.installDir, "port": strconv.Itoa(st.port), "bin": st.binPath, "host": st.plat.Runtime.Bind}
	vars["model_dir"] = llamaModelDir(st)
	st.mu.Unlock()
	for _, values := range overrides {
		for key, value := range values {
			vars[key] = value
		}
	}
	if vars["host"] == "" {
		vars["host"] = "127.0.0.1"
	}
	env, err := resolveChildEnv(st.plat.Runtime.Env, vars)
	if err != nil {
		return nil, err
	}
	if st.manifest != nil && st.manifest.Engine == "llamacpp" {
		if err := validateLlamaPath(vars["model_dir"]); err != nil {
			return nil, err
		}
		cache, err := llamaCachePath(vars["model_dir"])
		if err != nil {
			return nil, err
		}
		env["LLAMA_CACHE"], env["HF_HUB_CACHE"] = cache, cache
	}
	return env, nil
}

// llamaOwnedEnvironmentKey reports whether key names environment the engine
// manager sets on every llama launch from the owned model directory. Launch
// settings may not replace these: a saved value would never reach the engine.
func llamaOwnedEnvironmentKey(key string) bool {
	return environmentKey(key) == environmentKey("LLAMA_CACHE") || environmentKey(key) == environmentKey("HF_HUB_CACHE")
}

func resolveChildEnv(spec, vars map[string]string) (map[string]string, error) {
	env := make(map[string]string, len(spec))
	for k, v := range spec {
		resolved, err := resolvePlaceholders(v, vars)
		if err != nil {
			return nil, err
		}
		if (k == "LLAMA_CACHE" || k == "HF_HUB_CACHE") && resolved != "" {
			resolved, err = llamaCachePath(resolved)
			if err != nil {
				return nil, err
			}
		}
		env[k] = resolved
	}
	return env, nil
}

func llamaCachePath(path string) (string, error) {
	if runtime.GOOS != "windows" {
		return path, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if strings.HasPrefix(abs, `\\?\`) {
		return abs, nil
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(abs, `\\`), nil
	}
	return `\\?\` + abs, nil
}

func plainWindowsPath(path string) string {
	if runtime.GOOS != "windows" {
		return path
	}
	if strings.HasPrefix(path, `\\?\UNC\`) {
		return `\\` + strings.TrimPrefix(path, `\\?\UNC\`)
	}
	return strings.TrimPrefix(path, `\\?\`)
}

func commandEnv(extra ...map[string]string) []string {
	env := os.Environ()
	for _, values := range extra {
		for k, v := range values {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Cancel every process spawned by a bounded CLI/install operation. The default
// CommandContext kill targets only the parent and can leave a downloader alive.
func configureCommandCancel(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return signalPID(cmd.Process.Pid, true)
	}
}
