// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/** The renderer receives only display facts and exact selectors, never raw roster principals. */
export interface NCCLReplacementSelector {
    buildOperationId: string
    reviewId: string
    operationId: string
}

export interface NCCLReplacementTarget {
    nodeId: string
    state: string
    reason?: string
    attempt: number
    cleanupConfirmed: boolean
    plan?: {
        nccl: string
        ncclTests: string
        parallelJobs: 2
        maxBuildSeconds: 1800
        maxAttempts: 3
    }
    retryClosed: boolean
}

export interface NCCLReplacementReview extends NCCLReplacementSelector {
    groupId: string
    expiresAt: number
    canBuild: boolean
    targets: NCCLReplacementTarget[]
}

export interface NCCLReplacementOperation extends NCCLReplacementSelector {
    groupId: string
    state: 'running' | 'cancelling' | 'completed' | 'failed' | 'cancelled' | 'interrupted'
    stage: string
    revision: number
    startedAt: number
    finishedAt?: number
    targets: NCCLReplacementTarget[]
    cleanupConfirmed: boolean
    adopted: boolean
    retrySourceStatus: 'current' | 'changed' | 'unknown'
}

export interface NCCLReplacementStatus {
    operation: NCCLReplacementOperation | null
    recoveryRequired: boolean
    reviewClosed?: boolean
    /** Exact new record is durably absent after a completed, non-uncertain operation. */
    registrationAbsent?: boolean
    /** The exact new record was validated and confirmed by Engine Manager. */
    registrationConfirmed?: boolean
}

export interface NCCLReplacementAdoption {
    operation: NCCLReplacementOperation
    adoptedAt: number
}

export function replacementHeld(operation: NCCLReplacementOperation): boolean {
    return (
        ['running', 'cancelling'].includes(operation.state) ||
        ['adopting-runtime', 'cancelling-adoption', 'adoption-publication-unknown'].includes(
            operation.stage
        ) ||
        !operation.cleanupConfirmed ||
        operation.targets.some(target => !target.cleanupConfirmed)
    )
}

export function replacementRetryable(operation: NCCLReplacementOperation): boolean {
    return (
        !operation.adopted &&
        operation.retrySourceStatus === 'current' &&
        ['failed', 'cancelled', 'interrupted'].includes(operation.state) &&
        !replacementHeld(operation) &&
        operation.targets.some(target => target.state !== 'built') &&
        operation.targets.every(
            target =>
                !target.retryClosed &&
                (target.state === 'built' ||
                    target.attempt === 1 ||
                    target.attempt === 2 ||
                    (target.state === 'reviewed' &&
                        target.cleanupConfirmed &&
                        target.attempt === 0))
        )
    )
}
