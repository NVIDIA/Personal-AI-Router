// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"unsafe"
)

func intelDRMDriver(node string) string {
	driver, err := filepath.EvalSymlinks(filepath.Join("/sys/class/drm", filepath.Base(node), "device/driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(driver)
}

// intelVRAMTotalFromDRM reads physical device capacity using its driver ABI.
func intelVRAMTotalFromDRM(node string) (uint64, bool) {
	switch intelDRMDriver(node) {
	case "xe":
		return intelXeVRAMTotal(node)
	case "i915":
		total, _, ok := readI915VRAM(node)
		return total, ok && total > 0
	default:
		return 0, false
	}
}

// Linux v7.0 include/uapi/drm/xe_drm.h: query=1 is MEM_REGIONS.
// No GEM/VM/EXEC_QUEUE/EXEC ioctl is issued. Global used is deliberately ignored.
type xeMemoryQuery struct {
	Extensions uint64
	Query      uint32
	Size       uint32
	Data       uint64
	Reserved   [2]uint64
}

func intelXeVRAMTotal(node string) (uint64, bool) {
	if unsafe.Sizeof(xeMemoryQuery{}) != 40 {
		return 0, false
	}
	f, err := os.Open(node)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	q := xeMemoryQuery{Query: 1}
	query := func() bool {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), 0xc0286440, uintptr(unsafe.Pointer(&q)))
		return errno == 0
	}
	if !query() || q.Size < 8 || q.Size > 1<<20 {
		return 0, false
	}
	buf := make([]byte, q.Size)
	var pinned runtime.Pinner
	pinned.Pin(&buf[0])
	defer pinned.Unpin()
	q.Data = uint64(uintptr(unsafe.Pointer(&buf[0])))
	if !query() || q.Size < 8 || uint64(q.Size) > uint64(len(buf)) {
		return 0, false
	}
	return parseXeVRAMTotal(buf[:q.Size])
}

// total_size is usable bytes, not advertised physical/marketing capacity.
// Reject incomplete arrays and unknown/zero totals rather than publish fake 0.
func parseXeVRAMTotal(buf []byte) (uint64, bool) {
	const header, stride = 8, 88
	if len(buf) < header || binary.LittleEndian.Uint32(buf[4:8]) != 0 {
		return 0, false
	}
	count := binary.LittleEndian.Uint32(buf[:4])
	if uint64(count) > uint64((len(buf)-header)/stride) {
		return 0, false
	}
	var total uint64
	for i := uint32(0); i < count; i++ {
		offset := header + int(i)*stride
		if binary.LittleEndian.Uint16(buf[offset:offset+2]) != 1 { // VRAM, not SYSMEM
			continue
		}
		n := binary.LittleEndian.Uint64(buf[offset+8 : offset+16])
		if n == 0 || n > ^uint64(0)-total {
			return 0, false
		}
		total += n
	}
	return total, total > 0
}
