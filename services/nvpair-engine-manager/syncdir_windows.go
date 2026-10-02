// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// syncDir is a no-op on Windows. There is no directory handle to flush:
// FlushFileBuffers rejects one, and NTFS journals the rename's metadata itself.
func syncDir(string) error { return nil }

const (
	replaceAttempts = 20
	replaceBackoff  = 25 * time.Millisecond
)

// replaceFile renames tmp over path. Go opens files without FILE_SHARE_DELETE,
// so while any process is reading the target — a status snapshot reads PATH
// ownership records without a lock — MoveFileEx fails with a sharing violation.
// Those reads are short, so the rename waits them out instead of failing.
func replaceFile(tmp, path string) error {
	var err error
	for attempt := 0; attempt < replaceAttempts; attempt++ {
		if err = os.Rename(tmp, path); err == nil || !transientReplaceError(err) {
			return err
		}
		time.Sleep(replaceBackoff)
	}
	return err
}

func transientReplaceError(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}
