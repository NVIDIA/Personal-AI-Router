// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineProcessStatus, EngineType } from '@/shared/types/engines'

interface EngineControlFacts {
    type: EngineType
    processStatus: EngineProcessStatus
    enabled?: boolean
    managed?: boolean
    adopted?: boolean
    installSupported?: boolean
    installReason?: string
}

export function engineEnabled(facts: EngineControlFacts): boolean {
    return facts.type === 'vllm'
        ? facts.enabled === true
        : (facts.enabled ?? facts.processStatus === 'running')
}

export function engineLifecycleAllowed(facts: EngineControlFacts): boolean {
    return facts.type === 'vllm' ? facts.managed === true : facts.adopted !== true
}

export function engineInstallAllowed(facts: EngineControlFacts): boolean {
    return facts.type === 'vllm'
        ? facts.adopted === false && facts.installSupported === true
        : facts.adopted !== true && facts.installSupported !== false
}

export function engineInstallUnavailableReason(facts: EngineControlFacts): string {
    if (facts.processStatus !== 'not-installed' || facts.installSupported !== false) return ''
    return facts.installReason?.trim() || 'Managed installation is unavailable on this host.'
}

/** Remote in-place update is product-owned only for explicitly managed vLLM. */
export function engineUpdateTargetAllowed(
    facts: EngineControlFacts,
    isLocalNode: boolean
): boolean {
    return isLocalNode || (facts.type === 'vllm' && facts.managed === true)
}
