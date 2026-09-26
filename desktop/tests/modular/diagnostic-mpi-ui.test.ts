// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { createElement, type ComponentType } from 'react'
import { renderToStaticMarkup } from 'react-dom/server'
import { beforeEach, describe, expect, it } from 'vitest'
import ManagedNCCLCard from '@/ui/components/ClusterSettings/ManagedNCCLCard'
import {
    parseDiagnosticMPIManagedInventory,
    parseDiagnosticMPIReview
} from '@/shared/utils/diagnostic-mpi'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useDiagnosticMPIStore } from '@/ui/stores/diagnostic-mpi.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { rawInventory, rawReview } from './diagnostic-mpi-fixtures'

const Card = ManagedNCCLCard as ComponentType<{
    initialInventory: ReturnType<typeof parseDiagnosticMPIManagedInventory>
    initialReview: ReturnType<typeof parseDiagnosticMPIReview>
}>

describe('managed NCCL Cluster UI', () => {
    beforeEach(() => {
        useDiagnosticMPIStore.getState().reset()
        useConnectionStore.setState({ selfId: 'node-a' })
        useNodesStore.setState({
            nodes: new Map([
                [
                    'node-a',
                    {
                        id: 'node-a',
                        name: 'Spark A',
                        status: 'active',
                        ipAddress: '192.0.2.1',
                        port: 5000,
                        allIpAddresses: ['192.0.2.1'],
                        topology: {},
                        os: 'Linux'
                    } as never
                ],
                [
                    'node-b',
                    {
                        id: 'node-b',
                        name: 'Spark B',
                        status: 'active',
                        ipAddress: '192.0.2.2',
                        port: 5000,
                        allIpAddresses: ['192.0.2.2'],
                        topology: {},
                        os: 'Linux'
                    } as never
                ]
            ])
        })
    })

    it('states the Socket/management boundary before presenting review and Start', () => {
        const markup = renderToStaticMarkup(
            createElement(Card, {
                initialInventory: parseDiagnosticMPIManagedInventory(rawInventory),
                initialReview: parseDiagnosticMPIReview(rawReview)
            })
        )
        expect(markup).toContain('Managed NCCL correctness smoke')
        expect(markup).toContain('management IPv4 interface with NCCL Socket transport')
        expect(markup).toContain('does not use RDMA')
        expect(markup).toContain('Start NCCL smoke')
        expect(markup).toContain('Close review')
        expect(markup).toContain('node-a')
        expect(markup).toContain('node-b')
        expect(markup).not.toMatch(/password|privateKey|stderr/i)
    })
})
