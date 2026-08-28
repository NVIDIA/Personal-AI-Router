// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

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

describe('remote pull progress', () => {
    it('keeps indeterminate percent when the wire omits percent', () => {
        const state = getModularBridgeState()
        const progressEvents: Array<{ percent?: number }> = []
        unsubscribe = subscribePush(event => {
            if (event.channel === 'engines:progress-changed') {
                progressEvents.push(event.payload)
            }
        })

        state.beginRemoteModelPull('remote-node', 'lm-studio', 'demo-model')
        state.applyRemoteEngineProgress({
            node: 'remote-node',
            engine: 'lmstudio',
            op: 'pull',
            stage: 'pulling',
            message: 'demo-model'
        })

        expect(progressEvents.at(-1)?.percent).toBeUndefined()
    })

    it('advances percent when the wire carries byte progress', () => {
        const state = getModularBridgeState()
        const progressEvents: Array<{ percent?: number }> = []
        unsubscribe = subscribePush(event => {
            if (event.channel === 'engines:progress-changed') {
                progressEvents.push(event.payload)
            }
        })

        state.beginRemoteModelPull('remote-node', 'ollama', 'llama3.1:8b')
        state.applyRemoteEngineProgress({
            node: 'remote-node',
            engine: 'ollama',
            op: 'pull',
            stage: 'pulling',
            percent: 42,
            message: 'llama3.1:8b'
        })

        expect(progressEvents.at(-1)?.percent).toBe(42)
    })
})

describe('mergePullProgressPercent', () => {
    it('preserves existing percent when the frame is indeterminate', () => {
        expect(mergePullProgressPercent(0, undefined)).toBeUndefined()
        expect(mergePullProgressPercent(0, 15)).toBe(15)
        expect(mergePullProgressPercent(55, 15)).toBe(55)
    })
})
