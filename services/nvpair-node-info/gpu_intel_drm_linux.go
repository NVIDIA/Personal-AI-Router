// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"encoding/binary"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"unsafe"
)

// Direct DRM query for Intel GPU memory regions. The i915 driver in stock
// Debian/Ubuntu kernels does not expose the sysfs mem_info_vram_total /
// mem_info_vram_used attributes that our first-tier sysfs walk relies on —
// those files only appear on very new i915 builds and on the xe driver. On
// every other host with an Arc adapter, /sys/class/drm/card*/device/ has no
// VRAM readout at all, so `intel-gpu-tools` (intel_gpu_top) uses the
// DRM_IOCTL_I915_QUERY ioctl against the render node to get both the
// probed_size (total VRAM) and the unallocated_size (free VRAM) per memory
// region. This file re-implements that query in pure Go so PAIR can report
// real Arc VRAM totals and dynamic VRAM-used without depending on xpu-smi or
// on newer i915 sysfs.
//
// Reference: https://github.com/mkuoppal/intel-gpu-tools/blob/master/tools/intel_gpu_top.c
// (I915_MEMORY_CLASS_DEVICE is the discrete-VRAM class the ioctl reports.)

const (
	// drmIoctlI915Query = _IOWR('d', 0x40+0x39, struct drm_i915_query).
	// struct drm_i915_query is 16 bytes (u32 num_items, u32 flags, u64
	// items_ptr). _IOWR direction is 3, so the encoded value is
	// (3<<30) | (16<<16) | ('d'<<8) | 0x79 = 0xC0106479. Stable across
	// every Linux release since i915 gained the query interface in 4.14.
	drmIoctlI915Query = 0xC0106479

	// drmI915QueryMemoryRegions selects the memory-regions payload.
	// Returned buffer starts with `struct drm_i915_query_memory_regions`
	// (u32 num_regions + 3 * u32 padding), followed by one
	// `struct drm_i915_memory_region_info` per region.
	drmI915QueryMemoryRegions = 4

	// i915MemoryClassDevice is the discrete-VRAM class returned in
	// `region.memory_class`. Class 0 (SYSTEM) is host RAM the driver can
	// map for the GPU; class 1 (DEVICE) is what a user thinks of as
	// "VRAM" and what shows up on the marketing box.
	i915MemoryClassDevice = 1

	// drmMemoryRegionInfoSize matches the on-wire layout of
	// `struct drm_i915_memory_region_info`:
	//
	//   struct drm_i915_gem_memory_class_instance {
	//     __u16 memory_class;
	//     __u16 memory_instance;
	//   } region;                    // 4 bytes
	//   __u32 rsvd0;                 // 4
	//   __u64 probed_size;           // 8
	//   __u64 unallocated_size;      // 8
	//   __u64 rsvd1[8];              // 64  (reserved padding the kernel emits)
	//
	// Total: 88 bytes. A regression here would misread every region
	// past the first, so keep this literal aligned with the kernel UAPI.
	drmMemoryRegionInfoSize = 88

	// drmMemoryRegionsHeaderSize is the fixed header preceding the
	// region array (num_regions u32 + 3 * u32 padding).
	drmMemoryRegionsHeaderSize = 16
)

// drmI915Query mirrors `struct drm_i915_query` for the ioctl's first
// argument. Kept as a package-private literal so callers pass a
// heap-allocated backing store into the ioctl (ioctl requires stable
// addresses; a Go stack pointer might migrate mid-call).
type drmI915Query struct {
	NumItems uint32
	Flags    uint32
	ItemsPtr uint64
}

// drmI915QueryItem mirrors `struct drm_i915_query_item`.
// A two-pass call fills length on the first pass (data_ptr=0), then
// allocates a buffer of that size and re-issues with data_ptr set.
type drmI915QueryItem struct {
	QueryID uint64
	Length  int32
	Flags   uint32
	DataPtr uint64
}

// intelVRAMFromDRM queries a single render node for its discrete VRAM
// totals. Returns (total, used, true) when the ioctl succeeds and a
// class=DEVICE region is present. On any failure it returns ok=false
// and the caller falls back to whatever other source is available.
//
// This is safe to call every stats-collector tick: the DRM query is a
// short kernel-side lookup with no driver serialization, and the
// probed_size stays constant while unallocated_size tracks the live
// residency of driver-owned buffers.
func intelVRAMFromDRM(renderNode string) (total uint64, used uint64, ok bool) {
	fd, err := syscall.Open(renderNode, syscall.O_RDONLY|syscall.O_CLOEXEC, 0)
	if err != nil {
		slog.Debug("open render node failed", "path", renderNode, "err", err)
		return 0, 0, false
	}
	defer syscall.Close(fd)

	// Pass 1: length probe. data_ptr=0 signals "tell me how large the
	// payload is". The kernel writes the required length into item.Length
	// and returns without touching data_ptr. Any negative length means
	// the query isn't supported on this driver (an older i915 build, an
	// xe-driven card, etc.).
	item := drmI915QueryItem{QueryID: drmI915QueryMemoryRegions}
	if err := ioctlI915Query(fd, &item); err != nil {
		slog.Debug("i915 query length probe failed", "path", renderNode, "err", err)
		return 0, 0, false
	}
	if item.Length <= 0 || item.Length > 1<<20 {
		// A payload above 1 MiB would be well past any conceivable
		// region count and almost certainly a driver bug; refuse to
		// allocate for it rather than trusting the number.
		return 0, 0, false
	}

	// Pass 2: fetch the actual bytes into a Go slice. The backing array
	// is pinned for the duration of the syscall because Syscall retains
	// the pointer until the kernel returns.
	buf := make([]byte, item.Length)
	item.DataPtr = uint64(uintptr(unsafe.Pointer(&buf[0])))
	if err := ioctlI915Query(fd, &item); err != nil {
		slog.Debug("i915 query fetch failed", "path", renderNode, "err", err)
		return 0, 0, false
	}
	return parseI915MemoryRegions(buf)
}

// ioctlI915Query issues one DRM_IOCTL_I915_QUERY carrying a single
// query item. Split out so the two-pass sequence stays readable and so
// unit tests can inject failures in a future refactor without going
// through the syscall path.
func ioctlI915Query(fd int, item *drmI915QueryItem) error {
	q := drmI915Query{
		NumItems: 1,
		ItemsPtr: uint64(uintptr(unsafe.Pointer(item))),
	}
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		uintptr(fd),
		uintptr(drmIoctlI915Query),
		uintptr(unsafe.Pointer(&q)),
	)
	if errno != 0 {
		return errors.New(errno.Error())
	}
	return nil
}

// parseI915MemoryRegions walks the payload the kernel wrote and returns
// the class=DEVICE region's total + used bytes. Rows past the buffer
// are dropped rather than treated as an error; a truncated payload is
// still valid so long as the class=DEVICE row landed in the readable
// prefix.
func parseI915MemoryRegions(buf []byte) (uint64, uint64, bool) {
	if len(buf) < drmMemoryRegionsHeaderSize {
		return 0, 0, false
	}
	numRegions := binary.LittleEndian.Uint32(buf[0:4])
	offset := drmMemoryRegionsHeaderSize
	for i := uint32(0); i < numRegions; i++ {
		if offset+drmMemoryRegionInfoSize > len(buf) {
			break
		}
		memClass := binary.LittleEndian.Uint16(buf[offset : offset+2])
		probed := binary.LittleEndian.Uint64(buf[offset+8 : offset+16])
		unallocated := binary.LittleEndian.Uint64(buf[offset+16 : offset+24])
		offset += drmMemoryRegionInfoSize
		if memClass != i915MemoryClassDevice {
			continue
		}
		var used uint64
		if probed >= unallocated {
			used = probed - unallocated
		}
		return probed, used, true
	}
	return 0, 0, false
}

// intelRenderNode resolves the /dev/dri/renderD* path a DRM card is
// backed by. Every physical GPU exposes both a primary node (cardN,
// requires GRAPHICS caps) and a render node (renderDNNN, opens with
// only the render group); we prefer the render node because it needs
// no privileged group membership beyond `render` and is the one
// intel_gpu_top uses too.
//
// The mapping lives under /sys/class/drm/cardN/device/drm/renderD*; a
// card that publishes no render node (some virtualized GPUs) returns
// an empty string, which callers treat as "no DRM query available".
func intelRenderNode(cardPath string) string {
	matches, err := filepath.Glob(filepath.Join(cardPath, "device", "drm", "renderD*"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	// The sysfs entry is just the name; the character device lives
	// under /dev/dri.
	dev := filepath.Join("/dev/dri", filepath.Base(matches[0]))
	if _, err := os.Stat(dev); err != nil {
		return ""
	}
	return dev
}

// intelRenderNodeForBDF is the xpu-smi-path counterpart of
// intelRenderNode: given a PCI BDF (which xpu-smi discovery reports),
// walk /sys/bus/pci/devices/<bdf>/drm/renderD* and return the /dev
// path. Falls back to an empty string when nothing matches, so callers
// treat it as "sysfs is our only vram source for this adapter".
func intelRenderNodeForBDF(bdf string) string {
	if bdf == "" {
		return ""
	}
	base := filepath.Join("/sys/bus/pci/devices", strings.ToLower(bdf), "drm")
	matches, err := filepath.Glob(filepath.Join(base, "renderD*"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)
	dev := filepath.Join("/dev/dri", filepath.Base(matches[0]))
	if _, err := os.Stat(dev); err != nil {
		return ""
	}
	return dev
}
