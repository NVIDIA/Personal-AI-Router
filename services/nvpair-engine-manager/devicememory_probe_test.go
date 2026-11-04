// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Run the embedded production probe with device APIs supplied by the fixture.
// Discrete and unknown devices deliberately reject any attempt to read host RAM.
const deviceMemoryProbeHarness = `
import json
import sys
import types

sys.excepthook = sys.__excepthook__
fixture = json.load(sys.stdin)
gib = 1 << 30
backend = fixture.get("backend", "unknown")
total = fixture.get("total_gib", 16) * gib
free = fixture.get("free_gib", 4) * gib

def system_memory():
    if not fixture.get("allow_system", False):
        raise RuntimeError("system RAM must not be read for this device")
    return types.SimpleNamespace(total=fixture.get("system_total_gib", 16) * gib,
                                 available=fixture.get("system_free_gib", 12) * gib)

def device_memory(device):
    if fixture.get("device_memory_error", False):
        raise RuntimeError("device memory unavailable")
    return free, total

cuda_properties = types.SimpleNamespace(total_memory=total)
xpu_properties = types.SimpleNamespace(total_memory=total)
if not fixture.get("unknown_topology", False):
    cuda_properties.is_integrated = fixture.get("integrated", False)
    xpu_properties.is_integrated_gpu = fixture.get("integrated", False)

torch = types.SimpleNamespace(
    cuda=types.SimpleNamespace(is_available=lambda: backend in ("cuda", "rocm"),
        mem_get_info=device_memory, get_device_properties=lambda device: cuda_properties),
    xpu=types.SimpleNamespace(is_available=lambda: backend == "xpu",
        mem_get_info=device_memory, get_device_properties=lambda device: xpu_properties),
    backends=types.SimpleNamespace(mps=types.SimpleNamespace(
        is_available=lambda: fixture.get("mps_available", False))))
platform = types.SimpleNamespace(get_device_total_memory=lambda device: total,
                                is_cpu=lambda: backend == "cpu")
if fixture.get("has_device_free", False):
    platform.get_device_available_memory = lambda device: free
sys.modules["torch"] = torch
sys.modules["psutil"] = types.SimpleNamespace(virtual_memory=system_memory)
sys.modules["platform"] = types.SimpleNamespace(machine=lambda: fixture.get("arch", "x86_64"))
sys.modules["vllm.platforms"] = types.SimpleNamespace(current_platform=platform)
sys.platform = fixture.get("os", "linux")
`

func TestDeviceMemoryProbe_UsesSystemRAMOnlyForVerifiedSharedDevices(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is required to exercise the embedded vLLM memory probe")
	}
	accept := func(name, input string, want deviceMemoryProbe) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(python, "-I", "-c", deviceMemoryProbeHarness+"\n"+deviceMemoryProbeScript)
			cmd.Stdin = strings.NewReader(input)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
			var result deviceMemoryProbe
			require.NoError(t, json.Unmarshal(output, &result))
			assert.Equal(t, want, result)
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
	for _, tc := range []struct{ name, input, message string }{
		{"unknown device matching host RAM", `{}`, "the selected vLLM backend does not report free memory"},
		{"Intel Mac with Metal is not proof of shared RAM", `{"os":"darwin","arch":"x86_64","mps_available":true}`, "the selected vLLM backend does not report free memory"},
		{"Apple Silicon without Metal available", `{"os":"darwin","arch":"arm64"}`, "the selected vLLM backend does not report free memory"},
		{"discrete CUDA query failure", `{"backend":"cuda","device_memory_error":true}`, "device memory unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(python, "-I", "-c", deviceMemoryProbeHarness+"\n"+deviceMemoryProbeScript)
			cmd.Stdin = strings.NewReader(tc.input)
			output, err := cmd.CombinedOutput()
			require.Error(t, err)
			assert.Contains(t, string(output), tc.message)
		})
	}
}
