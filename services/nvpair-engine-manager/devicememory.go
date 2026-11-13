// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"time"
)

//go:embed devicememory_probe.py
var deviceMemoryProbeScript string

const deviceMemoryProbeTimeout = time.Minute

type deviceMemoryProbe struct {
	Total  int64 `json:"total"`
	Free   int64 `json:"free"`
	Shared bool  `json:"shared"`
}

var nativeDeviceMemory = func(ctx context.Context) (int64, int64, bool) { return nvidiaSMIMemory(ctx, nil) }
var probeVLLMDeviceMemory = runVLLMDeviceMemoryProbe

func detectDeviceMemory(ctx context.Context, bin string, env map[string]string) (total, free int64, ok bool) {
	if ctx.Err() != nil {
		return 0, 0, false
	}
	total, free, ok = nativeDeviceMemory(ctx)
	if ctx.Err() != nil {
		return 0, 0, false
	}
	if ok {
		return total, free, ok
	}
	reading, err := probeVLLMDeviceMemory(ctx, bin, env)
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("could not measure free device memory; leaving memory sizing to vLLM", "err", err)
		}
		return 0, 0, false
	}
	if reading.Shared {
		reading.Free = max(reading.Free-unifiedMemoryHeadroom, 0)
	}
	return reading.Total, reading.Free, true
}

// runVLLMDeviceMemoryProbe uses the engine's environment so device and platform
// selection match the launch. It stops when its start is cancelled, even if
// Python or the device runtime is still initializing. Its timeout bounds the wait.
func runVLLMDeviceMemoryProbe(ctx context.Context, bin string, env map[string]string) (deviceMemoryProbe, error) {
	python, err := vllmPython(bin)
	if err != nil {
		return deviceMemoryProbe{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, deviceMemoryProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-c", deviceMemoryProbeScript)
	cmd.Dir = os.TempDir()
	cmd.Env = processEnvironment(bin, env)
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return deviceMemoryProbe{}, fmt.Errorf("device memory probe: %w", err)
	}
	return parseDeviceMemoryProbe(out)
}

func parseDeviceMemoryProbe(out []byte) (deviceMemoryProbe, error) {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	var reading deviceMemoryProbe
	if len(lines) == 0 || json.Unmarshal(lines[len(lines)-1], &reading) != nil {
		return deviceMemoryProbe{}, errors.New("device memory probe printed no result")
	}
	if reading.Total <= 0 || reading.Free < 0 || reading.Free > reading.Total {
		return deviceMemoryProbe{}, errors.New("device memory probe printed an invalid result")
	}
	return reading, nil
}
