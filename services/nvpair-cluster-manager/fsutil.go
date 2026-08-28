// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// renameFile is the replace step of atomicWrite. Tests inject failures here.
var renameFile = os.Rename

const (
	atomicWriteMaxAttempts = 10
	atomicWriteRetryBase   = 50 * time.Millisecond
)

// atomicWrite writes data to path via the standard write-to-tmp + rename
// replace pattern, so a crash mid-write leaves either the previous file intact
// or the new one fully written — never a torn half-file. The temp file lives in
// the same directory as the destination so the rename stays on one filesystem.
//
// On Windows, sibling clustertrust readers can briefly hold admission.json open
// for ReadFile; os.Rename then fails with Access Denied / sharing violation.
// Those errors are retried with short backoff. Temp names are unique so a
// crashed prior .tmp cannot block the next write.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create dir %s: %w", dir, err)
	}
	tmp, err := uniqueTmpPath(path)
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp, data, perm); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	var renameErr error
	for attempt := 1; attempt <= atomicWriteMaxAttempts; attempt++ {
		renameErr = renameFile(tmp, path)
		if renameErr == nil {
			return nil
		}
		if !isTransientReplaceError(renameErr) || attempt == atomicWriteMaxAttempts {
			break
		}
		time.Sleep(atomicWriteRetryBase * time.Duration(attempt))
	}
	_ = os.Remove(tmp)
	return fmt.Errorf("rename %s -> %s: %w", tmp, path, renameErr)
}

func uniqueTmpPath(path string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mint tmp name for %s: %w", path, err)
	}
	return path + "." + hex.EncodeToString(b[:]) + ".tmp", nil
}

// isTransientReplaceError reports whether err is a replace conflict that may
// clear once a concurrent reader releases the destination file. Windows
// ERROR_ACCESS_DENIED (5) and ERROR_SHARING_VIOLATION (32) are matched by
// errno only on Windows; elsewhere we match the platform error text Go surfaces.
func isTransientReplaceError(err error) bool {
	if err == nil {
		return false
	}
	if runtime.GOOS == "windows" {
		var errno syscall.Errno
		if errors.As(err, &errno) && (errno == 5 || errno == 32) {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "access is denied") ||
		strings.Contains(msg, "sharing violation") ||
		strings.Contains(msg, "being used by another process")
}
