// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"os"
	"slices"
	"testing"
	"unsafe"
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
func TestDXGIAdapterDesc1Size(t *testing.T) {
	const expected = 312
	if got := unsafe.Sizeof(dxgiAdapterDesc1{}); got != expected {
		t.Fatalf("dxgiAdapterDesc1 size = %d, want %d", got, expected)
	}
}

func TestIsVirtualDisplayAdapter(t *testing.T) {
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
		if got := isVirtualDisplayAdapter(tc.name); got != tc.want {
			t.Errorf("isVirtualDisplayAdapter(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLuidUint64(t *testing.T) {
	cases := []struct {
		low  uint32
		high int32
		want uint64
	}{
		{0, 0, 0},
		{0x54f0, 0, 0x54f0},
		{0x000054f0, 0x00000001, 0x00000001000054f0},
		{0xffffffff, -1, 0xffffffffffffffff},
	}
	for _, tc := range cases {
		if got := luidUint64(tc.low, tc.high); got != tc.want {
			t.Errorf("luidUint64(%#x, %d) = %#x, want %#x", tc.low, tc.high, got, tc.want)
		}
	}
}

func TestKeepPhysicalAdapter(t *testing.T) {
	physical := map[uint64]struct{}{
		0x1000: {},
		0x2000: {},
	}
	cases := []struct {
		name     string
		luid     uint64
		physical map[uint64]struct{}
		want     bool
	}{
		{"nil map keeps all", 0x9999, nil, true},
		{"empty map keeps all", 0x9999, map[uint64]struct{}{}, true},
		{"listed LUID kept", 0x1000, physical, true},
		{"unlisted LUID dropped", 0x9999, physical, false},
		{"second listed LUID kept", 0x2000, physical, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keepPhysicalAdapter(tc.luid, tc.physical); got != tc.want {
				t.Errorf("keepPhysicalAdapter(%#x) = %v, want %v", tc.luid, got, tc.want)
			}
		})
	}
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
