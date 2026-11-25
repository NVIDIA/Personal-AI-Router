// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// The desktop and TUI can each own a manager. Keep the lock file beside the
// canonical directory so directory replacement never changes its identity.
// Closing the file releases the lock. Removing it could let two owners lock
// different inodes at the same name.
func lockEngineInstall(dir string) (*os.File, error) {
	if !filepath.IsAbs(dir) {
		return nil, fmt.Errorf("cannot lock a relative engine installation path")
	}
	path := dir + ".update-lock"
	guard, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	entry, err := os.Lstat(path)
	if err == nil && entry.Mode().IsRegular() {
		var file os.FileInfo
		file, err = guard.Stat()
		if err == nil && os.SameFile(file, entry) {
			if err = tryEngineInstallLock(guard); err == nil {
				return guard, nil
			}
		}
	}
	guard.Close()
	return nil, fmt.Errorf("engine installation lock is busy or invalid: %s", path)
}
