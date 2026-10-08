// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io/fs"
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
func TestBundledManifestSetMatchesEngineTable(t *testing.T) {
	entries, err := fs.ReadDir(bundledManifests, "manifests")
	require.NoError(t, err)

	have := map[string]bool{}
	for _, e := range entries {
		have[strings.TrimSuffix(e.Name(), ".json")] = true
	}

	for _, engine := range engines.All() {
		assert.Contains(t, have, engine.Name, "engine table entry has no bundled manifest")
		delete(have, engine.Name)
	}
	assert.Empty(t, have, "bundled manifests have no entry in nvpair-shared/engines")
}
