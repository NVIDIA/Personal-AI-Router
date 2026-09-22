// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({
    app: { isPackaged: false, getAppPath: () => process.cwd() },
    BrowserWindow: { getAllWindows: () => [] }
}))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))
vi.mock('@/shared/utils/log', () => ({
    createStructuredLogger: () => ({
        info: vi.fn(),
        warn: vi.fn(),
        error: vi.fn(),
        verbose: vi.fn()
    })
}))
vi.mock('@/electron/config/ui-config', () => ({ isFirstRun: () => false }))
vi.mock('@/electron/service-bridge/manual-nodes-store', () => ({ listManualNodeEntries: () => [] }))
vi.mock('@/electron/service-bridge/node-info-poller', () => ({
    startNodeInfoPoller: vi.fn(),
    stopNodeInfoPoller: vi.fn()
}))

import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import { getModularBridgeState, type ProxyEngine } from '@/electron/service-bridge/modular-state'

const engines = [
    { engine: 'ollama', source: 'ollama-proxy', port: 11434 },
    { engine: 'lm-studio', source: 'lmstudio-proxy', port: 1234 },
    { engine: 'llamacpp', source: 'llamacpp-proxy', port: 8080 }
] satisfies { engine: ProxyEngine; source: string; port: number }[]

describe.each(engines)('$engine broker proxy events', ({ engine, source, port }) => {
    it('updates the reported port on live ready and rebind frames', () => {
        const supervisor = getModularSupervisor()
        const state = getModularBridgeState()
        // Exercise production dispatch and normalization without spawning a broker.
        supervisor['handleNotification']({
            source: 'broker',
            method: `${source}:ready`,
            params: { port }
        })
        expect(state.getProxyPort(engine)).toBe(port)
        supervisor['handleNotification']({
            source: 'broker',
            method: `${source}:ready`,
            params: { port: port + 1 }
        })
        expect(state.getProxyPort(engine)).toBe(port + 1)
    })

    it('attributes live discovered nodes to the reporting engine', () => {
        const id = `reported-${engine}`
        getModularSupervisor()['handleNotification']({
            source: 'broker',
            method: `${source}:node/discovered`,
            params: { id, host: id, port, addresses: ['192.0.2.1'] }
        })
        const state = getModularBridgeState()
        expect(state.getNodesInitial().nodes[id]).toBeDefined()
        expect(
            state
                .getEngineInitialState()
                .statuses.find(s => s.nodeId === id && s.engineType === engine)
        ).toBeDefined()
        getModularSupervisor()['handleNotification']({
            source: 'broker',
            method: `${source}:node/removed`,
            params: { id }
        })
        expect(state.getNodesInitial().nodes[id]).toBeUndefined()
    })
})
