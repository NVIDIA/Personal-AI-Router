// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { DiagnosticMPIReviewRequest } from '@/shared/types/diagnostic-mpi'
import {
    parseDiagnosticMPIManagedInventory,
    parseDiagnosticMPIOperation,
    parseDiagnosticMPIReview
} from '@/shared/utils/diagnostic-mpi'
import { useDiagnosticMPIStore } from '@/ui/stores/diagnostic-mpi.store'
import {
    buildId,
    operationId,
    rawInventory,
    rawOperation,
    rawReview,
    reviewId
} from './diagnostic-mpi-fixtures'

const inventory = parseDiagnosticMPIManagedInventory(rawInventory)
const review = parseDiagnosticMPIReview(rawReview)
const operation = parseDiagnosticMPIOperation(rawOperation)
const api = {
    getDiagnosticMpiInventory: vi.fn(),
    reviewDiagnosticMpi: vi.fn(),
    approveDiagnosticMpi: vi.fn(),
    getDiagnosticMpiStatus: vi.fn(),
    cancelDiagnosticMpi: vi.fn(),
    recoverDiagnosticMpi: vi.fn(),
    closeDiagnosticMpiReview: vi.fn()
}

describe('managed NCCL store', () => {
    beforeEach(() => {
        Object.values(api).forEach(mock => mock.mockReset())
        vi.stubGlobal('window', { pairApi: { setup: api } })
        useDiagnosticMPIStore.getState().reset()
    })

    afterEach(() => vi.unstubAllGlobals())

    it('loads the safe adopted-build inventory', async () => {
        api.getDiagnosticMpiInventory.mockResolvedValueOnce(inventory)
        await useDiagnosticMPIStore.getState().refreshInventory()
        expect(useDiagnosticMPIStore.getState()).toMatchObject({
            inventory,
            pending: null,
            error: ''
        })
    })

    it('reviews and starts one exact node set without resending authority', async () => {
        api.reviewDiagnosticMpi.mockResolvedValueOnce(review)
        api.approveDiagnosticMpi.mockResolvedValueOnce(operation)
        const request: DiagnosticMPIReviewRequest = {
            buildOperationId: buildId,
            nodeIds: ['node-a', 'node-b'],
            network: 'fabric'
        }
        await useDiagnosticMPIStore.getState().requestReview(request)
        expect(api.reviewDiagnosticMpi).toHaveBeenCalledExactlyOnceWith(request)
        await useDiagnosticMPIStore.getState().approveReview()
        expect(api.approveDiagnosticMpi).toHaveBeenCalledExactlyOnceWith({
            reviewId,
            operationId,
            groupId: `pair-smoke-${operationId}`,
            ownerNodeId: 'node-a',
            memberNodeIds: ['node-a', 'node-b'],
            recipeId: 'pair-two-spark-nccl-socket-correctness-v2'
        })
        expect(useDiagnosticMPIStore.getState()).toMatchObject({
            review: null,
            operation,
            operationKnown: true,
            pending: null
        })
    })

    it('recovers and positively closes one retained unstarted review', async () => {
        const reference = {
            reviewId,
            buildOperationId: buildId,
            operationId,
            groupId: `pair-smoke-${operationId}`,
            ownerNodeId: 'node-a',
            memberNodeIds: ['node-a', 'node-b'],
            recipeId: 'pair-two-spark-nccl-socket-correctness-v2' as const
        }
        api.recoverDiagnosticMpi.mockResolvedValueOnce({
            reference,
            operation: null,
            recoveryRequired: false
        })
        api.closeDiagnosticMpiReview.mockResolvedValueOnce({
            operation: null,
            reviewClosed: true
        })
        const selection = { buildOperationId: buildId, nodeIds: ['node-a', 'node-b'] }
        await useDiagnosticMPIStore.getState().recover(selection)
        expect(useDiagnosticMPIStore.getState().recovery?.reference).toEqual(reference)
        await useDiagnosticMPIStore.getState().closeReview()
        expect(api.closeDiagnosticMpiReview).toHaveBeenCalledExactlyOnceWith(reference)
        expect(useDiagnosticMPIStore.getState()).toMatchObject({
            review: null,
            recovery: null,
            operation: null,
            operationKnown: false
        })
    })

    it('never resends one review approval after uncertain settlement', async () => {
        api.reviewDiagnosticMpi.mockResolvedValueOnce(review)
        api.approveDiagnosticMpi.mockRejectedValueOnce(new Error('status unavailable'))
        const selection = { buildOperationId: buildId, nodeIds: ['node-a', 'node-b'] }
        const request: DiagnosticMPIReviewRequest = { ...selection, network: 'management' }
        await useDiagnosticMPIStore.getState().requestReview(request)
        await useDiagnosticMPIStore.getState().approveReview()
        await useDiagnosticMPIStore.getState().approveReview()
        expect(api.approveDiagnosticMpi).toHaveBeenCalledOnce()
        expect(useDiagnosticMPIStore.getState()).toMatchObject({
            review: null,
            approvalAttemptedReviewId: reviewId,
            approvalRecoverySelection: selection,
            operationKnown: false,
            pending: null
        })
        expect(useDiagnosticMPIStore.getState().error).toMatch(/will not be resent/i)

        api.recoverDiagnosticMpi.mockResolvedValueOnce({
            reference: null,
            operation: null,
            recoveryRequired: false
        })
        await useDiagnosticMPIStore.getState().recover({
            buildOperationId: '0'.repeat(32),
            nodeIds: ['node-b', 'node-c']
        })
        expect(api.recoverDiagnosticMpi).toHaveBeenCalledExactlyOnceWith(selection)
        expect(useDiagnosticMPIStore.getState()).toMatchObject({
            approvalAttemptedReviewId: null,
            approvalRecoverySelection: null
        })

        const nextReview = { ...review, reviewId: 'f'.repeat(32) }
        api.reviewDiagnosticMpi.mockResolvedValueOnce(nextReview)
        await useDiagnosticMPIStore.getState().requestReview(request)
        expect(useDiagnosticMPIStore.getState()).toMatchObject({
            review: nextReview,
            approvalAttemptedReviewId: null
        })
    })

    it('drops an effect reply from a previous service generation', async () => {
        let resolve!: (value: typeof review) => void
        api.reviewDiagnosticMpi.mockReturnValueOnce(
            new Promise<typeof review>(done => {
                resolve = done
            })
        )
        const pending = useDiagnosticMPIStore.getState().requestReview({
            buildOperationId: buildId,
            nodeIds: ['node-a', 'node-b'],
            network: 'management'
        })
        useDiagnosticMPIStore.getState().reset()
        resolve(review)
        await pending
        expect(useDiagnosticMPIStore.getState()).toMatchObject({
            review: null,
            operation: null,
            pending: null,
            error: ''
        })
    })
})
