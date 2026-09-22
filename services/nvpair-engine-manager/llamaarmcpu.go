// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Static PNP identities survive an unbound/broken GPU driver. Do not classify
// hardware by a failed CUDA runtime probe or by the absence of working devices.
const llamaARMInventoryCommand = `$ErrorActionPreference='Stop'; $cpus=@(Get-CimInstance Win32_Processor -ErrorAction Stop | Select-Object Architecture,Manufacturer); $devices=@(Get-CimInstance Win32_PnPEntity -ErrorAction Stop); $nv=@($devices | Where-Object { $_.PNPDeviceID -match 'VEN_10DE' -or ($_.HardwareID -join ' ') -match 'VEN_10DE' -or $_.Manufacturer -match 'NVIDIA' }); @{cpus=$cpus;device_count=$devices.Count;nvidia_hardware=($nv.Count -gt 0)} | ConvertTo-Json -Depth 4 -Compress`

type llamaARMInventory struct {
	CPUs []struct {
		Architecture *int   `json:"Architecture"`
		Manufacturer string `json:"Manufacturer"`
	} `json:"cpus"`
	DeviceCount    int   `json:"device_count"`
	NVIDIAHardware *bool `json:"nvidia_hardware"`
}

func llamaARMRequiresCUDA(data []byte) (bool, error) {
	var inventory llamaARMInventory
	if err := json.Unmarshal(data, &inventory); err != nil {
		return false, fmt.Errorf("read Windows ARM hardware policy: %w", err)
	}
	if len(inventory.CPUs) == 0 || inventory.DeviceCount <= 0 || inventory.NVIDIAHardware == nil {
		return false, errors.New("Windows ARM hardware inventory incomplete; refusing to infer CPU-only support")
	}
	cuda := *inventory.NVIDIAHardware
	for _, cpu := range inventory.CPUs {
		if cpu.Architecture == nil || *cpu.Architecture != 12 || strings.TrimSpace(cpu.Manufacturer) == "" {
			return false, errors.New("Windows ARM CPU identity unavailable; refusing to infer CPU-only support")
		}
		cuda = cuda || strings.Contains(strings.ToLower(cpu.Manufacturer), "nvidia")
	}
	return cuda, nil
}

func (e *Executor) prepareLlamaWindowsARM(ctx context.Context, st *engineState, stage string) (string, map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	// Legacy test/custom recipes without CPUFetch stay CUDA-required.
	if st.plat.Install.CPUFetch == nil {
		return e.prepareLlamaUpstream(ctx, st, stage)
	}
	queryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	env, err := childEnv(st)
	if err != nil {
		return "", nil, err
	}
	query := e.armHardwareQuery
	if query == nil {
		query = func(ctx context.Context, env map[string]string) (string, error) {
			return e.runCommandOutput(ctx, []string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command", llamaARMInventoryCommand}, env)
		}
	}
	raw, err := query(queryCtx, env)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		return "", nil, fmt.Errorf("Windows ARM hardware query failed; CPU fallback was not selected: %w", err)
	}
	cuda, err := llamaARMRequiresCUDA([]byte(raw))
	if err != nil {
		return "", nil, err
	}
	if cuda {
		return e.prepareLlamaUpstream(ctx, st, stage)
	}
	return e.prepareLlamaARMCPU(ctx, st, stage)
}

func (e *Executor) prepareLlamaARMCPU(ctx context.Context, st *engineState, stage string) (string, map[string]any, error) {
	fetch := st.plat.Install.CPUFetch
	if fetch == nil || fetch.SHA256 == "" {
		return "", nil, errors.New("Windows ARM CPU installation requires its pinned official installer")
	}
	home := filepath.Join(stage, "cpu")
	if err := os.MkdirAll(home, 0700); err != nil {
		return "", nil, err
	}
	env, err := llamaInstallerEnv(st, home)
	if err != nil {
		return "", nil, err
	}
	env["SKIP_CUDA"], env["SKIP_VULKAN"] = "1", "1"
	script, err := e.downloadLimited(ctx, "llamacpp", fetch, maxLlamaInstallerBytes)
	if err != nil {
		return "", nil, err
	}
	defer os.Remove(script)
	e.emitInstallProgress("llamacpp", "installing", -1)
	if err := e.runCommand(ctx, llamaInstallerArgs("windows", script), env); err != nil {
		return "", nil, fmt.Errorf("official Windows ARM CPU installer: %w", err)
	}
	candidate := filepath.Join(home, "llama-app")
	identity, licenses, err := e.validateLlamaApp(ctx, st, candidate, 10826, false)
	if err != nil {
		return "", nil, err
	}
	identity["source"], identity["acceleration_policy"] = "official-pinned-cpu", "cpu"
	identity["installer_url"], identity["installer_sha256"] = fetch.URL, fetch.SHA256
	if err := os.WriteFile(filepath.Join(candidate, "THIRD-PARTY-LICENSES.txt"), []byte(licenses), 0600); err != nil {
		return "", nil, err
	}
	return candidate, identity, ctx.Err()
}
