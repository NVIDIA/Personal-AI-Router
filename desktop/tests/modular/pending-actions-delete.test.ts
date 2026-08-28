// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ServiceError } from '@/shared/types/errors'
import type { EngineStatePatch } from '@/shared/types/engine-api'
import type { ModelItem } from '@/shared/types/engines'
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
                onProgress: () => noopUnsub
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

describe('pending-actions store: model actions', () => {
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
                models: [loadedModel('llama3.2')]
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
