// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { subscribePush } from '@/electron/service-bridge/push-bus'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

let unsubscribe = (): void => {}

afterEach(() => {
    unsubscribe()
    unsubscribe = () => {}
})

describe('local engine PATH ownership', () => {
    it('projects the engine-manager path_managed fact onto the local status', () => {
        const state = getModularBridgeState()
        const reported: (boolean | undefined)[] = []
        unsubscribe = subscribePush(event => {
            if (
                event.channel === 'engines:state-changed' &&
                event.payload.engineType === 'ollama' &&
                event.payload.status
            ) {
                reported.push(event.payload.status.pathManaged)
            }
        })
        state.setSelfId('local-node')

        state.applyEngineManagerStatus({
            engine: 'ollama',
            installed: true,
            running: false,
            port: 11434,
            path_managed: true
        })
        expect(reported.at(-1)).toBe(true)
        expect(state.localEnginePathManaged('ollama')).toBe(true)

        state.applyEngineManagerStatus({
            engine: 'ollama',
            installed: false,
            running: false,
            port: 11434,
            path_managed: false
        })
        expect(reported.at(-1)).toBe(false)
        expect(state.localEnginePathManaged('ollama')).toBe(false)
    })
})
