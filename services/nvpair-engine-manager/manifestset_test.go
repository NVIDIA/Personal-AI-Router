// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"nvpair-shared/engines"
)

// nvpair-shared/engines states that an engine's Name must match its manifest
// basename, and argues that declaring the set once turns a mismatch into a
// build error. For the broker, proxy and scheduler it does, because they all
// read the table. This component does not: it embeds manifests/*.json and
// never imports engines, so adding an engine to the table without a manifest
// (or the reverse) still compiles.
//
// The failure that produces is the one the package comment describes — an
// engine that discovers and advertises but never resolves a model owner —
// which is invisible until inference is attempted. The existing manifest tests
// validate content, not the set, so this closes the last gap.
func TestBundledManifestSet_MatchesEngineTable(t *testing.T) {
	entries, err := fs.ReadDir(bundledManifests, "manifests")
	require.NoError(t, err)

	have := map[string]bool{}
	for _, e := range entries {
		have[strings.TrimSuffix(e.Name(), ".json")] = true
	}

	for _, engine := range engines.All() {
		if engine.Unmanaged {
			// An unmanaged engine kind is a routing identity without a
			// launch policy. A bundled manifest would be a promise to
			// install and launch what PAIR by definition never manages, so
			// its absence is the invariant — not a gap.
			assert.NotContains(t, have, engine.Name, "unmanaged engine %s must not ship a launch manifest", engine.Name)
			continue
		}
		assert.Contains(t, have, engine.Name, "engine table entry has no bundled manifest")
		delete(have, engine.Name)
	}
	assert.Empty(t, have, "bundled manifests have no entry in nvpair-shared/engines")
}

// The manifest-set assertion above only covers what ships in the binary. This
// pins the boundary against what a user drops in: a manifest must not be able
// to manufacture a launch policy for an engine the shared table declares
// unmanaged. Directory loads surface it as a hard error naming the file (the
// same treatment an invalid manifest gets); the override overlay, whose policy
// is to skip one bad file rather than brick startup, must still not load it.
func TestRegistry_RefusesManifestsForUnmanagedEngines(t *testing.T) {
	dir := t.TempDir()
	manifest := validManifest()
	manifest.Engine = "custom"
	manifest.DisplayName = "Custom"
	data, err := json.Marshal(manifest)
	require.NoError(t, err, "marshal fixture manifest")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "custom.json"), data, 0600))

	plain := NewRegistry()
	err = plain.LoadDir(dir)
	require.Error(t, err, "a manifest for an unmanaged engine must not load")
	assert.Contains(t, err.Error(), "custom.json", "error must name the file")
	assert.NotContains(t, plain.Names(), "custom")

	overlaid := buildBundledRegistry()
	require.NoError(t, overlaid.LoadOverrideDir(dir), "the overlay path skips bad files, it must not hard-fail")
	assert.NotContains(t, overlaid.Names(), "custom", "a dropped override must not register the unmanaged engine")
}
