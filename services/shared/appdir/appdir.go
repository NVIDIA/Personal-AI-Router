// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package appdir resolves the single per-user data directory for NVIDIA
// Personal AI Router, so every component agrees on one location.
//
// Layout: <base>/Nvidia Corporation/Personal AI Router, where <base> is:
//   - Windows: %LocalAppData% (deliberately non-roaming — this is
//     machine-specific state that must not sync across machines).
//   - Linux:   $XDG_CONFIG_HOME or ~/.config.
//   - macOS:   ~/Library/Application Support.
package appdir

import (
	"os"
	"path/filepath"
	"runtime"
)

const (
	// orgDir / appDir are the two-level vendor/product path segments.
	orgDir = "Nvidia Corporation"
	appDir = "Personal AI Router"
)

// Dir returns the per-user data directory, creating no files. It errors only
// when the platform base directory can't be determined.
func Dir() (string, error) {
	base, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, orgDir, appDir), nil
}

// Path joins elems onto Dir().
func Path(elems ...string) (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(append([]string{d}, elems...)...), nil
}

// baseDir picks the platform base. On Windows we use %LocalAppData% rather than
// os.UserConfigDir (which returns roaming %AppData%), so this node-specific
// state stays local. Elsewhere os.UserConfigDir already returns the right base.
func baseDir() (string, error) {
	if runtime.GOOS == "windows" {
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			return la, nil
		}
	}
	return os.UserConfigDir()
}
