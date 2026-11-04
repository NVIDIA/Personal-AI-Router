// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn(() => 'local-node')
    },
    supervisor: {
        hasProcess: vi.fn(() => true),
        refreshEngineModels: vi.fn(),
        refreshRemoteEngineStatus: vi.fn(),
        importLocalModel: vi.fn(),
        pullModel: vi.fn(),
        pullModelRemote: vi.fn(),
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
vi.mock('@/electron/service-bridge/model-catalog', () => ({ getEngineHubModels: vi.fn() }))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

describe('custom and local model commands', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        mocks.state.getSelfId.mockReturnValue('local-node')
        mocks.supervisor.hasProcess.mockReturnValue(true)
    })

    it('dispatches refreshModels to supervisor.refreshEngineModels on local node', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'refreshModels',
            engineType: 'ollama',
            nodeId: 'local-node'
        })

        expect(mocks.supervisor.refreshEngineModels).toHaveBeenCalledOnce()
        expect(mocks.supervisor.refreshEngineModels).toHaveBeenCalledWith('ollama', 'ollama')
    })

    it('dispatches importLocalModel to supervisor.importLocalModel on local node', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'importLocalModel',
            engineType: 'ollama',
            nodeId: 'local-node',
            model: 'my-custom-model',
            filePath: '/models/llama-3.2-1b.gguf'
        })

        expect(mocks.supervisor.importLocalModel).toHaveBeenCalledOnce()
        expect(mocks.supervisor.importLocalModel).toHaveBeenCalledWith(
            'ollama',
            'ollama',
            'my-custom-model',
            '/models/llama-3.2-1b.gguf'
        )
    })

    it('dispatches refreshModels to supervisor.refreshRemoteEngineStatus on remote node', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'refreshModels',
            engineType: 'ollama',
            nodeId: 'remote-peer-node'
        })

        expect(mocks.supervisor.refreshRemoteEngineStatus).toHaveBeenCalledOnce()
        expect(mocks.supervisor.refreshRemoteEngineStatus).toHaveBeenCalledWith('remote-peer-node')
    })

    it('refuses importLocalModel on remote node because files reside on host machine', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'importLocalModel',
            engineType: 'ollama',
            nodeId: 'remote-peer-node',
            model: 'my-custom-model',
            filePath: '/models/llama-3.2-1b.gguf'
        })

        expect(mocks.supervisor.importLocalModel).not.toHaveBeenCalled()
        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            expect.stringContaining(
                'Importing a local model file is only available on the local node'
            ),
            'warning',
            'engine-cmd:remote:importLocalModel'
        )
    })
})
