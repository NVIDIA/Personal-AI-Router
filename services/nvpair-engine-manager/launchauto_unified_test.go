// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// DGX Spark (GB10) fixture: about 121.69 GiB of shared memory, with 41.7 GiB
// available. Native telemetry reports "[N/A]", so the runtime probe must confirm
// shared memory before using the system's readings.
const (
	sparkTotal = int64(127600524) * 1024
	sparkAvail = int64(43684000) * 1024
)

const llama8BFP8Config = `{
  "max_position_embeddings": 131072,
  "num_hidden_layers": 32,
  "num_attention_heads": 32,
  "num_key_value_heads": 8,
  "head_dim": 128,
  "torch_dtype": "bfloat16",
  "rope_scaling": {"rope_type": "llama3", "factor": 8.0}
}`

// The runtime confirms shared memory after native memory telemetry is missing.
func withUnifiedMemoryGPU(t *testing.T, total, available int64) {
	t.Helper()
	withDeviceMemoryProviders(t,
		func() (int64, int64, bool) { return gpuMemoryFromQuery("[N/A], [N/A]\n") },
		func(string) (deviceMemoryProbe, error) {
			return deviceMemoryProbe{Total: total, Free: available, Shared: true}, nil
		},
	)
	old := deviceMemory
	deviceMemory = detectDeviceMemory
	t.Cleanup(func() { deviceMemory = old })
}

func TestGPUMemoryFromQuery_ReadsADiscreteGPU(t *testing.T) {
	total, free, ok := gpuMemoryFromQuery("49140, 40000\n")

	require.True(t, ok)
	assert.Equal(t, int64(49140*1024*1024), total)
	assert.Equal(t, int64(40000*1024*1024), free)
}

func TestGPUMemoryFromQuery_RejectsMissingValuesWithoutAssumingSharedMemory(t *testing.T) {
	test := func(name, output string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			total, free, ok := gpuMemoryFromQuery(output)

			assert.False(t, ok)
			assert.Zero(t, total)
			assert.Zero(t, free)
		})
	}
	test("bracketed N/A", "[N/A], [N/A]\n")
	test("bare N/A", "N/A, N/A\n")
	test("not supported", "[Not Supported], [Not Supported]\n")
}

func TestGPUMemoryFromQuery_IsUnavailableWhenOnlyOneFigureIsMissing(t *testing.T) {
	_, _, ok := gpuMemoryFromQuery("[N/A], 1000\n")

	assert.False(t, ok)
}

func TestDetectDeviceMemory_ReportsNoFreeMemoryBelowSharedStartupHeadroom(t *testing.T) {
	withUnifiedMemoryGPU(t, sparkTotal, gib)

	_, free, ok := detectDeviceMemory("vllm")

	require.True(t, ok)
	assert.Zero(t, free)
}

func TestPlanHFContext_KeepsACachedModelWithinTheSharedMemoryBudget(t *testing.T) {
	withUnifiedMemoryGPU(t, sparkTotal, sparkAvail)
	cache := cachedModel(t, "nvidia/Llama-3.1-8B-Instruct-FP8", llama8BFP8Config, 9_100_000_000)

	extra, _ := planHFContext([]string{"serve", "--model", "nvidia/Llama-3.1-8B-Instruct-FP8"}, cache, 65536, nil, nil, "")

	// Leave 3 GiB for startup and 1 GiB for the desktop and other engines.
	assert.LessOrEqual(t, utilOf(t, extra)*float64(sparkTotal), float64(sparkAvail-4*gib))
	assert.Equal(t, "65536", flagOf(t, extra, "--max-model-len"), "the full context fits in available memory")
}

func TestPlanHFContext_CapsAnUncachedModelAtTheSharedMemoryBudget(t *testing.T) {
	withUnifiedMemoryGPU(t, sparkTotal, sparkAvail)

	extra, _ := planHFContext([]string{"serve", "--model", "nvidia/Llama-3.1-8B-Instruct-FP8"}, t.TempDir(), 65536, nil, nil, "")

	// Without model metadata, the launch can use only available memory less
	// the 3 GiB startup reserve and 1 GiB margin.
	assert.LessOrEqual(t, utilOf(t, extra)*float64(sparkTotal), float64(sparkAvail-4*gib))
}

func TestPlanHFContext_LeavesTheShareToVLLMWhenAUnifiedMemoryMachineHasNothingFree(t *testing.T) {
	withUnifiedMemoryGPU(t, sparkTotal, gib)

	extra, _ := planHFContext([]string{"serve", "--model", "nvidia/Llama-3.1-8B-Instruct-FP8"}, t.TempDir(), 65536, nil, nil, "")

	assert.Empty(t, flagOf(t, extra, "--gpu-memory-utilization"))
}
