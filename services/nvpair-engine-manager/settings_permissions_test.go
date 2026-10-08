// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSettingsOverrideRestrictsExistingPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	e := settingsExecutor(t, false)
	require.NoError(t, os.Chmod(e.overrideDir, 0755))
	path := filepath.Join(e.overrideDir, "fake.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"engine":"fake"}`), 0644))
	environment := []string{"PAIR_TEST=private"}
	require.NoError(t, e.persistRuntimeConfig("fake", 54321, nil, &environment))
	for path, want := range map[string]os.FileMode{e.overrideDir: 0700, path: 0600} {
		info, err := os.Stat(path)
		require.NoError(t, err)
		{
			got := info.Mode().Perm()
			require.Equal(t, want, got, "permissions for %s", path)
		}
	}
}
