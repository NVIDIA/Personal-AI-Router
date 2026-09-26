// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
    buildId,
    operationId,
    rawInventory,
    rawOperation,
    rawReview,
    reviewId
} from './diagnostic-mpi-fixtures'

const mocks = vi.hoisted(() => ({
    supervisor: { ready: true, callProcess: vi.fn() }
}))
vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))

import { diagnosticMPIHandlers } from '@/electron/service-bridge/diagnostic-mpi-handlers'
import { JsonRpcResponseError } from '@/electron/service-bridge/json-rpc-subprocess'

const binding = {
    operationId,
    groupId: `pair-smoke-${operationId}`,
    ownerNodeId: 'node-a',
    memberNodeIds: ['node-a', 'node-b'],
    recipeId: 'pair-two-spark-nccl-socket-correctness-v2' as const
}

describe('managed NCCL Desktop bridge', () => {
    beforeEach(() => {
        mocks.supervisor.ready = true
        mocks.supervisor.callProcess.mockReset()
    })

    it('strips private registry material before returning inventory', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce(rawInventory)
        const result = await diagnosticMPIHandlers['engine:diagnostic-managed-runtimes']()
        expect(result.records[0].targets).toEqual([
            { nodeId: 'node-a', local: true },
            { nodeId: 'node-b', local: false }
        ])
        expect(JSON.stringify(result)).not.toMatch(/password|privateKey|hostKey|clusterPin/i)
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:diagnostic-managed-runtimes',
            {},
            30_000
        )
    })

    it('injects only the fixed management Socket review authority', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce(rawReview)
        const result = await diagnosticMPIHandlers['engine:diagnostic-mpi-review']({
            buildOperationId: buildId,
            nodeIds: ['node-b', 'node-a']
        })
        expect(result.reviewId).toBe(reviewId)
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:diagnostic-mpi-review',
            {
                buildOperationId: buildId,
                memberNodeIds: ['node-a', 'node-b'],
                network: 'management',
                dedicatedTestWindow: true
            },
            150_000
        )
    })

    it('never resends approval and settles a lost reply through exact status', async () => {
        mocks.supervisor.callProcess
            .mockRejectedValueOnce(new Error('reply lost'))
            .mockResolvedValueOnce(rawOperation)
        const result = await diagnosticMPIHandlers['engine:diagnostic-mpi-approve']({
            reviewId,
            ...binding
        })
        expect(result.operationId).toBe(operationId)
        expect(mocks.supervisor.callProcess).toHaveBeenCalledTimes(2)
        expect(mocks.supervisor.callProcess).toHaveBeenNthCalledWith(
            1,
            'broker',
            'engine:diagnostic-mpi-approve',
            { ownerNodeId: 'node-a', reviewId },
            110_000
        )
        expect(mocks.supervisor.callProcess).toHaveBeenNthCalledWith(
            2,
            'broker',
            'engine:diagnostic-mpi-status',
            {
                ownerNodeId: 'node-a',
                operationId,
                groupId: `pair-smoke-${operationId}`
            },
            110_000
        )
    })

    it('keeps recipe binding client-side and rejects status recipe drift', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce({
            ...rawOperation,
            recipeId: 'pair-two-spark-nccl-socket-smoke-v1'
        })
        await expect(
            diagnosticMPIHandlers['engine:diagnostic-mpi-status'](binding)
        ).rejects.toThrow(/different selection/i)
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:diagnostic-mpi-status',
            {
                ownerNodeId: 'node-a',
                operationId,
                groupId: `pair-smoke-${operationId}`
            },
            110_000
        )
    })

    it('projects operation text from typed state instead of backend text', async () => {
        for (const message of [
            'access_token: abc123',
            'client_secret: abc123',
            'credentials: abc123',
            'Everything is peachy according to the backend.'
        ]) {
            mocks.supervisor.callProcess.mockResolvedValueOnce({ ...rawOperation, message })
            const result = await diagnosticMPIHandlers['engine:diagnostic-mpi-status'](binding)
            expect(result.message).toBe(
                'PAIR is running the bounded Socket NCCL correctness smoke.'
            )
            expect(JSON.stringify(result)).not.toContain(message)
        }
    })

    it('recovers and closes only the exact retained review reference', async () => {
        const reference = {
            reviewId,
            buildOperationId: buildId,
            ...binding
        }
        mocks.supervisor.callProcess
            .mockResolvedValueOnce({ reference, operation: null, recoveryRequired: false })
            .mockResolvedValueOnce({ operation: null, reviewClosed: true })

        const recovered = await diagnosticMPIHandlers['engine:diagnostic-mpi-recover']({
            buildOperationId: buildId,
            nodeIds: ['node-a', 'node-b']
        })
        expect(recovered.reference).toEqual(reference)
        await expect(
            diagnosticMPIHandlers['engine:diagnostic-mpi-close-review'](reference)
        ).resolves.toEqual({ operation: null, reviewClosed: true })
        expect(mocks.supervisor.callProcess).toHaveBeenNthCalledWith(
            2,
            'broker',
            'engine:diagnostic-mpi-status',
            { ownerNodeId: 'node-a', reviewId, closeUnstartedReview: true },
            110_000
        )
    })

    it('never surfaces arbitrary backend error text', async () => {
        for (const message of [
            'failed(/home/operator/private.log)',
            'open(../private/key)',
            'Authorization: Bearer abc123',
            'access_token: abc123',
            'client_secret: abc123',
            'credentials: abc123',
            'Everything is peachy according to the backend.'
        ]) {
            mocks.supervisor.callProcess.mockRejectedValueOnce(
                new JsonRpcResponseError(`-32000: ${message}`)
            )
            const result = diagnosticMPIHandlers['engine:diagnostic-mpi-review']({
                buildOperationId: buildId,
                nodeIds: ['node-a', 'node-b']
            })
            await expect(result).rejects.toThrow(
                'PAIR could not create the managed NCCL review. No backend diagnostic text was exposed.'
            )
            await expect(result).rejects.not.toThrow(message)
        }
    })
})
