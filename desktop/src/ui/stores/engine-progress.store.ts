// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Engine progress store -- ephemeral, auto-clearing.
 * Populated entirely by engine progress push events. Entries are removed when
 * the service sends a progress-cleared event (operation complete/error).
 *
 * On reconnect, the initial state snapshot includes active progress entries
 * so in-flight operations resume displaying immediately.
 */
import { create } from 'zustand'
import type { EngineInitialState } from '@/shared/types/engine-api'
import type { EngineOperationType, EngineProgress, EngineType } from '@/shared/types/engines'
import { emptyEngineProgress, engineProgressKey } from '@/shared/utils/engine-progress'

interface EngineProgressState {
    progress: Map<string, EngineProgress>
    initialize(initial?: EngineInitialState): Promise<void>
    cleanup(): void
    /**
     * Always defined; uses {@link emptyEngineProgress} with `status: 'idle'` when no entry exists.
     */
    getProgress(
        nodeId: string,
        engineType: EngineType,
        operation: EngineOperationType,
        model?: string
    ): EngineProgress
    getProgressForNode(nodeId: string): EngineProgress[]
}

let unsubs: Array<() => void> = []

export const useEngineProgressStore = create<EngineProgressState>((set, get) => ({
    progress: new Map(),

    initialize: async initialSnapshot => {
        // Idempotent: drop any prior subscriptions before re-subscribing so a
        // state:request-refresh (which re-runs initialize) never stacks handlers.
        unsubs.forEach(u => u())
        unsubs = []
        try {
            const initial = initialSnapshot ?? (await window.pairApi.engines.getInitialState())
            const map = new Map<string, EngineProgress>()
            for (const p of initial.activeProgress) {
                map.set(engineProgressKey(p), p)
            }
            set({ progress: map })
        } catch {
            console.error('Failed to initialize engine progress store')
        }

        if (!window.pairApi) return

        unsubs.push(
            window.pairApi.engines.onProgress(progress => {
                const key = engineProgressKey(progress)
                const map = new Map(get().progress)
                map.set(key, progress)
                set({ progress: map })
            }),
            window.pairApi.engines.onProgressRemove(key => {
                const map = new Map(get().progress)
                if (map.delete(key)) {
                    set({ progress: map })
                }
            }),
            // Drop a departed node's in-flight progress entries so a stuck spinner
            // (e.g. a remote pull whose terminal was missed) can't linger after the
            // node is gone.
            window.pairApi.nodes.onRemove(nodeId => {
                const prefix = `${nodeId}:`
                const map = new Map(get().progress)
                let changed = false
                for (const key of Array.from(map.keys())) {
                    if (key.startsWith(prefix)) {
                        map.delete(key)
                        changed = true
                    }
                }
                if (changed) set({ progress: map })
            })
        )
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
    },

    getProgress: (nodeId, engineType, operation, model) => {
        return (
            get().progress.get(engineProgressKey({ nodeId, engineType, operation, model })) ??
            emptyEngineProgress({ nodeId, engineType, operation, model })
        )
    },

    getProgressForNode: nodeId => {
        const results: EngineProgress[] = []
        for (const [key, p] of get().progress) {
            if (key.startsWith(`${nodeId}:`)) results.push(p)
        }
        return results
    }
}))
