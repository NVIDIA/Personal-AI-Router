// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLatestLLMSter(t *testing.T) {
	root := t.TempDir()
	cli := filepath.Join(root, "bin", "lms.exe")
	install := func(version string) string {
		dir := filepath.Join(root, "llmster", version)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		path := filepath.Join(dir, "llmster.exe")
		require.NoError(t, os.WriteFile(path, nil, 0o755))
		return path
	}

	assert.Empty(t, latestLLMSter(cli), "nothing installed")

	install("0.0.9-2")
	install("0.0.27-1")
	newest := install("0.0.27-3")
	// A version directory without the executable is not an install.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "llmster", "0.1.0-1"), 0o755))

	assert.Equal(t, newest, latestLLMSter(cli), "numeric, not lexical, version order")
}

func TestCompareVersionParts(t *testing.T) {
	assert.Positive(t, compareVersionParts(versionParts("0.0.27-1"), versionParts("0.0.9-2")))
	assert.Negative(t, compareVersionParts(versionParts("0.0.27"), versionParts("0.0.27-1")))
	assert.Zero(t, compareVersionParts(versionParts("1.2"), versionParts("1.2")))
}
