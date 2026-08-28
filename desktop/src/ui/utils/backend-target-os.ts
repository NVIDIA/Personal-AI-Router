// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { PlatformDisplayName } from '@/shared/types/platform'
import { displayNameToPlatform } from '@/shared/utils/platform'
import type { EngineType } from '@/shared/types/engines'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'

/** Whether PAIR can auto-install this backend on the given cluster OS. */
export function canAutoInstallBackendForOs(
    backendType: EngineType,
    targetOs: PlatformDisplayName
): boolean {
    const targetPlatform = displayNameToPlatform(targetOs)
    const platforms = EngineCapabilities[backendType].hasInstall
    return platforms.includes(targetPlatform)
}
