// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"testing"
)

// configSubdir is the two-level vendor/product path appdir appends under
// whichever base the platform resolves. Mirrored from nvpair-shared/appdir
// (orgDir, appDir) because a test that plants a file where a child manager will
// look has to reproduce that layout exactly.
const configSubdir = "Nvidia Corporation/Personal AI Router"

// isolatedConfig is a throwaway location for a child manager's per-user state.
//
// cfg and home are separate because os.UserConfigDir reads a different variable
// per platform and roots them differently: Windows and Linux use the directory
// named by APPDATA/LOCALAPPDATA/XDG_CONFIG_HOME directly, while macOS derives
// $HOME/Library/Application Support. A caller that needs to plant a file where
// the child will look must therefore write to every candidate — see engineDirs.
type isolatedConfig struct {
	cfg  string
	home string
}

// newIsolatedConfig allocates the throwaway directories for one test.
func newIsolatedConfig(t *testing.T) isolatedConfig {
	t.Helper()
	return isolatedConfig{cfg: t.TempDir(), home: t.TempDir()}
}

// env is the environment that points a child's appdir and home directory at
// this location.
//
// Every variable os.UserConfigDir, os.UserHomeDir and appdir read is set, not
// just the ones the current platform uses, because a missed one is no isolation
// at all on that platform: macOS derives its config from HOME, and on Windows
// USERPROFILE is the home an engine manifest can name, such as LM Studio's
// %USERPROFILE%\.lmstudio.
func (c isolatedConfig) env() map[string]string {
	return map[string]string{
		"APPDATA":         c.cfg,
		"LOCALAPPDATA":    c.cfg,
		"XDG_CONFIG_HOME": c.cfg,
		"HOME":            c.home,
		"USERPROFILE":     c.home,
	}
}

// engineDirs is every directory the child might resolve its engines directory
// to. A test planting a manifest writes it to all of them so the child finds it
// whichever platform it is running on.
func (c isolatedConfig) engineDirs() []string {
	return []string{
		filepath.Join(c.cfg, configSubdir, "engines"),
		filepath.Join(c.home, "Library", "Application Support", configSubdir, "engines"),
	}
}
