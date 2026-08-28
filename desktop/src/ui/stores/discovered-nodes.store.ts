// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type { AvailableNode } from '@/shared/types/cluster'

interface DiscoveredNodesStore {
    nodes: AvailableNode[]
    initialize: () => Promise<void>
    refresh: () => Promise<void>
    cleanup: () => void
}

let unsubs: Array<() => void> = []

export const useDiscoveredNodesStore = create<DiscoveredNodesStore>(set => ({
    nodes: [],

    initialize: async () => {
        await useDiscoveredNodesStore.getState().refresh()

        if (!window.pairApi) return
        unsubs.push(
            window.pairApi.discovery.onNodesChanged(nodes => {
                set({ nodes })
            })
        )
    },

    refresh: async () => {
        try {
            const nodes = await window.pairApi.discovery.getNodes()
            set({ nodes })
        } catch {
            set({ nodes: [] })
        }
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
        set({ nodes: [] })
    }
}))
