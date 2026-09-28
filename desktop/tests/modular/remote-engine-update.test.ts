// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import type { EngineType } from '@/shared/types/engines'

const mocks = vi.hoisted(() => ({
    state: {
        beginRemoteEngineOp: vi.fn(),
        applyRemoteEngineStatusResult: vi.fn(),
        clearPendingRemoteEngineOp: vi.fn()
    }
}))

vi.mock('electron', () => ({
    app: {
        isPackaged: false,
        getAppPath: () => process.cwd()
    }
}))
vi.mock('@/shared/utils/log', () => ({
    createStructuredLogger: () => ({
        info: vi.fn(),
        warn: vi.fn(),
        error: vi.fn(),
        verbose: vi.fn()
    })
}))
vi.mock('@/electron/config/ui-config', () => ({ isFirstRun: () => false }))
vi.mock('@/electron/service-bridge/broadcaster', () => ({ emitBridgePush: vi.fn() }))
vi.mock('@/electron/service-bridge/manual-nodes-store', () => ({
    listManualNodeEntries: () => []
}))
vi.mock('@/electron/service-bridge/node-info-poller', () => ({
    startNodeInfoPoller: vi.fn(),
    stopNodeInfoPoller: vi.fn()
}))
vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.state,
    isProxyEngine: () => true,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => [],
    parseWorkloadsInitial: () => [],
    PROXY_ENGINES: ['ollama', 'lm-studio', 'vllm'],
    PROXY_NODE_SOURCES: ['ollama-proxy', 'lmstudio-proxy', 'vllm-proxy']
}))

import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'

interface RemoteUpdateHarness {
    callProcess: ReturnType<typeof vi.fn>
    reportError: ReturnType<typeof vi.fn>
    updateEngineRemote(nodeId: string, engine: string, engineType: EngineType): Promise<void>
}

const supervisor = getModularSupervisor() as unknown as RemoteUpdateHarness

describe('remote engine update supervisor', () => {
    beforeEach(() => {
        vi.clearAllMocks()
        supervisor.callProcess = vi.fn()
        supervisor.reportError = vi.fn()
    })

    it('uses the product route and applies its terminal authoritative status', async () => {
        const status = {
            engine: 'vllm',
            installed: true,
            running: false,
            healthy: false,
            managed: true,
            version: '0.29.0'
        }
        supervisor.callProcess.mockResolvedValue({ opId: 'update-op', status })

        await supervisor.updateEngineRemote('peer-b', 'vllm', 'vllm')

        expect(mocks.state.beginRemoteEngineOp).toHaveBeenCalledWith('peer-b', 'vllm', 'installing')
        expect(supervisor.callProcess).toHaveBeenCalledWith(
            'broker',
            'engine:remote-update',
            { node: 'peer-b', engine: 'vllm' },
            expect.any(Number)
        )
        expect(mocks.state.applyRemoteEngineStatusResult).toHaveBeenCalledWith(
            'peer-b',
            'vllm',
            status
        )
        expect(mocks.state.clearPendingRemoteEngineOp).toHaveBeenCalledWith('peer-b', 'vllm')
        expect(supervisor.reportError).not.toHaveBeenCalled()
    })

    it('clears optimistic state and attributes a failed update to the target', async () => {
        supervisor.callProcess.mockRejectedValue(new Error('peer refused update'))

        await supervisor.updateEngineRemote('peer-c', 'vllm', 'vllm')

        expect(supervisor.reportError).toHaveBeenCalledWith(
            'Failed to update vllm on peer-c: peer refused update',
            'error',
            'engine-remote-update:peer-c:vllm',
            {
                id: 'engine-manager:update-failed:vllm',
                nodeId: 'peer-c',
                engineType: 'vllm',
                operation: 'update',
                action: 'retry'
            }
        )
        expect(mocks.state.clearPendingRemoteEngineOp).toHaveBeenCalledWith('peer-c', 'vllm')
    })
})
