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
    it('keeps the enabled proxy set and ordering unchanged', () => {
        expect(PROXY_ENGINES).toEqual(['ollama', 'lm-studio'])
        expect(PROXY_NODE_SOURCES).toEqual(['ollama-proxy', 'lmstudio-proxy'])
        expect(isProxyEngine('ollama')).toBe(true)
        expect(isProxyEngine('lm-studio')).toBe(true)
        expect(isProxyEngine('llama-cpp')).toBe(false)
    })

    it('round-trips engine, manager, and relay source identities', () => {
        for (const engine of PROXY_ENGINES) {
            const source = proxySourceForEngine(engine)
            expect(proxyEngineFromSource(source)).toBe(engine)
            expect(proxyEngineFromManagerId(engineManagerName(engine))).toBe(engine)
        }
        expect(proxyEngineFromSource('other-proxy')).toBeNull()
        expect(proxyEngineFromManagerId('other')).toBeNull()
        expect(proxyEngineFromManagerId('llamacpp')).toBeNull()
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
            })
        ])
    })
})
