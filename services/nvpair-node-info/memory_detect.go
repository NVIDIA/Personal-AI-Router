// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

package main

import (
	"log"

	"github.com/jaypipes/ghw"
)

// detectMemoryTotal returns total physical RAM in bytes via ghw on Windows,
// Linux, and fallback platforms. macOS has a gopsutil implementation because
// ghw does not implement memory inventory there. Called once at startup and
// cached — the number doesn't change at runtime.
//
// Returns 0 on failure, which propagates through buildResponse as an
// absent "memory" object in JSON via omitempty on the pointer field.
// That's stricter than partial data: if we can't read the total we
// also don't surface used, because "used X bytes" without a total is
// unactionable for any UI.
//
// ghw's TotalPhysicalBytes is an int64 (signed) for historical
// reasons; we clamp negatives to 0 defensively even though in practice
// any modern host reports a positive value.
func detectMemoryTotal() uint64 {
	mem, err := ghw.Memory()
	if err != nil {
		log.Printf("memory detection error: %v", err)
		return 0
	}
	if mem == nil || mem.TotalPhysicalBytes <= 0 {
		return 0
	}
	return uint64(mem.TotalPhysicalBytes)
}
