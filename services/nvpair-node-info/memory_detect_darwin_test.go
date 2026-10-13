// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDetectMemoryTotalDarwin(t *testing.T) {
	require.NotEqual(t, uint64(0), detectMemoryTotal(), "detectMemoryTotal() returned zero on macOS")
}
