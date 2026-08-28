// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import { IDLE_DEMO_STATE, type DemoState } from '@/shared/types/inference-demo'
import { isElectron } from '@/ui/api/bootstrap'

/**
 * Mirror of the main-process Inference Demo state.
 *
 * The demo is node-local and owned entirely by main, so this store never
 * originates state — it only reflects `demo:state` pushes and exposes the two
 * commands. Every window receives the same broadcasts, which is what lets the
 * toast survive tab changes and appear consistently wherever the user is.
 *
 * Lifecycle is driven from `@/ui/stores/init`, like every other subscribing
 * store. Unlike the rest, it is deliberately not part of the cluster
 * connect/disconnect cycle: the demo is local to this node and must keep
 * running (and stay stoppable) regardless of cluster state.
 */
interface InferenceDemoStore {
    state: DemoState
    initialize: () => void
    cleanup: () => void
}

let unsubscribe: (() => void) | null = null

export const useInferenceDemoStore = create<InferenceDemoStore>(set => ({
    state: IDLE_DEMO_STATE,

    initialize: () => {
        if (!isElectron) return
        unsubscribe?.()

        // Subscribe before hydrating. The reverse order has a window where a
        // push that lands mid-fetch is overwritten by the older snapshot.
        unsubscribe = window.windowApi.inferenceDemo.onStateChanged(state => set({ state }))

        window.windowApi.inferenceDemo
            .getState()
            .then(state => {
                // Only hydrate if no push has arrived in the meantime.
                if (useInferenceDemoStore.getState().state.status === 'idle') set({ state })
            })
            .catch(err => console.error('Failed to fetch inference demo state:', err))
    },

    cleanup: () => {
        unsubscribe?.()
        unsubscribe = null
        set({ state: IDLE_DEMO_STATE })
    }
}))
