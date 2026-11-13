// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func withDeviceMemoryProviders(t *testing.T, native func() (int64, int64, bool), probe func(string) (deviceMemoryProbe, error)) {
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
		func() (int64, int64, bool) { return 48 * gib, 32 * gib, true },
		func(string) (deviceMemoryProbe, error) {
			t.Fatal("the vLLM probe must not run when native telemetry works")
			return deviceMemoryProbe{}, nil
		},
	)

	total, free, ok := detectDeviceMemory("/opt/vllm/bin/vllm")

	require.True(t, ok)
	assert.Equal(t, 48*gib, total)
	assert.Equal(t, 32*gib, free)
}

func TestDetectDeviceMemory_FallsBackToTheSelectedVLLMBackend(t *testing.T) {
	withDeviceMemoryProviders(t,
		func() (int64, int64, bool) { return 0, 0, false },
		func(bin string) (deviceMemoryProbe, error) {
			require.Equal(t, "/opt/vllm/bin/vllm", bin)
			return deviceMemoryProbe{Total: 64 * gib, Free: 40 * gib}, nil
		},
	)

	total, free, ok := detectDeviceMemory("/opt/vllm/bin/vllm")

	require.True(t, ok)
	assert.Equal(t, 64*gib, total)
	assert.Equal(t, 40*gib, free)
}

func TestDetectDeviceMemory_UsesVRAMWhenNvidiaSMIReportsNA(t *testing.T) {
	withDeviceMemoryProviders(t,
		func() (int64, int64, bool) { return gpuMemoryFromQuery("[N/A], [N/A]\n") },
		func(string) (deviceMemoryProbe, error) {
			return deviceMemoryProbe{Total: 16 * gib, Free: 4 * gib, Shared: false}, nil
		},
	)

	total, free, ok := detectDeviceMemory("vllm")

	require.True(t, ok)
	assert.Equal(t, 16*gib, total)
	assert.Equal(t, 4*gib, free)
}

func TestDetectDeviceMemory_ReservesStartupHeadroomOnSharedMemory(t *testing.T) {
	withDeviceMemoryProviders(t,
		func() (int64, int64, bool) { return 0, 0, false },
		func(string) (deviceMemoryProbe, error) {
			return deviceMemoryProbe{Total: 64 * gib, Free: 20 * gib, Shared: true}, nil
		},
	)

	_, free, ok := detectDeviceMemory("vllm")

	require.True(t, ok)
	assert.Equal(t, 17*gib, free, "leave 3 GiB of the available 20 GiB for startup")
}

func TestDetectDeviceMemory_LeavesSizingToVLLMWhenEveryProviderFails(t *testing.T) {
	withDeviceMemoryProviders(t,
		func() (int64, int64, bool) { return 0, 0, false },
		func(string) (deviceMemoryProbe, error) { return deviceMemoryProbe{}, errors.New("unsupported backend") },
	)

	_, _, ok := detectDeviceMemory("vllm")

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
