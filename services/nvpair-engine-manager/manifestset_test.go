// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io/fs"
	"strings"
	"testing"

	"nvpair-shared/engines"
)

// Managed engines require a manifest whose basename matches their shared Name.
// Adopt-only engines require the opposite: bundling a managed manifest would
// claim lifecycle authority PAIR does not hold. This component embeds
// manifests/*.json, so the compiler cannot enforce either half of that contract.
//
// The failure that produces is the one the package comment describes — an
// engine that discovers and advertises but never resolves a model owner —
// which is invisible until inference is attempted. The existing manifest tests
// validate content, not the set, so this closes the last gap.
func TestBundledManifestSetMatchesEngineTable(t *testing.T) {
	entries, err := fs.ReadDir(bundledManifests, "manifests")
	if err != nil {
		t.Fatal(err)
	}

	have := map[string]bool{}
	for _, e := range entries {
		have[strings.TrimSuffix(e.Name(), ".json")] = true
	}

	for _, engine := range engines.All() {
		if engine.AdoptOnly {
			if have[engine.Name] {
				t.Errorf("adopt-only engine %q must not bundle a managed manifest", engine.Name)
				delete(have, engine.Name)
			}
			continue
		}
		if !have[engine.Name] {
			t.Errorf("no manifests/%s.json for engine %q", engine.Name, engine.Name)
		}
		delete(have, engine.Name)
	}
	for name := range have {
		t.Errorf("manifests/%s.json has no entry in nvpair-shared/engines", name)
	}
}
