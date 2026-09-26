// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn(() => 'local-node'),
        beginLocalEngineOp: vi.fn()
    },
    supervisor: {
        hasProcess: vi.fn(() => true),
        callProcess: vi.fn().mockResolvedValue({ accepted: true }),
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

describe('local model load command', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        mocks.state.getSelfId.mockReturnValue('local-node')
        mocks.supervisor.hasProcess.mockReturnValue(true)
        mocks.supervisor.callProcess.mockResolvedValue({ accepted: true })
    })

    it('refuses a local llama update without uninstalling, installing, or acting', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType: 'llamacpp',
            nodeId: 'local-node'
        })
        // No engine:uninstall, engine:install, or engine:action leaves the
        // bridge, and no optimistic lifecycle op is begun: the refusal is the
        // whole response.
        expect(mocks.supervisor.sendProcess).not.toHaveBeenCalled()
        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
        expect(mocks.state.beginLocalEngineOp).not.toHaveBeenCalled()
        expect(mocks.supervisor.reportError).toHaveBeenCalledExactlyOnceWith(
            'llama.cpp has no managed update; uninstall and reinstall the managed runtime instead.',
            'warning',
            'engine-cmd:update:llamacpp',
            { engineType: 'llamacpp', operation: 'update' }
        )
    })

    it('refuses a remote llama update like every other remote update', async () => {
        mocks.state.getSelfId.mockReturnValue('other-node')
        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType: 'llamacpp',
            nodeId: 'peer-node'
        })
        expect(mocks.supervisor.sendProcess).not.toHaveBeenCalled()
        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
        expect(mocks.supervisor.reportError).toHaveBeenCalledExactlyOnceWith(
            'update is only available on the local node — remote uninstall/update is not supported yet.',
            'warning',
            'engine-cmd:remote:update'
        )
    })

    it('keeps the uninstall-then-install update pair for the other managed engines', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType: 'ollama',
            nodeId: 'local-node'
        })
        expect(mocks.state.beginLocalEngineOp).toHaveBeenCalledExactlyOnceWith(
            'ollama',
            'installing'
        )
        expect(mocks.supervisor.sendProcess).toHaveBeenCalledTimes(2)
        expect(mocks.supervisor.sendProcess).toHaveBeenNthCalledWith(
            1,
            'broker',
            'engine:uninstall',
            { engine: 'ollama' },
            expect.any(Function),
            true
        )
        expect(mocks.supervisor.sendProcess).toHaveBeenNthCalledWith(
            2,
            'broker',
            'engine:install',
            { engine: 'ollama', start: true },
            expect.any(Function),
            true
        )
        expect(mocks.supervisor.reportError).not.toHaveBeenCalled()
    })

    it('routes llama load and cancel to the owning engine manager', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'loadModel',
            engineType: 'llamacpp',
            nodeId: 'local-node',
            model: 'owner/model:Q4'
        })
        expect(mocks.supervisor.sendProcess).toHaveBeenCalledWith(
            'broker',
            'engine:action',
            { engine: 'llamacpp', action: 'load_model', params: { model: 'owner/model:Q4' } },
            expect.any(Function),
            true
        )
        await handleServiceBridgeInvoke('engine:command', {
            command: 'cancelPull',
            engineType: 'llamacpp',
            nodeId: 'local-node',
            model: 'owner/model:Q4'
        })
        expect(mocks.supervisor.sendProcess).toHaveBeenLastCalledWith(
            'broker',
            'engine:action',
            { engine: 'llamacpp', action: 'cancel_pull', params: { model: 'owner/model:Q4' } },
            expect.any(Function),
            // Observed like the load above: a cancel the backend refuses has to
            // clear the optimistic row rather than look like it succeeded.
            true
        )
    })

    it('observes and attributes an Ollama load rejection to the pending model row', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'loadModel',
            engineType: 'ollama',
            nodeId: 'local-node',
            model: 'llama3.2'
        })

        expect(mocks.supervisor.sendProcess).toHaveBeenCalledOnce()
        const [name, method, params, onFailure, observeResponse] =
            mocks.supervisor.sendProcess.mock.calls[0]
        expect([name, method, params, observeResponse]).toEqual([
            'broker',
            'engine:action',
            {
                engine: 'ollama',
                action: 'run_model',
                params: { model: 'llama3.2', stream: false }
            },
            true
        ])

        onFailure('timeout awaiting response headers', true)
        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'Failed to load model on ollama: timeout awaiting response headers',
            'error',
            'engine-cmd:load model:ollama',
            {
                nodeId: 'local-node',
                engineType: 'ollama',
                operation: 'load',
                modelName: 'llama3.2'
            }
        )
    })

    it('leaves the LM Studio load path unobserved', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'loadModel',
            engineType: 'lm-studio',
            nodeId: 'local-node',
            model: 'publisher/demo'
        })

        expect(mocks.supervisor.sendProcess).toHaveBeenCalledOnce()
        expect(mocks.supervisor.sendProcess.mock.calls[0]).toHaveLength(4)
        expect(mocks.supervisor.sendProcess).toHaveBeenCalledWith(
            'broker',
            'engine:action',
            {
                engine: 'lmstudio',
                action: 'load_model',
                params: { model: 'publisher/demo' }
            },
            expect.any(Function)
        )
    })
})
