// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type {
    DiagnosticMPIReconcileCode,
    DiagnosticMPIReconcileOperation,
    DiagnosticMPIReconcileOperationState,
    DiagnosticMPIReconcileRequest,
    DiagnosticMPIReconcileResult,
    DiagnosticMPIReconcileState
} from '@/shared/types/diagnostic-mpi-reconcile'

const token = /^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$/
const operationId = /^[a-f0-9]{32}$/
const maxParticipants = 3
const maxRetainedRunsPerParticipant = 128
const maxInventoryFailuresPerParticipant = 1
const maxReconcileOperations =
    maxParticipants * (maxRetainedRunsPerParticipant + maxInventoryFailuresPerParticipant)
const operationStates = new Set<DiagnosticMPIReconcileOperationState>([
    'cleanup-confirmed',
    'recovery-required'
])
const resultStates = new Set<DiagnosticMPIReconcileState>(['completed', 'recovery-required'])
const codes = new Set<DiagnosticMPIReconcileCode>([
    'cleanup-unconfirmed',
    'binding-unavailable',
    'inventory-unconfirmed',
    'participant-unavailable'
])
const publicMessages = new Set([
    'Owned diagnostic resources are cleaned up.',
    'PAIR could not confirm complete cleanup for this retained diagnostic operation.',
    'PAIR could not confirm the complete retained diagnostic inventory on this node.',
    'The paired participant did not return a confirmed diagnostic cleanup result.'
])

function objectValue(value: unknown, label: string): Record<string, unknown> {
    if (value === null || typeof value !== 'object' || Array.isArray(value)) {
        throw new Error(`Invalid diagnostic cleanup ${label}`)
    }
    return value as Record<string, unknown>
}

function exactKeys(value: Record<string, unknown>, allowed: string[], label: string): void {
    const expected = new Set(allowed)
    if (Object.keys(value).some(key => !expected.has(key))) {
        throw new Error(`Invalid diagnostic cleanup ${label} fields`)
    }
}

function optionalToken(value: unknown, label: string, pattern = token): string | undefined {
    if (value === undefined) return undefined
    if (typeof value !== 'string' || !pattern.test(value)) {
        throw new Error(`Invalid diagnostic cleanup ${label}`)
    }
    return value
}

function parseOperation(value: unknown): DiagnosticMPIReconcileOperation {
    const operation = objectValue(value, 'operation')
    exactKeys(
        operation,
        ['nodeId', 'operationId', 'ownerNodeId', 'state', 'cleanupConfirmed', 'code', 'message'],
        'operation'
    )
    if (typeof operation.nodeId !== 'string' || !token.test(operation.nodeId)) {
        throw new Error('Invalid diagnostic cleanup node identity')
    }
    if (typeof operation.state !== 'string' || !operationStates.has(operation.state as never)) {
        throw new Error('Invalid diagnostic cleanup operation state')
    }
    if (typeof operation.cleanupConfirmed !== 'boolean') {
        throw new Error('Invalid diagnostic cleanup confirmation')
    }
    const state = operation.state as DiagnosticMPIReconcileOperationState
    if (operation.cleanupConfirmed !== (state === 'cleanup-confirmed')) {
        throw new Error('Diagnostic cleanup state contradicts its confirmation')
    }
    if (typeof operation.message !== 'string' || !publicMessages.has(operation.message)) {
        throw new Error('Invalid diagnostic cleanup public message')
    }
    const parsed: DiagnosticMPIReconcileOperation = {
        nodeId: operation.nodeId,
        state,
        cleanupConfirmed: operation.cleanupConfirmed,
        message: operation.message
    }
    const parsedOperationId = optionalToken(
        operation.operationId,
        'operation identity',
        operationId
    )
    const ownerNodeId = optionalToken(operation.ownerNodeId, 'owner identity')
    if (parsedOperationId) parsed.operationId = parsedOperationId
    if (ownerNodeId) parsed.ownerNodeId = ownerNodeId
    let code: DiagnosticMPIReconcileCode | undefined
    if (operation.code !== undefined) {
        if (typeof operation.code !== 'string' || !codes.has(operation.code as never)) {
            throw new Error('Invalid diagnostic cleanup result code')
        }
        code = operation.code as DiagnosticMPIReconcileCode
    }
    if (state === 'cleanup-confirmed') {
        if (!parsedOperationId || !ownerNodeId) {
            throw new Error('Confirmed diagnostic cleanup lacks its operation binding')
        }
        if (code) throw new Error('Confirmed diagnostic cleanup cannot carry a failure code')
    } else if (!code) {
        throw new Error('Unconfirmed diagnostic cleanup lacks its failure code')
    }
    if (code) parsed.code = code
    return parsed
}

export function parseDiagnosticMPIReconcileRequest(value: unknown): DiagnosticMPIReconcileRequest {
    const request = objectValue(value, 'request')
    exactKeys(request, [], 'request')
    return {}
}

export function parseDiagnosticMPIReconcileResult(value: unknown): DiagnosticMPIReconcileResult {
    const result = objectValue(value, 'result')
    exactKeys(result, ['state', 'recoveryRequired', 'operations'], 'result')
    if (typeof result.state !== 'string' || !resultStates.has(result.state as never)) {
        throw new Error('Invalid diagnostic cleanup result state')
    }
    if (typeof result.recoveryRequired !== 'boolean' || !Array.isArray(result.operations)) {
        throw new Error('Invalid diagnostic cleanup result')
    }
    const state = result.state as DiagnosticMPIReconcileState
    if (result.recoveryRequired !== (state === 'recovery-required')) {
        throw new Error('Diagnostic cleanup result contradicts its recovery state')
    }
    if (result.operations.length > maxReconcileOperations) {
        throw new Error('Diagnostic cleanup result exceeds its bound')
    }
    const operations = result.operations.map(parseOperation)
    const operationBindings = new Set<string>()
    for (const operation of operations) {
        if (operation.operationId) {
            const binding = `${operation.nodeId}\u0000${operation.operationId}`
            if (operationBindings.has(binding)) {
                throw new Error('Diagnostic cleanup result contains duplicate operations')
            }
            operationBindings.add(binding)
        }
    }
    if (
        result.recoveryRequired !== operations.some(operation => !operation.cleanupConfirmed) ||
        (!result.recoveryRequired &&
            operations.some(operation => operation.state !== 'cleanup-confirmed'))
    ) {
        throw new Error('Diagnostic cleanup operations do not reconcile')
    }
    return { state, recoveryRequired: result.recoveryRequired, operations }
}
