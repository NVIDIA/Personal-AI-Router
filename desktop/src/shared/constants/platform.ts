// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Single source of truth for platform identity.
 * Key = Node.js process.platform value (code-level).
 * Value = human-readable display name (used on wire, UI, proto OsType).
 */
export const PlatformMap = {
    win32: 'Windows',
    darwin: 'MacOS',
    linux: 'Linux'
} as const
