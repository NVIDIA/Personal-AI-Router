// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import type { EngineType } from '@/shared/types/engines'
import { engineManagerName } from '@/shared/utils/engines'

const NODE = 'start-pending-self'

function processStatus(engineType: EngineType) {
    return getModularBridgeState()
        .getEngineInitialState()
        .statuses.find(status => status.nodeId === NODE && status.engineType === engineType)
        ?.processStatus
}

function reportStopped(engineType: EngineType): void {
    getModularBridgeState().applyEngineManagerStatus({
        engine: engineManagerName(engineType),
        installed: true,
        running: false,
        healthy: false,
        port: 8001
    })
}

// vLLM and Ollama allow their readiness probe minutes. Until the engine-manager
// reports the engine up, or the start failed, the engine is starting: shown as
// stopped, its toggle would send another Start instead of Stop.
describe('a local engine start', () => {
    beforeEach(() => {
        vi.useFakeTimers()
        const state = getModularBridgeState()
        state.setSelfId(NODE)
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [{ hostUuid: NODE, name: 'start-host', ipAddress: '127.0.0.1', port: 14318 }]
            }
        })
    })

    afterEach(() => {
        for (const engineType of ['vllm', 'ollama'] as const) {
            getModularBridgeState().clearPendingEngineOp(engineType)
        }
        vi.useRealTimers()
    })

    it('stays starting through the readiness window of a slow engine', () => {
        const test = (engineType: EngineType, readinessMs: number) => {
            reportStopped(engineType)
            getModularBridgeState().beginLocalEngineOp(engineType, 'starting')

            vi.advanceTimersByTime(readinessMs)

            expect(processStatus(engineType)).toBe('starting')
        }
        test('vllm', 480_000)
        test('ollama', 600_000)
    })

    it('ends when the engine-manager reports the outcome', () => {
        reportStopped('vllm')
        getModularBridgeState().beginLocalEngineOp('vllm', 'starting')

        getModularBridgeState().applyEngineManagerStatus({
            engine: 'vllm',
            installed: true,
            running: true,
            healthy: true,
            port: 8001
        })

        expect(processStatus('vllm')).toBe('running')
    })

    it('gives up after the lifecycle envelope so a lost notification cannot leave it spinning', () => {
        reportStopped('vllm')
        getModularBridgeState().beginLocalEngineOp('vllm', 'starting')

        vi.advanceTimersByTime(14 * 60_000 + 1_000)

        expect(processStatus('vllm')).toBe('stopped')
    })

    it('keeps the short net for a stop', () => {
        getModularBridgeState().applyEngineManagerStatus({
            engine: 'vllm',
            installed: true,
            running: true,
            healthy: true,
            port: 8001
        })
        getModularBridgeState().beginLocalEngineOp('vllm', 'stopping')

        vi.advanceTimersByTime(91_000)

        expect(processStatus('vllm')).toBe('running')
    })

    // A start this app did not send (install-then-start, a settings restart,
    // a restore at launch, the terminal interface) has no pending op here. The
    // engine-manager reports it as starting, so it is not shown as off.
    it('shows a start the engine-manager reports, whoever began it', () => {
        const report = (running: boolean, starting: boolean) =>
            getModularBridgeState().applyEngineManagerStatus({
                engine: 'vllm',
                installed: true,
                running,
                starting,
                healthy: running,
                port: 8001
            })

        report(false, true)
        expect(processStatus('vllm')).toBe('starting')

        vi.advanceTimersByTime(20 * 60_000)
        expect(processStatus('vllm')).toBe('starting')

        report(true, false)
        expect(processStatus('vllm')).toBe('running')

        report(false, false)
        expect(processStatus('vllm')).toBe('stopped')
    })
})
