// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { ServiceStatus } from '@/shared/types/ipc-channels'
import { usePendingActionsStore } from '@/ui/stores/pending-actions.store'

/**
 * Renderer store guard: with the modular service stopped no engine
 * command can reach the broker, so the store must not enter (and must drop) the
 * optimistic "working" state that would otherwise spin until the safety-net
 * timeout. Runs in the `node` unit project with a stubbed `window`.
 */

let statusCb: ((status: ServiceStatus) => void) | null = null

function makeFakeWindow() {
    const noopUnsub = (): void => {}
    return {
        pairApi: {
            engines: {
                onStateChanged: () => noopUnsub,
                onProgress: () => noopUnsub
            },
            errors: {
                onUpdate: () => noopUnsub
            }
        },
        windowApi: {
            service: {
                getStatus: vi
                    .fn()
                    .mockResolvedValue({ connectorStatus: 'connected', weSpawned: true }),
                onStatusChanged: (cb: (status: ServiceStatus) => void) => {
                    statusCb = cb
                    return noopUnsub
                }
            }
        }
    }
}

function pushStatus(connectorStatus: ServiceStatus['connectorStatus']): void {
    statusCb?.({ connectorStatus, weSpawned: true })
}

describe('pending-actions store honours the modular connector status', () => {
    beforeEach(() => {
        statusCb = null
        vi.stubGlobal('window', makeFakeWindow())
        usePendingActionsStore.getState().initialize()
    })

    afterEach(() => {
        usePendingActionsStore.getState().cleanup()
        vi.unstubAllGlobals()
    })

    it('records no optimistic entry while the service is stopped', () => {
        pushStatus('disconnected')

        usePendingActionsStore
            .getState()
            .begin({ command: 'toggle', engineType: 'ollama', nodeId: 'node-1' })

        expect(
            usePendingActionsStore.getState().getLifecyclePending('node-1', 'ollama')
        ).toBeUndefined()
    })

    it('drops in-flight entries the instant the connector reports disconnected', () => {
        pushStatus('connected')

        usePendingActionsStore
            .getState()
            .begin({ command: 'toggle', engineType: 'ollama', nodeId: 'node-1' })
        expect(usePendingActionsStore.getState().getLifecyclePending('node-1', 'ollama')).toBe(
            'toggle'
        )

        pushStatus('disconnected')
        expect(
            usePendingActionsStore.getState().getLifecyclePending('node-1', 'ollama')
        ).toBeUndefined()
    })
})
