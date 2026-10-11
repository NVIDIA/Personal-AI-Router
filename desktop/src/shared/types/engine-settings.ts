// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

export interface EngineSettingsConfig {
    serverPort: number
    proxyPort: number
    /** Arguments and leading environment assignments, without an executable/subcommand. */
    launchText: string
}

import type { EngineManagerName } from '@/shared/types/engines'

export interface EngineSettingsTarget {
    nodeId: string
    /** Engine-manager spelling — the settings contract is relayed verbatim. */
    engine: EngineManagerName
}

export interface EngineSettingsRequest extends EngineSettingsTarget {
    expectedRevision: number
    requestId?: string
    settings: EngineSettingsConfig
    resolution?: 'server' | 'launch'
}

export interface EngineSettingsSnapshot extends EngineSettingsTarget {
    revision: number
    appliedRevision: number
    sequence: number
    epoch: string
    settings: EngineSettingsConfig
    effectiveServerPort: number
    effectiveProxyPort: number
    running: boolean
    adopted: boolean
    editable: boolean
    reason: string
    format: string
    phase: 'idle' | 'applying' | 'succeeded' | 'failed'
    error: string
    requestId: string
}

export interface EngineSettingsPreview {
    settings: EngineSettingsConfig
    revision: number
    conflict?: { serverPort: number; launchPort: number }
    errors: Record<string, string>
    restart: boolean
    rebind: boolean
}

export interface EngineSettingsReceipt {
    revision: number
    phase: 'applying' | 'succeeded' | 'failed'
}
