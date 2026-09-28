// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"golang.org/x/sys/windows"
	"path/filepath"
	"strings"
)

func onboardingLocalArtifactPath(file string) bool {
	if !filepath.IsAbs(file) || strings.ContainsRune(file, '\x00') {
		return false
	}
	volume := filepath.VolumeName(file)
	if len(volume) != 2 || volume[1] != ':' || strings.Contains(file[2:], ":") {
		return false
	}
	root, err := windows.UTF16PtrFromString(volume + `\`)
	if err != nil {
		return false
	}
	switch windows.GetDriveType(root) {
	case windows.DRIVE_FIXED, windows.DRIVE_REMOVABLE, windows.DRIVE_CDROM, windows.DRIVE_RAMDISK:
		return true
	}
	return false
}
