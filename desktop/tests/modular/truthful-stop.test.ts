// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn(() => 'local-node'),
        beginLocalEngineOp: vi.fn(),
        clearPendingEngineOp: vi.fn()
    },
    supervisor: {
        hasProcess: vi.fn(() => true),
        callProcess: vi.fn().mockResolvedValue({ running: true }),
        sendProcess: vi.fn(),
        reportError: vi.fn()
    }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state,
    isProxyEngine: () => false,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => []
}))
vi.mock('@/electron/model-hub', () => ({ getEngineHubModels: vi.fn() }))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

describe('truthful local stop', () => {
    it('observes a rejected stop, clears pending state, and reports the error', async () => {
        mocks.state.getSelfId.mockReturnValue('local-node')
        mocks.supervisor.hasProcess.mockReturnValue(true)
        mocks.supervisor.callProcess.mockResolvedValue({ running: true })

        await handleServiceBridgeInvoke('engine:command', {
            command: 'toggle',
            engineType: 'lm-studio',
            nodeId: 'local-node'
        })

        await vi.waitFor(() => expect(mocks.supervisor.sendProcess).toHaveBeenCalledOnce())
        const [name, method, params, onFailure, observeResponse] =
            mocks.supervisor.sendProcess.mock.calls[0]
        expect([name, method, params, observeResponse]).toEqual([
            'broker',
            'engine:stop',
            { engine: 'lmstudio' },
            true
        ])

        onFailure('cannot stop an externally managed process', true)

        expect(mocks.state.clearPendingEngineOp).toHaveBeenCalledWith('lm-studio')
        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'Failed to stop lmstudio: cannot stop an externally managed process',
            'error',
            'engine-cmd:toggle:lmstudio'
        )
    })
})
