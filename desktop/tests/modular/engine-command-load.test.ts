// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn(() => 'local-node'),
        beginLocalModelSwitch: vi.fn(),
        clearLocalModelSwitch: vi.fn()
    },
    supervisor: {
        hasProcess: vi.fn(() => true),
        sendProcess: vi.fn(),
        callProcess: vi.fn(),
        reportError: vi.fn(),
        setIdleTimeout: vi.fn()
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

describe('local model load command', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        mocks.state.getSelfId.mockReturnValue('local-node')
        mocks.supervisor.hasProcess.mockReturnValue(true)
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

    describe('vLLM model switch', () => {
        const load = () =>
            handleServiceBridgeInvoke('engine:command', {
                command: 'loadModel',
                engineType: 'vllm',
                nodeId: 'local-node',
                model: 'Qwen/Qwen3-8B'
            })
        const flush = () => new Promise(resolve => setTimeout(resolve, 0))

        it('awaits load_model and shows the engine as starting, not stopped, until it finishes', async () => {
            let finish: (value: undefined) => void = () => undefined
            mocks.supervisor.callProcess.mockReturnValue(
                new Promise(resolve => {
                    finish = resolve
                })
            )
            await load()

            // The switch restarts the engine, so it is marked before the request is sent.
            expect(mocks.state.beginLocalModelSwitch).toHaveBeenCalledWith('vllm')
            expect(mocks.supervisor.sendProcess).not.toHaveBeenCalled()
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'engine:action',
                { engine: 'vllm', action: 'load_model', params: { model: 'Qwen/Qwen3-8B' } },
                20 * 60_000
            )
            // Still in progress: the marker stays.
            await flush()
            expect(mocks.state.clearLocalModelSwitch).not.toHaveBeenCalled()

            finish(undefined)
            await flush()
            expect(mocks.state.clearLocalModelSwitch).toHaveBeenCalledWith('vllm')
            expect(mocks.supervisor.reportError).not.toHaveBeenCalled()
        })

        it('attributes a failure to the model row and ends the marker', async () => {
            mocks.supervisor.callProcess.mockRejectedValue(
                new Error('did not become ready; the previous model was restored')
            )
            await load()
            await flush()

            expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
                'Failed to load model on vllm: did not become ready; the previous model was restored',
                'error',
                'engine-cmd:load model:vllm',
                {
                    nodeId: 'local-node',
                    engineType: 'vllm',
                    operation: 'load',
                    modelName: 'Qwen/Qwen3-8B'
                }
            )
            // A failed switch will not bring the engine back by itself.
            expect(mocks.state.clearLocalModelSwitch).toHaveBeenCalledWith('vllm')
        })
    })

    it('does not mark an Ollama load as a model switch', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'loadModel',
            engineType: 'ollama',
            nodeId: 'local-node',
            model: 'llama3.2'
        })
        expect(mocks.state.beginLocalModelSwitch).not.toHaveBeenCalled()
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

    it('routes llama.cpp load and unload through its manifest actions', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'loadModel',
            engineType: 'llama-cpp',
            nodeId: 'local-node',
            model: 'ggml-org/gemma-3-1b-it-GGUF:Q4_K_M'
        })
        await handleServiceBridgeInvoke('engine:command', {
            command: 'unloadModel',
            engineType: 'llama-cpp',
            nodeId: 'local-node',
            model: 'ggml-org/gemma-3-1b-it-GGUF:Q4_K_M'
        })

        expect(mocks.supervisor.sendProcess).toHaveBeenNthCalledWith(
            1,
            'broker',
            'engine:action',
            {
                engine: 'llamacpp',
                action: 'load_model',
                params: { model: 'ggml-org/gemma-3-1b-it-GGUF:Q4_K_M' }
            },
            expect.any(Function)
        )
        expect(mocks.supervisor.sendProcess).toHaveBeenNthCalledWith(
            2,
            'broker',
            'engine:action',
            {
                engine: 'llamacpp',
                action: 'unload_model',
                params: { model: 'ggml-org/gemma-3-1b-it-GGUF:Q4_K_M' }
            },
            expect.any(Function)
        )
    })

    describe('idle limit', () => {
        const choose = (engineType: 'vllm' | 'ollama', expiry: string) =>
            handleServiceBridgeInvoke('engine:command', {
                command: 'setModelExpiry',
                engineType,
                nodeId: 'local-node',
                model: 'Qwen/Qwen3-8B',
                expiry
            })

        it('sends the chosen limit for vLLM as seconds', async () => {
            await choose('vllm', '30m')
            expect(mocks.supervisor.setIdleTimeout).toHaveBeenCalledWith('vllm', 'vllm', 1800)
            await choose('vllm', '-1')
            expect(mocks.supervisor.setIdleTimeout).toHaveBeenLastCalledWith('vllm', 'vllm', 0)
        })

        it('refuses a value that is not an idle limit, and an engine without one', async () => {
            await choose('vllm', '10s')
            await choose('ollama', '10m')
            expect(mocks.supervisor.setIdleTimeout).not.toHaveBeenCalled()
            expect(mocks.supervisor.reportError).toHaveBeenCalledTimes(2)
        })
    })
})
