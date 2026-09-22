// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({
    BrowserWindow: { getAllWindows: () => [] }
}))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import type { EngineProgress } from '@/shared/types/engines'

let unsubscribe: (() => void) | undefined
afterEach(() => {
    unsubscribe?.()
    const state = getModularBridgeState()
    state.finishRemoteModelPull('peer', 'ollama', 'demo')
    state.finishRemoteModelPull('peer', 'lm-studio', 'owner/model')
})

describe('model download cancellation', () => {
    it('keeps Canceling visible across late progress and rejects duplicate cancellation', () => {
        const state = getModularBridgeState()
        const events: EngineProgress[] = []
        unsubscribe = subscribePush(event => {
            if (event.channel === 'engines:progress-changed') events.push(event.payload)
        })
        state.beginRemoteModelPull('peer', 'lm-studio', 'owner/model')
        expect(state.setModelPullCanceling('lm-studio', 'owner/model', true, 'peer')).toBe(true)
        expect(state.setModelPullCanceling('lm-studio', 'owner/model', true, 'peer')).toBe(false)
        state.applyRemoteEngineProgress({
            node: 'peer',
            engine: 'lmstudio',
            model: 'owner/model',
            op: 'pull',
            stage: 'downloading',
            percent: 35
        })
        expect(events.at(-1)).toMatchObject({ status: 'canceling', percent: 35 })
    })

    it('restores the previous status when cancellation fails', () => {
        const state = getModularBridgeState()
        const events: EngineProgress[] = []
        unsubscribe = subscribePush(event => {
            if (event.channel === 'engines:progress-changed') events.push(event.payload)
        })
        state.beginRemoteModelPull('peer', 'ollama', 'demo')
        state.applyRemoteEngineProgress({
            node: 'peer',
            engine: 'ollama',
            model: 'demo',
            op: 'pull',
            stage: 'downloading',
            percent: 25
        })
        state.setModelPullCanceling('ollama', 'demo', true, 'peer')
        state.setModelPullCanceling('ollama', 'demo', false, 'peer')
        expect(events.at(-1)).toMatchObject({ status: 'downloading', percent: 25 })
    })

    it('clears cancellation state when the pull finishes and allows retry', () => {
        const state = getModularBridgeState()
        state.beginRemoteModelPull('peer', 'ollama', 'demo')
        state.setModelPullCanceling('ollama', 'demo', true, 'peer')
        state.finishRemoteModelPull('peer', 'ollama', 'demo')
        expect(state.isRemoteModelPullActive('peer', 'ollama', 'demo')).toBe(false)
        state.beginRemoteModelPull('peer', 'ollama', 'demo')
        expect(state.setModelPullCanceling('ollama', 'demo', true, 'peer')).toBe(true)
    })
})
