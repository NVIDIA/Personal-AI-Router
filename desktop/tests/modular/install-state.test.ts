// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import type { EngineProcessStatus } from '@/shared/types/engines'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

let unsubscribe = (): void => {}

afterEach(() => {
    unsubscribe()
    unsubscribe = () => {}
    getModularBridgeState().clearPendingEngineOp('lm-studio')
    vi.useRealTimers()
})

describe('LM Studio install state', () => {
    it('does not revert a quiet install to Download after 90 seconds', () => {
        vi.useFakeTimers()
        const state = getModularBridgeState()
        const statuses: EngineProcessStatus[] = []
        unsubscribe = subscribePush(event => {
            if (
                event.channel === 'engines:state-changed' &&
                event.payload.engineType === 'lm-studio' &&
                event.payload.status
            ) {
                statuses.push(event.payload.status.processStatus)
            }
        })

        state.setSelfId('local-node')
        state.applyEngineManagerStatus({
            engine: 'lmstudio',
            installed: false,
            running: false,
            port: 1234
        })
        state.beginLocalEngineOp('lm-studio', 'installing')

        vi.advanceTimersByTime(120_000)
        expect(statuses.at(-1)).toBe('installing')

        state.applyEngineManagerProgress({ engine: 'lmstudio', stage: 'installing', percent: 75 })
        vi.advanceTimersByTime(30 * 60_000 - 1)
        expect(statuses.at(-1)).toBe('installing')
        vi.advanceTimersByTime(1)
        expect(statuses.at(-1)).toBe('not-installed')

        state.beginLocalEngineOp('lm-studio', 'starting')
        vi.advanceTimersByTime(90_000)
        expect(statuses.at(-1)).toBe('not-installed')
    })
})
