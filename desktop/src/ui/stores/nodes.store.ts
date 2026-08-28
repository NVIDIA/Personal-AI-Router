// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { NodeItem } from '@/shared/types/nodes'
import deepEqual from '@/ui/utils/deep-equal'
import { create } from 'zustand'

interface NodesStore {
    nodes: Map<string, NodeItem>
    fetchedNodes: boolean
    initialize: () => Promise<void>
    /** Re-fetch node list from service (e.g. after state reset / leave cluster). Does not add new listeners. */
    refresh: () => Promise<void>
    cleanup: () => void
    clearNodes: (selfId: string) => void
}

let unsubs: Array<() => void> = []

export const useNodesStore = create<NodesStore>((set, get) => ({
    nodes: new Map(),
    fetchedNodes: false,

    initialize: async () => {
        try {
            const { nodes: nodeMap, fetchedNodes } = await window.pairApi.nodes.getInitial()

            const map = new Map<string, NodeItem>()

            for (const [id, node] of Object.entries(nodeMap)) {
                map.set(id, node)
            }

            set({
                nodes: map,
                fetchedNodes
            })
        } catch (error) {
            console.error('Failed to initialize nodes store:', error)
        }

        if (window.pairApi) {
            unsubs.push(
                window.pairApi.nodes.onUpsert((node: NodeItem) => {
                    const prev = get().nodes
                    const existing = prev.get(node.id)
                    if (existing && deepEqual(existing, node)) return

                    const updated = new Map(prev)
                    updated.set(node.id, node)
                    set({ nodes: updated, fetchedNodes: true })
                }),
                window.pairApi.nodes.onRemove((nodeId: string) => {
                    const prev = get().nodes
                    if (!prev.has(nodeId)) return
                    const updated = new Map(prev)
                    updated.delete(nodeId)
                    set({ nodes: updated })
                })
            )
        }
    },

    refresh: async () => {
        try {
            const { nodes: nodeMap, fetchedNodes } = await window.pairApi.nodes.getInitial()

            const map = new Map<string, NodeItem>()

            for (const [id, node] of Object.entries(nodeMap)) {
                map.set(id, node)
            }

            set({
                nodes: map,
                fetchedNodes
            })
        } catch (error) {
            console.error('Failed to refresh nodes store:', error)
        }
    },

    clearNodes: (selfId: string) => {
        const prev = get().nodes
        const thisNode = prev.get(selfId)
        if (!thisNode) {
            set({ nodes: new Map() })
            return
        }

        set({ nodes: new Map([[selfId, thisNode]]) })
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
    }
}))
