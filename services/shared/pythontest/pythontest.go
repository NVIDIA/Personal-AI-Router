// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package pythontest finds runnable Python interpreters for tests.
package pythontest

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

// Python3 returns the path to a runnable Python 3 interpreter.
// It skips the test if neither python3 nor python can run Python 3.
func Python3(t *testing.T) string {
	t.Helper()
	// Windows may expose Store aliases even without an installed interpreter.
	// Check that the executable runs before returning it to the caller.
	for _, name := range []string{"python3", "python"} {
		python, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		err = exec.CommandContext(ctx, python, "-I", "-c", "import sys; sys.exit(0 if sys.version_info.major == 3 else 1)").Run()
		cancel()
		if err == nil {
			return python
		}
	}
	t.Skip("a runnable Python 3 interpreter (python3 or python) is required")
	return ""
}
