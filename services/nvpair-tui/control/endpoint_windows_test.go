// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package control

import (
	"fmt"
	"os"
	"testing"
)

// socketPath gives one test its own named pipe. The pipe namespace is
// machine-wide and flat, so the name carries the pid and the test name to keep
// parallel runs apart.
func socketPath(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf(`\\.\pipe\nvpair-tui-test-%d-%s`, os.Getpid(), sanitize(t.Name()))
}
