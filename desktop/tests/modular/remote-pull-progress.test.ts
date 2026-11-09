// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import type { EngineProgress } from '@/shared/types/engines'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import {
    isBackendPullFailureMessage,
    isPullInfrastructureError,
    mergePullProgressPercent,
    resolvePullCatchError
} from '@/electron/service-bridge/pull-error-handling'

let unsubscribe = (): void => {}

afterEach(() => {
    unsubscribe()
    unsubscribe = () => {}
})

describe('pull error handling', () => {
    it('treats peer loss, timeouts, and process-down as infrastructure failures', () => {
        expect(isPullInfrastructureError('jsonrpc peer closed')).toBe(true)
        expect(isPullInfrastructureError('broker engine:action timed out')).toBe(true)
        expect(isPullInfrastructureError('broker is not running')).toBe(true)
        expect(
            isPullInfrastructureError(
                'LM Studio experienced an error while downloading a model: timeout'
            )
        ).toBe(false)
    })

    it('skips duplicate backend pull failure copy', () => {
        const backend = 'LM Studio experienced an error while downloading a model: Download failed'
        expect(isBackendPullFailureMessage(backend)).toBe(true)
        expect(resolvePullCatchError(backend)).toBeNull()
    })

    it('skips backend copy even when the detail looks like infrastructure', () => {
        const notRunning =
            'Fake Engine experienced an error while downloading a model: engine "fake" is not running'
        expect(resolvePullCatchError(notRunning)).toBeNull()

        const timedOut =
            'LM Studio experienced an error while downloading a model: connection timed out'
        expect(resolvePullCatchError(timedOut)).toBeNull()
    })

    it('reports initiator-side remote pull failures', () => {
        const message = 'node peer-1 is not a discovered ec peer'
        expect(resolvePullCatchError(message)).toBe(message)
    })

    it('maps infrastructure failures to the neutral lost-connection message', () => {
        expect(resolvePullCatchError('broker engine:remote-pull-model timed out')).toBe(
            'The connection to the engine service was lost while downloading a model.'
        )
    })
})

/** Collect the pull progress pushed to the renderer. */
function capturePullProgress(): EngineProgress[] {
    const frames: EngineProgress[] = []
    unsubscribe = subscribePush(event => {
        if (event.channel === 'engines:progress-changed' && event.payload.operation === 'pull') {
            frames.push(event.payload)
        }
    })
    return frames
}

// The frames carry the model the way the peer stamps it: it is what keys a
// frame to the row of the download it reports on.
describe('remote pull progress', () => {
    afterEach(() => {
        const state = getModularBridgeState()
        state.finishRemoteModelPull('remote-node', 'lm-studio', 'demo-model')
        state.finishRemoteModelPull('remote-node', 'ollama', 'llama3.1:8b')
        state.finishRemoteModelPull('remote-node', 'ollama', 'qwen3:4b')
    })

    it('keeps indeterminate percent when the wire omits percent', () => {
        const state = getModularBridgeState()
        const progress = capturePullProgress()

        state.beginRemoteModelPull('remote-node', 'lm-studio', 'demo-model')
        state.applyRemoteEngineProgress({
            node: 'remote-node',
            engine: 'lmstudio',
            model: 'demo-model',
            op: 'pull',
            stage: 'pulling',
            message: 'demo-model'
        })

        expect(progress.at(-1)).toMatchObject({ model: 'demo-model', status: 'pulling' })
        expect(progress.at(-1)?.percent).toBeUndefined()
    })

    it('advances percent when the wire carries byte progress', () => {
        const state = getModularBridgeState()
        const progress = capturePullProgress()

        state.beginRemoteModelPull('remote-node', 'ollama', 'llama3.1:8b')
        state.applyRemoteEngineProgress({
            node: 'remote-node',
            engine: 'ollama',
            model: 'llama3.1:8b',
            op: 'pull',
            stage: 'pulling',
            percent: 42,
            message: 'llama3.1:8b'
        })

        expect(progress.at(-1)).toMatchObject({ model: 'llama3.1:8b', percent: 42 })
    })

    // An engine downloads one model at a time and queues the rest, so a node can
    // have several pulls outstanding on one engine, and only the model a frame
    // names says which of them it reports on.
    it('applies a frame only to the download its model names', () => {
        const state = getModularBridgeState()
        const progress = capturePullProgress()
        const frame = { node: 'remote-node', engine: 'ollama', op: 'pull' }

        state.beginRemoteModelPull('remote-node', 'ollama', 'llama3.1:8b')
        state.beginRemoteModelPull('remote-node', 'ollama', 'qwen3:4b')
        state.applyRemoteEngineProgress({
            ...frame,
            model: 'llama3.1:8b',
            stage: 'downloading',
            percent: 60
        })
        state.applyRemoteEngineProgress({ ...frame, model: 'qwen3:4b', stage: 'queued' })

        expect(progress.at(-1)).toMatchObject({ model: 'qwen3:4b', status: 'queued' })
        // Sharing an entry, the queued download would have taken the other's
        // percentage as its own.
        expect(progress.at(-1)?.percent).toBeUndefined()
    })

    // Without a model a frame cannot be told apart from any other download on
    // its engine, so it is credited to none of them.
    it('credits no download with a frame that names no model', () => {
        const state = getModularBridgeState()
        const progress = capturePullProgress()
        const frame = { node: 'remote-node', engine: 'ollama', op: 'pull' }

        state.beginRemoteModelPull('remote-node', 'ollama', 'llama3.1:8b')
        state.applyRemoteEngineProgress({ ...frame, stage: 'downloading', percent: 60 })
        state.applyRemoteEngineProgress({ ...frame, model: 'llama3.1:8b', stage: 'pulling' })

        expect(progress.at(-1)).toMatchObject({ model: 'llama3.1:8b', status: 'pulling' })
        expect(progress.at(-1)?.percent).toBeUndefined()
    })
})

describe('local pull progress', () => {
    afterEach(() => {
        const state = getModularBridgeState()
        state.finishModelPull('ollama', 'llama3.1:8b')
        state.finishModelPull('ollama', 'qwen3:4b')
    })

    it('applies a frame only to the download its model names', () => {
        const state = getModularBridgeState()
        state.setSelfId('local-node')
        const progress = capturePullProgress()

        state.beginModelPull('ollama', 'llama3.1:8b')
        state.beginModelPull('ollama', 'qwen3:4b')
        state.applyLocalEngineProgress({
            engine: 'ollama',
            model: 'llama3.1:8b',
            op: 'pull',
            stage: 'downloading',
            percent: 60
        })
        state.applyLocalEngineProgress({
            engine: 'ollama',
            model: 'qwen3:4b',
            op: 'pull',
            stage: 'queued'
        })

        expect(progress.at(-1)).toMatchObject({ model: 'qwen3:4b', status: 'queued' })
        expect(progress.at(-1)?.percent).toBeUndefined()
    })

    // The engine manager reports every download on the machine, a terminal UI's
    // included, and only the ones this app started have a row to update.
    it('ignores a frame for a download this app did not start', () => {
        const state = getModularBridgeState()
        state.setSelfId('local-node')
        const progress = capturePullProgress()

        state.applyLocalEngineProgress({
            engine: 'ollama',
            model: 'qwen3:4b',
            op: 'pull',
            stage: 'downloading',
            percent: 5
        })

        expect(progress).toEqual([])
    })
})

describe('mergePullProgressPercent', () => {
    it('preserves existing percent when the frame is indeterminate', () => {
        expect(mergePullProgressPercent(0, undefined)).toBeUndefined()
        expect(mergePullProgressPercent(0, 15)).toBe(15)
        expect(mergePullProgressPercent(55, 15)).toBe(55)
    })
})
