// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ServiceError } from '@/shared/types/errors'
import type { EngineProcessStatus, EngineStatusData, EngineType } from '@/shared/types/engines'

type WelcomeInstallOutcome = 'pending' | 'complete' | 'failed'

export function isWelcomeEngineInstalled(status: EngineProcessStatus | undefined): boolean {
    return status === 'running' || status === 'stopped'
}

/** Unknown means the authoritative engine snapshot has not identified this engine yet. */
export function isWelcomeEngineInstallable(status: EngineProcessStatus | undefined): boolean {
    return status === 'not-installed'
}

export function areWelcomeEnginesInstalled(
    statusByNode: ReadonlyMap<string, ReadonlyMap<EngineType, EngineStatusData>>,
    nodeId: string,
    candidates: readonly EngineType[]
): boolean {
    const statuses = statusByNode.get(nodeId)
    return candidates.every(engine =>
        isWelcomeEngineInstalled(statuses?.get(engine)?.processStatus)
    )
}

export function hasWelcomeEngineStatuses(
    statusByNode: ReadonlyMap<string, ReadonlyMap<EngineType, unknown>>,
    nodeId: string,
    candidates: readonly EngineType[]
): boolean {
    const statuses = statusByNode.get(nodeId)
    return candidates.every(engine => statuses?.has(engine) === true)
}

export function getWelcomeInstallOutcome(
    statuses: Array<{ engineType: EngineType; status: EngineProcessStatus }>,
    started: ReadonlySet<EngineType>,
    failed: ReadonlySet<EngineType> = new Set()
): WelcomeInstallOutcome {
    if (statuses.every(({ status }) => isWelcomeEngineInstalled(status))) {
        return 'complete'
    }
    if (
        statuses.every(
            ({ engineType, status }) =>
                isWelcomeEngineInstalled(status) ||
                (failed.has(engineType) && status === 'not-installed') ||
                (started.has(engineType) && status === 'not-installed')
        )
    ) {
        return 'failed'
    }
    return 'pending'
}

export function targetForInstallError(
    error: ServiceError,
    targets: readonly EngineType[]
): EngineType | null {
    const explicit = error.engineType === 'lmstudio' ? 'lm-studio' : error.engineType
    if (explicit && targets.includes(explicit as EngineType)) return explicit as EngineType

    const text = `${error.id} ${error.message}`.toLowerCase()
    return (
        targets.find(engineType => {
            const aliases = [
                engineType,
                engineType.replaceAll('-', ''),
                engineType.replaceAll('-', ' ')
            ]
            return aliases.some(alias => text.includes(alias))
        }) ?? null
    )
}

export function isTargetInstallError(
    error: ServiceError,
    nodeId: string,
    targets: readonly EngineType[]
): boolean {
    if (error.nodeId && error.nodeId !== nodeId) return false
    const errorEngine = error.engineType === 'lmstudio' ? 'lm-studio' : error.engineType
    if (errorEngine && !targets.includes(errorEngine as EngineType)) return false
    return (
        error.operation === 'install' ||
        error.id.toLowerCase().includes('install') ||
        /\binstall(?:ing|ation|ed)?\b/i.test(error.message)
    )
}
