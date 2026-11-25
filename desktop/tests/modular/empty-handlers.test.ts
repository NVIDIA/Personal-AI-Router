// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'
import type { getEngineHubModels } from '@/electron/service-bridge/model-catalog'
import type { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import type { getModularBridgeState } from '@/electron/service-bridge/modular-state'

type Supervisor = ReturnType<typeof getModularSupervisor>
type BridgeState = ReturnType<typeof getModularBridgeState>

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn<BridgeState['getSelfId']>(),
        beginLocalEngineOp: vi.fn<BridgeState['beginLocalEngineOp']>(),
        clearPendingEngineOp: vi.fn<BridgeState['clearPendingEngineOp']>()
    },
    supervisor: {
        callProcess: vi.fn<Supervisor['callProcess']>(),
        hasProcess: vi.fn<Supervisor['hasProcess']>(() => true),
        sendProcess: vi.fn<Supervisor['sendProcess']>(),
        reportError: vi.fn<Supervisor['reportError']>()
    }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => [],
    parseWorkloadsInitial: () => ({})
}))
vi.mock('@/electron/service-bridge/model-catalog', () => ({
    getEngineHubModels: vi.fn<typeof getEngineHubModels>()
}))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

describe('local engine update command', () => {
    it('sends one managed Ollama update', async () => {
        mocks.supervisor.sendProcess.mockClear()
        mocks.state.getSelfId.mockReturnValue('self')
        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType: 'ollama',
            nodeId: 'self'
        })

        expect(mocks.supervisor.sendProcess).toHaveBeenCalledOnce()
        expect(mocks.supervisor.sendProcess).toHaveBeenCalledWith(
            'broker',
            'engine:update',
            { engine: 'ollama' },
            expect.any(Function),
            true
        )
    })

    it('retains the local node and UI engine identifier when a managed update is rejected', async () => {
        mocks.state.getSelfId.mockReturnValue('self')
        mocks.supervisor.sendProcess.mockImplementationOnce((_name, _method, _params, onError) => {
            onError?.('update rejected', true)
        })
        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType: 'ollama',
            nodeId: 'self'
        })
        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'Failed to update ollama: update rejected',
            'error',
            undefined,
            {
                id: 'engine-manager:update-failed:ollama',
                nodeId: 'self',
                engineType: 'ollama',
                operation: 'update',
                action: 'retry'
            }
        )
    })

    it('does not dispatch a peer update to the local engine manager', async () => {
        mocks.supervisor.sendProcess.mockClear()
        mocks.state.getSelfId.mockReturnValue('self')
        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType: 'ollama',
            nodeId: 'peer'
        })

        expect(mocks.supervisor.sendProcess).not.toHaveBeenCalled()
        expect(mocks.supervisor.reportError).toHaveBeenCalled()
    })
})

describe('manual node removal', () => {
    // The desktop knows a broker node key. Its request must select that key
    // explicitly so an identical worker entry name cannot remove another entry.
    it('identifies a broker node key separately from a manual entry ID', async () => {
        const nodeId = 'manual:lab'
        mocks.state.getSelfId.mockReturnValue('self')
        mocks.supervisor.callProcess.mockResolvedValue({ nodeId, removed: true })

        const result = await handleServiceBridgeInvoke('nodes:remove-member', { nodeId })

        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'node/remove', {
            nodeId
        })
        expect(result).toEqual({ nodeId, removed: true })
    })
})
