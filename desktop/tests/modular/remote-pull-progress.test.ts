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

    it('projects remote update frames onto the existing install progress row', () => {
        const state = getModularBridgeState()
        const progressEvents: Array<{ operation?: string; status?: string; percent?: number }> = []
        unsubscribe = subscribePush(event => {
            if (event.channel === 'engines:progress-changed') {
                progressEvents.push(event.payload)
            }
        })

        state.applyRemoteEngineProgress({
            node: 'remote-update-node',
            engine: 'vllm',
            op: 'update',
            stage: 'downloading',
            percent: 37
        })

        expect(progressEvents.at(-1)).toMatchObject({
            operation: 'install',
            status: 'downloading',
            percent: 37
        })
    })

    it('projects typed distribution and runtime preparation onto their exact node rows', () => {
        const state = getModularBridgeState()
        const progressEvents: Array<{
            operation?: string
            operationId?: string
            model?: string
            status?: string
            percent?: number
        }> = []
        unsubscribe = subscribePush(event => {
            if (event.channel === 'engines:progress-changed') progressEvents.push(event.payload)
        })
        const model = `owner/model@${'a'.repeat(40)}`
        const operationId = 'b'.repeat(32)
        state.beginVllmJourney('remote-distribution-node', 'distribute', operationId, model)
        state.applyRemoteEngineProgress({
            node: 'remote-distribution-node',
            engine: 'vllm',
            op: 'distribute',
            stage: 'receiving',
            percent: 61
        })
        expect(progressEvents.at(-1)).toMatchObject({
            operation: 'distribute',
            operationId,
            model,
            status: 'receiving',
            percent: 61
        })

        state.beginVllmJourney('remote-prepare-node', 'prepare', operationId)
        state.applyRemoteEngineProgress({
            node: 'remote-prepare-node',
            engine: 'vllm',
            op: 'qwen38-prepare',
            stage: 'installing-runtime',
            percent: 25
        })
        expect(progressEvents.at(-1)).toMatchObject({
            operation: 'prepare',
            operationId,
            status: 'installing-runtime',
            percent: 25
        })
        state.finishVllmJourney('remote-distribution-node', 'distribute')
        state.finishVllmJourney('remote-prepare-node', 'prepare')
    })

    it("carries a model copy's network and ignores it anywhere else", () => {
        const state = getModularBridgeState()
        const events: Array<{ operation?: string; network?: string; status?: string }> = []
        unsubscribe = subscribePush(event => {
            if (event.channel === 'engines:progress-changed') events.push(event.payload)
        })
        const model = `owner/model@${'a'.repeat(40)}`
        const operationId = 'd'.repeat(32)
        state.beginVllmJourney('copy-node', 'distribute', operationId, model)
        const copyFrame = (network: string) =>
            state.applyRemoteEngineProgress({
                node: 'copy-node',
                engine: 'vllm',
                op: 'distribute',
                stage: 'receiving',
                percent: 10,
                network
            })
        copyFrame('fabric')
        expect(events.at(-1)).toMatchObject({ operation: 'distribute', network: 'fabric' })
        copyFrame('management')
        expect(events.at(-1)).toMatchObject({ operation: 'distribute', network: 'management' })
        copyFrame('wifi')
        expect(events.at(-1)).not.toHaveProperty('network')
        state.finishVllmJourney('copy-node', 'distribute')

        state.beginVllmJourney('prepare-node', 'prepare', operationId)
        state.applyRemoteEngineProgress({
            node: 'prepare-node',
            engine: 'vllm',
            op: 'qwen38-prepare',
            stage: 'installing-runtime',
            network: 'fabric'
        })
        expect(events.at(-1)).not.toHaveProperty('network')
        state.finishVllmJourney('prepare-node', 'prepare')

        state.setSelfId('local-copy-node')
        state.beginVllmJourney('local-copy-node', 'distribute', operationId, model)
        state.applyLocalEngineProgress({
            engine: 'vllm',
            op: 'distribute',
            stage: 'receiving',
            percent: 20,
            network: 'fabric'
        })
        expect(events.at(-1)).toMatchObject({
            operation: 'distribute',
            status: 'receiving',
            network: 'fabric'
        })
        state.finishVllmJourney('local-copy-node', 'distribute')
    })

    it('projects local Qwen preparation install frames onto the prepare row', () => {
        const state = getModularBridgeState()
        const events: Array<{ operation?: string; operationId?: string; percent?: number }> = []
        unsubscribe = subscribePush(event => {
            if (event.channel === 'engines:progress-changed') events.push(event.payload)
        })
        const nodeId = 'local-prepare-node'
        const operationId = 'c'.repeat(32)
        state.setSelfId(nodeId)
        state.beginVllmJourney(nodeId, 'prepare', operationId)
        expect(
            state.applyLocalVllmPrepareProgress({
                engine: 'vllm',
                stage: 'installing-qwen38-offline',
                percent: 75
            })
        ).toBe(true)
        expect(events.at(-1)).toMatchObject({
            operation: 'prepare',
            operationId,
            percent: 75
        })
        state.finishVllmJourney(nodeId, 'prepare')
    })
})

describe('mergePullProgressPercent', () => {
    it('preserves existing percent when the frame is indeterminate', () => {
        expect(mergePullProgressPercent(0, undefined)).toBeUndefined()
        expect(mergePullProgressPercent(0, 15)).toBe(15)
        expect(mergePullProgressPercent(55, 15)).toBe(55)
    })
})
