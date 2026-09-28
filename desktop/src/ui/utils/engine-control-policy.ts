// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { EngineProcessStatus, EngineType } from '@/shared/types/engines'
import type { VllmServingGroupRoute } from '@/shared/types/vllm-group-status'

interface EngineControlFacts {
    type: EngineType
    processStatus: EngineProcessStatus
    enabled?: boolean
    managed?: boolean
    adopted?: boolean
    installSupported?: boolean
    installReason?: string
    servingGroup?: VllmServingGroupRoute
}

/** A serving-group rank runs vLLM on the node without the one-node enable. */
export function engineEnabled(facts: EngineControlFacts): boolean {
    if (facts.type !== 'vllm') return facts.enabled ?? facts.processStatus === 'running'
    const groupState = facts.servingGroup?.state
    return (
        facts.enabled === true ||
        (facts.processStatus === 'running' &&
            (groupState === 'starting' || groupState === 'started' || groupState === 'ready'))
    )
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
