// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
    parseDiagnosticMPIReconcileRequest,
    parseDiagnosticMPIReconcileResult
} from '@/shared/utils/diagnostic-mpi-reconcile'

const operationId = 'a'.repeat(32)
const completed = {
    state: 'completed',
    recoveryRequired: false,
    operations: [
        {
            nodeId: 'node-a',
            operationId,
            ownerNodeId: 'node-a',
            state: 'cleanup-confirmed',
            cleanupConfirmed: true,
            message: 'Owned diagnostic resources are cleaned up.'
        }
    ]
}

describe('diagnostic MPI cleanup contract', () => {
    it('accepts only an empty request and bounded public operation results', () => {
        expect(parseDiagnosticMPIReconcileRequest({})).toEqual({})
        expect(parseDiagnosticMPIReconcileResult(completed)).toEqual(completed)
        expect(() => parseDiagnosticMPIReconcileRequest({ password: 'secret' })).toThrow(/fields/i)
        expect(() =>
            parseDiagnosticMPIReconcileResult({
                ...completed,
                operations: [{ ...completed.operations[0], stderr: 'private failure' }]
            })
        ).toThrow(/fields/i)
    })

    it('rejects contradictory cleanup state and duplicate operation identities', () => {
        expect(() =>
            parseDiagnosticMPIReconcileResult({
                ...completed,
                operations: [{ ...completed.operations[0], cleanupConfirmed: false }]
            })
        ).toThrow(/contradicts/i)
        expect(() =>
            parseDiagnosticMPIReconcileResult({
                ...completed,
                operations: [completed.operations[0], completed.operations[0]]
            })
        ).toThrow(/duplicate/i)
    })

    it('accepts only backend-owned failure codes and redacted public messages', () => {
        const failed = {
            state: 'recovery-required',
            recoveryRequired: true,
            operations: [
                {
                    nodeId: 'node-a',
                    state: 'recovery-required',
                    cleanupConfirmed: false,
                    code: 'inventory-unconfirmed',
                    message:
                        'PAIR could not confirm the complete retained diagnostic inventory on this node.'
                }
            ]
        }
        expect(parseDiagnosticMPIReconcileResult(failed)).toEqual(failed)
        expect(() =>
            parseDiagnosticMPIReconcileResult({
                ...completed,
                operations: [{ ...completed.operations[0], code: 'cleanup-unconfirmed' }]
            })
        ).toThrow(/cannot carry/i)
        expect(() =>
            parseDiagnosticMPIReconcileResult({
                ...failed,
                operations: [{ ...failed.operations[0], code: undefined }]
            })
        ).toThrow(/failure code/i)
        expect(() =>
            parseDiagnosticMPIReconcileResult({
                ...failed,
                operations: [{ ...failed.operations[0], message: 'password=private' }]
            })
        ).toThrow(/public message/i)
    })

    it('keeps one shared operation legible on each participant', () => {
        const result = parseDiagnosticMPIReconcileResult({
            ...completed,
            operations: [
                completed.operations[0],
                { ...completed.operations[0], nodeId: 'node-b', ownerNodeId: 'node-a' }
            ]
        })
        expect(result.operations.map(operation => operation.nodeId)).toEqual(['node-a', 'node-b'])
    })

    it('accepts the complete three-node retained bound plus one inventory row per node', () => {
        const nodes = ['node-a', 'node-b', 'node-c']
        const operations = nodes.flatMap((nodeId, nodeIndex) => [
            ...Array.from({ length: 128 }, (_, index) => ({
                nodeId,
                operationId: (nodeIndex * 128 + index + 1).toString(16).padStart(32, '0'),
                ownerNodeId: 'node-a',
                state: 'cleanup-confirmed',
                cleanupConfirmed: true,
                message: 'Owned diagnostic resources are cleaned up.'
            })),
            {
                nodeId,
                state: 'recovery-required',
                cleanupConfirmed: false,
                code: 'inventory-unconfirmed',
                message:
                    'PAIR could not confirm the complete retained diagnostic inventory on this node.'
            }
        ])
        expect(
            parseDiagnosticMPIReconcileResult({
                state: 'recovery-required',
                recoveryRequired: true,
                operations
            }).operations
        ).toHaveLength(387)
        expect(() =>
            parseDiagnosticMPIReconcileResult({
                state: 'recovery-required',
                recoveryRequired: true,
                operations: [
                    ...operations,
                    {
                        nodeId: 'node-d',
                        state: 'recovery-required',
                        cleanupConfirmed: false,
                        code: 'inventory-unconfirmed',
                        message:
                            'PAIR could not confirm the complete retained diagnostic inventory on this node.'
                    }
                ]
            })
        ).toThrow(/exceeds/i)
    })
})

const mocks = vi.hoisted(() => ({
    supervisor: { ready: true, callProcess: vi.fn() }
}))
vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))

import { diagnosticMPIReconcileHandlers } from '@/electron/service-bridge/diagnostic-mpi-reconcile-handlers'

describe('diagnostic MPI cleanup bridge', () => {
    beforeEach(() => {
        mocks.supervisor.ready = true
        mocks.supervisor.callProcess.mockReset()
    })

    it('sends only the empty product request and parses the public result', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce(completed)
        const result = await diagnosticMPIReconcileHandlers['engine:diagnostic-mpi-reconcile']({})
        expect(result).toEqual(completed)
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:diagnostic-mpi-reconcile',
            {},
            185_000
        )

        await expect(
            diagnosticMPIReconcileHandlers['engine:diagnostic-mpi-reconcile']({
                password: 'secret'
            } as never)
        ).rejects.toThrow(/fields/i)
        expect(mocks.supervisor.callProcess).toHaveBeenCalledTimes(1)
    })
})
