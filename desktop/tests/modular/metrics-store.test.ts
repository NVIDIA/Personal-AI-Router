// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, expect, it, vi } from 'vitest'
import type { NodeItemMetrics } from '@/shared/types/metrics'
import { useMetricsStore } from '@/ui/stores/metrics.store'

afterEach(() => {
    useMetricsStore.getState().cleanup()
    useMetricsStore.getState().clearAll()
    vi.unstubAllGlobals()
})

// The renderer independently protects chart history even if duplicate or
// out-of-order events reach it from the main-process bridge.
it('appends only newer observations, including readings with unchanged values', () => {
    let deliver: ((metrics: NodeItemMetrics) => void) | undefined
    const onUpdate = vi.fn<Window['pairApi']['metrics']['onUpdate']>(handler => {
        deliver = handler
        return () => {
            deliver = undefined
        }
    })
    const onRemove = vi.fn<Window['pairApi']['nodes']['onRemove']>(() => () => {})
    vi.stubGlobal('window', { pairApi: { nodes: { onRemove }, metrics: { onUpdate } } })
    useMetricsStore.getState().initialize()

    const sample = (timestamp: number): NodeItemMetrics => ({
        id: 'sampled-node',
        current: {
            timestamp,
            cpuUtilization: 7,
            memoryUsage: 25,
            gpuUtilization: [],
            gpuVramUsage: []
        },
        historical: []
    })
    if (!deliver) throw new Error('metrics subscription was not installed')
    deliver(sample(10_000))
    const generation = useMetricsStore.getState().generation

    deliver(sample(10_000))
    deliver(sample(9_999))
    expect(useMetricsStore.getState().generation).toBe(generation)

    deliver(sample(12_000))
    const history = useMetricsStore.getState().nodeMetrics.get('sampled-node')
    expect(history?.cpuUtilization.slice(-2)).toEqual([
        { timestamp: 10_000, value: 7 },
        { timestamp: 12_000, value: 7 }
    ])
})
