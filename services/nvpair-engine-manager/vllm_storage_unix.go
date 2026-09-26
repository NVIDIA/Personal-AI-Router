// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package main

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

func platformVLLMFilesystem(path string) (uint64, string, error) {
	var filesystem unix.Statfs_t
	if err := unix.Statfs(path, &filesystem); err != nil {
		return 0, "", err
	}
	blockSize := uint64(filesystem.Bsize)
	if blockSize == 0 || filesystem.Bavail > math.MaxUint64/blockSize {
		return 0, "", fmt.Errorf("vLLM filesystem availability overflow")
	}
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return 0, "", err
	}
	return filesystem.Bavail * blockSize, fmt.Sprint(uint64(stat.Dev)), nil
}

func platformVLLMAllocatedBytes(path string) (uint64, uint64, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return 0, 0, err
	}
	if stat.Blocks < 0 || uint64(stat.Blocks) > math.MaxUint64/512 {
		return 0, 0, fmt.Errorf("vLLM allocated byte count overflow")
	}
	bytes := uint64(stat.Blocks) * 512
	if bytes < 4096 {
		bytes = 4096
	}
	return bytes, uint64(stat.Nlink), nil
}
