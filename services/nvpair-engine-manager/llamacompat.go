// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// This is the qualified Windows Vulkan executable, not a rolling build rule.
// Requalify or remove this profile when the vendor identity changes.
const llamaB580BinarySHA256 = "58a3601bb2760652050595bba9b1f8cc9b4380d63427b045f0ba8099716b9ee6"
const llamaB580Profile = "b10826-73a43d1f6-windows-vulkan-b580"

var llamaB580Device = regexp.MustCompile(`(?m)^[\t ]*(Vulkan[0-9]+):[\t ]*Intel(?:\(R\))? Arc(?:\(TM\))? B580 Graphics(?:[\t \r\n]|$)`)

func llamaB580Receipt(data []byte) bool {
	var receipt struct {
		BinarySHA256    string `json:"binary_sha256"`
		InstallerSHA256 string `json:"installer_sha256"`
		Platform        string `json:"platform"`
	}
	return json.Unmarshal(data, &receipt) == nil && receipt.Platform == "windows/amd64" &&
		receipt.BinarySHA256 == llamaB580BinarySHA256 &&
		receipt.InstallerSHA256 == "455084203db0c864f4eb218bc82792b4304458a96211c385275d8337a5049851"
}

// prepareLlamaCompatibility runs only at the owned process-start boundary, so
// older installs receive defaults on their next start without a reinstall.
// The caller holds opMu; no durable state or runtime/model bytes are changed.
func (e *Executor) prepareLlamaCompatibility(ctx context.Context, st *engineState, bin string, args []string, env map[string]string) error {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" || st.manifest == nil || st.manifest.Engine != "llamacpp" {
		return nil
	}
	st.mu.Lock()
	adopted := st.adopted
	st.mu.Unlock()
	if adopted || !isOurEngineImage(bin, filepath.Join(st.installDir, "runtime", "llama.exe")) {
		return nil
	}
	if err := validateLlamaOwnedPaths(st.installDir); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(st.installDir, "runtime", "pair-install.json"))
	if err != nil {
		return nil // Unknown/external provenance is not a managed compatibility target.
	}
	if !llamaB580Receipt(data) {
		return nil
	}
	f, err := os.Open(bin)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(llamaArchiveWriter{ctx, h}, f)
	f.Close()
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != llamaB580BinarySHA256 {
		return fmt.Errorf("llama compatibility identity changed: reinstall the managed runtime before starting")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	devices, err := e.runCommandOutput(ctx, []string{bin, "cli", "--list-devices"}, env)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err != nil {
		return fmt.Errorf("llama compatibility device enumeration failed; retry Start after checking the managed runtime")
	}
	selected, err := llamaB580Selected(devices, args, env)
	if err != nil || !selected {
		return err
	}
	if err := applyLlamaB580Defaults(args, env); err != nil {
		return err
	}
	const detail = "COOPMAT, COOPMAT2, INTEGER_DOT_PRODUCT, F16 and BFLOAT16 disabled; flash attention off"
	slog.Info("llama compatibility profile", "profile", llamaB580Profile, "options", detail)
	st.logs.append("stderr", "compatibility profile "+llamaB580Profile+": "+detail)
	return nil
}

func llamaB580Selected(devices string, args []string, env map[string]string) (bool, error) {
	rows := llamaB580Device.FindAllStringSubmatch(devices, -1)
	if len(rows) == 0 {
		return false, nil
	}
	layers, explicitLayers, err := llamaCompatibilityOption(args, env, "LLAMA_ARG_N_GPU_LAYERS", "--gpu-layers", "--n-gpu-layers", "-ngl")
	if err != nil {
		return false, err
	}
	if n, parseErr := strconv.Atoi(strings.TrimSpace(layers)); explicitLayers && parseErr == nil && n == 0 {
		return false, nil
	}
	selection, explicit, err := llamaCompatibilityOption(args, env, "LLAMA_ARG_DEVICE", "--device", "-dev")
	if err != nil {
		return false, err
	}
	if !explicit || selection == "" {
		return true, nil // Vendor automatic selection can use the enumerated B580.
	}
	for _, device := range strings.Split(selection, ",") {
		for _, row := range rows {
			if device == row[1] {
				return true, nil
			}
		}
	}
	return false, nil
}

func applyLlamaB580Defaults(args []string, env map[string]string) error {
	// Model presets can override router defaults for individual children. Keep
	// their contents private and require explicit reconciliation before serving.
	if preset, _, err := llamaCompatibilityOption(args, env, "LLAMA_ARG_MODELS_PRESET", "--models-preset"); err != nil {
		return err
	} else if preset != "" {
		return fmt.Errorf("llama B580 compatibility cannot verify per-model preset options; remove --models-preset / LLAMA_ARG_MODELS_PRESET and use runtime options with flash attention off, then retry Start")
	}
	defaults := map[string]string{
		"GGML_VK_DISABLE_COOPMAT": "1", "GGML_VK_DISABLE_COOPMAT2": "1",
		"GGML_VK_DISABLE_INTEGER_DOT_PRODUCT": "1", "GGML_VK_DISABLE_F16": "1",
		"GGML_VK_DISABLE_BFLOAT16": "1", "LLAMA_ARG_FLASH_ATTN": "off",
	}
	// Validate everything before changing this process's environment. Windows
	// names are case-insensitive; manifest options override inherited options.
	missing := map[string]string{}
	for key, want := range defaults {
		var flags []string
		if key == "LLAMA_ARG_FLASH_ATTN" {
			flags = []string{"--flash-attn", "-fa"}
		}
		value, present, err := llamaCompatibilityOption(args, env, key, flags...)
		if err != nil {
			return err
		}
		if present && value != want {
			return fmt.Errorf("llama B580 compatibility requires %s=%s; remove or change the explicit option and retry Start", key, want)
		}
		if !present {
			missing[key] = want
		}
	}
	for key, value := range missing {
		env[key] = value
	}
	return nil
}

// Match the existing child environment precedence, then the vendor's CLI
// precedence. Conflicting Windows spellings in an unordered map are ambiguous.
func llamaCompatibilityOption(args []string, env map[string]string, key string, flags ...string) (string, bool, error) {
	// The five Vulkan disable variables use getenv presence, not Boolean values.
	// Preserve existing values (including empty and "0") without changing behavior.
	if len(flags) == 0 {
		if _, present := os.LookupEnv(key); present {
			return "1", true, nil
		}
		for name := range env {
			if strings.EqualFold(name, key) {
				return "1", true, nil
			}
		}
		return "", false, nil
	}
	value, present := os.LookupEnv(key)
	canonical := func(value string) string {
		if key == "LLAMA_ARG_FLASH_ATTN" && (value == "0" || value == "false" || value == "disabled") {
			return "off"
		}
		return value
	}
	value = canonical(value)
	found := false
	for k, v := range env {
		if strings.EqualFold(k, key) {
			v = canonical(v)
			if found && v != value {
				return "", false, fmt.Errorf("llama compatibility: conflicting spellings of %s in runtime.env; keep one value and retry Start", key)
			}
			value, present, found = v, true, true
		}
	}
	for i, arg := range args {
		name, v, inline := strings.Cut(arg, "=")
		for _, flag := range flags {
			if name != flag {
				continue
			}
			if !inline {
				negativeLayer := false
				if key == "LLAMA_ARG_N_GPU_LAYERS" && i+1 < len(args) {
					_, parseErr := strconv.Atoi(args[i+1])
					negativeLayer = parseErr == nil
				}
				if i+1 == len(args) || (strings.HasPrefix(args[i+1], "-") && !negativeLayer) {
					return "", false, fmt.Errorf("llama compatibility: %s needs an explicit value; correct runtime.args and retry Start", flag)
				}
				v = args[i+1]
			}
			value, present = canonical(v), true
		}
	}
	return value, present, nil
}
