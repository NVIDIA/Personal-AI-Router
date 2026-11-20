// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

// DRM_IOCTL_I915_QUERY reports discrete memory regions when i915 does not
// expose capacity in sysfs. The kernel only provides reliable unallocated
// memory accounting with CAP_PERFMON or CAP_SYS_ADMIN. Without those
// capabilities, it can return the full capacity as unallocated.
// See Linux include/uapi/drm/i915_drm.h, drm_i915_memory_region_info.

const (
	// drmIoctlI915Query = _IOWR('d', 0x40+0x39, struct drm_i915_query).
	// struct drm_i915_query is 16 bytes (u32 num_items, u32 flags, u64
	// items_ptr). _IOWR direction is 3, so the encoded value is
	// (3<<30) | (16<<16) | ('d'<<8) | 0x79 = 0xC0106479.
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

// drmI915Query mirrors `struct drm_i915_query`. ItemsPtr holds a native
// address as a 64-bit integer, so the caller must pin the pointed-to item.
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

// intelVRAMFromDRM samples i915 only. Xe capacity uses a different ioctl,
// and Xe usage is collected through the CLI or sysfs when available.
func intelVRAMFromDRM(renderNode string) (total uint64, used uint64, ok bool) {
	if intelDRMDriver(renderNode) != "i915" {
		return 0, 0, false
	}
	return readI915VRAM(renderNode)
}

func readI915VRAM(renderNode string) (total uint64, used uint64, ok bool) {
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
	var pinned runtime.Pinner
	pinned.Pin(&item)
	defer pinned.Unpin()
	if err := ioctlI915Query(fd, &item); err != nil {
		slog.Debug("i915 query length probe failed", "path", renderNode, "err", err)
		return 0, 0, false
	}
	if item.Length < drmMemoryRegionsHeaderSize || item.Length > 1<<20 {
		// A payload above 1 MiB would be well past any conceivable
		// region count and almost certainly a driver bug; refuse to
		// allocate for it rather than trusting the number.
		return 0, 0, false
	}

	// Pass 2: pin the backing array because the kernel receives its
	// address through the integer DataPtr field, not as a syscall argument.
	buf := make([]byte, item.Length)
	pinned.Pin(&buf[0])
	item.DataPtr = uint64(uintptr(unsafe.Pointer(&buf[0])))
	if err := ioctlI915Query(fd, &item); err != nil {
		slog.Debug("i915 query fetch failed", "path", renderNode, "err", err)
		return 0, 0, false
	}
	return parseI915MemoryRegions(buf, item.Length)
}

// ioctlI915Query issues DRM_IOCTL_I915_QUERY with a caller-pinned item.
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
		return errno
	}
	return nil
}

// Sum all DEVICE regions. A truncated reply cannot establish a card total.
func parseI915MemoryRegions(buf []byte, length int32) (uint64, uint64, bool) {
	// The ioctl can succeed while Length contains a per-item error.
	if length < drmMemoryRegionsHeaderSize || int(length) > len(buf) {
		return 0, 0, false
	}
	buf = buf[:length]
	numRegions := binary.LittleEndian.Uint32(buf[0:4])
	if uint64(numRegions) > uint64((len(buf)-drmMemoryRegionsHeaderSize)/drmMemoryRegionInfoSize) {
		return 0, 0, false
	}
	var total, used uint64
	usageValid := true
	offset := drmMemoryRegionsHeaderSize
	for i := uint32(0); i < numRegions; i++ {
		memClass := binary.LittleEndian.Uint16(buf[offset : offset+2])
		probed := binary.LittleEndian.Uint64(buf[offset+8 : offset+16])
		unallocated := binary.LittleEndian.Uint64(buf[offset+16 : offset+24])
		offset += drmMemoryRegionInfoSize
		if memClass != i915MemoryClassDevice {
			continue
		}
		if probed == 0 || probed > ^uint64(0)-total {
			return 0, 0, false
		}
		total += probed
		if unallocated > probed {
			usageValid = false
		} else {
			used += probed - unallocated
		}
	}
	if !usageValid {
		used = 0
	}
	return total, used, total > 0
}

// Resolve the render node by PCI address for both CLI and sysfs discovery.
func intelRenderNodeForBDF(bdf string) string {
	if bdf == "" {
		return ""
	}
	root := os.Getenv("GHW_CHROOT")
	matches, err := filepath.Glob(filepath.Join(root, "/sys/bus/pci/devices", bdf, "drm", "renderD*"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	dev := filepath.Join(root, "/dev/dri", filepath.Base(matches[0]))
	if _, err := os.Stat(dev); err != nil {
		return ""
	}
	return dev
}
