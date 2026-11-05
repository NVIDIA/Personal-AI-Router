// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ServiceError } from '@/shared/types/errors'
import type { EngineStatePatch } from '@/shared/types/engine-api'
import type { EngineType, ModelItem } from '@/shared/types/engines'
import type { ServiceStatus } from '@/shared/types/ipc-channels'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'

/**
 * vLLM serves one model per process, so Load restarts the engine on the model
 * and it reports stopped on the way. The pending Load, one entry in the
 * pending-actions store, is what shows the engine as busy for that whole time:
 * there is no second tracker with its own timer and clearing rules.
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

function load(nodeId: string, engineType: EngineType, model: string): void {
    usePendingActionsStore.getState().begin({ command: 'loadModel', engineType, nodeId, model })
}

function lifecyclePending(nodeId: string, engineType: EngineType) {
    return usePendingActionsStore.getState().getLifecyclePending(nodeId, engineType)
}

describe('a vLLM Load is one pending action for the row and the engine', () => {
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

    it('lets a lifecycle command already in flight win', () => {
        usePendingActionsStore
            .getState()
            .begin({ command: 'toggle', engineType: 'vllm', nodeId: 'local-node' })
        load('local-node', 'vllm', 'Qwen/Qwen3-8B')

        expect(lifecyclePending('local-node', 'vllm')).toBe('toggle')
    })
})
