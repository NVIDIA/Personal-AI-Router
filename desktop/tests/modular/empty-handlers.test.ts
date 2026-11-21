// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'
import type { EngineManagerName, EngineType } from '@/shared/types/engines'
import type { getEngineHubModels } from '@/electron/service-bridge/model-catalog'
import type { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import type { getModularBridgeState } from '@/electron/service-bridge/modular-state'

type Supervisor = ReturnType<typeof getModularSupervisor>
type BridgeState = ReturnType<typeof getModularBridgeState>

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn<BridgeState['getSelfId']>(),
        canCheckEngineUpdate: vi.fn<BridgeState['canCheckEngineUpdate']>(),
        beginLocalEngineOp: vi.fn<BridgeState['beginLocalEngineOp']>(),
        clearPendingEngineOp: vi.fn<BridgeState['clearPendingEngineOp']>()
    },
    supervisor: {
        callProcess: vi.fn<Supervisor['callProcess']>(),
        hasProcess: vi.fn<Supervisor['hasProcess']>(() => true),
        installOrUpdateEngineRemote: vi.fn<Supervisor['installOrUpdateEngineRemote']>(),
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
    it.each<{ engineType: EngineType; engine: EngineManagerName }>([
        { engineType: 'ollama', engine: 'ollama' },
        { engineType: 'lm-studio', engine: 'lmstudio' },
        { engineType: 'llama-cpp', engine: 'llamacpp' },
        { engineType: 'vllm', engine: 'vllm' }
    ])('sends one managed update for $engineType as $engine', async ({ engineType, engine }) => {
        mocks.supervisor.sendProcess.mockClear()
        mocks.state.getSelfId.mockReturnValue('self')
        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType,
            nodeId: 'self'
        })

        expect(mocks.supervisor.sendProcess).toHaveBeenCalledOnce()
        expect(mocks.supervisor.sendProcess).toHaveBeenCalledWith(
            'broker',
            'engine:update',
            { engine },
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
            engineType: 'lm-studio',
            nodeId: 'self'
        })
        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'Failed to update lmstudio: update rejected',
            'error',
            undefined,
            {
                id: 'engine-manager:update-failed:lmstudio',
                nodeId: 'self',
                engineType: 'lm-studio',
                operation: 'update',
                action: 'retry'
            }
        )
    })

    it('waits for a local node identity before routing an update', async () => {
        mocks.supervisor.sendProcess.mockClear()
        mocks.supervisor.installOrUpdateEngineRemote.mockClear()
        mocks.state.getSelfId.mockReturnValue(null)
        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType: 'ollama',
            nodeId: 'peer'
        })
        expect(mocks.supervisor.sendProcess).not.toHaveBeenCalled()
        expect(mocks.supervisor.installOrUpdateEngineRemote).not.toHaveBeenCalled()
        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'Select an identified node to update its engine.',
            'warning',
            'engine-cmd:update'
        )
    })
})

describe('remote engine update command', () => {
    it('routes the update to the selected peer and engine', async () => {
        mocks.supervisor.sendProcess.mockClear()
        mocks.state.getSelfId.mockReturnValue('self')
        mocks.state.canCheckEngineUpdate.mockReturnValue(true)
        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType: 'lm-studio',
            nodeId: 'peer'
        })

        expect(mocks.state.canCheckEngineUpdate).toHaveBeenCalledWith('peer', 'lm-studio')
        expect(mocks.supervisor.installOrUpdateEngineRemote).toHaveBeenCalledWith(
            'peer',
            'lmstudio',
            'lm-studio',
            'update'
        )
        expect(mocks.supervisor.sendProcess).not.toHaveBeenCalled()
    })

    it('declines a peer whose engine is unavailable', async () => {
        mocks.supervisor.installOrUpdateEngineRemote.mockClear()
        mocks.state.getSelfId.mockReturnValue('self')
        mocks.state.canCheckEngineUpdate.mockReturnValue(false)
        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType: 'lm-studio',
            nodeId: 'peer'
        })

        expect(mocks.supervisor.installOrUpdateEngineRemote).not.toHaveBeenCalled()
        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'Updating requires a connected, paired node with this engine installed.',
            'warning',
            'engine-cmd:remote:update'
        )
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
