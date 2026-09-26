// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { IFabricApi } from '@/ui/api/fabric-api'
import type {
    CableRetainedRuns,
    CableReview,
    CableRun,
    FabricOperation,
    FabricReview
} from '@/shared/types/fabric'
import { bindFabricApi, useFabricStore, watchFabric } from '@/ui/stores/fabric.store'

const reviewId = 'cable-review-a'
const operationId = 'a'.repeat(32)
const cableReview: CableReview = {
    reviewId,
    ownerNodeId: 'node-a',
    targets: [],
    available: true,
    remainingMs: 20_000
}
const cableRun: CableRun = {
    runId: operationId,
    reviewId,
    ownerNodeId: 'node-a',
    revision: 1,
    state: 'completed',
    targets: [],
    edges: [],
    result: 'incomplete',
    directness: 'unverified',
    remainingMs: 0,
    freshnessRemainingMs: 0,
    cleanupConfirmed: true,
    startedAt: 1,
    finishedAt: 2,
    message: 'No complete match.'
}
const fabricReview: FabricReview = {
    schemaVersion: 1,
    reviewId: operationId,
    ownerNodeId: 'node-a',
    recipeId: 'spark-two-node-temporary-addresses-v1',
    persistence: 'until-reboot',
    state: 'ready',
    executable: true,
    remainingMs: 20_000,
    targets: [],
    blockers: [],
    effectsApplied: false
}
const fabricRefusal: FabricOperation = {
    schemaVersion: 1,
    operationId,
    reviewId: operationId,
    ownerNodeId: 'node-a',
    state: 'not-started',
    targets: [],
    cleanupConfirmed: true,
    effectsApplied: false,
    message: 'No setup started.',
    createdAt: 1,
    expiresAt: 0
}

function releasedCableRun(): CableRun {
    return {
        ...cableRun,
        state: 'failed',
        cleanupConfirmed: false,
        cleanupRecovery: {
            attemptId: 'b'.repeat(32),
            reviewId: 'cable-cleanup-review-a',
            revision: 2,
            state: 'released',
            holdReleased: true,
            code: 'released',
            message: 'Cleanup hold released.',
            startedAt: 3,
            finishedAt: 4,
            targets: []
        }
    }
}

function retainedHold(runId = operationId): CableRetainedRuns {
    return {
        ownerNodeId: 'node-a',
        held: true,
        limited: false,
        runs: [
            {
                runId,
                reviewId,
                ownerNodeId: 'node-a',
                revision: 1,
                state: 'failed',
                cleanupConfirmed: false,
                startedAt: 1,
                nodeIds: ['node-a'],
                ports: []
            }
        ]
    }
}

describe('fabric store authority', () => {
    let api: Record<keyof IFabricApi, ReturnType<typeof vi.fn>>

    beforeEach(() => {
        useFabricStore.getState().reset()
        api = {
            getInventory: vi.fn(),
            reviewCable: vi.fn(),
            approveCable: vi.fn(),
            getCableStatus: vi.fn(),
            cancelCable: vi.fn(),
            getRetainedCableRuns: vi.fn(),
            reviewCableCleanup: vi.fn(),
            verifyCableCleanup: vi.fn(),
            cancelCableCleanup: vi.fn(),
            reviewFabric: vi.fn(),
            approveFabric: vi.fn(),
            getFabricStatus: vi.fn(),
            cancelFabric: vi.fn(),
            recoverFabric: vi.fn(),
            getRetainedFabricOperations: vi.fn()
        }
        bindFabricApi(api as unknown as IFabricApi)
    })

    afterEach(() => {
        vi.useRealTimers()
    })

    it('never resends a lost cable approval and settles only from exact review-id status', async () => {
        useFabricStore.setState({ cableReview })
        api.approveCable.mockRejectedValueOnce(new Error('reply lost'))
        api.getCableStatus.mockResolvedValueOnce(cableRun)

        await useFabricStore.getState().approveCable()

        expect(api.approveCable).toHaveBeenCalledExactlyOnceWith(reviewId)
        expect(api.getCableStatus).toHaveBeenCalledExactlyOnceWith({ reviewId })
        expect(useFabricStore.getState().cableRun).toEqual(cableRun)
        expect(useFabricStore.getState().cableKnown).toBe(true)
    })

    it('does not invent applying state when approval is lost and adopts status refusal as truth', async () => {
        useFabricStore.setState({ fabricReview })
        api.approveFabric.mockRejectedValueOnce(new Error('reply lost'))
        api.getFabricStatus.mockResolvedValueOnce(fabricRefusal)

        await useFabricStore.getState().approveFabric(false)

        expect(api.approveFabric).toHaveBeenCalledExactlyOnceWith(operationId, false)
        expect(api.getFabricStatus).toHaveBeenCalledExactlyOnceWith(operationId)
        expect(useFabricStore.getState().fabricOperation).toEqual(fabricRefusal)
        expect(useFabricStore.getState().fabricKnown).toBe(true)
    })

    it('adopts the newest retained fabric operation that still needs cleanup', async () => {
        const applied: FabricOperation = {
            ...fabricRefusal,
            state: 'recovery-required',
            cleanupConfirmed: false,
            effectsApplied: true,
            message: 'Confirm cleanup.'
        }
        api.getRetainedCableRuns.mockResolvedValueOnce({
            ownerNodeId: 'node-a',
            held: false,
            limited: false,
            runs: []
        })
        api.getRetainedFabricOperations.mockResolvedValueOnce({ operations: [applied] })

        await useFabricStore.getState().refreshRetained()

        expect(useFabricStore.getState().fabricOperation).toEqual(applied)
        expect(useFabricStore.getState().fabricKnown).toBe(true)
    })

    it('replaces a cleaned current operation with the older retained cleanup hold', async () => {
        const active: FabricOperation = {
            ...fabricRefusal,
            state: 'active',
            cleanupConfirmed: false,
            effectsApplied: true,
            message: 'Fabric active.'
        }
        const cleaned: FabricOperation = {
            ...active,
            state: 'cancelled',
            cleanupConfirmed: true,
            message: 'Cleanup confirmed.'
        }
        const older: FabricOperation = {
            ...active,
            operationId: 'b'.repeat(32),
            reviewId: 'b'.repeat(32),
            state: 'recovery-required',
            createdAt: 0,
            message: 'Older cleanup remains required.'
        }
        useFabricStore.setState({ fabricOperation: active, fabricKnown: true })
        api.cancelFabric.mockResolvedValueOnce(cleaned)
        api.getFabricStatus.mockResolvedValueOnce(cleaned)
        api.getRetainedFabricOperations.mockResolvedValueOnce({ operations: [older] })

        await useFabricStore.getState().cancelFabric()

        expect(api.cancelFabric).toHaveBeenCalledExactlyOnceWith(operationId)
        expect(useFabricStore.getState().fabricOperation).toEqual(older)
        expect(useFabricStore.getState().fabricKnown).toBe(true)
    })

    it('clears a current operation removed by another controller', async () => {
        useFabricStore.setState({ fabricOperation: fabricRefusal, fabricKnown: true })
        api.getRetainedCableRuns.mockResolvedValueOnce({
            ownerNodeId: 'node-a',
            held: false,
            limited: false,
            runs: []
        })
        api.getRetainedFabricOperations.mockResolvedValueOnce({ operations: [] })

        await useFabricStore.getState().refreshRetained()

        expect(useFabricStore.getState().fabricOperation).toBeNull()
        expect(useFabricStore.getState().fabricKnown).toBe(true)
    })

    it('ignores a stale empty retained response that crosses fabric approval', async () => {
        let releaseRetained!: (value: { operations: FabricOperation[] }) => void
        const staleRetained = new Promise<{ operations: FabricOperation[] }>(resolve => {
            releaseRetained = resolve
        })
        const active: FabricOperation = {
            ...fabricRefusal,
            state: 'active',
            cleanupConfirmed: false,
            effectsApplied: true,
            message: 'Fabric active.'
        }
        api.getRetainedCableRuns.mockResolvedValueOnce({
            ownerNodeId: 'node-a',
            held: false,
            limited: false,
            runs: []
        })
        api.getRetainedFabricOperations.mockReturnValueOnce(staleRetained)
        const staleRefresh = useFabricStore.getState().refreshRetained()
        await vi.waitFor(() => expect(api.getRetainedFabricOperations).toHaveBeenCalledOnce())

        useFabricStore.setState({ fabricReview })
        api.approveFabric.mockResolvedValueOnce(active)
        api.getFabricStatus.mockResolvedValueOnce(active)
        await useFabricStore.getState().approveFabric(false)
        releaseRetained({ operations: [] })
        await staleRefresh

        expect(useFabricStore.getState().fabricOperation).toEqual(active)
        expect(useFabricStore.getState().fabricKnown).toBe(true)
    })

    it('discovers an external retained operation while the local slot is empty', async () => {
        vi.useFakeTimers()
        const external: FabricOperation = {
            ...fabricRefusal,
            operationId: 'c'.repeat(32),
            reviewId: 'c'.repeat(32),
            state: 'recovery-required',
            cleanupConfirmed: false,
            effectsApplied: true,
            message: 'External cleanup required.'
        }
        api.getRetainedCableRuns.mockResolvedValue({
            ownerNodeId: 'node-a',
            held: false,
            limited: false,
            runs: []
        })
        api.getRetainedFabricOperations
            .mockResolvedValueOnce({ operations: [] })
            .mockResolvedValue({ operations: [external] })

        const stop = watchFabric()
        await vi.advanceTimersByTimeAsync(0)
        for (let i = 0; i < 8 && !useFabricStore.getState().fabricOperation; i++)
            await Promise.resolve()

        expect(useFabricStore.getState().fabricOperation).toEqual(external)
        stop()
        await vi.advanceTimersByTimeAsync(2000)
    })

    it('refreshes a stale retained hold after another controller releases cleanup', async () => {
        const releasedRun = releasedCableRun()
        useFabricStore.setState({ retained: retainedHold(), cableRun })
        api.getCableStatus.mockResolvedValueOnce(releasedRun)
        api.getRetainedCableRuns.mockResolvedValueOnce({
            ownerNodeId: 'node-a',
            held: false,
            limited: false,
            runs: []
        })

        await useFabricStore.getState().refreshCable({ runId: operationId })

        expect(api.getCableStatus).toHaveBeenCalledExactlyOnceWith({ runId: operationId })
        expect(api.getRetainedCableRuns).toHaveBeenCalledOnce()
        expect(useFabricStore.getState().cableRun).toEqual(releasedRun)
        expect(useFabricStore.getState().retained).toEqual({
            ownerNodeId: 'node-a',
            held: false,
            limited: false,
            runs: []
        })
    })

    it('preserves an unrelated retained hold when this run was released', async () => {
        const releasedRun = releasedCableRun()
        const unrelated = retainedHold('d'.repeat(32))
        useFabricStore.setState({ retained: unrelated, cableRun })
        api.getCableStatus.mockResolvedValueOnce(releasedRun)

        await useFabricStore.getState().refreshCable({ runId: operationId })

        expect(api.getRetainedCableRuns).not.toHaveBeenCalled()
        expect(useFabricStore.getState().retained).toEqual(unrelated)
    })
})
