// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineType } from '@/shared/types/engines'
import { EnabledEngineTypes } from '@/shared/constants/engines'
import { EngineCapabilities } from '@/ui/constants/engine-capabilities'
import type { PlatformDisplayName } from '@/shared/types/platform'
import { displayNameToPlatform } from '@/shared/utils/platform'

export const WELCOME_STEP_HEADINGS = ["Welcome, let's get you set up", 'Install engines'] as const

export const WELCOME_STEP_SUB_HEADINGS = ['', 'You can update later by clicking on a node'] as const

export const WELCOME_ENGINE_DEFAULT_SELECTED: Record<EngineType, boolean> = {
    ollama: true,
    'lm-studio': true,
    'llama-cpp': true,
    vllm: true,
    // The welcome step offers installs, and Custom is not an install;
    // `getWelcomeEngineCandidates` already drops it (empty hasInstall), but the
    // saved-selections record has to carry every engine.
    custom: false
}

export function getWelcomeEngineCandidates(os: PlatformDisplayName): EngineType[] {
    const platform = displayNameToPlatform(os)
    return EnabledEngineTypes.filter(t => {
        const caps = EngineCapabilities[t]
        if (!caps.hasInstall.length) return false
        if (caps.hasInstallPath) return false
        return caps.hasInstall.includes(platform)
    })
}
