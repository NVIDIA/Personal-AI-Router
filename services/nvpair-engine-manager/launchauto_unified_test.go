// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A DGX Spark (GB10) as seen while a model load failed: 121.69 GiB of unified
// memory of which about 41.7 GiB was available. The GPU reports "[N/A]" for its
// own memory, so the system's figures are the only ones there are.
const (
	sparkMemInfo = "MemTotal:       127600524 kB\nMemFree:         43590000 kB\nMemAvailable:    43684000 kB\nBuffers:            30000 kB\n"
	sparkTotal   = int64(127600524) * 1024
	sparkAvail   = int64(43684000) * 1024
)

const llama8BFP8Config = `{"max_position_embeddings":131072,"num_hidden_layers":32,"num_attention_heads":32,"num_key_value_heads":8,"head_dim":128,"torch_dtype":"bfloat16","rope_scaling":{"rope_type":"llama3","factor":8.0}}`

func withSystemMemory(t *testing.T, meminfo string) {
	t.Helper()
	old := meminfoPath
	meminfoPath = filepath.Join(t.TempDir(), "meminfo")
	if err := os.WriteFile(meminfoPath, []byte(meminfo), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { meminfoPath = old })
}

// withUnifiedMemoryGPU makes nvidia-smi answer the way it does on a GPU that
// shares the system's memory, so the whole path from its output to the planned
// launch arguments runs.
func withUnifiedMemoryGPU(t *testing.T, meminfo string) {
	t.Helper()
	withSystemMemory(t, meminfo)
	old := deviceMemory
	deviceMemory = func(string) (int64, int64, bool) { return gpuMemoryFromQuery("[N/A], [N/A]\n") }
	t.Cleanup(func() { deviceMemory = old })
}

func TestGPUMemoryFromQuery_ReadsADiscreteGPU(t *testing.T) {
	total, free, ok := gpuMemoryFromQuery("49140, 40000\n")
	if !ok || total != 49140*1024*1024 || free != 40000*1024*1024 {
		t.Fatalf("got total=%d free=%d ok=%v, want the two MiB figures in bytes", total, free, ok)
	}
}

func TestGPUMemoryFromQuery_UsesSystemMemoryWhenTheGPUReportsNA(t *testing.T) {
	for _, tc := range []struct{ name, output string }{
		{"bracketed N/A, as nvidia-smi prints with nounits", "[N/A], [N/A]\n"},
		{"bare N/A", "N/A, N/A\n"},
		{"Not Supported", "[Not Supported], [Not Supported]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withSystemMemory(t, sparkMemInfo)
			total, free, ok := gpuMemoryFromQuery(tc.output)
			if !ok || total != sparkTotal {
				t.Fatalf("got total=%d ok=%v, want the system's %d", total, ok, sparkTotal)
			}
			if want := sparkAvail - unifiedMemoryHeadroom; free != want {
				t.Fatalf("free = %d, want what is available less vLLM's startup headroom (%d)", free, want)
			}
		})
	}
}

func TestGPUMemoryFromQuery_IsUnavailableWhenOnlyOneFigureIsMissing(t *testing.T) {
	withSystemMemory(t, sparkMemInfo)
	if _, _, ok := gpuMemoryFromQuery("[N/A], 1000\n"); ok {
		t.Fatal("one missing figure is not a unified-memory GPU; the reading is unusable")
	}
}

func TestGPUMemoryFromQuery_IsUnavailableWhenSystemMemoryCannotBeRead(t *testing.T) {
	old := meminfoPath
	meminfoPath = filepath.Join(t.TempDir(), "absent")
	t.Cleanup(func() { meminfoPath = old })
	if _, _, ok := gpuMemoryFromQuery("[N/A], [N/A]\n"); ok {
		t.Fatal("with no system figures there is no reading to size a share from")
	}
}

func TestGPUMemoryFromQuery_ReportsNoFreeMemoryWhenLessThanTheHeadroomIsAvailable(t *testing.T) {
	withSystemMemory(t, "MemTotal: 127600524 kB\nMemAvailable: 1000000 kB\n")
	_, free, ok := gpuMemoryFromQuery("[N/A], [N/A]\n")
	if !ok || free != 0 {
		t.Fatalf("free = %d ok=%v, want 0 rather than a negative figure", free, ok)
	}
}

// The failure behind the report: with about 41 GiB available, the launch asked
// vLLM for nothing, so it wanted its default 0.92 of 121.69 GiB (111.95 GiB) and
// stopped before readiness.
func TestPlanHFContext_CapsTheGPUShareOnAUnifiedMemoryMachine(t *testing.T) {
	withUnifiedMemoryGPU(t, sparkMemInfo)
	cache := cachedModel(t, "nvidia/Llama-3.1-8B-Instruct-FP8", llama8BFP8Config, 9_100_000_000)
	extra, _ := planHFContext([]string{"serve", "--model", "nvidia/Llama-3.1-8B-Instruct-FP8"}, cache, 65536, "")

	util := utilOf(t, extra)
	request := util * float64(sparkTotal)
	if limit := float64(sparkAvail - unifiedMemoryHeadroom - freeMargin); request > limit {
		t.Fatalf("--gpu-memory-utilization %.2f asks for %.1f GiB but only %.1f GiB can be claimed", util, request/float64(gib), limit/float64(gib))
	}
	if got := flagOf(t, extra, "--max-model-len"); got != "65536" {
		t.Fatalf("max-model-len = %q, want 65536 (it fits in the memory that is free): %v", got, extra)
	}
}

func TestPlanHFContext_CapsTheGPUShareOfAModelNotYetDownloadedOnAUnifiedMemoryMachine(t *testing.T) {
	withUnifiedMemoryGPU(t, sparkMemInfo)
	extra, _ := planHFContext([]string{"serve", "--model", "nvidia/Llama-3.1-8B-Instruct-FP8"}, t.TempDir(), 65536, "")

	util := utilOf(t, extra)
	if limit := float64(sparkAvail-unifiedMemoryHeadroom-freeMargin) / float64(sparkTotal); util > limit {
		t.Fatalf("--gpu-memory-utilization %.2f is above the %.2f that is free: %v", util, limit, extra)
	}
}

func TestPlanHFContext_LeavesTheShareToVLLMWhenAUnifiedMemoryMachineHasNothingFree(t *testing.T) {
	withUnifiedMemoryGPU(t, "MemTotal: 127600524 kB\nMemAvailable: 1000000 kB\n")
	extra, _ := planHFContext([]string{"serve", "--model", "nvidia/Llama-3.1-8B-Instruct-FP8"}, t.TempDir(), 65536, "")
	if v, _ := launchFlagValue(extra, "--gpu-memory-utilization"); v != "" {
		t.Fatalf("no share can be sized from nothing free; got %q in %v", v, extra)
	}
}
