// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"os"
	"slices"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDXGIAdapterDesc1Size pins the in-Go layout of dxgiAdapterDesc1 to the
// C ABI's DXGI_ADAPTER_DESC1 size on amd64/arm64 Windows (8-byte SIZE_T).
//
//	256  Description[128]uint16
//	  4  VendorID
//	  4  DeviceID
//	  4  SubSysID
//	  4  Revision
//	  4  (alignment padding before SIZE_T)
//	  8  DedicatedVideoMemory  (SIZE_T)
//	  8  DedicatedSystemMemory (SIZE_T)
//	  8  SharedSystemMemory    (SIZE_T)
//	  4  AdapterLuidLow
//	  4  AdapterLuidHigh
//	  4  Flags
//	  4  (trailing padding to 8-byte alignment)
//	---
//	312 bytes
//
// If this assertion fires, a field has been reordered, retyped, or the build
// has somehow targeted 32-bit Windows — none of which we want silently passing
// garbage to DXGI.
func TestDXGIAdapterDesc1_MatchesNativeStructureSize(t *testing.T) {
	const expected = 312
	require.Equal(t, uintptr(expected), unsafe.Sizeof(dxgiAdapterDesc1{}), "dxgiAdapterDesc1 size")
}

func TestIsVirtualDisplayAdapter_RecognizesVirtualAdapters(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"NVIDIA GeForce RTX 5080", false},
		{"AMD Radeon RX 7900 XTX", false},
		{"Intel(R) UHD Graphics 770", false},
		{"Microsoft Remote Display Adapter", true},
		{"microsoft remote display adapter", true},
		{"REMOTE DISPLAY", true},
		{"Microsoft Basic Display Adapter", true},
		{"Basic Display something", false}, // needs "microsoft basic display"
		{"Contoso Virtual Display Adapter", true},
		{"Microsoft Hyper-V Video", false},
		{"", false},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, isVirtualDisplayAdapter(tc.name), "isVirtualDisplayAdapter")
	}
}

func TestLuidUint64_CombinesHighAndLowParts(t *testing.T) {
	test := func(name string, low uint32, high int32, want uint64) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, luidUint64(low, high))
		})
	}
	test("zero", 0, 0, 0)
	test("low word", 0x54f0, 0, 0x54f0)
	test("both words", 0x000054f0, 0x00000001, 0x00000001000054f0)
	test("signed high word", 0xffffffff, -1, 0xffffffffffffffff)
}

// The whole-enumeration gate still drops a Remote Desktop phantom clone while
// the registry knows the real card, and refuses to apply a registry that knows
// none of the current adapters. That stale state at boot used to publish an
// empty GPU list until the service was restarted, even though DXGI (the Windows
// graphics API) had found the adapters.
func TestSelectPhysicalAdapters_FiltersOnlyWhenRegistryMatchesAdapters(t *testing.T) {
	card := adapterCandidate{
		gpu:  GPUInfo{Name: "Test Adapter", statsKey: "luid_card"},
		luid: 0x1000,
	}
	clone := adapterCandidate{
		gpu:  GPUInfo{Name: "Test Adapter", statsKey: "luid_clone"},
		luid: 0x2000,
	}
	both := []adapterCandidate{card, clone}

	test := func(name string, candidates []adapterCandidate, physical map[uint64]struct{}, want []adapterCandidate) {
		t.Run(name, func(t *testing.T) {
			// Keep inputs and expectations independent even when a case uses
			// the same fixture for both, so an in-place change cannot mask a bug.
			input := slices.Clone(candidates)
			expected := slices.Clone(want)
			assert.Equal(t, expected, selectPhysicalAdapters(input, physical))
		})
	}
	test("gate keeps the listed subset", both, map[uint64]struct{}{0x1000: {}}, []adapterCandidate{card})
	test("gate that would drop every adapter is skipped", both, map[uint64]struct{}{0x9999: {}, 0xaaaa: {}}, both)
	test("all listed adapters are kept", both, map[uint64]struct{}{0x1000: {}, 0x2000: {}}, both)
	test("nil map keeps all", both, nil, both)
	test("empty map keeps all", both, map[uint64]struct{}{}, both)
	t.Run("no candidates stays empty", func(t *testing.T) {
		assert.Empty(t, selectPhysicalAdapters(nil, map[uint64]struct{}{0x1000: {}}))
	})
}

// This smoke test requires NVPAIR_LIVE_GPU=1 and a Windows host with GPUs.
// Registry filtering scenarios are covered above with fixed adapter lists.
func TestDetectGPUs_ReportsLiveAdapters(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_GPU") == "" {
		t.Skip("set NVPAIR_LIVE_GPU=1 to run against the host's GPUs")
	}
	gpus := detectGPUs()
	require.NotEmpty(t, gpus, "detectGPUs reported no adapters on a host that has them")
	for i, gpu := range gpus {
		t.Logf("GPU %d: %s (%d MiB, %s)", i, gpu.Name, gpu.VramBytes/(1024*1024), gpu.statsKey)
	}
}
