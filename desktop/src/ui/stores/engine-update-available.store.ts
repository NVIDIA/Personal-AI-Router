// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Engine update-available store -- ephemeral, push-driven.
 *
 * Sole owner of per-engine "update available" state in the renderer. The
 * backend reports an available engine update per node and delivers it as an
 * `engines:state-changed` push patch scoped to the originating `nodeId`. We
 * mirror those into a `Map<{nodeId, engineType}, UpdateAvailableInfo>` so
 * `EditNodeApp` can surface update affordances for any node — local **or**
 * remote — that the user has access to.
 *
 * Entries are cleared on an explicit `updateAvailable: null` patch and when a
 * node departs (`nodes:remove`), so a rejoin never resurfaces a stale update
 * affordance. On reconnect the store self-resets; updates re-arrive on the
 * backend's next report.
 */
import { create } from 'zustand'
import type { EngineInitialState } from '@/shared/types/engine-api'
import type { UpdateAvailableInfo } from '@/ui/types/engine-info'
import type { EngineType } from '@/shared/types/engines'

function key(nodeId: string, engineType: string): string {
    return `${nodeId}:${engineType}`
}

interface EngineUpdateAvailableState {
    entries: Map<string, UpdateAvailableInfo>
    initialize(initial?: EngineInitialState): Promise<void>
    cleanup(): void
    /**
     * Returns the update info for a (node, engine) pair, or `undefined` when
     * none is currently published. Returning `undefined` lets callers spread
     * directly into `BackendInfo` without sentinel handling.
     */
    getInfo(nodeId: string, engineType: EngineType): UpdateAvailableInfo | undefined
}

let unsubs: Array<() => void> = []

export const useEngineUpdateAvailableStore = create<EngineUpdateAvailableState>((set, get) => ({
    entries: new Map(),

    initialize: async initialSnapshot => {
        // Idempotent: drop any prior subscriptions before re-subscribing so a
        // state:request-refresh (which re-runs initialize) never stacks handlers.
        unsubs.forEach(u => u())
        unsubs = []
        try {
            const initial = initialSnapshot ?? (await window.pairApi.engines.getInitialState())
            const map = new Map<string, UpdateAvailableInfo>()
            for (const update of initial.updateAvailable) {
                map.set(key(update.nodeId, update.engineType), {
                    currentVersion: update.currentVersion,
                    latestVersion: update.latestVersion,
                    releaseUrl: update.releaseUrl,
                    installType: update.installType
                })
            }
            set({ entries: map })
        } catch {
            console.error('Failed to initialize engine update store')
        }

        if (!window.pairApi) return
        unsubs.push(
            window.pairApi.engines.onStateChanged(patch => {
                if (patch.updateAvailable === undefined) return
                const map = new Map(get().entries)
                if (patch.updateAvailable === null) {
                    if (map.delete(key(patch.nodeId, patch.engineType))) {
                        set({ entries: map })
                    }
                    return
                }
                map.set(key(patch.nodeId, patch.engineType), {
                    currentVersion: patch.updateAvailable.currentVersion,
                    latestVersion: patch.updateAvailable.latestVersion,
                    releaseUrl: patch.updateAvailable.releaseUrl,
                    installType: patch.updateAvailable.installType
                })
                set({ entries: map })
            }),
            // Drop a departed node's update entries so a rejoin never resurfaces a
            // stale "update available" affordance.
            window.pairApi.nodes.onRemove(nodeId => {
                const prefix = `${nodeId}:`
                const map = new Map(get().entries)
                let changed = false
                for (const entryKey of Array.from(map.keys())) {
                    if (entryKey.startsWith(prefix)) {
                        map.delete(entryKey)
                        changed = true
                    }
                }
                if (changed) set({ entries: map })
            })
        )
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
        set({ entries: new Map() })
    },

    getInfo: (nodeId, engineType) => {
        return get().entries.get(key(nodeId, engineType))
    }
}))
