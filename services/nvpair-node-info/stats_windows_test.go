// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"testing"
	"unsafe"
)

// TestLuidKey locks down the PDH instance-name format. If this ever drifts
// from PDH's actual output the per-adapter join performed by
// statsCollector.decodeSnapshot will silently always miss and every GPU
// will report zeroed dynamic fields — a regression that would otherwise
// only surface via a visual comparison against Task Manager. The two test
// cases below cover the zero-high case (the overwhelming majority of real
// adapters) and a non-zero high case, which additionally verifies we use
// bit-reinterpretation for the signed HighPart rather than numeric sign-
// extension.
func TestLuidKey(t *testing.T) {
	cases := []struct {
		name string
		low  uint32
		high int32
		want string
	}{
		{"typical", 0x000054F0, 0x00000000, "luid_0x00000000_0x000054f0_phys_0"},
		{"high-bit-set", 0xDEADBEEF, -1, "luid_0xffffffff_0xdeadbeef_phys_0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := luidKey(c.low, c.high)
			if got != c.want {
				t.Fatalf("luidKey(%#x, %#x) = %q, want %q", c.low, c.high, got, c.want)
			}
		})
	}
}

// TestPDHFmtCounterValueSize pins the scalar pdhFmtCounterValue to the
// C PDH_FMT_COUNTERVALUE size on amd64/arm64 Windows: DWORD CStatus +
// 4 bytes of padding + 8-byte PDH_FMT_COUNTERVALUE union payload = 16.
// decodeCPU hands a pointer to one of these directly to
// PdhGetFormattedCounterValue, so a layout drift would corrupt PDH's
// write into the CPU percentage.
func TestPDHFmtCounterValueSize(t *testing.T) {
	const expected = 16
	if got := unsafe.Sizeof(pdhFmtCounterValue{}); got != expected {
		t.Fatalf("pdhFmtCounterValue size = %d, want %d", got, expected)
	}
}

// TestPDHFmtCounterValueItemSize pins the in-Go layout of
// pdhFmtCounterValueItemW to the C PDH_FMT_COUNTERVALUE_ITEM_W size on
// amd64/arm64 Windows: 8-byte LPWSTR + 16-byte PDH_FMT_COUNTERVALUE = 24.
// If this fires, someone has reordered the fields or we've accidentally
// compiled for 32-bit Windows — either way PDH would be handed garbage
// and every lookup would silently fail.
func TestPDHFmtCounterValueItemSize(t *testing.T) {
	const expected = 24
	if got := unsafe.Sizeof(pdhFmtCounterValueItemW{}); got != expected {
		t.Fatalf("pdhFmtCounterValueItemW size = %d, want %d", got, expected)
	}
}

// TestMemoryStatusExSize pins the in-Go layout of memoryStatusEx to
// the C MEMORYSTATUSEX size on amd64/arm64 Windows:
// 4-byte DWORD + 4-byte DWORD + 7 × 8-byte DWORDLONG = 64.
// A drift here would have GlobalMemoryStatusEx reject our call with
// ERROR_INVALID_PARAMETER because dwLength no longer matches, or
// (worse, if we set dwLength to the wrong size manually) corrupt the
// stack when the OS writes past our struct.
func TestMemoryStatusExSize(t *testing.T) {
	const expected = 64
	if got := unsafe.Sizeof(memoryStatusEx{}); got != expected {
		t.Fatalf("memoryStatusEx size = %d, want %d", got, expected)
	}
}
