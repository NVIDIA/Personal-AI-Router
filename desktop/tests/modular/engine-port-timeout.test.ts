// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn(() => 'local-node'),
        getProxyPort: vi.fn(() => 11434),
        beginLocalEngineOp: vi.fn(),
        failLocalEngineOp: vi.fn()
    },
    supervisor: {
        hasProcess: vi.fn(() => true),
        callProcess: vi.fn(),
        callProxy: vi.fn(),
        sendProcess: vi.fn(),
        reportError: vi.fn()
    }
}))

vi.mock('@/electron/service-bridge/modular-supervisor', () => ({
    getModularSupervisor: () => mocks.supervisor
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state,
    isProxyEngine: (value: string) => value === 'ollama' || value === 'lm-studio',
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => []
}))
vi.mock('@/electron/model-hub', () => ({ getEngineHubModels: vi.fn() }))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'
import { MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS } from '@/shared/constants/modular-runtime'

describe('engine port lifecycle timeout', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        mocks.state.getSelfId.mockReturnValue('local-node')
        mocks.state.getProxyPort.mockReturnValue(11434)
        mocks.supervisor.hasProcess.mockReturnValue(true)
        mocks.supervisor.callProcess.mockResolvedValue({})
        mocks.supervisor.callProxy.mockResolvedValue({})
    })

    it('keeps a single engine-port rebind outside the backend readiness budget', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'setPorts',
            engineType: 'ollama',
            nodeId: 'local-node',
            enginePort: 11435
        })

        await vi.waitFor(() =>
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'engine:set-port',
                { engine: 'ollama', port: 11435 },
                MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS
            )
        )
        expect(MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS).toBe(11 * 60_000)
    })

    it('uses the same bounded allowance across a collision-safe stop, rebind, and restart', async () => {
        mocks.supervisor.callProcess.mockImplementation(async (_name: string, method: string) =>
            method === 'engine:status' ? { running: true, port: 1235 } : {}
        )

        await handleServiceBridgeInvoke('engine:command', {
            command: 'setPorts',
            engineType: 'ollama',
            nodeId: 'local-node',
            enginePort: 11434,
            proxyPort: 1235
        })

        await vi.waitFor(() =>
            expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
                'broker',
                'engine:start',
                { engine: 'ollama' },
                MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS
            )
        )
        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
            'broker',
            'engine:stop',
            { engine: 'ollama' },
            MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS
        )
        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith(
            'broker',
            'engine:set-port',
            { engine: 'ollama', port: 11434 },
            MODULAR_ENGINE_LIFECYCLE_CALL_TIMEOUT_MS
        )
    })
})
