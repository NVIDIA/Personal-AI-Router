// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import type { EngineModels, EngineType } from '@/shared/types/engines'
import type { JsonObject } from '@/electron/service-bridge/json-rpc-subprocess'

// A vLLM peer advertises only the model it is serving, for routing, and reports
// what it has downloaded apart, so the model manager can list it.
function remoteModels(
    nodeId: string,
    node: JsonObject,
    engine: EngineType
): EngineModels | undefined {
    const state = getModularBridgeState()
    state.setSelfId('remote-vllm-self')
    state.handleNotification({
        source: 'broker',
        method: 'discovery:nodes-changed',
        params: {
            nodes: [
                {
                    hostUuid: nodeId,
                    name: `${nodeId}-host`,
                    ipAddress: '192.0.2.50',
                    port: 14318,
                    ...node
                }
            ]
        }
    })
    return state
        .getEngineInitialState()
        .models.find(models => models.nodeId === nodeId && models.engineType === engine)
}

describe('remote vLLM model rows', () => {
    it('keeps the downloaded models of a stopped peer as idle rows', () => {
        const models = remoteModels(
            'remote-vllm-stopped',
            {
                loadedByEngine: { vllm: [] },
                downloadedByEngine: {
                    vllm: ['Qwen/Qwen3.5-4B', 'mistralai/Ministral-3-3B-Instruct-2512']
                }
            },
            'vllm'
        )

        expect(models?.inventoryKnown).toBe(true)
        expect(models?.models).toEqual([
            expect.objectContaining({ name: 'Qwen/Qwen3.5-4B', status: 'idle' }),
            expect.objectContaining({
                name: 'mistralai/Ministral-3-3B-Instruct-2512',
                status: 'idle'
            })
        ])
    })

    it('marks the cached model loaded when it is served under an alias', () => {
        const models = remoteModels(
            'remote-vllm-alias',
            {
                models: ['qwen-api'],
                modelsByEngine: { vllm: ['qwen-api'] },
                loadedByEngine: { vllm: ['Qwen/Qwen3.5-4B'] },
                downloadedByEngine: {
                    vllm: ['Qwen/Qwen3.5-4B', 'mistralai/Ministral-3-3B-Instruct-2512']
                }
            },
            'vllm'
        )

        expect(models?.models).toEqual([
            expect.objectContaining({ name: 'Qwen/Qwen3.5-4B', status: 'loaded' }),
            expect.objectContaining({
                name: 'mistralai/Ministral-3-3B-Instruct-2512',
                status: 'idle'
            })
        ])
    })

    it('reports an unread inventory as unknown rather than empty', () => {
        const nodeId = 'remote-vllm-unread'
        const vllm = remoteModels(nodeId, {}, 'vllm')
        expect(vllm?.models).toEqual([])
        expect(vllm?.inventoryKnown).toBe(false)

        // An engine whose list comes from the running server is unchanged: it
        // shows "Start engine to see models" while stopped.
        const ollama = getModularBridgeState()
            .getEngineInitialState()
            .models.find(models => models.nodeId === nodeId && models.engineType === 'ollama')
        expect(ollama?.inventoryKnown).toBe(true)
    })
})
