// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

describe('node identity contract', () => {
    it('clears withdrawn broker provenance while proxy presence survives', () => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: [
                {
                    hostUuid: 'identity-withdrawn-host',
                    name: 'workstation',
                    ipAddress: '192.0.2.21',
                    port: 14318,
                    discoveryRecordId: 'peer:identity-withdrawn-host',
                    identityVerification: 'pinned'
                }
            ]
        })
        state.handleNotification({
            source: 'ollama-proxy',
            method: 'node/discovered',
            params: {
                id: 'identity-withdrawn-host',
                host: 'workstation',
                port: 11434,
                addresses: ['192.0.2.21']
            }
        })
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: []
        })

        const node = state.getAvailableNodes().find(node => node.id === 'identity-withdrawn-host')
        expect(node).toBeDefined()
        expect(node).not.toHaveProperty('discoveryRecordId')
        expect(node).not.toHaveProperty('identityVerification')
    })

    it('preserves broker provenance through proxy and public hardware updates', () => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: [
                {
                    hostUuid: 'identity-contract-host',
                    name: 'workstation',
                    ipAddress: '192.0.2.20',
                    port: 14318,
                    discoveryRecordId: 'observation:candidate-a',
                    identityVerification: 'unverified'
                }
            ]
        })
        state.handleNotification({
            source: 'ollama-proxy',
            method: 'node/discovered',
            params: {
                id: 'identity-contract-host',
                host: 'workstation',
                port: 11434,
                addresses: ['192.0.2.20']
            }
        })
        state.mergeNodeInfoResponse('identity-contract-host', {
            hostUuid: 'identity-contract-host',
            discoveryRecordId: 'peer:identity-contract-host',
            identityVerification: 'pinned',
            GPUs: [{ name: 'Display GPU' }]
        })

        expect(
            state.getAvailableNodes().find(node => node.id === 'identity-contract-host')
        ).toMatchObject({
            discoveryRecordId: 'observation:candidate-a',
            identityVerification: 'unverified'
        })
        expect(state.getNodesInitial().nodes['identity-contract-host'].topology.gpus).toHaveLength(
            1
        )
    })
})
