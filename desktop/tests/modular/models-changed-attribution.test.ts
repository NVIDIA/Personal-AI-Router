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

/**
 * Model names as the 2026-09-26 desktop.log carried them. The process on
 * port 11434 was a HiveMind gateway answering as Ollama, so its `/api/tags`
 * legitimately listed `local_llamacpp` and LM Studio's embedding key, and its
 * `/api/ps` repeated rows. An `engine:models-changed` push is
 * `{ engine: <trigger>, models: <full multi-engine snapshot> }`, so a push
 * tagged `llamacpp` carries every Ollama name too. None of that is
 * cross-wiring: each engine row must be built only from its own
 * `modelsByEngine` / `loadedByEngine` bucket.
 */
const FEDERATED = 'text-embedding-nomic-embed-text-v1.5'
const OLLAMA_MODELS = [
    'qwen3-coder-next:latest',
    'gemma4:latest',
    'qwen3.8:27b',
    'qwen2.5:0.5b',
    'local_llamacpp',
    FEDERATED
]
const LLAMACPP_MODEL = 'ggml-org/tinygemma3-GGUF:Q8_0'

const MODELS_BY_ENGINE = {
    llamacpp: [LLAMACPP_MODEL],
    lmstudio: [FEDERATED],
    ollama: OLLAMA_MODELS
}
const FLAT_UNION = [LLAMACPP_MODEL, ...OLLAMA_MODELS]

function rows(nodeId: string, engine: ProxyEngine): { names: string[]; loaded: string[] } {
    const models =
        getModularBridgeState()
            .getEngineInitialState()
            .models.find(entry => entry.nodeId === nodeId && entry.engineType === engine)?.models ??
        []
    return {
        names: models.map(model => model.name),
        loaded: models.filter(model => model.status === 'loaded').map(model => model.name)
    }
}

function seedSelfNode(nodeId: string): void {
    const state = getModularBridgeState()
    state.setSelfId(nodeId)
    for (const [engine, port] of [
        ['ollama', 11434],
        ['lmstudio', 1234],
        ['llamacpp', 8081]
    ] as const) {
        state.applyEngineManagerStatus({
            engine,
            installed: true,
            running: true,
            healthy: true,
            port
        })
    }
    state.handleNotification({
        source: 'broker',
        method: 'discovery:nodes-changed',
        params: {
            nodes: [
                {
                    hostUuid: nodeId,
                    name: 'attribution-host',
                    ipAddress: '127.0.0.1',
                    port: 14318,
                    models: FLAT_UNION,
                    modelsByEngine: MODELS_BY_ENGINE,
                    loadedByEngine: { llamacpp: [], lmstudio: [], ollama: [] }
                }
            ]
        }
    })
}

function pushModelsChanged(engine: string, loadedByEngine: Record<string, string[]>): void {
    // Exercise the production broker -> supervisor -> state dispatch without a
    // broker process, exactly as the relayed engine-manager frame arrives.
    getModularSupervisor()['handleNotification']({
        source: 'broker',
        method: 'engine:models-changed',
        params: {
            engine,
            models: { models: FLAT_UNION, modelsByEngine: MODELS_BY_ENGINE, loadedByEngine }
        }
    })
}

describe('engine:models-changed per-engine attribution', () => {
    it('keeps each engine row on its own bucket when a llamacpp-tagged push carries the whole snapshot', () => {
        const nodeId = 'models-changed-attribution-self'
        seedSelfNode(nodeId)

        // Assembled from the log's 4:36:39 PM and 4:54:21 PM frames: trigger
        // engine llamacpp, Ollama residency reported with duplicated rows.
        pushModelsChanged('llamacpp', {
            llamacpp: [LLAMACPP_MODEL],
            lmstudio: [],
            ollama: ['qwen2.5:0.5b', 'qwen3.8:27b', 'qwen2.5:0.5b', 'qwen3.8:27b']
        })

        expect(rows(nodeId, 'llamacpp')).toEqual({
            names: [LLAMACPP_MODEL],
            loaded: [LLAMACPP_MODEL]
        })
        expect(rows(nodeId, 'ollama')).toEqual({
            names: OLLAMA_MODELS,
            loaded: ['qwen3.8:27b', 'qwen2.5:0.5b']
        })
        expect(rows(nodeId, 'lm-studio')).toEqual({ names: [FEDERATED], loaded: [] })
    })

    it('marks a name two engines both serve loaded only under the engine reporting it resident', () => {
        const nodeId = 'models-changed-attribution-shared'
        seedSelfNode(nodeId)

        pushModelsChanged('ollama', { llamacpp: [], lmstudio: [], ollama: [FEDERATED] })
        expect(rows(nodeId, 'ollama').loaded).toEqual([FEDERATED])
        expect(rows(nodeId, 'lm-studio').loaded).toEqual([])
        expect(rows(nodeId, 'llamacpp').loaded).toEqual([])

        pushModelsChanged('lmstudio', { llamacpp: [], lmstudio: [FEDERATED], ollama: [] })
        expect(rows(nodeId, 'ollama').loaded).toEqual([])
        expect(rows(nodeId, 'lm-studio').loaded).toEqual([FEDERATED])
        expect(rows(nodeId, 'lm-studio').names).toEqual([FEDERATED])
    })
})
