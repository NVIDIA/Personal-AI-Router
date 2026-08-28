// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Engine models store -- populated entirely by engine state patches.
 * No component ever calls a setter directly. Only push handlers update this store.
 */
import { create } from 'zustand'
import type { EngineInitialState } from '@/shared/types/engine-api'
import type { EngineModels, EngineType } from '@/shared/types/engines'
import { emptyEngineModels } from '@/shared/utils/engines'

function modelsKey(nodeId: string, engineType: EngineType): string {
    return `${nodeId}:${engineType}`
}

interface EngineModelsState {
    models: Map<string, EngineModels>
    initialize(initial?: EngineInitialState): Promise<void>
    cleanup(): void
    /** Always defined; uses {@link emptyEngineModels} when no list has been pushed yet. */
    getModels(nodeId: string, engineType: EngineType): EngineModels
}

let unsubs: Array<() => void> = []

export const useEngineModelsStore = create<EngineModelsState>((set, get) => ({
    models: new Map(),

    initialize: async initialSnapshot => {
        // Idempotent: drop any prior subscriptions before re-subscribing so a
        // state:request-refresh (which re-runs initialize) never stacks handlers.
        unsubs.forEach(u => u())
        unsubs = []
        try {
            const initial = initialSnapshot ?? (await window.pairApi.engines.getInitialState())
            const map = new Map<string, EngineModels>()
            for (const m of initial.models) {
                map.set(modelsKey(m.nodeId, m.engineType), m)
            }
            set({ models: map })
        } catch {
            console.error('Failed to initialize engine models store')
        }

        if (!window.pairApi) return

        unsubs.push(
            window.pairApi.engines.onStateChanged(patch => {
                const models = patch.models
                if (!models) return
                const key = modelsKey(models.nodeId, models.engineType)
                const map = new Map(get().models)
                map.set(key, models)
                set({ models: map })
            }),
            // Drop a departed node's model lists so a rejoin never shows stale
            // models from before it left.
            window.pairApi.nodes.onRemove(nodeId => {
                const prefix = `${nodeId}:`
                const map = new Map(get().models)
                let changed = false
                for (const key of Array.from(map.keys())) {
                    if (key.startsWith(prefix)) {
                        map.delete(key)
                        changed = true
                    }
                }
                if (changed) set({ models: map })
            })
        )
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
    },

    getModels: (nodeId, engineType) => {
        return (
            get().models.get(modelsKey(nodeId, engineType)) ?? emptyEngineModels(nodeId, engineType)
        )
    }
}))
