// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn(() => 'local-node'),
        beginLocalEngineOp: vi.fn(),
        clearPendingEngineOp: vi.fn()
    },
    supervisor: {
        hasProcess: vi.fn(() => true),
        sendProcess: vi.fn(),
        callProcess: vi.fn(),
        reportError: vi.fn(),
        toggleEngineRemote: vi.fn()
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
vi.mock('@/electron/service-bridge/model-catalog', () => ({ getEngineHubModels: vi.fn() }))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

// A start can take minutes (vLLM loads for up to eight), and while it runs the
// engine reports not running, so a toggle would send a second start. Stop is
// explicit, and the engine-manager cancels the start in flight.
describe('stopping an engine that is still starting', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        mocks.state.getSelfId.mockReturnValue('local-node')
    })

    it('sends engine:stop locally without asking whether it is running', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'stop',
            engineType: 'vllm',
            nodeId: 'local-node'
        })

        expect(mocks.state.beginLocalEngineOp).toHaveBeenCalledWith('vllm', 'stopping')
        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
        expect(mocks.supervisor.sendProcess).toHaveBeenCalledOnce()
        const [name, method, params, onFailure, observeResponse] =
            mocks.supervisor.sendProcess.mock.calls[0]
        expect([name, method, params, observeResponse]).toEqual([
            'broker',
            'engine:stop',
            { engine: 'vllm' },
            true
        ])

        onFailure('cannot stop engine "vllm": it is running under external management')
        expect(mocks.state.clearPendingEngineOp).toHaveBeenCalledWith('vllm')
        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'Failed to stop vllm: cannot stop engine "vllm": it is running under external management',
            'error',
            'engine-cmd:stop:vllm'
        )
    })

    it('sends a stop to a remote peer whatever its last reported state', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'stop',
            engineType: 'vllm',
            nodeId: 'remote-node'
        })

        expect(mocks.supervisor.toggleEngineRemote).toHaveBeenCalledWith(
            'remote-node',
            'vllm',
            'vllm',
            true
        )
    })
})
