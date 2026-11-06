// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import type { EngineType } from '@/shared/types/engines'

const NODE = 'model-switch-self'

function vllmStatus(running: boolean) {
    return { engine: 'vllm', installed: true, running, healthy: running, port: 8001 }
}

function processStatus(engineType: EngineType) {
    return getModularBridgeState()
        .getEngineInitialState()
        .statuses.find(status => status.nodeId === NODE && status.engineType === engineType)
        ?.processStatus
}

describe('engine status while a model switch restarts the engine', () => {
    beforeEach(() => {
        const state = getModularBridgeState()
        state.setSelfId(NODE)
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    { hostUuid: NODE, name: 'switch-host', ipAddress: '127.0.0.1', port: 14318 }
                ]
            }
        })
        state.applyEngineManagerStatus(vllmStatus(true))
    })

    afterEach(() => {
        vi.useRealTimers()
        getModularBridgeState().clearLocalModelSwitch('vllm')
    })

    it('shows starting, not stopped, while the engine is down for the restart', () => {
        const state = getModularBridgeState()
        expect(processStatus('vllm')).toBe('running')

        // The engine is still serving the old model until it is stopped.
        state.beginLocalModelSwitch('vllm')
        expect(processStatus('vllm')).toBe('running')

        // The restart reports the engine stopped on its way down. That is not the
        // engine being turned off, so it must not read as stopped.
        state.applyEngineManagerStatus(vllmStatus(false))
        expect(processStatus('vllm')).toBe('starting')
        expect(state.isLocalModelSwitching('vllm')).toBe(true)

        // Back up: it is running, and a stray later notification does not undo that.
        state.applyEngineManagerStatus(vllmStatus(true))
        expect(processStatus('vllm')).toBe('running')
        state.clearLocalModelSwitch('vllm')
        expect(state.isLocalModelSwitching('vllm')).toBe(false)
        expect(processStatus('vllm')).toBe('running')
    })

    it('does not leave a stale marker that makes a later manual stop look like starting', () => {
        const state = getModularBridgeState()
        state.beginLocalModelSwitch('vllm') // a switch that needed no restart
        state.clearLocalModelSwitch('vllm') // the request finished
        state.applyEngineManagerStatus(vllmStatus(false)) // the user turns it off
        expect(processStatus('vllm')).toBe('stopped')
    })

    it('shows the real status again when the switch fails', () => {
        const state = getModularBridgeState()
        state.beginLocalModelSwitch('vllm')
        state.applyEngineManagerStatus(vllmStatus(false))
        expect(processStatus('vllm')).toBe('starting')

        state.clearLocalModelSwitch('vllm')
        expect(processStatus('vllm')).toBe('stopped')
        expect(state.isLocalModelSwitching('vllm')).toBe(false)
    })

    it('gives up after a bounded time so a lost notification cannot leave it spinning', () => {
        vi.useFakeTimers()
        const state = getModularBridgeState()
        state.beginLocalModelSwitch('vllm')
        state.applyEngineManagerStatus(vllmStatus(false))
        expect(processStatus('vllm')).toBe('starting')

        vi.advanceTimersByTime(20 * 60_000 + 1_000)
        expect(processStatus('vllm')).toBe('stopped')
    })

    it('leaves other engines alone', () => {
        const state = getModularBridgeState()
        state.beginLocalModelSwitch('vllm')
        state.applyEngineManagerStatus({
            engine: 'llamacpp',
            installed: true,
            running: false,
            healthy: false,
            port: 8081
        })
        expect(processStatus('llama-cpp')).toBe('stopped')
        expect(state.isLocalModelSwitching('llama-cpp')).toBe(false)
    })
})
