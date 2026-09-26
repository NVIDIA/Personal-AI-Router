// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createElement, type ComponentType } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { beforeEach, describe, expect, it } from 'vitest'
import FabricSetupCard from '@/ui/components/ClusterSettings/FabricSetupCard'
import type { NodeItem } from '@/shared/types/nodes'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useFabricStore } from '@/ui/stores/fabric.store'
import { useNodesStore } from '@/ui/stores/nodes.store'

const NOW = Date.now()
const FabricCard = FabricSetupCard as ComponentType<{
    initialTopology: {
        selfId: string
        nodes: NodeItem[]
        inventory: NonNullable<ReturnType<typeof useFabricStore.getState>['inventory']>
    }
}>

function node(id: string): NodeItem {
    return {
        id,
        name: `Spark ${id.slice(-1).toUpperCase()}`,
        status: 'active',
        ipAddress: `192.0.2.${id.charCodeAt(id.length - 1)}`,
        port: 14318,
        allIpAddresses: [],
        topology: {
            cpu: { model: '', cores: 0, threads: 0 },
            gpus: [],
            ram: 0,
            storage: []
        },
        os: 'Linux'
    }
}

describe('fabric topology UI', () => {
    beforeEach(() => {
        useConnectionStore.setState({ connected: true, selfId: 'node-a', clusterId: 'cluster-a' })
        useNodesStore.setState({
            nodes: new Map(['node-a', 'node-b', 'node-c'].map(id => [id, node(id)])),
            fetchedNodes: true
        })
        useFabricStore.getState().reset()
        useFabricStore.setState({
            inventory: {
                nodes: ['node-a', 'node-b', 'node-c'].map((nodeId, index) => ({
                    nodeId,
                    status: 'observed',
                    observedAt: NOW,
                    ports: ['p0', 'p1'].map(portName => ({
                        switchId: `switch-${index}`,
                        portName,
                        eligible: true
                    }))
                }))
            }
        })
    })

    it('renders the auto-detected ring, wiring, and advanced-only fallback', () => {
        const seededNodes = ['node-a', 'node-b', 'node-c'].map(node)
        const inventory = useFabricStore.getState().inventory!
        const markup = renderToStaticMarkup(
            createElement(FabricCard, {
                initialTopology: { selfId: 'node-a', nodes: seededNodes, inventory }
            })
        )
        expect(markup).toContain('Detected three-node ring candidate')
        expect(markup).toContain('Exact ports found')
        expect(markup).toContain('ring physical-port wiring diagram')
        expect(markup).toContain('p0 ↔ p1')
        expect(markup).toContain('Advanced fallback: choose nodes and physical-port IDs')
        expect(markup).toContain('Physical peer edges are not checked yet')
    })
})
