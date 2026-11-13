// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({
    app: { isPackaged: false, getAppPath: () => process.cwd() },
    BrowserWindow: { getAllWindows: () => [] }
}))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import { MODULAR_CANCEL_PULL_TIMEOUT_MS } from '@/shared/constants/modular-runtime'
import type { EngineProgress } from '@/shared/types/engines'

let unsubscribe: (() => void) | undefined
afterEach(() => {
    unsubscribe?.()
    vi.restoreAllMocks()
    const state = getModularBridgeState()
    state.finishRemoteModelPull('peer', 'ollama', 'demo')
    state.finishRemoteModelPull('peer', 'lm-studio', 'owner/model')
    state.finishModelPull('vllm', 'Qwen/Test')
    state.finishModelPull('ollama', 'demo')
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

    // A cancel can arrive after a download's last frame, which its row shows
    // until the awaited pull clears it. Nothing is left to stop, and
    // "Canceling" would hide how the download ended.
    it('leaves a download that has already ended showing how it ended', () => {
        const state = getModularBridgeState()
        state.setSelfId('local-node')
        const events: EngineProgress[] = []
        unsubscribe = subscribePush(event => {
            if (event.channel === 'engines:progress-changed') events.push(event.payload)
        })
        state.beginModelPull('vllm', 'Qwen/Test')
        state.applyLocalEngineProgress({
            engine: 'vllm',
            model: 'Qwen/Test',
            op: 'pull',
            stage: 'success',
            percent: 100
        })

        expect(state.setModelPullCanceling('vllm', 'Qwen/Test', true)).toBe(false)
        expect(events.at(-1)).toMatchObject({ status: 'success', percent: 100 })
    })
})

// A download can end, and its row clear, while its cancel is still awaited, and
// the model can then be downloaded again. The wire names both attempts by
// engine and model alone, so each cancel has to stay with the attempt it was
// sent for: the earlier one must neither block the retry's cancel nor, settling
// late, take the retry's row out of "Canceling".
describe('cancelling a download retried while an earlier cancel is awaited', () => {
    const where = [
        {
            name: 'on this machine',
            nodeId: undefined,
            method: 'engine:cancel-pull',
            params: { engine: 'ollama', model: 'demo' }
        },
        {
            name: 'on a peer',
            nodeId: 'peer',
            method: 'engine:remote-cancel-pull',
            params: { node: 'peer', engine: 'ollama', model: 'demo' }
        }
    ]

    function retryWhileCancelAwaited(nodeId: string | undefined) {
        const state = getModularBridgeState()
        state.setSelfId('local-node')
        const supervisor = getModularSupervisor()
        const replies: Array<(err?: Error) => void> = []
        const call = vi
            .spyOn(supervisor, 'callProcess')
            .mockImplementation(
                () =>
                    new Promise((resolve, reject) =>
                        replies.push(err => (err ? reject(err) : resolve(null)))
                    )
            )
        const begin = () =>
            nodeId
                ? state.beginRemoteModelPull(nodeId, 'ollama', 'demo')
                : state.beginModelPull('ollama', 'demo')
        const cancel = () => supervisor.cancelModelPull('ollama', 'ollama', 'demo', nodeId)

        begin()
        const first = cancel()
        if (nodeId) state.finishRemoteModelPull(nodeId, 'ollama', 'demo')
        else state.finishModelPull('ollama', 'demo')
        begin()
        return { supervisor, call, replies, cancel, first }
    }

    it.each(where)('sends the retry’s cancel $name', async ({ nodeId, method, params }) => {
        const { call, replies, cancel, first } = retryWhileCancelAwaited(nodeId)

        const retry = cancel()
        expect(call).toHaveBeenCalledTimes(2)
        expect(call).toHaveBeenLastCalledWith(
            'broker',
            method,
            params,
            MODULAR_CANCEL_PULL_TIMEOUT_MS
        )

        // The earlier cancel settling releases its own guard, not the retry's.
        replies[0]()
        await first
        const repeat = cancel()
        expect(call).toHaveBeenCalledTimes(2)

        replies[1]()
        await Promise.all([retry, repeat])
    })

    it.each(where)(
        'keeps the retry in Canceling when the earlier cancel is rejected late $name',
        async ({ nodeId }) => {
            const { supervisor, replies, cancel, first } = retryWhileCancelAwaited(nodeId)
            vi.spyOn(supervisor, 'reportError').mockImplementation(() => {})
            const events: EngineProgress[] = []
            unsubscribe = subscribePush(event => {
                if (event.channel === 'engines:progress-changed') events.push(event.payload)
            })

            const retry = cancel()
            expect(events.at(-1)).toMatchObject({ model: 'demo', status: 'canceling' })

            replies[0](new Error('engine ollama is not running'))
            await first
            expect(events.at(-1)).toMatchObject({ model: 'demo', status: 'canceling' })

            replies[1]()
            await retry
        }
    )
})
