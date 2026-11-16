// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ServiceError } from '@/shared/types/errors'
import type { EngineStatePatch } from '@/shared/types/engine-api'
import type { EngineType, ModelItem } from '@/shared/types/engines'
import type { ServiceStatus } from '@/shared/types/ipc-channels'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'

/**
 * Two ways the "Deleting…" spinner used to outlive the operation:
 *
 * 1. The delete failed. The main process reported the error but stamped no
 *    `engineType`/`modelName`, so the store could not attribute it and the row
 *    stayed disabled until the safety-net timeout — up to a minute after the
 *    user had already been told it failed.
 * 2. The delete succeeded, slowly. On LM Studio the backend does not reply until
 *    it has bounced the server and passed its readiness probe (`ready.timeout_s`
 *    is 60s on its own), which the 60s net could expire underneath.
 *
 * Slow Ollama loads have the same guarantee: a safety net must not re-enable
 * conflicting controls while the backend request is still valid.
 */

let errorsCb: ((errors: ServiceError[]) => void) | null = null
let stateCb: ((patch: EngineStatePatch) => void) | null = null

function loadedModel(name: string): ModelItem {
    return {
        name,
        size: 1,
        downloaded: true,
        status: 'loaded',
        parameterSize: '',
        quantization: '',
        family: '',
        digest: '',
        sizeVram: null,
        expiresAt: null,
        expiry: '-1',
        capabilities: []
    }
}

function makeFakeWindow() {
    const noopUnsub = (): void => {}
    return {
        pairApi: {
            engines: {
                onStateChanged: (cb: (patch: EngineStatePatch) => void) => {
                    stateCb = cb
                    return noopUnsub
                },
                onProgress: () => noopUnsub,
                onSettingsChanged: () => noopUnsub
            },
            errors: {
                onUpdate: (cb: (errors: ServiceError[]) => void) => {
                    errorsCb = cb
                    return noopUnsub
                }
            }
        },
        windowApi: {
            service: {
                getStatus: vi
                    .fn()
                    .mockResolvedValue({ connectorStatus: 'connected', weSpawned: true }),
                // The store's cached status already defaults to 'connected', so
                // these tests never need to push one.
                onStatusChanged: (_cb: (status: ServiceStatus) => void) => noopUnsub
            }
        }
    }
}

beforeEach(() => {
    vi.useFakeTimers()
    errorsCb = null
    stateCb = null
    vi.stubGlobal('window', makeFakeWindow())
    usePendingActionsStore.getState().initialize()
})

afterEach(() => {
    usePendingActionsStore.getState().cleanup()
    vi.unstubAllGlobals()
    vi.useRealTimers()
})

describe('pending-actions store: model actions', () => {
    it('clears the spinner as soon as a failed delete is reported', () => {
        const store = usePendingActionsStore.getState()
        store.begin({
            command: 'deleteModel',
            engineType: 'lm-studio',
            nodeId: 'node-1',
            model: 'publisher/demo'
        })
        expect(store.getModelPending('node-1', 'lm-studio', 'publisher/demo')).toBe('deleteModel')

        // What `ModularSupervisor.deleteModel` now emits on failure. The
        // nodeId/engineType/modelName triple is what makes it attributable.
        errorsCb?.([
            {
                id: 'pair-ui:engine-delete:lmstudio:publisher/demo',
                message: 'Failed to delete publisher/demo: engine "lmstudio" is not running',
                timestamp: Date.now(),
                severity: 'error',
                nodeId: 'node-1',
                engineType: 'lm-studio',
                operation: 'delete',
                modelName: 'publisher/demo'
            }
        ])

        expect(
            usePendingActionsStore
                .getState()
                .getModelPending('node-1', 'lm-studio', 'publisher/demo')
        ).toBeUndefined()
    })

    it('an unattributed error leaves other rows alone', () => {
        const store = usePendingActionsStore.getState()
        store.begin({
            command: 'deleteModel',
            engineType: 'lm-studio',
            nodeId: 'node-1',
            model: 'publisher/demo'
        })

        errorsCb?.([
            {
                id: 'pair-ui:something-else',
                message: 'unrelated',
                timestamp: Date.now(),
                severity: 'error',
                nodeId: 'node-1'
            }
        ])

        expect(
            usePendingActionsStore
                .getState()
                .getModelPending('node-1', 'lm-studio', 'publisher/demo')
        ).toBe('deleteModel')
    })

    it('keeps an LM Studio delete pending past the default net, since it awaits a restart', () => {
        usePendingActionsStore.getState().begin({
            command: 'deleteModel',
            engineType: 'lm-studio',
            nodeId: 'node-1',
            model: 'publisher/demo'
        })

        // Past the 60s default and past the main process's 120s RPC timeout.
        vi.advanceTimersByTime(130_000)
        expect(
            usePendingActionsStore
                .getState()
                .getModelPending('node-1', 'lm-studio', 'publisher/demo')
        ).toBe('deleteModel')

        vi.advanceTimersByTime(60_000)
        expect(
            usePendingActionsStore
                .getState()
                .getModelPending('node-1', 'lm-studio', 'publisher/demo')
        ).toBeUndefined()
    })

    it('leaves the default net in place for an Ollama delete, which never bounces', () => {
        usePendingActionsStore.getState().begin({
            command: 'deleteModel',
            engineType: 'ollama',
            nodeId: 'node-1',
            model: 'llama3.2'
        })

        vi.advanceTimersByTime(61_000)
        expect(
            usePendingActionsStore.getState().getModelPending('node-1', 'ollama', 'llama3.2')
        ).toBeUndefined()
    })

    it('keeps Ollama loads pending through the backend budget and clears on real outcomes', () => {
        const store = usePendingActionsStore.getState()
        for (const nodeId of ['local-node', 'remote-node']) {
            store.begin({
                command: 'loadModel',
                engineType: 'ollama',
                nodeId,
                model: 'llama3.2'
            })
        }

        // The remote path can withhold headers for 11 minutes. Both rows must
        // remain locked until backend truth arrives, not expire at the old 60s net.
        vi.advanceTimersByTime(11 * 60_000 + 1_000)
        expect(store.getModelPending('local-node', 'ollama', 'llama3.2')).toBe('loadModel')
        expect(store.getModelPending('remote-node', 'ollama', 'llama3.2')).toBe('loadModel')

        stateCb?.({
            nodeId: 'local-node',
            engineType: 'ollama',
            models: {
                nodeId: 'local-node',
                engineType: 'ollama',
                models: [loadedModel('llama3.2')],
                inventoryKnown: true
            }
        })
        errorsCb?.([
            {
                id: 'pair-ui:engine-load:remote-node:ollama:llama3.2',
                message: 'Failed to load llama3.2',
                timestamp: Date.now(),
                severity: 'error',
                nodeId: 'remote-node',
                engineType: 'ollama',
                operation: 'load',
                modelName: 'llama3.2'
            }
        ])

        expect(
            usePendingActionsStore.getState().getModelPending('local-node', 'ollama', 'llama3.2')
        ).toBeUndefined()
        expect(
            usePendingActionsStore.getState().getModelPending('remote-node', 'ollama', 'llama3.2')
        ).toBeUndefined()

        store.begin({
            command: 'loadModel',
            engineType: 'ollama',
            nodeId: 'remote-node',
            model: 'llama3.2'
        })
        vi.advanceTimersByTime(12 * 60_000 + 1_000)
        expect(
            usePendingActionsStore.getState().getModelPending('remote-node', 'ollama', 'llama3.2')
        ).toBeUndefined()
    })

    it('keeps a vLLM load locked while the engine restarts on the new model', () => {
        usePendingActionsStore.getState().begin({
            command: 'loadModel',
            engineType: 'vllm',
            nodeId: 'local-node',
            model: 'Qwen/Qwen3-8B'
        })

        // Well past the default net: readiness alone can take 8 minutes.
        vi.advanceTimersByTime(12 * 60_000)
        expect(
            usePendingActionsStore.getState().getModelPending('local-node', 'vllm', 'Qwen/Qwen3-8B')
        ).toBe('loadModel')

        vi.advanceTimersByTime(8 * 60_000 + 1_000)
        expect(
            usePendingActionsStore.getState().getModelPending('local-node', 'vllm', 'Qwen/Qwen3-8B')
        ).toBeUndefined()
    })

    it('leaves the default net in place for LM Studio loads', () => {
        usePendingActionsStore.getState().begin({
            command: 'loadModel',
            engineType: 'lm-studio',
            nodeId: 'local-node',
            model: 'publisher/demo'
        })

        vi.advanceTimersByTime(61_000)
        expect(
            usePendingActionsStore
                .getState()
                .getModelPending('local-node', 'lm-studio', 'publisher/demo')
        ).toBeUndefined()
    })
})

/**
 * vLLM serves one model per process, so Load restarts the engine on the model
 * and it reports stopped on the way. The pending Load, one entry in the
 * pending-actions store, is what shows the engine as busy for that whole time:
 * there is no second tracker with its own timer and clearing rules.
 */

function load(nodeId: string, engineType: EngineType, model: string): void {
    usePendingActionsStore.getState().begin({ command: 'loadModel', engineType, nodeId, model })
}

function lifecyclePending(nodeId: string, engineType: EngineType) {
    return usePendingActionsStore.getState().getLifecyclePending(nodeId, engineType)
}

describe('a vLLM Load is one pending action for the row and the engine', () => {
    it('holds the engine busy, local or remote, until the model is reported loaded', () => {
        for (const nodeId of ['local-node', 'remote-node']) load(nodeId, 'vllm', 'Qwen/Qwen3-8B')

        expect(lifecyclePending('local-node', 'vllm')).toBe('loadModel')
        expect(lifecyclePending('remote-node', 'vllm')).toBe('loadModel')

        stateCb?.({
            nodeId: 'local-node',
            engineType: 'vllm',
            models: {
                nodeId: 'local-node',
                engineType: 'vllm',
                models: [loadedModel('Qwen/Qwen3-8B')],
                inventoryKnown: true
            }
        })

        expect(lifecyclePending('local-node', 'vllm')).toBeUndefined()
        expect(lifecyclePending('remote-node', 'vllm')).toBe('loadModel')
    })

    it('releases the engine when the Load fails', () => {
        load('local-node', 'vllm', 'org/too-big')

        errorsCb?.([
            {
                id: 'pair-ui:engine-load:vllm:org/too-big',
                message: 'Failed to load org/too-big',
                timestamp: Date.now(),
                severity: 'error',
                nodeId: 'local-node',
                engineType: 'vllm',
                operation: 'load',
                modelName: 'org/too-big'
            }
        ])

        expect(lifecyclePending('local-node', 'vllm')).toBeUndefined()
    })

    it('gives up after the switch budget so a lost notification cannot leave it busy', () => {
        load('local-node', 'vllm', 'Qwen/Qwen3-8B')

        vi.advanceTimersByTime(19 * 60_000)
        expect(lifecyclePending('local-node', 'vllm')).toBe('loadModel')

        vi.advanceTimersByTime(2 * 60_000)
        expect(lifecyclePending('local-node', 'vllm')).toBeUndefined()
    })

    it('leaves engines whose Load does not restart them alone', () => {
        load('local-node', 'llama-cpp', 'org/model:Q4_K_M')
        load('local-node', 'ollama', 'llama3.2')

        expect(lifecyclePending('local-node', 'llama-cpp')).toBeUndefined()
        expect(lifecyclePending('local-node', 'ollama')).toBeUndefined()
    })

    it('shows a Stop sent during the Load as the lifecycle action', () => {
        load('local-node', 'vllm', 'Qwen/Qwen3-8B')
        usePendingActionsStore
            .getState()
            .begin({ command: 'stop', engineType: 'vllm', nodeId: 'local-node' })

        expect(lifecyclePending('local-node', 'vllm')).toBe('stop')
    })

    it('ends the Load when the user stops the engine or ejects', () => {
        const modelPending = (nodeId: string, engineType: EngineType, model: string) =>
            usePendingActionsStore.getState().getModelPending(nodeId, engineType, model)
        const begin = usePendingActionsStore.getState().begin

        // The engine-manager replies that a stopped Load was stopped, not that
        // it failed, so no error clears the entry: the stop has to.
        load('local-node', 'vllm', 'Qwen/Qwen3-8B')
        begin({ command: 'stop', engineType: 'vllm', nodeId: 'local-node' })
        expect(modelPending('local-node', 'vllm', 'Qwen/Qwen3-8B')).toBeUndefined()

        load('remote-node', 'vllm', 'Qwen/Qwen3-8B')
        begin({
            command: 'unloadModel',
            engineType: 'vllm',
            nodeId: 'remote-node',
            model: 'Qwen/Qwen2.5-0.5B-Instruct'
        })
        expect(modelPending('remote-node', 'vllm', 'Qwen/Qwen3-8B')).toBeUndefined()
        expect(modelPending('remote-node', 'vllm', 'Qwen/Qwen2.5-0.5B-Instruct')).toBe(
            'unloadModel'
        )

        // An engine whose Load does not start it keeps its Load.
        load('local-node', 'ollama', 'llama3.2')
        begin({ command: 'stop', engineType: 'ollama', nodeId: 'local-node' })
        expect(modelPending('local-node', 'ollama', 'llama3.2')).toBe('loadModel')
    })

    it('lets a lifecycle command already in flight win', () => {
        usePendingActionsStore
            .getState()
            .begin({ command: 'toggle', engineType: 'vllm', nodeId: 'local-node' })
        load('local-node', 'vllm', 'Qwen/Qwen3-8B')

        expect(lifecyclePending('local-node', 'vllm')).toBe('toggle')
    })
})
