// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import {
    isProxyEngine,
    PROXY_ENGINES,
    PROXY_NODE_SOURCES,
    proxyEngineFromManagerId,
    proxyEngineFromSource,
    proxySourceForEngine
} from '@/electron/service-bridge/proxy-engines'
import { engineManagerName } from '@/shared/utils/engines'

describe('proxy engine identity', () => {
    it('declares the complete proxy set in stable order', () => {
        expect(PROXY_ENGINES).toEqual(['ollama', 'lm-studio', 'llama-cpp', 'vllm'])
        expect(PROXY_NODE_SOURCES).toEqual([
            'ollama-proxy',
            'lmstudio-proxy',
            'llamacpp-proxy',
            'vllm-proxy'
        ])
        expect(isProxyEngine('ollama')).toBe(true)
        expect(isProxyEngine('lm-studio')).toBe(true)
        expect(isProxyEngine('llama-cpp')).toBe(true)
        expect(isProxyEngine('vllm')).toBe(true)
    })

    it('round-trips engine, manager, and relay source identities', () => {
        for (const engine of PROXY_ENGINES) {
            const source = proxySourceForEngine(engine)
            expect(proxyEngineFromSource(source)).toBe(engine)
            expect(proxyEngineFromManagerId(engineManagerName(engine))).toBe(engine)
        }
        expect(proxyEngineFromSource('other-proxy')).toBeNull()
        expect(proxyEngineFromManagerId('other')).toBeNull()
        expect(proxyEngineFromManagerId('llamacpp')).toBe('llama-cpp')
        expect(proxyEngineFromManagerId('vllm')).toBe('vllm')
    })

    it('routes each proxy notification to the mapped engine', () => {
        const state = getModularBridgeState()
        const nodeId = 'proxy-identity-remote'
        state.setSelfId('proxy-identity-self')

        state.handleNotification({
            source: 'ollama-proxy',
            method: 'node/discovered',
            params: {
                id: nodeId,
                host: 'proxy-identity-host',
                port: 11434,
                addresses: ['192.0.2.40'],
                ip: '192.0.2.40'
            }
        })
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: {
                id: nodeId,
                host: 'proxy-identity-host',
                port: 1234,
                addresses: ['192.0.2.40'],
                ip: '192.0.2.40'
            }
        })
        state.handleNotification({
            source: 'llamacpp-proxy',
            method: 'ready',
            params: { port: 8080 }
        })
        state.handleNotification({
            source: 'llamacpp-proxy',
            method: 'node/discovered',
            params: {
                id: nodeId,
                host: 'proxy-identity-host',
                port: 8080,
                addresses: ['192.0.2.40'],
                ip: '192.0.2.40'
            }
        })
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: nodeId,
                        name: 'proxy-identity-host',
                        ipAddress: '192.0.2.40',
                        port: 14318,
                        models: ['owner/alpha:Q4_K_M', 'owner/beta:Q4_K_M'],
                        modelsByEngine: {
                            llamacpp: ['owner/alpha:Q4_K_M', 'owner/beta:Q4_K_M']
                        },
                        loadedByEngine: { llamacpp: ['owner/alpha:Q4_K_M'] }
                    }
                ]
            }
        })
        // A peer lists every engine it can run; one it leaves out is unsupported.
        state.applyRemoteEngineFacts(nodeId, {
            engines: [
                { engine: 'ollama', installed: true, running: true, healthy: true, port: 11435 },
                { engine: 'lmstudio', installed: true, running: true, healthy: true, port: 1235 },
                {
                    engine: 'llamacpp',
                    installed: true,
                    running: true,
                    healthy: true,
                    port: 8081
                },
                { engine: 'vllm', installed: false, running: false, healthy: false, port: 0 }
            ]
        })

        const statuses = state
            .getEngineInitialState()
            .statuses.filter(status => status.nodeId === nodeId)
        expect(statuses).toEqual([
            expect.objectContaining({
                engineType: 'ollama',
                processStatus: 'running',
                proxyPort: 11434
            }),
            expect.objectContaining({
                engineType: 'lm-studio',
                processStatus: 'running',
                proxyPort: 1234
            }),
            expect.objectContaining({
                engineType: 'llama-cpp',
                processStatus: 'running',
                enginePort: 8081,
                proxyPort: 8080
            }),
            expect.objectContaining({ engineType: 'vllm', processStatus: 'not-installed' })
        ])

        const llamaModels = state
            .getEngineInitialState()
            .models.find(models => models.nodeId === nodeId && models.engineType === 'llama-cpp')
        expect(llamaModels?.models).toEqual([
            expect.objectContaining({ name: 'owner/alpha:Q4_K_M', status: 'loaded' }),
            expect.objectContaining({ name: 'owner/beta:Q4_K_M', status: 'idle' })
        ])
    })

    it('projects local llama.cpp lifecycle and residency into the initial snapshot', () => {
        const state = getModularBridgeState()
        const nodeId = 'proxy-identity-local'
        const model = 'ggml-org/gemma-3-1b-it-GGUF:Q4_K_M'
        state.setSelfId(nodeId)
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: nodeId,
                        name: 'proxy-identity-local-host',
                        ipAddress: '127.0.0.1',
                        port: 14318
                    }
                ]
            }
        })
        state.handleNotification({
            source: 'llamacpp-proxy',
            method: 'ready',
            params: { port: 8080 }
        })
        state.applyEngineManagerStatus({
            engine: 'llamacpp',
            installed: true,
            running: true,
            healthy: true,
            port: 8081
        })
        state.setLocalEngineModels('llama-cpp', [model])
        state.applyLocalModelSnapshot({ models: { loadedByEngine: { llamacpp: [model] } } })

        const initial = state.getEngineInitialState()
        expect(
            initial.statuses.find(
                status => status.nodeId === nodeId && status.engineType === 'llama-cpp'
            )
        ).toMatchObject({
            processStatus: 'running',
            enginePort: 8081,
            proxyPort: 8080
        })
        expect(
            initial.models.find(
                models => models.nodeId === nodeId && models.engineType === 'llama-cpp'
            )?.models
        ).toEqual([expect.objectContaining({ name: model, status: 'loaded' })])
    })
    it('projects local vLLM lifecycle and its single served model into the initial snapshot', () => {
        const state = getModularBridgeState()
        const nodeId = 'proxy-identity-vllm-local'
        const model = 'Qwen/Qwen2.5-0.5B-Instruct'
        state.setSelfId(nodeId)
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: nodeId,
                        name: 'proxy-identity-vllm-host',
                        ipAddress: '127.0.0.1',
                        port: 14318
                    }
                ]
            }
        })
        state.handleNotification({
            source: 'vllm-proxy',
            method: 'ready',
            params: { port: 8000 }
        })
        state.applyEngineManagerStatus({
            engine: 'vllm',
            installed: true,
            running: true,
            healthy: true,
            port: 8001
        })
        state.setLocalEngineModels('vllm', [model])
        state.applyLocalModelSnapshot({ models: { loadedByEngine: { vllm: [model] } } })

        const initial = state.getEngineInitialState()
        expect(
            initial.statuses.find(
                status => status.nodeId === nodeId && status.engineType === 'vllm'
            )
        ).toMatchObject({
            processStatus: 'running',
            enginePort: 8001,
            proxyPort: 8000
        })
        expect(
            initial.models.find(models => models.nodeId === nodeId && models.engineType === 'vllm')
                ?.models
        ).toEqual([expect.objectContaining({ name: model, status: 'loaded' })])
    })
    it('keeps cached vLLM models idle while the enabled engine awaits a request', () => {
        const state = getModularBridgeState()
        const nodeId = 'vllm-cached-awaiting-request'
        const models = ['org/model-a', 'org/model-b']
        state.setSelfId(nodeId)
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: nodeId,
                        name: 'awaiting-request',
                        ipAddress: '127.0.0.1',
                        port: 14318,
                        modelsByEngine: { vllm: models },
                        downloadedByEngine: { vllm: models },
                        loadedByEngine: { vllm: [] },
                        requestableByEngine: { vllm: models }
                    }
                ]
            }
        })
        state.applyEngineManagerStatus({
            engine: 'vllm',
            installed: true,
            enabled: true,
            running: false,
            awaiting_model: true,
            healthy: false,
            port: 8001
        })
        state.setLocalEngineModels('vllm', models)
        state.applyLocalModelSnapshot({ models: { loadedByEngine: { vllm: [] } } })
        const initial = state.getEngineInitialState()
        expect(
            initial.statuses.find(
                engine => engine.nodeId === nodeId && engine.engineType === 'vllm'
            )
        ).toMatchObject({ processStatus: 'awaiting-model' })
        expect(
            initial.models.find(engine => engine.nodeId === nodeId && engine.engineType === 'vllm')
                ?.models
        ).toEqual(models.map(name => expect.objectContaining({ name, status: 'idle' })))
    })
    it.each([
        {
            description: 'removes a remotely deleted row while awaiting a model',
            downloaded: ['org/model-a'],
            expected: ['org/model-a']
        },
        {
            description: 'clears the inventory after the last remote deletion',
            downloaded: [],
            expected: []
        },
        {
            description: 'adds a remotely downloaded row while awaiting a model',
            downloaded: ['org/model-a', 'org/model-b', 'org/model-c'],
            expected: ['org/model-a', 'org/model-b', 'org/model-c']
        },
        {
            description: 'preserves the inventory when its read is unavailable',
            downloaded: undefined,
            expected: ['org/model-a', 'org/model-b']
        }
    ])('$description', ({ downloaded, expected }) => {
        const state = getModularBridgeState()
        const nodeId = 'local-model-inventory-push'
        state.setSelfId(nodeId)
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: nodeId,
                        name: 'inventory-owner',
                        ipAddress: '127.0.0.1',
                        port: 14318
                    }
                ]
            }
        })
        state.applyEngineManagerStatus({
            engine: 'vllm',
            installed: true,
            enabled: true,
            running: false,
            awaiting_model: true,
            healthy: false,
            port: 8001
        })
        state.setLocalEngineModels('vllm', ['org/model-a', 'org/model-b'])

        state.applyLocalModelSnapshot({
            models: {
                loadedByEngine: { vllm: [] },
                ...(downloaded === undefined ? {} : { downloadedByEngine: { vllm: downloaded } })
            }
        })

        const initial = state.getEngineInitialState()
        expect(
            initial.models.find(row => row.nodeId === nodeId && row.engineType === 'vllm')
        ).toMatchObject({
            inventoryKnown: true,
            models: expected.map(name => expect.objectContaining({ name, status: 'idle' }))
        })
        expect(
            initial.statuses.find(row => row.nodeId === nodeId && row.engineType === 'vllm')
        ).toMatchObject({ processStatus: 'awaiting-model' })
    })

    it('keeps a loaded local path visible alongside downloaded inventory', () => {
        const state = getModularBridgeState()
        const nodeId = 'local-path-inventory-push'
        state.setSelfId(nodeId)
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: nodeId,
                        name: 'local-path-owner',
                        ipAddress: '127.0.0.1',
                        port: 14318
                    }
                ]
            }
        })

        state.applyLocalModelSnapshot({
            models: {
                downloadedByEngine: { vllm: ['org/model-a'] },
                loadedByEngine: { vllm: ['/models/local-model'] }
            }
        })

        expect(
            state
                .getEngineInitialState()
                .models.find(row => row.nodeId === nodeId && row.engineType === 'vllm')?.models
        ).toEqual([
            expect.objectContaining({ name: 'org/model-a', status: 'idle' }),
            expect.objectContaining({ name: '/models/local-model', status: 'loaded' })
        ])
    })
})
