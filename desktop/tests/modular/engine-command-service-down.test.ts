// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn(() => 'local-node')
    },
    supervisor: {
        // Broker absent: the modular service is stopped, so every engine command
        // hits the "not available" branch.
        hasProcess: vi.fn(() => false),
        callProcess: vi.fn(),
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

describe('engine command while the modular service is stopped', () => {
    it('stamps the local "not available" warning with node + engine so the pending clear path matches', async () => {
        mocks.state.getSelfId.mockReturnValue('local-node')
        mocks.supervisor.hasProcess.mockReturnValue(false)

        await handleServiceBridgeInvoke('engine:command', {
            command: 'toggle',
            engineType: 'ollama',
            nodeId: 'local-node'
        })

        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'toggle is not available until the modular backend is running.',
            'warning',
            'engine-cmd:toggle',
            { nodeId: 'local-node', engineType: 'ollama', modelName: undefined }
        )
    })

    it('stamps the model name for a model command so the model pending entry matches', async () => {
        mocks.state.getSelfId.mockReturnValue('local-node')
        mocks.supervisor.hasProcess.mockReturnValue(false)

        await handleServiceBridgeInvoke('engine:command', {
            command: 'pullModel',
            engineType: 'ollama',
            nodeId: 'local-node',
            model: 'llama3.1:8b'
        })

        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'pullModel is not available until the modular backend is running.',
            'warning',
            'engine-cmd:pullModel',
            { nodeId: 'local-node', engineType: 'ollama', modelName: 'llama3.1:8b' }
        )
    })

    it('stamps the remote node id (not self) so a remote command matches its own pending key', async () => {
        mocks.state.getSelfId.mockReturnValue('local-node')
        mocks.supervisor.hasProcess.mockReturnValue(false)

        await handleServiceBridgeInvoke('engine:command', {
            command: 'toggle',
            engineType: 'ollama',
            nodeId: 'remote-node'
        })

        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'toggle is not available until the modular backend is running.',
            'warning',
            'engine-cmd:toggle',
            { nodeId: 'remote-node', engineType: 'ollama', modelName: undefined }
        )
    })
})
