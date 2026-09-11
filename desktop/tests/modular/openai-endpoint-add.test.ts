// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn(() => 'local-node')
    },
    supervisor: {
        callProcess: vi.fn(),
        sendProcess: vi.fn(),
        reportError: vi.fn()
    }
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

describe('nodes:add-endpoint (adopt an external OpenAI endpoint by URL)', () => {
    it('rejects a URL without a scheme before touching the service', async () => {
        const result = await handleServiceBridgeInvoke('nodes:add-endpoint', {
            url: 'localhost:8888'
        })

        expect(result.ok).toBe(false)
        expect(result.error).toMatch(/http:\/\//)
        expect(mocks.supervisor.callProcess).not.toHaveBeenCalled()
    })

    it('relays a full http URL to the broker as node/add with openai_base_url', async () => {
        mocks.supervisor.callProcess.mockResolvedValue(undefined)

        const result = await handleServiceBridgeInvoke('nodes:add-endpoint', {
            url: ' http://192.168.1.50:8888/v1 '
        })

        expect(mocks.supervisor.callProcess).toHaveBeenCalledWith('broker', 'node/add', {
            openai_base_url: 'http://192.168.1.50:8888/v1'
        })
        expect(result).toEqual({ ok: true })
    })

    it('surfaces a service failure as an inline error, not a thrown rejection', async () => {
        mocks.supervisor.callProcess.mockRejectedValue(
            new Error('node already registered for this endpoint')
        )

        const result = await handleServiceBridgeInvoke('nodes:add-endpoint', {
            url: 'http://192.168.1.50:8888/v1'
        })

        expect(result.ok).toBe(false)
        expect(result.error).toContain('node already registered')
    })
})
