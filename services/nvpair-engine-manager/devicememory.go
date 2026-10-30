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

var nativeDeviceMemory = nvidiaSMIMemory
var probeVLLMDeviceMemory = runVLLMDeviceMemoryProbe

func detectDeviceMemory(bin string) (total, free int64, ok bool) {
	if total, free, ok := nativeDeviceMemory(); ok {
		return total, free, true
	}
	reading, err := probeVLLMDeviceMemory(bin)
	if err != nil {
		slog.Warn("could not measure free device memory; leaving memory sizing to vLLM", "err", err)
		return 0, 0, false
	}
	if reading.Shared {
		reading.Free = max(reading.Free-unifiedMemoryHeadroom, 0)
	}
	return reading.Total, reading.Free, true
}

func runVLLMDeviceMemoryProbe(bin string) (deviceMemoryProbe, error) {
	python, err := vllmPython(bin)
	if err != nil {
		return deviceMemoryProbe{}, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), deviceMemoryProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-c", deviceMemoryProbeScript)
	cmd.Dir = os.TempDir()
	out, err := cmd.Output()
	if err != nil {
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
