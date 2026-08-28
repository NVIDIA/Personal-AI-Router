// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Engine status store -- populated entirely by engine state patches.
 * No component ever calls a setter directly. Only push handlers update this store.
 *
 * Structure: outer key = node id, inner key = engine type. Sparse: only types the
 * service has reported appear; use {@link getEnginesForNode} for a full UI row list.
 */
import { create } from 'zustand'
import type { EngineInitialState } from '@/shared/types/engine-api'
import { emptyEngineStatus } from '@/shared/utils/engines'
import type { EngineStatusByNode, EngineStatusData, EngineType } from '@/shared/types/engines'

interface EngineStatusState {
    statusByNode: EngineStatusByNode
    initialize(initial?: EngineInitialState): Promise<void>
    cleanup(): void
    /** Always defined; uses {@link emptyEngineStatus} when the service has not reported this pair. */
    getStatus(nodeId: string, engineType: EngineType): EngineStatusData
}

let unsubs: Array<() => void> = []

export const useEngineStatusStore = create<EngineStatusState>((set, get) => ({
    statusByNode: new Map(),

    initialize: async initialSnapshot => {
        // Idempotent: initialize() runs on connect and again on a
        // state:request-refresh without an intervening cleanup(), so drop any
        // prior subscriptions before re-subscribing or push handlers stack.
        unsubs.forEach(u => u())
        unsubs = []
        try {
            const initial = initialSnapshot ?? (await window.pairApi.engines.getInitialState())
            const byNode: EngineStatusByNode = new Map()
            for (const s of initial.statuses) {
                let inner = byNode.get(s.nodeId)
                if (!inner) {
                    inner = new Map()
                    byNode.set(s.nodeId, inner)
                }
                inner.set(s.engineType, s)
            }
            set({ statusByNode: byNode })
        } catch {
            console.error('Failed to initialize engine status store')
        }

        if (!window.pairApi) return

        unsubs.push(
            window.pairApi.engines.onStateChanged(patch => {
                const status = patch.status
                if (!status) return
                const { nodeId, engineType } = status
                const outer = new Map(get().statusByNode)
                const inner = new Map(outer.get(nodeId) ?? new Map<EngineType, EngineStatusData>())
                inner.set(engineType, status)
                outer.set(nodeId, inner)
                set({ statusByNode: outer })
            }),
            // Drop a departed node's status so a later rejoin never renders stale
            // installed/running state from before it left.
            window.pairApi.nodes.onRemove(nodeId => {
                if (!get().statusByNode.has(nodeId)) return
                const outer = new Map(get().statusByNode)
                outer.delete(nodeId)
                set({ statusByNode: outer })
            })
        )
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
    },

    getStatus: (nodeId, engineType) => {
        return (
            get().statusByNode.get(nodeId)?.get(engineType) ?? emptyEngineStatus(nodeId, engineType)
        )
    }
}))
