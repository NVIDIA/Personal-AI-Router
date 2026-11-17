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

	"nvpair-shared/pythontest"
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

	reading, ok := detectDeviceMemory(t.Context(), "/opt/vllm/bin/vllm", nil)

	require.True(t, ok)
	assert.Equal(t, 48*gib, reading.Total)
	assert.Equal(t, 32*gib, reading.Free)
}

func TestDetectDeviceMemory_FallsBackToTheSelectedVLLMBackend(t *testing.T) {
	withDeviceMemoryProviders(t,
		func(context.Context) (int64, int64, bool) { return 0, 0, false },
		func(_ context.Context, bin string, _ map[string]string) (deviceMemoryProbe, error) {
			require.Equal(t, "/opt/vllm/bin/vllm", bin)
			return deviceMemoryProbe{Total: 64 * gib, Free: 40 * gib, Runtime: "metal_mlx"}, nil
		},
	)

	reading, ok := detectDeviceMemory(t.Context(), "/opt/vllm/bin/vllm", nil)

	require.True(t, ok)
	assert.Equal(t, 64*gib, reading.Total)
	assert.Equal(t, 40*gib, reading.Free)
	assert.Equal(t, "metal_mlx", reading.Runtime)
}

func TestDetectDeviceMemory_UsesVRAMWhenNvidiaSMIReportsNA(t *testing.T) {
	withDeviceMemoryProviders(t,
		func(context.Context) (int64, int64, bool) { return gpuMemoryFromQuery("[N/A], [N/A]\n") },
		func(context.Context, string, map[string]string) (deviceMemoryProbe, error) {
			return deviceMemoryProbe{Total: 16 * gib, Free: 4 * gib, Shared: false}, nil
		},
	)

	reading, ok := detectDeviceMemory(t.Context(), "vllm", nil)

	require.True(t, ok)
	assert.Equal(t, 16*gib, reading.Total)
	assert.Equal(t, 4*gib, reading.Free)
}

func TestDetectDeviceMemory_ReservesStartupHeadroomOnSharedMemory(t *testing.T) {
	withDeviceMemoryProviders(t,
		func(context.Context) (int64, int64, bool) { return 0, 0, false },
		func(context.Context, string, map[string]string) (deviceMemoryProbe, error) {
			return deviceMemoryProbe{Total: 64 * gib, Free: 20 * gib, Shared: true}, nil
		},
	)

	reading, ok := detectDeviceMemory(t.Context(), "vllm", nil)

	require.True(t, ok)
	assert.Equal(t, 17*gib, reading.Free, "leave 3 GiB of the available 20 GiB for startup")
}

func TestDetectDeviceMemory_LeavesSizingToVLLMWhenEveryProviderFails(t *testing.T) {
	withDeviceMemoryProviders(t,
		func(context.Context) (int64, int64, bool) { return 0, 0, false },
		func(context.Context, string, map[string]string) (deviceMemoryProbe, error) {
			return deviceMemoryProbe{}, errors.New("unsupported backend")
		},
	)

	_, ok := detectDeviceMemory(t.Context(), "vllm", nil)

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

	reading, ok := detectDeviceMemory(t.Context(), "vllm", nil)

	require.True(t, ok)
	assert.Zero(t, reading.Free)
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
	python := pythontest.Python3(t)
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
		deviceMemoryProbe{Total: 16 * gib, Free: 12 * gib, Shared: true, Runtime: "metal_mlx"})
	accept("Apple Silicon preserves the engine available-memory reading", `{"backend":"metal","os":"darwin","arch":"arm64","mps_available":true,"has_device_free":true}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 4 * gib, Shared: true, Runtime: "metal_mlx"})
	accept("Metal MLX uses its working-set limit instead of physical RAM",
		`{"backend":"metal","os":"darwin","arch":"arm64","mps_available":true,"total_gib":48,"metal_limit_gib":36,"free_gib":40,"has_device_free":true}`,
		deviceMemoryProbe{Total: 36 * gib, Free: 36 * gib, Shared: true, Runtime: "metal_mlx"})
	accept("Metal MLX caps host availability at its working-set limit",
		`{"backend":"metal","os":"darwin","arch":"arm64","mps_available":true,"total_gib":48,"metal_limit_gib":36,"system_free_gib":40,"allow_system":true}`,
		deviceMemoryProbe{Total: 36 * gib, Free: 36 * gib, Shared: true, Runtime: "metal_mlx"})
	accept("Metal MPS uses physical RAM without an MLX limit",
		`{"backend":"metal","os":"darwin","arch":"arm64","mps_available":true,"metal_backend":"mps","total_gib":48,"metal_limit_gib":0,"free_gib":40,"has_device_free":true}`,
		deviceMemoryProbe{Total: 48 * gib, Free: 40 * gib, Shared: true, Runtime: "metal_mps"})
	accept("another Apple Silicon platform retains its own memory base",
		`{"os":"darwin","arch":"arm64","mps_available":true,"total_gib":48,"metal_limit_gib":36,"free_gib":40,"has_device_free":true}`,
		deviceMemoryProbe{Total: 48 * gib, Free: 40 * gib, Shared: true})
	accept("CPU explicitly uses system RAM", `{"backend":"cpu","allow_system":true}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 12 * gib, Shared: true})
	accept("CPU uses its selected memory node and limits instead of host RAM",
		`{"backend":"cpu","cpu_nodes":[3],"cpu_total_gib":8,"cpu_available_gib":2,"system_total_gib":64,"system_free_gib":48,"allow_system":true}`,
		deviceMemoryProbe{Total: 8 * gib, Free: 2 * gib, Shared: true})
	accept("CPU preserves the platform available-memory API",
		`{"backend":"cpu","has_device_free":true}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 4 * gib, Shared: true})
	accept("unknown engine with its own free-memory API", `{"has_device_free":true}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 4 * gib})
}

func TestDeviceMemoryProbe_UsesTheSelectedPlatformWhenOtherAcceleratorsAreAvailable(t *testing.T) {
	python := pythontest.Python3(t)
	test := func(name, input string, expected deviceMemoryProbe) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			// Fail any accelerator-memory query to prove the selected platform wins.
			output, err := runDeviceMemoryProbeFixture(python, input)
			require.NoError(t, err, "%s", output)
			var result deviceMemoryProbe
			require.NoError(t, json.Unmarshal(output, &result))
			assert.Equal(t, expected, result)
		})
	}
	test("CPU selected with CUDA available",
		`{"backend":"cpu","cuda_available":true,"allow_system":true,"device_memory_error":true}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 12 * gib, Shared: true})
	test("CPU selected with XPU available",
		`{"backend":"cpu","xpu_available":true,"allow_system":true,"device_memory_error":true}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 12 * gib, Shared: true})
	test("plugin selected with CUDA available",
		`{"cuda_available":true,"has_device_free":true,"device_memory_error":true}`,
		deviceMemoryProbe{Total: 16 * gib, Free: 4 * gib})
}

func TestDeviceMemoryProbe_RejectsUnavailableMemoryWithoutGuessingTopology(t *testing.T) {
	python := pythontest.Python3(t)
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
	reject("multiple XPU devices", `{"backend":"xpu","device_count":2}`,
		"multiple XPU devices require engine-owned memory sizing")
	reject("multiple CPU memory nodes", `{"backend":"cpu","cpu_nodes":[0,1]}`,
		"CPU memory sizing requires one visible memory node")
	reject("no CPU memory nodes", `{"backend":"cpu","cpu_nodes":[]}`,
		"CPU memory sizing requires one visible memory node")
	reject("Metal MLX with an unavailable working-set limit", `{"backend":"metal","os":"darwin","arch":"arm64","mps_available":true,"metal_limit_gib":0,"has_device_free":true}`,
		"Metal runtime did not report a usable memory limit")
}
