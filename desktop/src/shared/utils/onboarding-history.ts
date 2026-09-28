// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type {
    OnboardingHistoryClassification,
    OnboardingHistoryOperation,
    OnboardingHistorySummary
} from '@/shared/types/onboarding-history'

function objectValue(value: unknown): Record<string, unknown> | null {
    return value !== null && typeof value === 'object' && !Array.isArray(value)
        ? (value as Record<string, unknown>)
        : null
}

function count(value: unknown, field: string): number {
    if (!Number.isSafeInteger(value) || (value as number) < 0) {
        throw new Error(`Invalid setup history ${field}`)
    }
    return value as number
}

function boolean(value: unknown, field: string): boolean {
    if (typeof value !== 'boolean') throw new Error(`Invalid setup history ${field}`)
    return value
}

function classification(value: unknown): OnboardingHistoryClassification {
    if (value === 'history-only' || value === 'current' || value === 'invalid') return value
    throw new Error('Invalid setup history classification')
}

function parseOperation(value: unknown): OnboardingHistoryOperation {
    const row = objectValue(value)
    if (!row || typeof row.operation_id !== 'string' || !/^[a-f0-9]{32}$/.test(row.operation_id)) {
        throw new Error('Invalid setup history operation id')
    }
    if (typeof row.state !== 'string' || row.state.length === 0) {
        throw new Error('Invalid setup history state')
    }
    if (row.mutation_allowed !== false) {
        throw new Error('Setup history unexpectedly grants mutation authority')
    }
    const result: OnboardingHistoryOperation = {
        operationId: row.operation_id,
        state: row.state,
        classification: classification(row.classification),
        targetCount: count(row.target_count, 'target count'),
        mutationAllowed: false
    }
    if (row.finished_at !== undefined) result.finishedAt = count(row.finished_at, 'finish time')
    return result
}

export function parseOnboardingHistory(value: unknown): OnboardingHistorySummary {
    const summary = objectValue(value)
    if (!summary || !Array.isArray(summary.operations)) {
        throw new Error('Invalid setup history response')
    }
    const total = count(summary.total, 'total')
    const historyOnly = count(summary.history_only, 'history-only count')
    const current = count(summary.current, 'current count')
    const invalid = count(summary.invalid, 'invalid count')
    const recoveryRequired = boolean(summary.recovery_required, 'recovery flag')
    const diagnosticRecoveryRequired = boolean(
        summary.diagnostic_recovery_required,
        'diagnostic recovery flag'
    )
    const discoveryBlocked = boolean(summary.discovery_blocked, 'discovery flag')
    if (summary.mutation_supported !== false) {
        throw new Error('Setup history unexpectedly supports mutation')
    }
    if (total !== historyOnly + current + invalid) {
        throw new Error('Setup history counts do not reconcile')
    }
    if (invalid > 0 && (!recoveryRequired || !discoveryBlocked)) {
        throw new Error('Invalid setup history is not fail-closed')
    }
    const operations = summary.operations.map(parseOperation)
    if (operations.length > total) throw new Error('Setup history returned too many rows')
    const ids = new Set<string>()
    const rowCounts = { 'history-only': 0, current: 0, invalid: 0 }
    for (const operation of operations) {
        if (ids.has(operation.operationId)) throw new Error('Setup history contains duplicate ids')
        ids.add(operation.operationId)
        rowCounts[operation.classification] += 1
        if (operation.classification === 'invalid' && (!recoveryRequired || !discoveryBlocked)) {
            throw new Error('Invalid setup history row is not fail-closed')
        }
    }
    if (operations.length === total) {
        if (
            rowCounts['history-only'] !== historyOnly ||
            rowCounts.current !== current ||
            rowCounts.invalid !== invalid
        ) {
            throw new Error('Setup history rows do not reconcile')
        }
    } else if (!recoveryRequired || !discoveryBlocked || invalid === 0) {
        throw new Error('Incomplete setup history inventory is not fail-closed')
    }
    return {
        total,
        historyOnly,
        current,
        invalid,
        recoveryRequired,
        diagnosticRecoveryRequired,
        discoveryBlocked,
        mutationSupported: false,
        operations
    }
}
