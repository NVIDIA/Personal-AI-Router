// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { useConnectionStore } from '@/ui/stores/connection.store'
import {
    bindVllmGroupApi,
    currentVllmGroupOwner,
    useVllmGroupStore,
    vllmMutationBlockReason,
    vllmMutationsBlocked,
    type VllmGroupApi
} from '@/ui/stores/vllm-group.store'
import {
    directSocketReview,
    groupCheck,
    groupReview,
    groupRun,
    heldStatus,
    inactiveStatus,
    qwenGroupReview
} from '../fixtures/vllm-group'

const api = {
    getServingGroupStatus: vi.fn(),
    reviewServingGroup: vi.fn(),
    checkServingGroup: vi.fn(),
    startServingGroup: vi.fn(),
    stopServingGroup: vi.fn(),
    reconcileServingGroup: vi.fn(),
    requestServingGroupCleanup: vi.fn()
} satisfies Record<keyof VllmGroupApi, ReturnType<typeof vi.fn>>

const store = () => useVllmGroupStore.getState()
const selectionFor = (review = groupReview(), parallelism?: 'tensor' | 'pipeline') => ({
    nodeIds: review.plan.members.map(member => member.nodeId),
    model: review.plan.model,
    ...(parallelism ? { parallelism } : {})
})
const reserved = (run = groupRun()) => ({
    activationEnabled: true,
    reserved: true,
    reason: '',
    run
})

async function readInactive() {
    api.getServingGroupStatus.mockResolvedValue(inactiveStatus())
    await store().refresh()
}

describe('managed serving-group journey', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        bindVllmGroupApi(api as unknown as VllmGroupApi)
        useConnectionStore.setState({ connected: true, selfId: 'node-a', clusterId: 'cluster-a' })
        useVllmGroupStore.setState({
            known: false,
            status: null,
            error: null,
            lastRun: null,
            review: null,
            check: null,
            pending: null,
            uncertainStart: false,
            owner: null,
            current: false,
            actionError: ''
        })
    })

    it('holds every vLLM action until a fresh status read succeeds, then releases only the clear case', async () => {
        expect(vllmMutationsBlocked()).toBe(true)
        expect(vllmMutationBlockReason()).toMatch(/not yet known/)
        await readInactive()
        expect(store().known).toBe(true)
        expect(store().owner).toBe(currentVllmGroupOwner())
        expect(vllmMutationsBlocked()).toBe(false)
        api.getServingGroupStatus.mockResolvedValue(heldStatus())
        await store().refresh()
        expect(vllmMutationsBlocked('node-a', 'node-a', false)).toBe(true)
        expect(vllmMutationsBlocked('node-b', 'node-a', false)).toBe(true)
        expect(vllmMutationsBlocked('node-z', 'node-a', false)).toBe(false)
        expect(vllmMutationBlockReason()).toMatch(/fresh native admission required/)
        api.getServingGroupStatus.mockRejectedValue(new Error('-32000: journal unreadable'))
        await store().refresh()
        expect(vllmMutationsBlocked()).toBe(true)
        expect(store().error).toBe('-32000: journal unreadable')
    })

    it('never treats review or readiness as execution: Start stays held until PAIR admits the exact review', async () => {
        await readInactive()
        const review = directSocketReview()
        review.activationEnabled = false
        review.reason = 'distributed vLLM activation requires the supported Linux system owner'
        api.reviewServingGroup.mockResolvedValue(review)
        await store().requestReview(selectionFor())
        expect(api.reviewServingGroup).toHaveBeenCalledWith(selectionFor())
        expect(store().review?.reviewId).toBe(review.reviewId)

        await store().start(review.reviewId)
        expect(api.startServingGroup).not.toHaveBeenCalled()
        expect(store().actionError).toMatch(/Linux system owner/)

        const admitted = { ...review, activationEnabled: true, reason: '' }
        useVllmGroupStore.setState({ review: admitted, actionError: '' })
        await store().start(admitted.reviewId)
        expect(api.startServingGroup).not.toHaveBeenCalled()
        expect(store().actionError).toMatch(/Check participants/)

        api.checkServingGroup.mockResolvedValue(groupCheck(admitted, false))
        await store().checkReview()
        expect(api.checkServingGroup).toHaveBeenCalledWith(admitted.reviewId)
        await store().start(admitted.reviewId)
        expect(api.startServingGroup).not.toHaveBeenCalled()
        expect(store().actionError).toMatch(/not admitted/)

        api.checkServingGroup.mockResolvedValue({
            ...groupCheck(admitted),
            reviewId: 'b'.repeat(32)
        })
        await store().checkReview()
        expect(store().check).toBeNull()
        expect(store().actionError).toMatch(/different review/)

        useVllmGroupStore.setState({
            review: { ...admitted, expiresAt: Date.now() - 1 },
            check: groupCheck(admitted)
        })
        await store().start(admitted.reviewId)
        expect(api.startServingGroup).not.toHaveBeenCalled()
        expect(store().actionError).toMatch(/expired/)
    })

    it('consumes an admitted Start once, settles it by the fresh read, surfaces the refusal verbatim, and never resends', async () => {
        await readInactive()
        const review = groupReview()
        useVllmGroupStore.setState({ review, check: groupCheck(review) })
        api.startServingGroup.mockRejectedValueOnce(
            new Error('-32601: method not found: engine:vllm-group-start')
        )
        api.getServingGroupStatus.mockResolvedValue(inactiveStatus())
        await store().start(review.reviewId)
        expect(api.startServingGroup).toHaveBeenCalledTimes(1)
        expect(store().review).toBeNull()
        expect(store().uncertainStart).toBe(false)
        expect(store().actionError).toContain('-32601: method not found: engine:vllm-group-start')
        expect(store().actionError).toContain('did not retain the Start')
        expect(vllmMutationsBlocked()).toBe(false)

        await store().start(review.reviewId)
        expect(api.startServingGroup).toHaveBeenCalledTimes(1)
    })

    it('binds a successful Start to the retained run PAIR reports and holds until it is read', async () => {
        await readInactive()
        const review = groupReview()
        const run = groupRun('starting', review)
        useVllmGroupStore.setState({ review, check: groupCheck(review) })
        let settle: (status: unknown) => void = () => undefined
        api.startServingGroup.mockResolvedValueOnce(run)
        api.getServingGroupStatus.mockImplementationOnce(
            () =>
                new Promise(resolve => {
                    settle = resolve
                })
        )
        const starting = store().start(review.reviewId)
        await Promise.resolve()
        expect(store().uncertainStart).toBe(true)
        expect(vllmMutationsBlocked()).toBe(true)
        settle(reserved(run))
        await starting
        expect(store().uncertainStart).toBe(false)
        expect(store().lastRun?.runId).toBe(run.runId)
        expect(store().status?.run?.runId).toBe(run.runId)
        expect(store().actionError).toBe('')
        expect(vllmMutationsBlocked()).toBe(true)

        useVllmGroupStore.setState({
            review,
            check: groupCheck(review),
            status: inactiveStatus(),
            lastRun: null
        })
        api.startServingGroup.mockResolvedValueOnce({ ...run, planDigest: 'c'.repeat(64) })
        api.getServingGroupStatus.mockResolvedValue(inactiveStatus())
        await store().start(review.reviewId)
        expect(store().actionError).toMatch(/different or previous operation/)
        expect(store().uncertainStart).toBe(false)
    })

    it('binds Stop and Reconcile to the exact displayed operation, fences concurrent operations, and reports backend refusals verbatim', async () => {
        const run = groupRun('ready')
        api.getServingGroupStatus.mockResolvedValue(reserved(run))
        await store().refresh()
        await store().stop({ runId: run.runId, generation: 2 })
        expect(api.stopServingGroup).not.toHaveBeenCalled()
        expect(store().actionError).toMatch(/changed/)

        useVllmGroupStore.setState({ pending: 'cleanup' })
        await store().stop()
        expect(api.stopServingGroup).not.toHaveBeenCalled()
        expect(store().actionError).toMatch(/in progress/)
        useVllmGroupStore.setState({ pending: null })

        api.stopServingGroup.mockRejectedValueOnce(new Error('group operation generation changed'))
        await store().stop({ runId: run.runId, generation: run.generation })
        expect(store().actionError).toBe('group operation generation changed')

        const stopped = groupRun('stopped')
        api.stopServingGroup.mockResolvedValue({ ...inactiveStatus(), run: stopped })
        api.getServingGroupStatus.mockResolvedValue({ ...inactiveStatus(), run: stopped })
        await store().stop({ runId: run.runId, generation: run.generation })
        expect(api.stopServingGroup).toHaveBeenLastCalledWith({
            runId: run.runId,
            generation: run.generation
        })
        expect(store().status?.run?.cleanupConfirmed).toBe(true)
        expect(vllmMutationsBlocked()).toBe(false)

        await store().stop()
        expect(store().actionError).toMatch(/already confirms cleanup/)

        api.getServingGroupStatus.mockResolvedValue(reserved(run))
        await store().refresh()
        api.reconcileServingGroup.mockResolvedValue(reserved({ ...run, generation: 2 }))
        await store().reconcile({ runId: run.runId, generation: run.generation })
        expect(store().actionError).toMatch(/different operation/)
    })

    it('integrates cleanup with the lifecycle: exact binding, PAIR status as authority, unresolved refusals verbatim', async () => {
        api.getServingGroupStatus.mockResolvedValue(heldStatus())
        await store().refresh()
        const held = heldStatus().run!
        const binding = {
            runId: held.runId,
            generation: held.generation,
            planDigest: held.planDigest
        }

        await store().requestCleanup({ ...binding, generation: 12 })
        expect(api.requestServingGroupCleanup).not.toHaveBeenCalled()
        expect(store().actionError).toMatch(/changed/)

        api.requestServingGroupCleanup.mockRejectedValueOnce(
            new Error('rank node-b closure is unconfirmed; group remains held')
        )
        await store().requestCleanup(binding)
        expect(api.requestServingGroupCleanup).toHaveBeenCalledWith(binding)
        expect(store().actionError).toBe('rank node-b closure is unconfirmed; group remains held')
        expect(store().status?.reserved).toBe(true)
        expect(vllmMutationsBlocked()).toBe(true)

        const cleaned = heldStatus()
        cleaned.reserved = false
        cleaned.reason = ''
        cleaned.run!.state = 'failed'
        cleaned.run!.cleanupConfirmed = true
        cleaned.run!.ranks.forEach(rank => {
            rank.cleanupConfirmed = true
        })
        api.requestServingGroupCleanup.mockResolvedValueOnce(cleaned)
        api.getServingGroupStatus.mockResolvedValue(cleaned)
        await store().requestCleanup(binding)
        expect(store().actionError).toBe('')
        expect(store().status?.run?.cleanupConfirmed).toBe(true)
        expect(store().lastRun?.runId).toBe(held.runId)
        expect(vllmMutationsBlocked()).toBe(false)
        expect(api.getServingGroupStatus).toHaveBeenCalledTimes(3)

        api.getServingGroupStatus.mockResolvedValue(heldStatus())
        await store().refresh()
        api.requestServingGroupCleanup.mockResolvedValueOnce({
            ...cleaned,
            run: { ...cleaned.run!, generation: 14 }
        })
        await store().requestCleanup(binding)
        expect(store().actionError).toMatch(/different operation/)
        expect(vllmMutationsBlocked()).toBe(true)
    })

    it('takes its own follow-up read after an effect even while a poll read is in flight', async () => {
        api.getServingGroupStatus.mockResolvedValue(heldStatus())
        await store().refresh()
        const held = heldStatus().run!
        const binding = {
            runId: held.runId,
            generation: held.generation,
            planDigest: held.planDigest
        }

        let settlePoll: (status: unknown) => void = () => undefined
        api.getServingGroupStatus.mockImplementationOnce(
            () =>
                new Promise(resolve => {
                    settlePoll = resolve
                })
        )
        const poll = store().refresh()
        await Promise.resolve()

        api.requestServingGroupCleanup.mockRejectedValueOnce(
            new Error('cleanup remains unconfirmed; no replacement generation was admitted')
        )
        api.getServingGroupStatus.mockResolvedValue(heldStatus())
        const cleanup = store().requestCleanup(binding)
        await Promise.resolve()
        expect(store().known).toBe(false)

        settlePoll(heldStatus())
        await poll
        await cleanup
        expect(api.getServingGroupStatus).toHaveBeenCalledTimes(3)
        expect(store().known).toBe(true)
        expect(store().status?.reserved).toBe(true)
        expect(store().actionError).toBe(
            'cleanup remains unconfirmed; no replacement generation was admitted'
        )
        expect(vllmMutationsBlocked()).toBe(true)
    })

    it('binds a review to the exact selection, model, coordinator and requested parallelism', async () => {
        await readInactive()
        api.reviewServingGroup.mockResolvedValue(groupReview(3))
        await store().requestReview(selectionFor())
        expect(store().review).toBeNull()
        expect(store().actionError).toMatch(/different selection/)

        api.reviewServingGroup.mockResolvedValue(groupReview(2))
        await store().requestReview(selectionFor())
        expect(store().review).toBeNull()
        expect(store().actionError).toMatch(/different selection/)

        api.reviewServingGroup.mockResolvedValue(groupReview(2, 'pipeline'))
        await store().requestReview(selectionFor())
        expect(store().review?.plan.topology.pipelineParallel).toBe(2)

        api.reviewServingGroup.mockResolvedValue(groupReview(3, 'pipeline'))
        await store().requestReview(selectionFor(groupReview(3), 'tensor'))
        expect(store().review).toBeNull()
        expect(store().actionError).toMatch(/different selection/)

        api.reviewServingGroup.mockResolvedValue(groupReview(3, 'tensor'))
        await store().requestReview(selectionFor(groupReview(3), 'tensor'))
        expect(store().review?.plan.topology.tensorParallel).toBe(3)

        const qwen = qwenGroupReview()
        api.reviewServingGroup.mockResolvedValue(qwen)
        await store().requestReview(selectionFor(qwen, 'tensor'))
        expect(store().actionError).toMatch(/different selection/)
        await store().requestReview(selectionFor(qwen))
        expect(store().review?.plan.topology).toMatchObject({
            tensorParallel: 2,
            pipelineParallel: 1,
            expertParallel: 2
        })

        await store().requestReview({ nodeIds: ['node-b', 'node-a'], model: qwen.plan.model })
        expect(store().actionError).toMatch(/first selected node/)
    })

    it('invalidates on a controller change and reads again under the new owner instead of looping', async () => {
        const run = groupRun('ready')
        api.getServingGroupStatus.mockResolvedValue(reserved(run))
        await store().refresh()
        expect(store().lastRun?.runId).toBe(run.runId)

        useConnectionStore.setState({ connected: true, selfId: 'node-a', clusterId: 'cluster-b' })
        expect(store().known).toBe(false)
        expect(vllmMutationsBlocked()).toBe(true)

        api.getServingGroupStatus.mockResolvedValue(inactiveStatus())
        await store().refresh()
        expect(store().known).toBe(true)
        expect(store().owner).toBe(currentVllmGroupOwner())
        expect(store().lastRun).toBeNull()

        api.getServingGroupStatus.mockResolvedValue(
            reserved(
                groupRun('ready', {
                    ...groupReview(),
                    plan: { ...groupReview().plan, coordinator: 'node-b' }
                })
            )
        )
        await store().refresh()
        expect(store().known).toBe(false)
        expect(store().error).toMatch(/another controller/)
    })
})
