// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStrataManifest_DeclaresCommandLifecycleAndGuardedModelOps pins the
// bundled manifest's shape, so a manifest edit cannot quietly drop the
// lifecycle guarantees the Strata wiring relies on: a detached daemon
// stopped by its own CLI, readiness by HTTP health, a launch text PAIR
// may edit only through the reviewed controls, and model operations that
// match what Strata actually offers (no managed delete, no in-process
// unload).
func TestStrataManifest_DeclaresCommandLifecycleAndGuardedModelOps(t *testing.T) {
	reg := NewRegistry()
	require.NoError(t, reg.LoadFS(bundledManifests, "manifests"))
	manifest, ok := reg.Get("strata")
	require.True(t, ok, "no bundled strata manifest")
	require.NotEmpty(t, manifest.Platforms)

	for key, platform := range manifest.Platforms {
		t.Run(key, func(t *testing.T) {
			assert.Equal(t, "command", platform.Runtime.Mode,
				"strata daemonizes itself; PAIR must not own the process")
			require.NotEmpty(t, platform.Runtime.CLI)
			require.NotEmpty(t, platform.Runtime.Start, "command mode needs a start command")
			require.NotNil(t, platform.Runtime.Stop)
			assert.Equal(t, []string{"{cli}", "launch", "--stop"}, platform.Runtime.Stop.Cmd,
				"stop goes through Strata's own CLI")
			require.NotNil(t, platform.Runtime.Ready)
			assert.Contains(t, platform.Runtime.Ready.HTTP, "/health",
				"readiness is the server's health endpoint")
			require.NotNil(t, platform.Runtime.EditableLaunch)
			// Never an API key the health probe could not present. The access
			// kind itself is editable text (local or lan), not a fixed arg.
			assert.Contains(t, platform.Runtime.EditableLaunch.FixedArgs, "--no-api-key")
			require.NotEmpty(t, platform.Runtime.Start)
			assert.Contains(t, platform.Runtime.Start[0], "--access")
			assert.Contains(t, platform.Runtime.Start[0], "local")
		})
	}

	load := manifest.Actions["load_model"]
	require.NotNil(t, load.LaunchModel, "Strata's model is a launch argument")
	assert.Equal(t, "--model", load.LaunchModel.Flag)
	assert.Equal(t, "--family", load.LaunchModel.FamilyFlag,
		"a family/quant id must write both launch flags")

	assert.True(t, manifest.Actions["unload_model"].StopEngine,
		"one model per instance: unload stops it")

	list := manifest.Actions["list_models"]
	assert.Equal(t, []string{"{cli}", "models", "list", "--output", "json"}, list.Cmd)
	require.NotNil(t, list.Result)
	assert.Equal(t, "models", list.Result.Array)
	assert.Equal(t, "id", list.Result.Field)

	require.NotNil(t, manifest.Actions["loaded_models"].HTTP)
	assert.Equal(t, "/v1/models", manifest.Actions["loaded_models"].HTTP.Path)

	pull := manifest.Actions["pull_model"]
	require.NotEmpty(t, pull.Cmd)
	assert.Equal(t, []string{"{cli}", "install", "--family", "{family}", "--model", "{model}", "--yes"}, pull.Cmd,
		"download runs while the engine is down, named by family and quant")

	_, hasDelete := manifest.Actions["delete_model"]
	assert.False(t, hasDelete,
		"Strata model files live in a user-chosen library; there is no managed delete yet")
}
