// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !darwin && !windows

package main

import "fmt"

func platformVLLMFilesystem(string) (uint64, string, error) {
	return 0, "", fmt.Errorf("vLLM storage review is unsupported on this platform")
}

func platformVLLMAllocatedBytes(string) (uint64, uint64, error) {
	return 0, 0, fmt.Errorf("vLLM allocated storage review is unsupported on this platform")
}
