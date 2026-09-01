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
//
// live_test.go has referenced this name since the repository's initial import
// without anything defining it, so the live suite has never compiled. Defining
// it here fixes that as a side effect of needing the same value.
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

// env is the environment that points a child's appdir at this location.
//
// Every variable os.UserConfigDir consults is set, not just the ones the current
// platform happens to use. A missed variable is not weaker isolation — it is no
// isolation at all on that platform, and the child then reads and writes the
// developer's real PAIR data. HOME is the one that used to be missing here:
// macOS ignores APPDATA and XDG_CONFIG_HOME entirely, so a live test spawning a
// real engine manager pointed at the developer's actual engines and could
// install or uninstall against them.
func (c isolatedConfig) env() map[string]string {
	return map[string]string{
		"APPDATA":         c.cfg,
		"LOCALAPPDATA":    c.cfg,
		"XDG_CONFIG_HOME": c.cfg,
		"HOME":            c.home,
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
