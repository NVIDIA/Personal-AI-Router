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

func TestKeepPhysicalAdapter_FiltersByPhysicalAdapterIdentity(t *testing.T) {
	physical := map[uint64]struct{}{
		0x1000: {},
		0x2000: {},
	}
	test := func(name string, luid uint64, physical map[uint64]struct{}, want bool) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, keepPhysicalAdapter(luid, physical))
		})
	}
	test("nil map keeps all", 0x9999, nil, true)
	test("empty map keeps all", 0x9999, map[uint64]struct{}{}, true)
	test("listed LUID kept", 0x1000, physical, true)
	test("unlisted LUID dropped", 0x9999, physical, false)
	test("second listed LUID kept", 0x2000, physical, true)
}

// TestSelectPhysicalAdapters covers the whole-enumeration gate: it still drops
// an RDP phantom clone while the registry knows the real card, and it refuses
// to apply a registry that knows none of them — the stale state a node is in
// for the first minute after boot, which used to publish an empty GPU list
// until the service was restarted.
func TestSelectPhysicalAdapters(t *testing.T) {
	card := adapterCandidate{
		gpu:  GPUInfo{Name: "Test Adapter", statsKey: "luid_card"},
		luid: 0x1000,
	}
	clone := adapterCandidate{
		gpu:  GPUInfo{Name: "Test Adapter", statsKey: "luid_clone"},
		luid: 0x2000,
	}
	both := []adapterCandidate{card, clone}

	cases := []struct {
		name       string
		candidates []adapterCandidate
		physical   map[uint64]struct{}
		want       []string
	}{
		{
			name:       "gate keeps the listed subset",
			candidates: both,
			physical:   map[uint64]struct{}{0x1000: {}},
			want:       []string{"luid_card"},
		},
		{
			name:       "gate that would drop every adapter is skipped",
			candidates: both,
			physical:   map[uint64]struct{}{0x9999: {}, 0xaaaa: {}},
			want:       []string{"luid_card", "luid_clone"},
		},
		{
			name:       "nil map keeps all",
			candidates: both,
			physical:   nil,
			want:       []string{"luid_card", "luid_clone"},
		},
		{
			name:       "empty map keeps all",
			candidates: both,
			physical:   map[uint64]struct{}{},
			want:       []string{"luid_card", "luid_clone"},
		},
		{
			name:       "no candidates stays empty",
			candidates: nil,
			physical:   map[uint64]struct{}{0x1000: {}},
			want:       nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, c := range selectPhysicalAdapters(tc.candidates, tc.physical) {
				got = append(got, c.gpu.statsKey)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("selectPhysicalAdapters kept %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDetectGPUsLive runs only when NVPAIR_LIVE_GPU=1. On a real host,
// detection must report at least one adapter, and handing the gate a registry
// map that knows none of this boot's LUIDs — the literal stale-registry state
// — must still return every adapter rather than none.
func TestDetectGPUsLive(t *testing.T) {
	if os.Getenv("NVPAIR_LIVE_GPU") == "" {
		t.Skip("set NVPAIR_LIVE_GPU=1 to run against the host's GPUs")
	}
	gpus := detectGPUs()
	if len(gpus) == 0 {
		t.Fatal("detectGPUs reported no adapters on a host that has them")
	}
	for i, gpu := range gpus {
		t.Logf("GPU %d: %s (%d MiB, %s)", i, gpu.Name, gpu.VramBytes/(1024*1024), gpu.statsKey)
	}

	candidates := enumerateAdapterCandidates()
	t.Logf("enumerated %d candidate adapter(s), detection reported %d", len(candidates), len(gpus))
	if len(candidates) < len(gpus) {
		t.Fatalf("enumerated %d candidates but detection reported %d GPUs", len(candidates), len(gpus))
	}

	stale := make(map[uint64]struct{}, len(candidates))
	for _, c := range candidates {
		stale[^c.luid] = struct{}{}
	}
	kept := selectPhysicalAdapters(candidates, stale)
	if len(kept) != len(candidates) {
		t.Fatalf("a stale registry kept %d of %d adapters; the gate is not stale-safe",
			len(kept), len(candidates))
	}
	for _, c := range kept {
		t.Logf("stale-registry gate kept %s (%s)", c.gpu.Name, c.gpu.statsKey)
	}

	fresh := make(map[uint64]struct{}, len(candidates))
	for _, c := range candidates {
		fresh[c.luid] = struct{}{}
	}
	if got := selectPhysicalAdapters(candidates, fresh); len(got) != len(candidates) {
		t.Fatalf("a registry holding this boot's LUIDs kept %d of %d adapters",
			len(got), len(candidates))
	}
}
