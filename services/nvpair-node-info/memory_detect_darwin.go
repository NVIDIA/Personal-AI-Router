// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"log"

	"github.com/shirou/gopsutil/v4/mem"
)

// detectMemoryTotal returns the physical unified-memory capacity reported by
// hw.memsize through gopsutil's purego-backed Darwin implementation.
func detectMemoryTotal() uint64 {
	memory, err := mem.VirtualMemory()
	if err != nil {
		log.Printf("memory detection error: %v", err)
		return 0
	}
	return memory.Total
}
