// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path"
	"strings"
)

var errDiagnosticManagedTrust = errors.New("Managed input ownership or path permissions could not be verified. For PAIR-managed files, use Update enrolled devices to review repair; otherwise ask the device administrator.")
var errDiagnosticManagedTrustPlatform = errors.New("Managed input trust checks require Linux; this platform cannot admit native NCCL execution.")

// Match mpi_socket_native.Files: application/artifact ancestry never redirects;
// only the existing fixed system-tool catalog may resolve root-owned links.
func diagnosticManagedTrustedInput(filename string) (string, os.FileInfo, error) {
	uid, supported := diagnosticManagedTrustCurrentUID()
	if !supported {
		return "", nil, errDiagnosticManagedTrustPlatform
	}
	return diagnosticManagedTrustPath(filename, uid, os.Lstat, os.Readlink, diagnosticManagedTrustFileUID)
}

func diagnosticManagedTrustPath(filename string, uid int, lstat func(string) (os.FileInfo, error), readlink func(string) (string, error), owner func(os.FileInfo) (int, bool)) (string, os.FileInfo, error) {
	if !diagnosticManagedPath(filename) {
		return "", nil, errDiagnosticManagedTrust
	}
	system := false
	for _, fixed := range diagnosticMPISystemToolPaths() {
		if filename == fixed {
			system = true
			break
		}
	}
	current := filename
	for attempt := 0; attempt < 20; attempt++ {
		parts := strings.Split(strings.TrimPrefix(current, "/"), "/")
		resolved, changed := "/", false
		for position := -1; position < len(parts); position++ {
			if position >= 0 {
				resolved = path.Join(resolved, parts[position])
			}
			info, err := lstat(resolved)
			if err != nil {
				return "", nil, errDiagnosticManagedTrust
			}
			actual, known := owner(info)
			if !known || (actual != 0 && (system || actual != uid)) {
				return "", nil, errDiagnosticManagedTrust
			}
			if info.Mode()&os.ModeSymlink != 0 {
				if !system {
					return "", nil, errDiagnosticManagedTrust
				}
				target, err := readlink(resolved)
				if err != nil || target == "" {
					return "", nil, errDiagnosticManagedTrust
				}
				if !path.IsAbs(target) {
					target = path.Join(path.Dir(resolved), target)
				}
				current = path.Join(append([]string{target}, parts[position+1:]...)...)
				changed = true
				break
			}
			final := position == len(parts)-1
			if info.Mode().Perm()&0022 != 0 || (!final && !info.IsDir()) || (final && !info.Mode().IsRegular()) {
				return "", nil, errDiagnosticManagedTrust
			}
			if final {
				return current, info, nil
			}
		}
		if !changed {
			return "", nil, errDiagnosticManagedTrust
		}
	}
	return "", nil, errDiagnosticManagedTrust
}
