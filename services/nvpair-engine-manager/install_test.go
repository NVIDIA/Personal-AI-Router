// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The executor expands OS environment references in every install argument
// after resolving placeholders, so a "$name" in a script would be replaced
// before the shell ran it. Each platform's script must survive that untouched.
func TestVLLMInstallScripts_SurviveArgumentExpansion(t *testing.T) {
	reg := loadWithOverrides(t, t.TempDir())
	manifest, ok := reg.Get("vllm")
	require.True(t, ok, "vllm manifest not loaded")
	for key, platform := range manifest.Platforms {
		t.Run(key, func(t *testing.T) {
			argv, err := resolveArgs(platform.Install.Run, map[string]string{
				"download":    "/tmp/uv.tar.gz",
				"install_dir": "/tmp/vllm",
			})

			require.NoError(t, err)
			for i := range argv {
				assert.Equal(t, argv[i], expandPathForOS(argv[i], "linux"), "the executor rewrites install argument %d, which breaks the script", i)
			}
		})
	}
}
