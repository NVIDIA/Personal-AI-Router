// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsOurEngineImage(t *testing.T) {
	bin := filepath.Join("/opt", "nvpair", "ollama.exe")
	test := func(name, image string, want bool) {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, want, isOurEngineImage(image, bin))
		})
	}
	test("matching binary", bin, true)
	test("case insensitive", `/opt/nvpair/OLLAMA.EXE`, true)
	test("deleted binary", bin+` (deleted)`, true)
	test("other binary", `/opt/nvpair/other.exe`, false)
	test("empty image", ``, false)
	require.False(t, isOurEngineImage(bin, ``), "empty binPath must not match")
}

func TestIsManagedInstallPath(t *testing.T) {
	base := t.TempDir()
	inside := filepath.Join(base, "ollama", "bin", "ollama"+exeExt())
	outside := filepath.Join(t.TempDir(), "ollama"+exeExt())
	require.True(t, isManagedInstallPath(inside, filepath.Join(base, "ollama")), "managed child path (%v)", inside)
	require.False(t, isManagedInstallPath(outside, filepath.Join(base, "ollama")), "external path (%v)", outside)
	require.False(t, isManagedInstallPath("", filepath.Join(base, "ollama")), "empty binary path must not be managed")
}
