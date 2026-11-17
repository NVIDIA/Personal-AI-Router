// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'
vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

function vllmStatus(nodeId: string) {
    return getModularBridgeState()
        .getEngineInitialState()
        .statuses.find(row => row.nodeId === nodeId && row.engineType === 'vllm')
}

describe('vLLM awaiting model selection', () => {
    it('resolves local starting optimism when enabled without a serving model', () => {
        const state = getModularBridgeState()
        state.setSelfId('awaiting-self')
        state.beginLocalEngineOp('vllm', 'starting')
        state.applyEngineManagerStatus({
            engine: 'vllm',
            installed: true,
            running: false,
            starting: false,
            awaiting_model: true,
            healthy: false,
            port: 8001
        })
        expect(vllmStatus('awaiting-self')?.processStatus).toBe('awaiting-model')
        state.applyEngineManagerStatus({
            engine: 'vllm',
            installed: true,
            running: false,
            awaiting_model: false,
            healthy: false,
            port: 8001
        })
        expect(vllmStatus('awaiting-self')?.processStatus).toBe('stopped')
    })
    it('treats a peer awaiting a model as enabled for its toggle', () => {
        const state = getModularBridgeState()
        state.setSelfId('awaiting-peer-self')
        const peer = 'awaiting-peer'
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: peer,
                        name: 'peer',
                        ipAddress: '192.0.2.71',
                        port: 14318,
                        trusted: true,
                        clustered: true
                    }
                ]
            }
        })
        state.applyRemoteEngineFacts(peer, {
            engines: [
                {
                    engine: 'vllm',
                    installed: true,
                    running: false,
                    awaiting_model: true,
                    healthy: false,
                    port: 8001
                }
            ]
        })
        expect(vllmStatus(peer)?.processStatus).toBe('awaiting-model')
        expect(state.isRemoteEngineEnabled(peer, 'vllm')).toBe(true)
        state.applyRemoteEngineFacts(peer, {
            engines: [
                {
                    engine: 'vllm',
                    installed: true,
                    running: false,
                    awaiting_model: false,
                    healthy: false,
                    port: 8001
                }
            ]
        })
        expect(vllmStatus(peer)?.processStatus).toBe('stopped')
        expect(state.isRemoteEngineEnabled(peer, 'vllm')).toBe(false)
    })
})
