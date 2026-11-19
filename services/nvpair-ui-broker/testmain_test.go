// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"testing"
)

// Bare broker fixtures also consult the settings journal during startup. Keep
// all default app-data lookups away from the developer's live configuration.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "nvpair-broker-test-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, key := range []string{"LOCALAPPDATA", "APPDATA", "XDG_CONFIG_HOME", "HOME", "USERPROFILE"} {
		if err := os.Setenv(key, dir); err != nil {
			fmt.Fprintln(os.Stderr, err)
			_ = os.RemoveAll(dir)
			os.Exit(1)
		}
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
