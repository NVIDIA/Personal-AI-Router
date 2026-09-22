// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import (
	"errors"
	"log/slog"
	"os"
	"syscall"
)

// syncDir flushes a directory entry so a rename into it survives a crash.
// os.Rename is atomic with respect to readers, but nothing guarantees the new
// entry has reached stable storage; ext4's auto_da_alloc and APFS hide that,
// XFS, btrfs, and networked home directories do not.
//
// A filesystem that rejects the flush outright is not a failure to report. The
// rename has already committed by the time this runs, so the file on disk is
// correct; turning EINVAL into an error would surface a PATH warning on install
// or a cleanup error on uninstall for a write that succeeded. CIFS and 9p do
// exactly that — network home directories and WSL interop paths, which is the
// case the flush above is aimed at.
func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	if errors.Is(syncErr, syscall.EINVAL) || errors.Is(syncErr, syscall.ENOTSUP) {
		slog.Debug("filesystem does not support flushing a directory", "dir", dir, "err", syncErr)
		syncErr = nil
	}
	return errors.Join(syncErr, f.Close())
}
