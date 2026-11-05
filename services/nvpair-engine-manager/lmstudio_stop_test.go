// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLMStudioStop_UsesDaemonShutdownOnMacOS(t *testing.T) {
	for _, arch := range []string{"arm64", "amd64"} {
		t.Run(arch, func(t *testing.T) {
			reg := NewRegistry()
			require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
			manifest, ok := reg.Get("lmstudio")
			require.True(t, ok)
			platform, ok := manifest.PlatformFor("darwin", arch)
			require.True(t, ok)
			require.NotNil(t, platform.Runtime.Stop)
			assert.Equal(t, []string{"{cli}", "daemon", "down"}, platform.Runtime.Stop.Cmd,
				"server stop closes HTTP but leaves llmster and loaded models in memory")
		})
	}
}
