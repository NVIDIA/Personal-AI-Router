// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type { NodeItemMetrics } from '@/shared/types/metrics'
import type { PerformanceMetric } from '@/ui/types/types'

const MAX_DATA_POINTS = 60

interface GpuMetricsHistory {
    id: string
    data: PerformanceMetric[]
}

export interface NodeMetricsHistory {
    gpuUtilization: GpuMetricsHistory[]
    gpuVramUsage: GpuMetricsHistory[]
    cpuUtilization: PerformanceMetric[]
    memoryUsage: PerformanceMetric[]
}

interface MetricsStore {
    nodeMetrics: Map<string, NodeMetricsHistory>
    generation: number
    initialize: () => void
    clearAll: () => void
    cleanup: () => void
}

let unsubs: Array<() => void> = []

function pushMetric(arr: PerformanceMetric[], value: number | undefined, timestamp: number): void {
    if (arr.length >= MAX_DATA_POINTS) {
        arr.shift()
    }
    arr.push({ timestamp, value: value ?? 0 })
}

function createPrefill(baseTs: number): PerformanceMetric[] {
    return Array.from({ length: MAX_DATA_POINTS }, (_, i) => ({
        timestamp: baseTs - (MAX_DATA_POINTS - i) * 1000,
        value: 0
    }))
}

export const useMetricsStore = create<MetricsStore>((set, get) => ({
    nodeMetrics: new Map(),
    generation: 0,

    clearAll: () => {
        set({ nodeMetrics: new Map(), generation: get().generation + 1 })
    },

    initialize: () => {
        if (!window.pairApi) return

        unsubs.push(
            window.pairApi.nodes.onRemove((nodeId: string) => {
                const map = get().nodeMetrics
                if (map.has(nodeId)) {
                    map.delete(nodeId)
                    set({ generation: get().generation + 1 })
                }
            }),
            window.pairApi.metrics.onUpdate((metrics: NodeItemMetrics) => {
                const map = get().nodeMetrics
                let history = map.get(metrics.id)

                if (!history) {
                    const ts = metrics.current.timestamp
                    history = {
                        gpuUtilization: metrics.current.gpuUtilization.map(gpu => ({
                            id: gpu.id,
                            data: createPrefill(ts)
                        })),
                        gpuVramUsage: metrics.current.gpuVramUsage.map(gpu => ({
                            id: gpu.id,
                            data: createPrefill(ts)
                        })),
                        cpuUtilization: createPrefill(ts),
                        memoryUsage: createPrefill(ts)
                    }
                    map.set(metrics.id, history)
                }

                const lastTs = history.cpuUtilization[history.cpuUtilization.length - 1]?.timestamp
                if (lastTs === metrics.current.timestamp) return

                const ts = metrics.current.timestamp

                for (const gpu of metrics.current.gpuUtilization) {
                    let entry = history.gpuUtilization.find(h => h.id === gpu.id)
                    if (!entry) {
                        entry = { id: gpu.id, data: [] }
                        history.gpuUtilization.push(entry)
                    }
                    pushMetric(entry.data, gpu.value, ts)
                }

                for (const gpu of metrics.current.gpuVramUsage) {
                    let entry = history.gpuVramUsage.find(h => h.id === gpu.id)
                    if (!entry) {
                        entry = { id: gpu.id, data: [] }
                        history.gpuVramUsage.push(entry)
                    }
                    pushMetric(entry.data, gpu.value, ts)
                }

                pushMetric(history.cpuUtilization, metrics.current.cpuUtilization, ts)
                pushMetric(history.memoryUsage, metrics.current.memoryUsage, ts)

                map.set(metrics.id, { ...history })
                set({ generation: get().generation + 1 })
            })
        )
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
    }
}))
