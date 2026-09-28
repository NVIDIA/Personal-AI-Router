// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'

const mocks = vi.hoisted(() => ({
    state: {
        getSelfId: vi.fn(() => 'local-node'),
        beginLocalEngineOp: vi.fn(),
        clearPendingEngineOp: vi.fn(),
        localEnginePathManaged: vi.fn(() => false)
    },
    supervisor: {
        hasProcess: vi.fn(() => true),
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
    parseServiceErrors: () => []
}))
vi.mock('@/electron/model-hub', () => ({ getEngineHubModels: vi.fn() }))

import { handleServiceBridgeInvoke } from '@/electron/service-bridge/empty-handlers'
import type { JsonValue } from '@/shared/types/json'

function sentParams(method: string): JsonValue[] {
    return mocks.supervisor.sendProcess.mock.calls
        .filter(call => call[1] === method)
        .map(call => call[2])
}

describe('engine PATH consent on local lifecycle commands', () => {
    beforeEach(() => {
        mocks.supervisor.sendProcess.mockClear()
        mocks.state.localEnginePathManaged.mockReturnValue(false)
    })

    it('forwards the install answer to engine:install', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'install',
            engineType: 'ollama',
            nodeId: 'local-node',
            path: true
        })
        await handleServiceBridgeInvoke('engine:command', {
            command: 'install',
            engineType: 'ollama',
            nodeId: 'local-node',
            path: false
        })

        expect(sentParams('engine:install')).toEqual([
            { engine: 'ollama', start: true, path: true },
            { engine: 'ollama', start: true, path: false }
        ])
    })

    it('treats an omitted answer as no consent', async () => {
        await handleServiceBridgeInvoke('engine:command', {
            command: 'uninstall',
            engineType: 'ollama',
            nodeId: 'local-node'
        })

        expect(sentParams('engine:uninstall')).toEqual([{ engine: 'ollama', path: false }])
    })

    it('carries the existing PATH choice through both steps of an update', async () => {
        mocks.state.localEnginePathManaged.mockReturnValue(true)

        await handleServiceBridgeInvoke('engine:command', {
            command: 'update',
            engineType: 'ollama',
            nodeId: 'local-node'
        })

        expect(sentParams('engine:uninstall')).toEqual([{ engine: 'ollama', path: true }])
        expect(sentParams('engine:install')).toEqual([
            { engine: 'ollama', start: true, path: true }
        ])
    })
})
