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
        beginLocalEngineOp: vi.fn<BridgeState['beginLocalEngineOp']>()
    },
    supervisor: {
        hasProcess: vi.fn<Supervisor['hasProcess']>(() => true),
        sendProcess: vi.fn<Supervisor['sendProcess']>(),
        callProcess: vi.fn<Supervisor['callProcess']>(),
        reportError: vi.fn<Supervisor['reportError']>()
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

        assert.isDefined(onFailure)
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

        // The renderer's pending Load shows the engine as starting while vLLM
        // restarts, and backend truth ends it; the main process only sends the
        // request and attributes a failure to the model row.
        it('awaits load_model for as long as a switch may take', async () => {
            const reply = Promise.withResolvers<undefined>()
            mocks.supervisor.callProcess.mockReturnValue(reply.promise)

            await load()

            expect(mocks.supervisor.sendProcess).not.toHaveBeenCalled()
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'engine:action',
                { engine: 'vllm', action: 'load_model', params: { model: 'Qwen/Qwen3-8B' } },
                20 * 60_000
            )
            reply.resolve(undefined)
            await flush()
            expect(mocks.supervisor.reportError).not.toHaveBeenCalled()
        })

        // A Stop or Eject ends the Load, and the reply says so rather than
        // failing. The user who stopped it here needs no message; a stop made
        // elsewhere is reported, so the Load does not stay shown in progress.
        it('reports a stopped Load only when the stop came from elsewhere', async () => {
            const stopped = { model: 'Qwen/Qwen3-8B', restarted: false, stopped: true }
            mocks.supervisor.callProcess.mockResolvedValue(stopped)

            await load()
            await flush()
            expect(mocks.supervisor.reportError).toHaveBeenCalledWith(
                'Loading Qwen/Qwen3-8B on vllm was stopped before it finished.',
                'info',
                'engine-cmd:load model:vllm',
                {
                    nodeId: 'local-node',
                    engineType: 'vllm',
                    operation: 'load',
                    modelName: 'Qwen/Qwen3-8B'
                }
            )

            mocks.supervisor.reportError.mockClear()
            const reply = Promise.withResolvers<typeof stopped>()
            mocks.supervisor.callProcess.mockReturnValue(reply.promise)
            await load()
            await handleServiceBridgeInvoke('engine:command', {
                command: 'stop',
                engineType: 'vllm',
                nodeId: 'local-node'
            })
            reply.resolve(stopped)
            await flush()
            expect(mocks.supervisor.reportError).not.toHaveBeenCalled()
        })

        it('attributes a failure to the model row', async () => {
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
        })
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
})
