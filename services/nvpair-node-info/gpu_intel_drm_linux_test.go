// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"encoding/binary"
	"testing"
)

// buildRegionsPayload constructs a synthetic drm_i915_query_memory_regions
// buffer with the same on-wire layout the kernel emits. Keeps the parser
// test independent from an actual DRM device.
func buildRegionsPayload(regions []struct {
	class       uint16
	instance    uint16
	probed      uint64
	unallocated uint64
}) []byte {
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

// TestParseI915MemoryRegionsPicksDeviceClass pins that we surface the
// class=DEVICE region's totals, not the SYSTEM region — the same
// distinction intel_gpu_top makes. A regression here would blast
// system RAM in as VRAM on every Arc host.
func TestParseI915MemoryRegionsPicksDeviceClass(t *testing.T) {
	payload := buildRegionsPayload([]struct {
		class       uint16
		instance    uint16
		probed      uint64
		unallocated uint64
	}{
		{class: 0, instance: 0, probed: 64 << 30, unallocated: 60 << 30}, // SYSTEM
		{class: 1, instance: 0, probed: 6 << 30, unallocated: 5<<30 + 512<<20},
	})

	total, used, ok := parseI915MemoryRegions(payload)
	if !ok {
		t.Fatal("parseI915MemoryRegions returned ok=false on a well-formed payload")
	}
	if total != 6<<30 {
		t.Errorf("total = %d, want %d (class=DEVICE probed)", total, 6<<30)
	}
	// used = probed - unallocated = 6 GiB - (5 GiB + 512 MiB) = 512 MiB.
	if want := uint64(512 << 20); used != want {
		t.Errorf("used = %d, want %d", used, want)
	}
}

// TestParseI915MemoryRegionsNoDeviceClass verifies the parser reports
// failure when only a SYSTEM region is present. That's how the ioctl
// answers on a machine with no discrete Intel GPU under i915 — we must
// not fabricate a VRAM figure from the SYSTEM row.
func TestParseI915MemoryRegionsNoDeviceClass(t *testing.T) {
	payload := buildRegionsPayload([]struct {
		class       uint16
		instance    uint16
		probed      uint64
		unallocated uint64
	}{
		{class: 0, instance: 0, probed: 32 << 30, unallocated: 30 << 30},
	})
	if total, used, ok := parseI915MemoryRegions(payload); ok || total != 0 || used != 0 {
		t.Fatalf("expected (0,0,false) for system-only payload; got (%d,%d,%v)", total, used, ok)
	}
}

// TestParseI915MemoryRegionsUnallocatedOverProbed guards the arithmetic
// against a driver bug (or a mid-collection race) that reports more
// unallocated than probed. Used should clamp to 0 rather than
// underflowing to a huge number.
func TestParseI915MemoryRegionsUnallocatedOverProbed(t *testing.T) {
	payload := buildRegionsPayload([]struct {
		class       uint16
		instance    uint16
		probed      uint64
		unallocated uint64
	}{
		{class: 1, instance: 0, probed: 1 << 30, unallocated: 2 << 30},
	})
	total, used, ok := parseI915MemoryRegions(payload)
	if !ok || total != 1<<30 || used != 0 {
		t.Fatalf("expected clamped used=0; got total=%d used=%d ok=%v", total, used, ok)
	}
}

// TestParseI915MemoryRegionsTruncatedBufferStops confirms we don't
// scan past the end of the buffer when the reported num_regions exceeds
// what fits. Prevents an out-of-bounds slice on a malformed kernel reply.
func TestParseI915MemoryRegionsTruncatedBufferStops(t *testing.T) {
	// Header claims 5 regions but only one fits.
	buf := make([]byte, drmMemoryRegionsHeaderSize+drmMemoryRegionInfoSize)
	binary.LittleEndian.PutUint32(buf[0:4], 5)
	// Single region: class=DEVICE, probed=2 GiB, unallocated=1 GiB.
	off := drmMemoryRegionsHeaderSize
	binary.LittleEndian.PutUint16(buf[off:off+2], i915MemoryClassDevice)
	binary.LittleEndian.PutUint64(buf[off+8:off+16], 2<<30)
	binary.LittleEndian.PutUint64(buf[off+16:off+24], 1<<30)

	total, used, ok := parseI915MemoryRegions(buf)
	if !ok || total != 2<<30 || used != 1<<30 {
		t.Fatalf("truncated buffer parse = total=%d used=%d ok=%v; want total=2GiB used=1GiB", total, used, ok)
	}
}

// TestParseI915MemoryRegionsShortBuffer rejects a payload smaller than
// even the header — the ioctl should not produce this, but a defensive
// parser must not panic on it.
func TestParseI915MemoryRegionsShortBuffer(t *testing.T) {
	if _, _, ok := parseI915MemoryRegions(nil); ok {
		t.Fatal("expected failure on nil buffer")
	}
	if _, _, ok := parseI915MemoryRegions(make([]byte, 4)); ok {
		t.Fatal("expected failure on 4-byte buffer")
	}
}
