// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import type { EngineModels, EngineType } from '@/shared/types/engines'
import type { JsonObject } from '@/electron/service-bridge/json-rpc-subprocess'

function remoteModels(
    nodeId: string,
    node: JsonObject,
    engine: EngineType
): EngineModels | undefined {
    const state = getModularBridgeState()
    state.setSelfId('strata-peer-self')
    state.handleNotification({
        source: 'broker',
        method: 'discovery:nodes-changed',
        params: {
            nodes: [
                {
                    hostUuid: nodeId,
                    name: `${nodeId}-host`,
                    ipAddress: '192.0.2.60',
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

describe('peer rows for an engine with split name spaces', () => {
    it('appends the loaded name as a loaded row when rows use different names', () => {
        // Strata advertises run-config ids but serves under its config name;
        // the model actually in use must still get a row (and the green dot)
        // on a peer card, not only on the running node's local card.
        const models = remoteModels(
            'strata-peer-rig',
            {
                models: ['hy4-flash-sq-64', 'iq3_s', 'swift-iq3_s'],
                modelsByEngine: { strata: ['hy4-flash-sq-64', 'iq3_s', 'swift-iq3_s'] },
                loadedByEngine: { strata: ['qwen3.8-flash-next-iq3_s'] }
            },
            'strata'
        )

        expect(models?.models).toEqual([
            expect.objectContaining({ name: 'hy4-flash-sq-64', status: 'idle' }),
            expect.objectContaining({ name: 'iq3_s', status: 'idle' }),
            expect.objectContaining({ name: 'swift-iq3_s', status: 'idle' }),
            expect.objectContaining({
                name: 'qwen3.8-flash-next-iq3_s',
                status: 'loaded'
            })
        ])
    })

    it('does not duplicate a loaded name that already has a row', () => {
        const models = remoteModels(
            'strata-peer-alias',
            {
                modelsByEngine: { strata: ['qwen3.8-flash-next-iq3_s'] },
                loadedByEngine: { strata: ['qwen3.8-flash-next-iq3_s'] }
            },
            'strata'
        )

        expect(models?.models).toEqual([
            expect.objectContaining({ name: 'qwen3.8-flash-next-iq3_s', status: 'loaded' })
        ])
    })
})
