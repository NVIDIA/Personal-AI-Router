// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package control

import (
	"path/filepath"
	"testing"
)

// socketPath gives one test its own endpoint, short enough to stay inside the
// platform's sun_path limit.
func socketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(shortTempDir(t), "tui.sock")
}
