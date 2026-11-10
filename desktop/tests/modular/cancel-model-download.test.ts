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
    state.finishModelPull('vllm', 'Qwen/Test')
})

describe('model download cancellation', () => {
    it('keeps Canceling visible across late progress', () => {
        const state = getModularBridgeState()
        const events: EngineProgress[] = []
        unsubscribe = subscribePush(event => {
            if (event.channel === 'engines:progress-changed') events.push(event.payload)
        })
        state.beginRemoteModelPull('peer', 'lm-studio', 'owner/model')
        expect(state.setModelPullCanceling('lm-studio', 'owner/model', true, 'peer')).toBe(true)
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

    // A cancel may be re-issued after an earlier one stopped being awaited.
    // Each repeat must keep pointing at the status the download had before any
    // of them, or a rejection would restore the row to "canceling" — the state
    // it is being told the backend refused to enter.
    it('remembers the status from before the first cancel when one is re-issued', () => {
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
            percent: 40
        })
        expect(state.setModelPullCanceling('ollama', 'demo', true, 'peer')).toBe(true)
        expect(state.setModelPullCanceling('ollama', 'demo', true, 'peer')).toBe(true)
        state.setModelPullCanceling('ollama', 'demo', false, 'peer')
        expect(events.at(-1)).toMatchObject({ status: 'downloading', percent: 40 })
    })

    it('clears cancellation state when the pull finishes and allows retry', () => {
        const state = getModularBridgeState()
        const events: EngineProgress[] = []
        unsubscribe = subscribePush(event => {
            if (event.channel === 'engines:progress-changed') events.push(event.payload)
        })
        state.beginRemoteModelPull('peer', 'ollama', 'demo')
        state.setModelPullCanceling('ollama', 'demo', true, 'peer')
        state.finishRemoteModelPull('peer', 'ollama', 'demo')
        expect(state.isRemoteModelPullActive('peer', 'ollama', 'demo')).toBe(false)

        // A retry's progress must not show the finished download's "Canceling".
        state.beginRemoteModelPull('peer', 'ollama', 'demo')
        state.applyRemoteEngineProgress({
            node: 'peer',
            engine: 'ollama',
            model: 'demo',
            op: 'pull',
            stage: 'downloading',
            percent: 10
        })
        expect(events.at(-1)).toMatchObject({ status: 'downloading', percent: 10 })
        expect(state.setModelPullCanceling('ollama', 'demo', true, 'peer')).toBe(true)
    })

    // A download can end while its cancel is under way: it finished first, or
    // its engine could not confirm it stopped. Its last frame says which, so
    // it shows as that rather than "Canceling", and a cancel rejected after it
    // has no status from before to put back over it.
    it('shows how a download ended while its cancel was under way', () => {
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
            percent: 30
        })
        state.setModelPullCanceling('ollama', 'demo', true, 'peer')
        state.applyRemoteEngineProgress({
            node: 'peer',
            engine: 'ollama',
            model: 'demo',
            op: 'pull',
            stage: 'error',
            percent: -1,
            message: 'the download may still be running'
        })
        expect(events.at(-1)).toMatchObject({ status: 'error' })

        state.setModelPullCanceling('ollama', 'demo', false, 'peer')
        expect(events.at(-1)).toMatchObject({ status: 'error' })
    })

    it('shows how a local download ended while its cancel was under way', () => {
        const state = getModularBridgeState()
        state.setSelfId('local-node')
        const events: EngineProgress[] = []
        unsubscribe = subscribePush(event => {
            if (event.channel === 'engines:progress-changed') events.push(event.payload)
        })
        state.beginModelPull('vllm', 'Qwen/Test')
        state.setModelPullCanceling('vllm', 'Qwen/Test', true)
        state.applyLocalEngineProgress({
            engine: 'vllm',
            model: 'Qwen/Test',
            op: 'pull',
            stage: 'downloading',
            percent: 99
        })
        expect(events.at(-1)).toMatchObject({ status: 'canceling', percent: 99 })

        state.applyLocalEngineProgress({
            engine: 'vllm',
            model: 'Qwen/Test',
            op: 'pull',
            stage: 'success',
            percent: 100
        })
        expect(events.at(-1)).toMatchObject({ status: 'success', percent: 100 })

        state.setModelPullCanceling('vllm', 'Qwen/Test', false)
        expect(events.at(-1)).toMatchObject({ status: 'success', percent: 100 })
    })
})
