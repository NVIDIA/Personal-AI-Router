// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	_ "embed"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deviceMemoryProbeHarness supplies fake device APIs for the production probe.
// Each case sends a JSON fixture on stdin to a fresh Python process, which runs
// the harness followed by deviceMemoryProbeScript. The probe's imports resolve
// to the harness's fake modules, so no GPU, PyTorch, or vLLM install is needed.
// These tests check which memory source the probe uses and the values it returns.
// They do not check real library compatibility, the engine environment passed by
// the Go launcher, or cancellation of the production probe. The fixture file
// documents the input fields and rejects unexpected reads of system RAM.
//
//go:embed testdata/devicememory_probe_harness.py
var deviceMemoryProbeHarness string

// runDeviceMemoryProbeFixture gives each case a fresh Python process so its fake
// modules and platform settings cannot affect another case.
func runDeviceMemoryProbeFixture(python, input string) ([]byte, error) {
	cmd := exec.Command(python, "-I", "-c", deviceMemoryProbeHarness+"\n"+deviceMemoryProbeScript)
	cmd.Stdin = strings.NewReader(input)
	return cmd.CombinedOutput()
}

func TestDeviceMemoryProbe_UsesSystemRAMOnlyForVerifiedSharedDevices(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is required to exercise the embedded vLLM memory probe")
	}
	accept := func(name, input string, expected deviceMemoryProbe) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			output, err := runDeviceMemoryProbeFixture(python, input)
			require.NoError(t, err, "%s", output)
			var result deviceMemoryProbe
			require.NoError(t, json.Unmarshal(output, &result))
			assert.Equal(t, expected, result)
		})
	}
	accept("discrete CUDA with the same capacity as host RAM", `{"backend":"cuda"}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 4 * gib})
	accept("discrete ROCm with the same capacity as host RAM", `{"backend":"rocm"}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 4 * gib})
	accept("CUDA with unknown topology keeps device readings", `{"backend":"cuda","unknown_topology":true}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 4 * gib})
	accept("verified integrated CUDA uses available host RAM", `{"backend":"cuda","integrated":true,"allow_system":true}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 12 * gib, Shared: true})
	accept("integrated CUDA respects the device memory limit", `{"backend":"cuda","integrated":true,"allow_system":true,"total_gib":8}`,
		deviceMemoryProbe{Total: 8 * gib, Free: 8 * gib, Shared: true})
	accept("discrete XPU keeps device readings", `{"backend":"xpu"}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 4 * gib})
	accept("verified integrated XPU uses available host RAM", `{"backend":"xpu","integrated":true,"allow_system":true}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 12 * gib, Shared: true})
	accept("Apple Silicon Metal without a free-memory API", `{"backend":"metal","os":"darwin","arch":"arm64","mps_available":true,"allow_system":true}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 12 * gib, Shared: true})
	accept("Apple Silicon preserves the engine available-memory reading", `{"backend":"metal","os":"darwin","arch":"arm64","mps_available":true,"has_device_free":true}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 4 * gib, Shared: true})
	accept("CPU explicitly uses system RAM", `{"backend":"cpu","allow_system":true}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 12 * gib, Shared: true})
	accept("unknown engine with its own free-memory API", `{"has_device_free":true}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 4 * gib})
}

func TestDeviceMemoryProbe_RejectsUnavailableMemoryWithoutGuessingTopology(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is required to exercise the embedded vLLM memory probe")
	}
	reject := func(name, input, expectedMessage string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			output, err := runDeviceMemoryProbeFixture(python, input)
			require.Error(t, err)
			assert.Contains(t, string(output), expectedMessage)
		})
	}
	reject("unknown device matching host RAM", `{}`,
		"the selected vLLM backend does not report free memory")
	reject("Intel Mac with Metal is not proof of shared RAM", `{"os":"darwin","arch":"x86_64","mps_available":true}`,
		"the selected vLLM backend does not report free memory")
	reject("Apple Silicon without Metal available", `{"os":"darwin","arch":"arm64"}`,
		"the selected vLLM backend does not report free memory")
	reject("discrete CUDA query failure", `{"backend":"cuda","device_memory_error":true}`,
		"device memory unavailable")
	reject("multiple CUDA devices", `{"backend":"cuda","device_count":2}`,
		"multiple CUDA devices require engine-owned memory sizing")
}
