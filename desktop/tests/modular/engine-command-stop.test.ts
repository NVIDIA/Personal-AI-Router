// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { assert, beforeEach, describe, expect, it, vi } from 'vitest'
import type { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import type { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import type { getEngineHubModels } from '@/electron/service-bridge/model-catalog'

type BridgeState = ReturnType<typeof getModularBridgeState>
type Supervisor = ReturnType<typeof getModularSupervisor>

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn<BridgeState['getSelfId']>(() => 'local-node'),
        beginLocalEngineOp: vi.fn<BridgeState['beginLocalEngineOp']>(),
        clearPendingEngineOp: vi.fn<BridgeState['clearPendingEngineOp']>()
    },
    supervisor: {
        hasProcess: vi.fn<Supervisor['hasProcess']>(() => true),
        sendProcess: vi.fn<Supervisor['sendProcess']>(),
        callProcess: vi.fn<Supervisor['callProcess']>(),
        reportError: vi.fn<Supervisor['reportError']>(),
        toggleEngineRemote: vi.fn<Supervisor['toggleEngineRemote']>()
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
vi.mock('@/electron/service-bridge/model-catalog', () => ({
    getEngineHubModels: vi.fn<typeof getEngineHubModels>()
}))

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

        assert.isDefined(onFailure)
        onFailure('cannot stop engine "vllm": it is running under external management', true)
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
