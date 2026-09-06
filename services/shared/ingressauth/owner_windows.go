// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package ingressauth

import "io/fs"

// ownedByProcessUser is a no-op on Windows, where ownership and access are
// expressed through ACLs rather than a uid; the per-user %LOCALAPPDATA% data
// directory that holds the default key file is the protection there.
func ownedByProcessUser(fs.FileInfo) error { return nil }
