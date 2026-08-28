// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
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
