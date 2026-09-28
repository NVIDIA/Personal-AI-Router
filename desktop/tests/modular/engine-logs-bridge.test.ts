// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
    supervisor: { callProcess: vi.fn() },
    state: { getEngineInitialState: vi.fn() }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state,
    isProxyEngine: () => false,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => [],
    parseWorkloadsInitial: () => []
}))
vi.mock('@/electron/model-hub', () => ({ getEngineHubModels: vi.fn() }))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'

describe('engine log bridge', () => {
    beforeEach(() => vi.clearAllMocks())

    it('maps the closed engine type and returns the bounded local ring', async () => {
        mocks.supervisor.callProcess.mockResolvedValue({
            lines: [{ time: '03:54:45.481', stream: 'stderr', text: 'startup failed' }]
        })

        const snapshot = await handleServiceBridgeInvoke('engine:logs', {
            engineType: 'lm-studio'
        })

        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'engine:logs', {
            engine: 'lmstudio'
        })
        expect(snapshot.lines).toEqual([
            { time: '03:54:45.481', stream: 'stderr', text: 'startup failed' }
        ])
    })

    it('rejects malformed child output at the Electron boundary', async () => {
        mocks.supervisor.callProcess.mockResolvedValue({
            lines: [{ time: 'not-a-time', stream: 'stderr', text: 'bad' }]
        })
        await expect(
            handleServiceBridgeInvoke('engine:logs', { engineType: 'vllm' })
        ).rejects.toThrow('invalid log line')
    })

    it('accepts a near-limit aggregate snapshot', async () => {
        const textBytes = 256 * 1024 - 1024
        mocks.supervisor.callProcess.mockResolvedValue({
            lines: Array.from({ length: 4 }, (_, index) => ({
                time: `03:54:45.${481 + index}`,
                stream: 'stdout',
                text: 'a'.repeat(textBytes)
            }))
        })

        const snapshot = await handleServiceBridgeInvoke('engine:logs', {
            engineType: 'vllm'
        })
        expect(snapshot.lines).toHaveLength(4)
    })

    it('rejects line, aggregate, and count overflow at the Electron boundary', async () => {
        const line = (text: string) => ({ time: '03:54:45.481', stream: 'stdout', text })
        const cases = [
            {
                response: { lines: [line('x'.repeat(256 * 1024 + 1))] },
                message: 'invalid log line'
            },
            {
                response: {
                    lines: Array.from({ length: 5 }, () => line('x'.repeat(220 * 1024)))
                },
                message: 'invalid log snapshot'
            },
            {
                response: { lines: Array.from({ length: 2_001 }, () => line('')) },
                message: 'invalid log snapshot'
            }
        ]

        for (const { response, message } of cases) {
            mocks.supervisor.callProcess.mockResolvedValueOnce(response)
            await expect(
                handleServiceBridgeInvoke('engine:logs', { engineType: 'vllm' })
            ).rejects.toThrow(message)
        }
    })
})
