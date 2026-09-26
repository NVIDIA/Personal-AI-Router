//go:build !linux

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import "os"

func qwen38BundleOwnedMode(info os.FileInfo, directory bool) bool {
	if directory {
		return info.IsDir()
	}
	return info.Mode().IsRegular()
}
