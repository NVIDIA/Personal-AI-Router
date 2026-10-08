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
        expect(PROXY_ENGINES).toEqual(['ollama', 'lm-studio', 'llama-cpp'])
        expect(PROXY_NODE_SOURCES).toEqual(['ollama-proxy', 'lmstudio-proxy', 'llamacpp-proxy'])
        expect(isProxyEngine('ollama')).toBe(true)
        expect(isProxyEngine('lm-studio')).toBe(true)
        expect(isProxyEngine('llama-cpp')).toBe(true)
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
        state.applyRemoteEngineFacts(nodeId, {
            engines: [
                {
                    engine: 'llamacpp',
                    installed: true,
                    running: true,
                    healthy: true,
                    port: 8081
                }
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
            })
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
        state.applyLocalLoadedModels({ llamacpp: [model] })

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
})
