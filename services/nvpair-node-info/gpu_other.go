// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && !linux && !darwin

package main

import (
	"log"

	"github.com/jaypipes/ghw"
)

// detectGPUs is the fallback for hosts without a platform-specific detector.
// It reuses ghw to enumerate display adapters and returns a name only —
// VramBytes is left at 0 ("unknown") because ghw does not expose VRAM on any
// platform.
func detectGPUs() []GPUInfo {
	gpu, err := ghw.GPU()
	if err != nil {
		log.Printf("GPU detection error: %v", err)
		return nil
	}

	var gpus []GPUInfo
	for _, card := range gpu.GraphicsCards {
		name := "Unknown"
		if card.DeviceInfo != nil && card.DeviceInfo.Product != nil {
			name = card.DeviceInfo.Product.Name
		}
		gpus = append(gpus, GPUInfo{Name: name})
	}
	return gpus
}
