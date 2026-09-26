// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func platformVLLMFilesystem(path string) (uint64, string, error) {
	value, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, "", err
	}
	var available uint64
	if err := windows.GetDiskFreeSpaceEx(value, &available, nil, nil); err != nil {
		return 0, "", err
	}
	volume := make([]uint16, 32768)
	if err := windows.GetVolumePathName(value, &volume[0], uint32(len(volume))); err != nil {
		return 0, "", err
	}
	device := strings.ToLower(windows.UTF16ToString(volume))
	if device == "" {
		return 0, "", fmt.Errorf("vLLM volume identity is unavailable")
	}
	return available, device, nil
}

type vllmWindowsFileStandardInfo struct {
	AllocationSize int64
	EndOfFile      int64
	NumberOfLinks  uint32
	DeletePending  byte
	Directory      byte
	_              [2]byte
}

func platformVLLMAllocatedBytes(path string) (uint64, uint64, error) {
	value, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, err
	}
	handle, err := windows.CreateFile(value, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, 0, err
	}
	defer windows.CloseHandle(handle)
	var info vllmWindowsFileStandardInfo
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileStandardInfo, (*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return 0, 0, err
	}
	if info.AllocationSize < 0 {
		return 0, 0, fmt.Errorf("vLLM allocated byte count is invalid")
	}
	bytes := uint64(info.AllocationSize)
	if bytes < 4096 {
		bytes = 4096
	}
	return bytes, uint64(info.NumberOfLinks), nil
}
