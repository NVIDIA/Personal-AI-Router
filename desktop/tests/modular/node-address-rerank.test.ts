// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

// A broker snapshot carries the node's whole ranked address list, so an address
// it no longer ranks has to disappear from everything derived from that list —
// otherwise the desktop keeps retrying a dead endpoint for the life of the
// process and projects an address the broker no longer advertises. The proxy
// `node/*` feed discovers independently, so an address only it reported must
// survive that replacement, behind the broker's order.
describe('broker address re-rank', () => {
    it('drops an address the broker no longer ranks, keeping the proxy-only one behind it', () => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: 'uuid-rerank',
                        name: 'rerank-host',
                        ipAddress: '192.0.2.101',
                        ipAddresses: ['192.0.2.101', '192.0.2.102'],
                        port: 14318
                    }
                ]
            }
        })
        state.handleNotification({
            source: 'proxy',
            method: 'node/discovered',
            params: {
                id: 'uuid-rerank',
                host: 'rerank-host',
                port: 11434,
                addresses: ['198.51.100.9'],
                ip: '192.0.2.101'
            }
        })

        expect(pollHosts(state, 'uuid-rerank')).toEqual([
            '192.0.2.101',
            '192.0.2.102',
            '198.51.100.9'
        ])

        // The node re-ranked itself onto its second address and stopped
        // publishing the first.
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: 'uuid-rerank',
                        name: 'rerank-host',
                        ipAddress: '192.0.2.102',
                        ipAddresses: ['192.0.2.102'],
                        port: 14318
                    }
                ]
            }
        })

        expect(pollHosts(state, 'uuid-rerank')).toEqual(['192.0.2.102', '198.51.100.9'])
        expect(availableAddresses(state, 'uuid-rerank')).toEqual(['192.0.2.102', '198.51.100.9'])
        expect(state.getNodesInitial().nodes['uuid-rerank'].allIpAddresses).toEqual([
            '192.0.2.102',
            '198.51.100.9'
        ])
    })

    it('keeps the broker ranking when a proxy event refreshes the same node', () => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: 'uuid-proxy-refresh',
                        name: 'proxy-refresh-host',
                        ipAddress: '192.0.2.201',
                        ipAddresses: ['192.0.2.201', '192.0.2.202'],
                        port: 14318
                    }
                ]
            }
        })
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/updated',
            params: {
                id: 'uuid-proxy-refresh',
                host: 'proxy-refresh-host',
                port: 1234,
                addresses: ['203.0.113.7'],
                ip: '192.0.2.201',
                modelsByEngine: { lmstudio: [] }
            }
        })

        // The proxy feed replaces only its own contribution: the broker's
        // second-ranked address is still a poll candidate.
        expect(pollHosts(state, 'uuid-proxy-refresh')).toEqual([
            '192.0.2.201',
            '192.0.2.202',
            '203.0.113.7'
        ])
    })
})

type BridgeState = ReturnType<typeof getModularBridgeState>

function pollHosts(state: BridgeState, nodeId: string): string[] {
    const target = state.getNodeInfoPollTargets().find(entry => entry.id === nodeId)
    expect(target).toBeDefined()
    return target?.hosts ?? []
}

function availableAddresses(state: BridgeState, nodeId: string): string[] {
    const node = state.getAvailableNodes().find(entry => entry.id === nodeId)
    expect(node).toBeDefined()
    return node?.ipAddresses ?? []
}
