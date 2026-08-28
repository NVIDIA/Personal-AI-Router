// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { SupportedPlatform, PlatformDisplayName } from '@/shared/types/platform'
import { PlatformMap } from '@/shared/constants/platform'

const DISPLAY_TO_PLATFORM: Record<string, SupportedPlatform> = Object.fromEntries(
    Object.entries(PlatformMap).map(([k, v]) => [v, k as SupportedPlatform])
) as Record<string, SupportedPlatform>

/** Normalize any process.platform string to one of the three supported values. Non-win32/darwin = linux. */
function normalizePlatform(v: string): SupportedPlatform {
    if (v === 'win32') return 'win32'
    if (v === 'darwin') return 'darwin'
    return 'linux'
}

/** Get display name for a platform: 'win32' -> 'Windows', 'darwin' -> 'MacOS', 'linux' -> 'Linux'. */
export function platformDisplayName(platform: SupportedPlatform): PlatformDisplayName {
    return PlatformMap[platform]
}

/** Resolve display name back to platform: 'Windows' -> 'win32', 'MacOS' -> 'darwin', 'Linux' -> 'linux'. */
export function displayNameToPlatform(name: string): SupportedPlatform {
    return DISPLAY_TO_PLATFORM[name] ?? 'linux'
}

let _platform: SupportedPlatform | undefined

/** Returns the current OS platform, normalized to win32 | darwin | linux. */
export function currentPlatform(): SupportedPlatform {
    if (!_platform) {
        _platform = normalizePlatform(process.platform)
    }
    return _platform
}
