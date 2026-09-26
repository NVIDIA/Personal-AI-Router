// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import {
    groupCheck,
    groupReview,
    groupRun,
    heldStatus,
    inactiveStatus,
    qwenGroupReview,
    wire
} from '../fixtures/vllm-group'

const mocks = vi.hoisted(() => ({
    supervisor: { ready: true, callProcess: vi.fn() },
    state: {
        setVllmGroupStatus: vi.fn(),
        holdVllmGroupStatus: vi.fn(),
        getSelfId: vi.fn(() => 'node-a'),
        isEngineCommandAllowed: vi.fn(() => true),
        getEngineInitialState: vi.fn(),
        applyEngineManagerStatus: vi.fn()
    }
}))
vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state
}))

import { vllmGroupHandlers } from '@/electron/service-bridge/vllm-group-handlers'

const MODEL = 'owner/model@' + 'a'.repeat(40)
const OTHER = 'owner/other@' + 'b'.repeat(40)
const selectionFor = (review = groupReview(), parallelism?: 'tensor' | 'pipeline') => ({
    nodeIds: review.plan.members.map(member => member.nodeId),
    model: review.plan.model,
    ...(parallelism ? { parallelism } : {})
})

describe('managed serving-group bridge', () => {
    beforeEach(() => {
        mocks.supervisor.ready = true
        mocks.supervisor.callProcess.mockReset()
        mocks.state.setVllmGroupStatus.mockReset()
        mocks.state.holdVllmGroupStatus.mockReset()
        mocks.state.getSelfId.mockReset().mockReturnValue('node-a')
        mocks.state.isEngineCommandAllowed.mockReset().mockReturnValue(true)
        mocks.state.applyEngineManagerStatus.mockReset()
        mocks.state.getEngineInitialState.mockReset().mockReturnValue({
            statuses: [
                { nodeId: 'node-a', engineType: 'vllm', processStatus: 'stopped', managed: true }
            ],
            models: [
                {
                    nodeId: 'node-a',
                    engineType: 'vllm',
                    models: [
                        { name: MODEL, downloaded: true },
                        { name: OTHER, downloaded: false }
                    ]
                }
            ]
        })
    })

    it.each<'tensor' | 'pipeline'>(['tensor', 'pipeline'])(
        'forwards the exact %s selection and refuses a review for a different selection',
        async parallelism => {
            const review = groupReview(3, parallelism)
            const selection = selectionFor(review, parallelism)
            mocks.supervisor.callProcess.mockResolvedValueOnce(wire(review))
            const result = await vllmGroupHandlers['engine:vllm-group-review']({ selection })
            expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
                'broker',
                'engine:vllm-group-review',
                { selection },
                125_000
            )
            expect(result.plan.topology.tensorParallel).toBe(parallelism === 'tensor' ? 3 : 1)
            mocks.supervisor.callProcess.mockResolvedValueOnce(wire(groupReview(2)))
            await expect(
                vllmGroupHandlers['engine:vllm-group-review']({ selection })
            ).rejects.toThrow(/different selection/)
        }
    )

    it('gives the exact Qwen review its longer content budget', async () => {
        const review = qwenGroupReview()
        mocks.supervisor.callProcess.mockResolvedValueOnce(wire(review))
        await vllmGroupHandlers['engine:vllm-group-review']({ selection: selectionFor(review) })
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:vllm-group-review',
            { selection: selectionFor(review) },
            36 * 60_000
        )
    })

    it('checks by exact reviewId and refuses a check for another review', async () => {
        const review = groupReview()
        mocks.supervisor.callProcess.mockResolvedValueOnce(wire(groupCheck(review, false)))
        const check = await vllmGroupHandlers['engine:vllm-group-check']({
            reviewId: review.reviewId
        })
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:vllm-group-check',
            { reviewId: review.reviewId },
            30_000
        )
        expect(check.activationEnabled).toBe(false)
        mocks.supervisor.callProcess.mockResolvedValueOnce(
            wire({ ...groupCheck(review), reviewId: 'b'.repeat(32) })
        )
        await expect(
            vllmGroupHandlers['engine:vllm-group-check']({ reviewId: review.reviewId })
        ).rejects.toThrow(/different review/)
    })

    it('holds ownership around Start and reads status back whether Start succeeds or fails', async () => {
        const review = groupReview()
        mocks.supervisor.callProcess
            .mockResolvedValueOnce(wire(groupRun()))
            .mockResolvedValueOnce(
                wire({ activationEnabled: true, reserved: true, reason: '', run: groupRun() })
            )
        const run = await vllmGroupHandlers['engine:vllm-group-start']({
            reviewId: review.reviewId
        })
        expect(run.plan.limits).toEqual(review.plan.limits)
        expect(mocks.supervisor.callProcess).toHaveBeenNthCalledWith(
            1,
            'broker',
            'engine:vllm-group-start',
            { reviewId: review.reviewId },
            30_000
        )
        expect(mocks.supervisor.callProcess).toHaveBeenNthCalledWith(
            2,
            'broker',
            'engine:vllm-group-status',
            {},
            30_000
        )
        expect(mocks.state.holdVllmGroupStatus).toHaveBeenCalledTimes(1)
        expect(mocks.state.setVllmGroupStatus).toHaveBeenCalledTimes(1)

        mocks.supervisor.callProcess
            .mockRejectedValueOnce(new Error('method not found: engine:vllm-group-start'))
            .mockResolvedValueOnce(wire(inactiveStatus()))
        await expect(
            vllmGroupHandlers['engine:vllm-group-start']({ reviewId: review.reviewId })
        ).rejects.toThrow(/method not found/)
        expect(mocks.supervisor.callProcess).toHaveBeenCalledTimes(4)
        expect(mocks.state.setVllmGroupStatus).toHaveBeenCalledTimes(2)
    })

    it('forwards one-use administrator access for Start and clears the bridge copy', async () => {
        const review = groupReview()
        mocks.supervisor.callProcess
            .mockImplementationOnce((_process, method, params) => {
                expect(method).toBe('engine:vllm-group-start')
                expect(params).toEqual({
                    reviewId: review.reviewId,
                    elevation: [
                        { nodeId: 'node-a', elevationPassword: 'fixture-secret' },
                        { nodeId: 'node-b', nonInteractive: true }
                    ]
                })
                return Promise.resolve(wire(groupRun()))
            })
            .mockResolvedValueOnce(
                wire({ activationEnabled: true, reserved: true, reason: '', run: groupRun() })
            )
        await vllmGroupHandlers['engine:vllm-group-start']({
            reviewId: review.reviewId,
            elevation: [
                { nodeId: 'node-a', elevationPassword: 'fixture-secret' },
                { nodeId: 'node-b', nonInteractive: true }
            ]
        })
        expect(mocks.supervisor.callProcess.mock.calls[0]?.[2]).toEqual({
            reviewId: review.reviewId,
            elevation: [
                { nodeId: 'node-a', elevationPassword: '' },
                { nodeId: 'node-b', nonInteractive: true }
            ]
        })
    })

    it('reads status with an exact empty request and holds on failure', async () => {
        mocks.supervisor.callProcess.mockResolvedValueOnce(wire(heldStatus()))
        const status = await vllmGroupHandlers['engine:vllm-group-status'](undefined)
        expect(status.reserved).toBe(true)
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:vllm-group-status',
            {},
            30_000
        )
        expect(mocks.state.setVllmGroupStatus).toHaveBeenCalledWith(status)
        mocks.supervisor.callProcess.mockRejectedValueOnce(new Error('journal unreadable'))
        await expect(vllmGroupHandlers['engine:vllm-group-status'](undefined)).rejects.toThrow(
            'journal unreadable'
        )
        expect(mocks.state.holdVllmGroupStatus).toHaveBeenCalledTimes(1)
    })

    it.each(['engine:vllm-group-stop', 'engine:vllm-group-reconcile'] as const)(
        '%s binds the exact operation, publishes the reply as hold truth, and holds on drift',
        async method => {
            const run = groupRun('stopped')
            const operation = { runId: run.runId, generation: run.generation }
            mocks.supervisor.callProcess.mockResolvedValueOnce(wire({ ...inactiveStatus(), run }))
            const status = await vllmGroupHandlers[method](operation)
            expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
                'broker',
                method,
                operation,
                110_000
            )
            expect(mocks.state.setVllmGroupStatus).toHaveBeenCalledWith(status)

            mocks.supervisor.callProcess.mockResolvedValueOnce(
                wire({ ...inactiveStatus(), run: { ...run, generation: 2 } })
            )
            await expect(vllmGroupHandlers[method](operation)).rejects.toThrow(
                /different operation/
            )
            expect(mocks.state.holdVllmGroupStatus).toHaveBeenCalledTimes(1)

            mocks.supervisor.callProcess.mockRejectedValueOnce(
                new Error('group operation generation changed')
            )
            await expect(vllmGroupHandlers[method](operation)).rejects.toThrow(/generation changed/)
            expect(mocks.state.holdVllmGroupStatus).toHaveBeenCalledTimes(2)
        }
    )

    it('refuses malformed or credential-bearing requests before any broker call', async () => {
        const review = groupReview()
        await expect(
            vllmGroupHandlers['engine:vllm-group-review']({
                selection: { ...selectionFor(review), runtime: 'caller-override' } as never
            })
        ).rejects.toThrow()
        await expect(
            vllmGroupHandlers['engine:vllm-group-start']({
                reviewId: review.reviewId,
                elevation: [
                    {
                        nodeId: 'node-a',
                        elevationPassword: 'secret',
                        nonInteractive: true
                    }
                ]
            } as never)
        ).rejects.toThrow()
        await expect(
            vllmGroupHandlers['engine:vllm-group-stop']({ runId: '1'.repeat(32), generation: 0 })
        ).rejects.toThrow()
        await expect(
            vllmGroupHandlers['engine:vllm-group-reconcile']({
                runId: '1'.repeat(32),
                generation: 1,
                elevation: []
            } as never)
        ).rejects.toThrow()
        await expect(
            vllmGroupHandlers['engine:vllm-group-status']({ extra: true } as never)
        ).rejects.toThrow()
        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
    })

    it('refuses dispatch while PAIR is unavailable', async () => {
        mocks.supervisor.ready = false
        await expect(vllmGroupHandlers['engine:vllm-group-status'](undefined)).rejects.toThrow(
            /unavailable/
        )
        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
        expect(mocks.state.holdVllmGroupStatus).toHaveBeenCalledTimes(1)
    })
    it('selects a retained model only for this stopped managed vLLM from the reported library and publishes the reply status', async () => {
        const reply = {
            engine: 'vllm',
            model: MODEL,
            selected: true,
            status: {
                engine: 'vllm',
                installed: true,
                running: false,
                managed: true,
                selected_model: MODEL
            }
        }
        mocks.supervisor.callProcess.mockResolvedValueOnce(reply)
        const result = await vllmGroupHandlers['engine:vllm-select-model']({
            nodeId: 'node-a',
            model: MODEL
        })
        expect(mocks.supervisor.callProcess).toHaveBeenCalledExactlyOnceWith(
            'broker',
            'engine:vllm-select-model',
            { model: MODEL },
            10 * 60_000
        )
        expect(mocks.state.applyEngineManagerStatus).toHaveBeenCalledWith(reply.status)
        expect(result).toEqual({ nodeId: 'node-a', model: MODEL, selectedModel: MODEL })

        mocks.supervisor.callProcess.mockResolvedValueOnce({
            ...reply,
            status: { ...reply.status, selected_model: OTHER }
        })
        await expect(
            vllmGroupHandlers['engine:vllm-select-model']({ nodeId: 'node-a', model: MODEL })
        ).rejects.toThrow(/different model/)
    })

    it('refuses model selection for remote nodes, held groups, running engines, paths, and models outside the library before any broker call', async () => {
        await expect(
            vllmGroupHandlers['engine:vllm-select-model']({ nodeId: 'node-b', model: MODEL })
        ).rejects.toThrow(/this controller/)
        await expect(
            vllmGroupHandlers['engine:vllm-select-model']({ nodeId: 'node-a', model: '/models/x' })
        ).rejects.toThrow(/exact retained/)
        await expect(
            vllmGroupHandlers['engine:vllm-select-model']({ nodeId: 'node-a', model: OTHER })
        ).rejects.toThrow(/not in the retained vLLM library/)
        mocks.state.isEngineCommandAllowed.mockReturnValueOnce(false)
        await expect(
            vllmGroupHandlers['engine:vllm-select-model']({ nodeId: 'node-a', model: MODEL })
        ).rejects.toThrow(/held/)
        mocks.state.getEngineInitialState.mockReturnValueOnce({
            statuses: [
                { nodeId: 'node-a', engineType: 'vllm', processStatus: 'running', managed: true }
            ],
            models: []
        })
        await expect(
            vllmGroupHandlers['engine:vllm-select-model']({ nodeId: 'node-a', model: MODEL })
        ).rejects.toThrow(/Stop the PAIR-managed vLLM/)
        await expect(
            vllmGroupHandlers['engine:vllm-select-model']({
                nodeId: 'node-a',
                model: MODEL,
                path: '/x'
            } as never)
        ).rejects.toThrow()
        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
    })
})
