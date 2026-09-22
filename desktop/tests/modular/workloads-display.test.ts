// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { PreloadServiceTransport } from '@/shared/types/service-bridge'
import type { Workload } from '@/shared/types/workloads'
import { workloadExecutionNodeId, workloadKey } from '@/shared/utils/workloads'
import { createPairApi, type IWorkloadsApi } from '@/ui/api/pair-api'
import { useWorkloadsStore } from '@/ui/stores/workloads.store'

const sample: Workload = {
    id: '1',
    runId: 'a',
    model: 'owner/model:Q4',
    engine: 'llamacpp',
    state: 'running',
    originatedFrom: 'origin',
    scheduledOn: 'serving-node',
    createdAt: 100,
    startedAt: 101,
    completedAt: null,
    error: null,
    requesterId: null
}

it('exposes workload display without a cancellation command', () => {
    const transport: PreloadServiceTransport = {
        connected: true,
        invoke: vi.fn<PreloadServiceTransport['invoke']>(),
        subscribePush: vi.fn<PreloadServiceTransport['subscribePush']>(),
        onConnect: vi.fn(),
        onDisconnect: vi.fn(),
        onAuthFailure: vi.fn(),
        destroy: vi.fn()
    }
    expect(Object.keys(createPairApi(transport).workloads).sort()).toEqual([
        'getInitial',
        'onRemove',
        'onUpsert'
    ])
})

describe('workload display identity', () => {
    const getInitial = vi.fn<IWorkloadsApi['getInitial']>()
    const onUpsert = vi.fn<IWorkloadsApi['onUpsert']>()
    const onRemove = vi.fn<IWorkloadsApi['onRemove']>()
    const frames = vi.fn<typeof requestAnimationFrame>()

    beforeEach(() => {
        getInitial.mockResolvedValue({})
        onUpsert.mockReturnValue(() => {})
        onRemove.mockReturnValue(() => {})
        frames.mockReturnValue(1)
        vi.stubGlobal('requestAnimationFrame', frames)
        vi.stubGlobal('cancelAnimationFrame', vi.fn())
        vi.stubGlobal('window', {
            pairApi: { workloads: { getInitial, onUpsert, onRemove } satisfies IWorkloadsApi }
        })
        useWorkloadsStore.setState({ workloads: new Map() })
    })

    afterEach(() => {
        useWorkloadsStore.getState().cleanup()
        useWorkloadsStore.setState({ workloads: new Map() })
        vi.unstubAllGlobals()
    })

    it('keeps all engines and proxy runs distinct and removes only the reported identity', async () => {
        await useWorkloadsStore.getState().initialize()
        const upsert = onUpsert.mock.calls[0][0]
        for (const workload of [
            { ...sample, engine: 'ollama' },
            { ...sample, engine: 'lm-studio' },
            sample,
            { ...sample, runId: 'b' }
        ] satisfies Workload[]) {
            upsert(workload)
        }
        frames.mock.calls[0][0](0)
        expect(useWorkloadsStore.getState().workloads.size).toBe(4)

        onRemove.mock.calls[0][0]({
            workloadId: '1',
            originatedFrom: 'origin',
            engine: 'llamacpp',
            runId: 'a'
        })
        frames.mock.calls[1][0](0)
        const workloads = useWorkloadsStore.getState().workloads
        expect(workloads.size).toBe(3)
        expect(workloads.has(workloadKey('origin', '1', 'llamacpp', 'a'))).toBe(false)
        expect(workloads.get(workloadKey('origin', '1', 'llamacpp', 'b'))).toMatchObject({
            scheduledOn: 'serving-node',
            model: sample.model
        })
    })

    it('does not resurrect an exact removal from a pending desktop snapshot', async () => {
        const key = workloadKey('origin', '1', 'llamacpp', 'a')
        const pending = Promise.withResolvers<Record<string, Workload>>()
        getInitial.mockReturnValue(pending.promise)
        const initializing = useWorkloadsStore.getState().initialize()
        onRemove.mock.calls[0][0]({
            workloadId: '1',
            originatedFrom: 'origin',
            engine: 'llamacpp',
            runId: 'a'
        })
        frames.mock.calls[0][0](0)
        pending.resolve({ [key]: sample })
        await initializing
        expect(useWorkloadsStore.getState().workloads.has(key)).toBe(false)
    })

    it('attributes execution only to the reported destination', () => {
        expect(workloadExecutionNodeId(sample)).toBe('serving-node')
        expect(workloadExecutionNodeId({ ...sample, scheduledOn: null })).toBeNull()
        expect(workloadExecutionNodeId({ ...sample, scheduledOn: undefined })).toBeNull()
    })
})
