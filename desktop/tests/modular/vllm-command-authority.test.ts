// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { EngineCommandType } from '@/shared/types/engine-api'

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn(() => 'local-node'),
        isEngineCommandAllowed: vi.fn(() => false),
        beginLocalEngineOp: vi.fn(),
        clearPendingEngineOp: vi.fn()
    },
    supervisor: {
        hasProcess: vi.fn(() => true),
        callProcess: vi.fn(),
        sendProcess: vi.fn(),
        installEngineRemote: vi.fn(),
        updateEngineRemote: vi.fn(),
        reportError: vi.fn()
    }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state,
    isProxyEngine: () => true,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => []
}))
vi.mock('@/electron/model-hub', () => ({ getEngineHubModels: vi.fn() }))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

describe('vLLM command authority', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        mocks.state.isEngineCommandAllowed.mockReturnValue(false)
    })

    const commands: EngineCommandType[] = [
        'toggle',
        'install',
        'installAll',
        'uninstall',
        'update',
        'pullModel',
        'loadModel',
        'unloadModel',
        'deleteModel',
        'setModelExpiry'
    ]

    it.each(commands)('rejects %s without managed=true', async command => {
        await handleServiceBridgeInvoke('engine:command', {
            command,
            engineType: 'vllm',
            nodeId: 'local-node',
            model: 'owner/model',
            expiry: '1m'
        })

        expect(mocks.supervisor.sendProcess).not.toHaveBeenCalled()
        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            `${command} is unavailable for this vLLM host or ownership state.`,
            'warning',
            `engine-cmd:authority:${command}`,
            { nodeId: 'local-node', engineType: 'vllm', modelName: 'owner/model' }
        )
    })

    it('allows an explicitly managed vLLM command to reach Engine Manager', async () => {
        mocks.state.isEngineCommandAllowed.mockReturnValue(true)
        await handleServiceBridgeInvoke('engine:command', {
            command: 'uninstall',
            engineType: 'vllm',
            nodeId: 'local-node'
        })

        expect(mocks.state.beginLocalEngineOp).toHaveBeenCalledWith('vllm', 'uninstalling')
        expect(mocks.supervisor.sendProcess).toHaveBeenCalledWith(
            'broker',
            'engine:uninstall',
            { engine: 'vllm' },
            expect.any(Function),
            true
        )
    })

    it('installs supported local vLLM stopped so model selection remains explicit', async () => {
        mocks.state.isEngineCommandAllowed.mockReturnValue(true)
        await handleServiceBridgeInvoke('engine:command', {
            command: 'install',
            engineType: 'vllm',
            nodeId: 'local-node'
        })

        expect(mocks.state.beginLocalEngineOp).toHaveBeenCalledWith('vllm', 'installing')
        expect(mocks.supervisor.sendProcess).toHaveBeenCalledWith(
            'broker',
            'engine:install',
            { engine: 'vllm', start: false },
            expect.any(Function),
            true
        )
    })

    it('installs supported remote vLLM stopped so model selection remains explicit', async () => {
        mocks.state.isEngineCommandAllowed.mockReturnValue(true)
        await handleServiceBridgeInvoke('engine:command', {
            command: 'install',
            engineType: 'vllm',
            nodeId: 'remote-node'
        })

        expect(mocks.supervisor.installEngineRemote).toHaveBeenCalledWith(
            'remote-node',
            'vllm',
            'vllm',
            false
        )
    })

    it('routes local managed vLLM update to the atomic backend operation', async () => {
        mocks.state.isEngineCommandAllowed.mockReturnValue(true)
        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType: 'vllm',
            nodeId: 'local-node'
        })

        expect(mocks.state.beginLocalEngineOp).toHaveBeenCalledWith('vllm', 'installing')
        expect(mocks.supervisor.sendProcess).toHaveBeenCalledTimes(1)
        expect(mocks.supervisor.sendProcess).toHaveBeenCalledWith(
            'broker',
            'engine:update',
            { engine: 'vllm' },
            expect.any(Function),
            true
        )
        expect(mocks.supervisor.sendProcess).not.toHaveBeenCalledWith(
            'broker',
            'engine:uninstall',
            expect.anything(),
            expect.anything(),
            expect.anything()
        )
    })

    it('routes remote managed vLLM update through the product-owned peer operation', async () => {
        mocks.state.isEngineCommandAllowed.mockReturnValue(true)
        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType: 'vllm',
            nodeId: 'remote-node'
        })

        expect(mocks.supervisor.updateEngineRemote).toHaveBeenCalledWith(
            'remote-node',
            'vllm',
            'vllm'
        )
        expect(mocks.supervisor.sendProcess).not.toHaveBeenCalled()
    })

    it('keeps remote update closed for engines without managed peer ownership', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType: 'lm-studio',
            nodeId: 'remote-node'
        })

        expect(mocks.supervisor.updateEngineRemote).not.toHaveBeenCalled()
        expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
            'Remote update is only available for PAIR-managed vLLM.',
            'warning',
            'engine-cmd:remote:update'
        )
    })
})
