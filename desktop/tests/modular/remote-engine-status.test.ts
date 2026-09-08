// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

describe('remote engine status', () => {
    it('prefers authoritative stopped facts over proxy presence', () => {
        const state = getModularBridgeState()
        const remoteNodeId = 'remote-engine-status-remote'

        state.setSelfId('remote-engine-status-local')
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: {
                id: remoteNodeId,
                host: remoteNodeId,
                port: 1234,
                addresses: ['192.0.2.190'],
                ip: '192.0.2.190',
                modelsByEngine: { lmstudio: [] }
            }
        })
        expect(state.isRemoteEngineRunning(remoteNodeId, 'lm-studio')).toBe(true)

        state.applyRemoteEngineFacts(remoteNodeId, {
            engines: [
                {
                    engine: 'lmstudio',
                    installed: true,
                    running: false,
                    healthy: false,
                    port: 1235
                }
            ]
        })

        expect(state.isRemoteEngineRunning(remoteNodeId, 'lm-studio')).toBe(false)
        expect(
            state
                .getEngineInitialState()
                .statuses.find(
                    status => status.nodeId === remoteNodeId && status.engineType === 'lm-studio'
                )
        ).toMatchObject({
            processStatus: 'stopped',
            enginePort: 1235,
            // The proxy remains discoverable even though the engine behind it is stopped.
            proxyPort: 1234
        })
    })
})
