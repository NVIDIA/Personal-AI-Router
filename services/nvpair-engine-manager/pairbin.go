// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
)

// pairBinDir is the directory holding PAIR's own binaries, resolved from this
// process's own path rather than configured.
//
// It backs the {pair_bin} manifest placeholder, which exists so a manifest can
// run a helper PAIR ships instead of one the engine vendor publishes. The MLX
// manifest uses it to start mlx-pool -- a front end that keeps several
// mlx_lm.server processes alive and evicts the least recently used -- on the
// port PAIR already treats as the engine.
//
// Deriving it from os.Executable keeps it correct across the two layouts the
// binaries live in (services/build/bin when built from source, the app's
// cli-bin when packaged) with nothing to configure and nothing to get stale.
func pairBinDir() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Dir(exe)
}
