// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withDeviceMemoryProviders(t *testing.T, native func(context.Context) (int64, int64, bool), probe func(context.Context, string, map[string]string) (deviceMemoryProbe, error)) {
	t.Helper()
	oldNative := nativeDeviceMemory
	oldProbe := probeVLLMDeviceMemory
	nativeDeviceMemory = native
	probeVLLMDeviceMemory = probe
	t.Cleanup(func() {
		nativeDeviceMemory = oldNative
		probeVLLMDeviceMemory = oldProbe
	})
}

func TestDetectDeviceMemory_PrefersNativeTelemetry(t *testing.T) {
	withDeviceMemoryProviders(t,
		func(context.Context) (int64, int64, bool) { return 48 * gib, 32 * gib, true },
		func(context.Context, string, map[string]string) (deviceMemoryProbe, error) {
			t.Fatal("the vLLM probe must not run when native telemetry works")
			return deviceMemoryProbe{}, nil
		},
	)

	total, free, ok := detectDeviceMemory(t.Context(), "/opt/vllm/bin/vllm", nil)

	require.True(t, ok)
	assert.Equal(t, 48*gib, total)
	assert.Equal(t, 32*gib, free)
}

func TestDetectDeviceMemory_FallsBackToTheSelectedVLLMBackend(t *testing.T) {
	withDeviceMemoryProviders(t,
		func(context.Context) (int64, int64, bool) { return 0, 0, false },
		func(_ context.Context, bin string, _ map[string]string) (deviceMemoryProbe, error) {
			require.Equal(t, "/opt/vllm/bin/vllm", bin)
			return deviceMemoryProbe{Total: 64 * gib, Free: 40 * gib}, nil
		},
	)

	total, free, ok := detectDeviceMemory(t.Context(), "/opt/vllm/bin/vllm", nil)

	require.True(t, ok)
	assert.Equal(t, 64*gib, total)
	assert.Equal(t, 40*gib, free)
}

func TestDetectDeviceMemory_UsesVRAMWhenNvidiaSMIReportsNA(t *testing.T) {
	withDeviceMemoryProviders(t,
		func(context.Context) (int64, int64, bool) { return gpuMemoryFromQuery("[N/A], [N/A]\n") },
		func(context.Context, string, map[string]string) (deviceMemoryProbe, error) {
			return deviceMemoryProbe{Total: 16 * gib, Free: 4 * gib, Shared: false}, nil
		},
	)

	total, free, ok := detectDeviceMemory(t.Context(), "vllm", nil)

	require.True(t, ok)
	assert.Equal(t, 16*gib, total)
	assert.Equal(t, 4*gib, free)
}

func TestDetectDeviceMemory_ReservesStartupHeadroomOnSharedMemory(t *testing.T) {
	withDeviceMemoryProviders(t,
		func(context.Context) (int64, int64, bool) { return 0, 0, false },
		func(context.Context, string, map[string]string) (deviceMemoryProbe, error) {
			return deviceMemoryProbe{Total: 64 * gib, Free: 20 * gib, Shared: true}, nil
		},
	)

	_, free, ok := detectDeviceMemory(t.Context(), "vllm", nil)

	require.True(t, ok)
	assert.Equal(t, 17*gib, free, "leave 3 GiB of the available 20 GiB for startup")
}

func TestDetectDeviceMemory_LeavesSizingToVLLMWhenEveryProviderFails(t *testing.T) {
	withDeviceMemoryProviders(t,
		func(context.Context) (int64, int64, bool) { return 0, 0, false },
		func(context.Context, string, map[string]string) (deviceMemoryProbe, error) {
			return deviceMemoryProbe{}, errors.New("unsupported backend")
		},
	)

	_, _, ok := detectDeviceMemory(t.Context(), "vllm", nil)

	assert.False(t, ok)
}

func TestParseDeviceMemoryProbe_ReadsTheLastOutputLine(t *testing.T) {
	reading, err := parseDeviceMemoryProbe([]byte("vLLM startup log\n{\"total\":68719476736,\"free\":42949672960,\"shared\":true}\n"))

	require.NoError(t, err)
	assert.Equal(t, deviceMemoryProbe{Total: 64 * gib, Free: 40 * gib, Shared: true}, reading)
}

func TestParseDeviceMemoryProbe_RejectsImpossibleReadings(t *testing.T) {
	test := func(name, output string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			_, err := parseDeviceMemoryProbe([]byte(output))

			assert.Error(t, err)
		})
	}
	test("empty output", "")
	test("non-JSON output", "not json")
	test("zero total memory", `{"total":0,"free":0}`)
	test("free memory exceeds total memory", `{"total":10,"free":11}`)
}

// The runtime confirms shared memory after native memory telemetry is missing.
func withUnifiedMemoryGPU(t *testing.T, total, available int64) {
	t.Helper()
	withDeviceMemoryProviders(t,
		func(context.Context) (int64, int64, bool) { return gpuMemoryFromQuery("[N/A], [N/A]\n") },
		func(context.Context, string, map[string]string) (deviceMemoryProbe, error) {
			return deviceMemoryProbe{Total: total, Free: available, Shared: true}, nil
		},
	)
	old := deviceMemory
	deviceMemory = detectDeviceMemory
	t.Cleanup(func() { deviceMemory = old })
}

func TestDetectDeviceMemory_ReportsNoFreeMemoryBelowSharedStartupHeadroom(t *testing.T) {
	withUnifiedMemoryGPU(t, sparkTotal, gib)

	_, free, ok := detectDeviceMemory(t.Context(), "vllm", nil)

	require.True(t, ok)
	assert.Zero(t, free)
}

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
