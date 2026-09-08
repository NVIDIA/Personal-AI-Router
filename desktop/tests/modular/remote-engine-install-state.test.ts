// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

function lmStudioStatus(nodeId: string) {
    return getModularBridgeState()
        .getEngineInitialState()
        .statuses.find(status => status.nodeId === nodeId && status.engineType === 'lm-studio')
}

describe('remote engine install state', () => {
    it('omits status when a remote engine has no facts and no advertisement', () => {
        const state = getModularBridgeState()
        const remoteNodeId = 'remote-unknown'

        state.setSelfId('local-self')
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: remoteNodeId,
                        name: 'unknown-host',
                        ipAddress: '192.0.2.60',
                        port: 14318,
                        trusted: true,
                        clustered: true
                    }
                ]
            }
        })

        // Discovery alone creates the node with empty engine presence. Without
        // facts that must not become a confident "stopped" toggle.
        expect(lmStudioStatus(remoteNodeId)).toBeUndefined()
        expect(state.isRemoteEngineRunning(remoteNodeId, 'lm-studio')).toBe(false)
    })

    it('maps authoritative installed:false to not-installed', () => {
        const state = getModularBridgeState()
        const remoteNodeId = 'remote-not-installed'

        state.setSelfId('local-self')
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: remoteNodeId,
                        name: 'peer-no-lms',
                        ipAddress: '192.0.2.61',
                        port: 14318,
                        trusted: true,
                        clustered: true
                    }
                ]
            }
        })

        state.applyRemoteEngineFacts(remoteNodeId, {
            engines: [
                {
                    engine: 'lmstudio',
                    installed: false,
                    running: false,
                    healthy: false
                }
            ]
        })

        expect(lmStudioStatus(remoteNodeId)).toMatchObject({
            processStatus: 'not-installed',
            enginePort: null,
            proxyPort: null
        })
        expect(state.isRemoteEngineRunning(remoteNodeId, 'lm-studio')).toBe(false)
    })

    it('maps authoritative installed:true running:false to stopped', () => {
        const state = getModularBridgeState()
        const remoteNodeId = 'remote-stopped'

        state.setSelfId('local-self')
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: remoteNodeId,
                        name: 'peer-stopped-lms',
                        ipAddress: '192.0.2.62',
                        port: 14318,
                        trusted: true,
                        clustered: true
                    }
                ]
            }
        })

        state.applyRemoteEngineFacts(remoteNodeId, {
            engines: [
                {
                    engine: 'lmstudio',
                    installed: true,
                    running: false,
                    healthy: false,
                    port: 1234
                }
            ]
        })

        expect(lmStudioStatus(remoteNodeId)).toMatchObject({
            processStatus: 'stopped',
            enginePort: 1234,
            proxyPort: null
        })
    })

    it('treats proxy presence as running when facts are absent on an unclustered peer', () => {
        const state = getModularBridgeState()
        const remoteNodeId = 'remote-presence'

        state.setSelfId('local-self')
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: {
                id: remoteNodeId,
                host: remoteNodeId,
                port: 1234,
                addresses: ['192.0.2.63'],
                ip: '192.0.2.63',
                modelsByEngine: { lmstudio: [] }
            }
        })

        expect(state.isRemoteEngineRunning(remoteNodeId, 'lm-studio')).toBe(true)
        expect(lmStudioStatus(remoteNodeId)).toMatchObject({
            processStatus: 'running',
            enginePort: null,
            proxyPort: 1234
        })
    })

    it('does not infer running from proxy presence alone for a clustered peer', () => {
        const state = getModularBridgeState()
        const remoteNodeId = 'remote-clustered-presence'

        state.setSelfId('local-self')
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: remoteNodeId,
                        name: 'clustered-peer',
                        ipAddress: '192.0.2.65',
                        port: 14318,
                        trusted: true,
                        clustered: true
                    }
                ]
            }
        })
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: {
                id: remoteNodeId,
                host: remoteNodeId,
                port: 1234,
                addresses: ['192.0.2.65'],
                ip: '192.0.2.65',
                modelsByEngine: { lmstudio: [] }
            }
        })

        expect(lmStudioStatus(remoteNodeId)).toBeUndefined()
        expect(state.isRemoteEngineRunning(remoteNodeId, 'lm-studio')).toBe(false)
    })

    it('retracts a stale stopped projection when authoritative facts say not installed', () => {
        const state = getModularBridgeState()
        const remoteNodeId = 'remote-stale-stopped'

        state.setSelfId('local-self')
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: remoteNodeId,
                        name: 'peer-stale',
                        ipAddress: '192.0.2.64',
                        port: 14318,
                        trusted: true,
                        clustered: true
                    }
                ]
            }
        })

        state.applyRemoteEngineFacts(remoteNodeId, {
            engines: [
                {
                    engine: 'lmstudio',
                    installed: true,
                    running: false,
                    healthy: false,
                    port: 1234
                }
            ]
        })
        expect(lmStudioStatus(remoteNodeId)?.processStatus).toBe('stopped')

        state.applyRemoteEngineFacts(remoteNodeId, {
            engines: [
                {
                    engine: 'lmstudio',
                    installed: false,
                    running: false,
                    healthy: false
                }
            ]
        })
        expect(lmStudioStatus(remoteNodeId)).toMatchObject({
            processStatus: 'not-installed',
            enginePort: null
        })
    })
})
