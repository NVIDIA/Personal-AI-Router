// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package ingressauth

import "io/fs"

// checkKeyFileAccess is a no-op on Windows, where ownership and access are
// expressed through ACLs rather than a uid and mode bits; the per-user
// %LOCALAPPDATA% data directory that holds the default key file is the
// protection there.
func checkKeyFileAccess(fs.FileInfo) error { return nil }
