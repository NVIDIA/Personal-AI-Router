// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import "testing"

func TestDetectMemoryTotalDarwin(t *testing.T) {
	if total := detectMemoryTotal(); total == 0 {
		t.Fatal("detectMemoryTotal() returned zero on macOS")
	}
}
