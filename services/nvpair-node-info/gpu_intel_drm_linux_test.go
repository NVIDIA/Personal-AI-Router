// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"encoding/binary"
	"testing"
)

type i915MemoryRegionFixture struct {
	class       uint16
	instance    uint16
	probed      uint64
	unallocated uint64
}

func buildRegionsPayload(regions []i915MemoryRegionFixture) []byte {
	buf := make([]byte, drmMemoryRegionsHeaderSize+len(regions)*drmMemoryRegionInfoSize)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(regions)))
	// Bytes 4..16 are the header padding the kernel writes as zero.
	off := drmMemoryRegionsHeaderSize
	for _, r := range regions {
		binary.LittleEndian.PutUint16(buf[off:off+2], r.class)
		binary.LittleEndian.PutUint16(buf[off+2:off+4], r.instance)
		// rsvd0 (u32) stays zero.
		binary.LittleEndian.PutUint64(buf[off+8:off+16], r.probed)
		binary.LittleEndian.PutUint64(buf[off+16:off+24], r.unallocated)
		// rsvd1[8] (64 bytes) stays zero.
		off += drmMemoryRegionInfoSize
	}
	return buf
}

func TestParseI915MemoryRegions_SumsDeviceRegions(t *testing.T) {
	payload := buildRegionsPayload([]i915MemoryRegionFixture{
		{class: 0, instance: 0, probed: 64 << 30, unallocated: 60 << 30}, // SYSTEM
		{class: 1, instance: 0, probed: 8 << 30, unallocated: 6 << 30},
		{class: 1, instance: 1, probed: 8 << 30, unallocated: 5 << 30},
	})

	total, used, ok := parseI915MemoryRegions(payload, int32(len(payload)))
	if !ok || total != 16<<30 || used != 5<<30 {
		t.Fatalf("total = %d, used = %d, ok = %v, want 16 GiB total and 5 GiB used", total, used, ok)
	}
}

// TestParseI915MemoryRegionsNoDeviceClass verifies the parser reports
// failure when only a SYSTEM region is present. That's how the ioctl
// answers on a machine with no discrete Intel GPU under i915 — we must
// not fabricate a VRAM figure from the SYSTEM row.
func TestParseI915MemoryRegionsNoDeviceClass(t *testing.T) {
	payload := buildRegionsPayload([]i915MemoryRegionFixture{
		{class: 0, instance: 0, probed: 32 << 30, unallocated: 30 << 30},
	})
	if total, used, ok := parseI915MemoryRegions(payload, int32(len(payload))); ok || total != 0 || used != 0 {
		t.Fatalf("expected (0,0,false) for system-only payload; got (%d,%d,%v)", total, used, ok)
	}
}

// TestParseI915MemoryRegionsUnallocatedOverProbed guards the arithmetic
// against a driver bug (or a mid-collection race) that reports more
// unallocated than probed. Used should clamp to 0 rather than
// underflowing to a huge number.
func TestParseI915MemoryRegionsUnallocatedOverProbed(t *testing.T) {
	payload := buildRegionsPayload([]i915MemoryRegionFixture{
		{class: 1, instance: 0, probed: 2 << 30, unallocated: 1 << 30},
		{class: 1, instance: 1, probed: 1 << 30, unallocated: 2 << 30},
	})
	total, used, ok := parseI915MemoryRegions(payload, int32(len(payload)))
	if !ok || total != 3<<30 || used != 0 {
		t.Fatalf("total = %d, used = %d, ok = %v, want capacity only", total, used, ok)
	}
}

func TestParseI915MemoryRegions_RejectsTruncatedRegions(t *testing.T) {
	buf := buildRegionsPayload([]i915MemoryRegionFixture{{class: 1, probed: 2 << 30, unallocated: 1 << 30}})
	binary.LittleEndian.PutUint32(buf[:4], 2)
	if _, _, ok := parseI915MemoryRegions(buf, int32(len(buf))); ok {
		t.Fatal("accepted a partial capacity report")
	}
}

// TestParseI915MemoryRegionsShortBuffer rejects a payload smaller than
// even the header — the ioctl should not produce this, but a defensive
// parser must not panic on it.
func TestParseI915MemoryRegionsShortBuffer(t *testing.T) {
	if _, _, ok := parseI915MemoryRegions(nil, 0); ok {
		t.Fatal("expected failure on nil buffer")
	}
	if _, _, ok := parseI915MemoryRegions(make([]byte, 4), 4); ok {
		t.Fatal("expected failure on 4-byte buffer")
	}
}

func TestParseI915MemoryRegions_RespectsReturnedLength(t *testing.T) {
	buf := make([]byte, drmMemoryRegionsHeaderSize+drmMemoryRegionInfoSize)
	binary.LittleEndian.PutUint32(buf[:4], 1)
	region := buf[drmMemoryRegionsHeaderSize:]
	binary.LittleEndian.PutUint16(region[:2], i915MemoryClassDevice)
	binary.LittleEndian.PutUint64(region[8:16], 2<<30)
	binary.LittleEndian.PutUint64(region[16:24], 1<<30)

	cases := []struct {
		name   string
		length int32
	}{
		{"per-item error", -22},
		{"device region outside returned length", drmMemoryRegionsHeaderSize},
		{"length exceeds allocation", int32(len(buf)) + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, ok := parseI915MemoryRegions(buf, tc.length); ok {
				t.Fatalf("accepted a memory result with ioctl length %d", tc.length)
			}
		})
	}
}
