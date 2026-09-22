// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { JsonRpcTimeoutError } from '@/electron/service-bridge/json-rpc-subprocess'
import { MODULAR_CANCEL_PULL_TIMEOUT_MS } from '@/shared/constants/modular-runtime'

const mocks = vi.hoisted(() => ({
    bridgeState: {
        handleNotification: vi.fn(),
        getSelfId: vi.fn(() => null),
        getProxyPort: vi.fn(() => null),
        isModelPullActive: vi.fn(() => true),
        isRemoteModelPullActive: vi.fn(() => true),
        setModelPullCanceling: vi.fn(() => true)
    },
    emitBridgePush: vi.fn()
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

vi.mock('@/electron/config/ui-config', () => ({
    isFirstRun: () => false
}))

vi.mock('@/electron/service-bridge/broadcaster', () => ({
    emitBridgePush: mocks.emitBridgePush
}))

vi.mock('@/electron/service-bridge/manual-nodes-store', () => ({
    listManualNodeEntries: () => []
}))

vi.mock('@/electron/service-bridge/node-info-poller', () => ({
    startNodeInfoPoller: vi.fn(),
    stopNodeInfoPoller: vi.fn()
}))

vi.mock('@/electron/service-bridge/modular-state', () => ({
    getModularBridgeState: () => mocks.bridgeState,
    isUpstreamUnreachableError: () => false,
    parseServiceErrors: () => [],
    parseWorkloadsInitial: () => [],
    PROXY_ENGINES: ['ollama', 'lm-studio'],
    PROXY_NODE_SOURCES: ['ollama-proxy', 'lmstudio-proxy']
}))

import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'

const supervisor = getModularSupervisor()

describe('cancelling a model download', () => {
    beforeEach(() => {
        vi.restoreAllMocks()
        mocks.bridgeState.setModelPullCanceling.mockClear().mockReturnValue(true)
        mocks.bridgeState.isModelPullActive.mockReturnValue(true)
        mocks.bridgeState.isRemoteModelPullActive.mockReturnValue(true)
    })

    it('gives the backend a budget above the peer response-header timeout', async () => {
        const call = vi.spyOn(supervisor, 'callProcess').mockResolvedValue(null)

        await supervisor.cancelModelPull('ollama', 'ollama', 'demo')

        expect(call).toHaveBeenCalledWith(
            'broker',
            'engine:cancel-pull',
            { engine: 'ollama', model: 'demo' },
            MODULAR_CANCEL_PULL_TIMEOUT_MS
        )
        // The peer's own header budget is 30s and starts before it writes one,
        // so an outer budget that merely matched it would expire first.
        expect(MODULAR_CANCEL_PULL_TIMEOUT_MS).toBeGreaterThan(30_000)
    })

    // The backend is still stopping the transfer and cleaning up after it. Its
    // own settling event clears the row; dropping it back to "downloading" here
    // would tell the user the cancel failed seconds before it succeeds.
    it('leaves the row in Canceling when the request outlives its own budget', async () => {
        const reportError = vi.spyOn(supervisor, 'reportError').mockImplementation(() => {})
        vi.spyOn(supervisor, 'callProcess').mockRejectedValue(
            new JsonRpcTimeoutError('broker engine:cancel-pull timed out')
        )

        await supervisor.cancelModelPull('ollama', 'ollama', 'demo')

        expect(mocks.bridgeState.setModelPullCanceling).toHaveBeenCalledTimes(1)
        expect(mocks.bridgeState.setModelPullCanceling).toHaveBeenCalledWith(
            'ollama',
            'demo',
            true,
            undefined
        )
        expect(reportError).not.toHaveBeenCalled()
    })

    it('restores the previous status when the backend rejects the cancel', async () => {
        const reportError = vi.spyOn(supervisor, 'reportError').mockImplementation(() => {})
        vi.spyOn(supervisor, 'callProcess').mockRejectedValue(
            new Error('engine ollama is not running')
        )

        await supervisor.cancelModelPull('ollama', 'ollama', 'demo')

        expect(mocks.bridgeState.setModelPullCanceling).toHaveBeenCalledWith(
            'ollama',
            'demo',
            false,
            undefined
        )
        expect(reportError).toHaveBeenCalledWith(
            expect.stringContaining('Failed to cancel download of demo'),
            'error',
            'engine-cancel-pull:local:ollama:demo',
            expect.objectContaining({ engineType: 'ollama', operation: 'pull', modelName: 'demo' })
        )
    })

    // A download on a peer is cancelled through the initiating node's
    // engine-manager, which relays it over the cluster's `ec` surface. The row
    // and its budget are the local ones either way, so only the method, the
    // node it names, and the error's own key change.
    it('routes a peer node cancellation through the remote method', async () => {
        const call = vi.spyOn(supervisor, 'callProcess').mockResolvedValue(null)

        await supervisor.cancelModelPull('ollama', 'ollama', 'demo', 'uuid-b')

        expect(call).toHaveBeenCalledWith(
            'broker',
            'engine:remote-cancel-pull',
            { node: 'uuid-b', engine: 'ollama', model: 'demo' },
            MODULAR_CANCEL_PULL_TIMEOUT_MS
        )
        expect(mocks.bridgeState.setModelPullCanceling).toHaveBeenCalledWith(
            'ollama',
            'demo',
            true,
            'uuid-b'
        )
    })

    it('leaves a peer node row in Canceling when the request outlives its budget', async () => {
        const reportError = vi.spyOn(supervisor, 'reportError').mockImplementation(() => {})
        vi.spyOn(supervisor, 'callProcess').mockRejectedValue(
            new JsonRpcTimeoutError('broker engine:remote-cancel-pull timed out')
        )

        await supervisor.cancelModelPull('ollama', 'ollama', 'demo', 'uuid-b')

        expect(mocks.bridgeState.setModelPullCanceling).toHaveBeenCalledTimes(1)
        expect(mocks.bridgeState.setModelPullCanceling).toHaveBeenCalledWith(
            'ollama',
            'demo',
            true,
            'uuid-b'
        )
        expect(reportError).not.toHaveBeenCalled()
    })

    // The error key carries the node, so two peers downloading the same model
    // report separately instead of overwriting one another's row.
    it('restores the peer node row and keys its error by node when the cancel is rejected', async () => {
        const reportError = vi.spyOn(supervisor, 'reportError').mockImplementation(() => {})
        vi.spyOn(supervisor, 'callProcess').mockRejectedValue(
            new Error('node uuid-b is not a discovered ec peer')
        )

        await supervisor.cancelModelPull('ollama', 'ollama', 'demo', 'uuid-b')

        expect(mocks.bridgeState.setModelPullCanceling).toHaveBeenCalledWith(
            'ollama',
            'demo',
            false,
            'uuid-b'
        )
        expect(reportError).toHaveBeenCalledWith(
            expect.stringContaining('Failed to cancel download of demo'),
            'error',
            'engine-cancel-pull:uuid-b:ollama:demo',
            expect.objectContaining({
                engineType: 'ollama',
                nodeId: 'uuid-b',
                operation: 'pull',
                modelName: 'demo'
            })
        )
    })

    // Each path checks its own registry before sending anything: a stale click
    // on a row whose download already settled must not reach the backend, and
    // must not put the row back into "Canceling".
    it('sends nothing when the download it names is no longer active', async () => {
        const call = vi.spyOn(supervisor, 'callProcess').mockResolvedValue(null)
        mocks.bridgeState.isModelPullActive.mockReturnValue(false)
        mocks.bridgeState.isRemoteModelPullActive.mockReturnValue(false)

        await supervisor.cancelModelPull('ollama', 'ollama', 'demo')
        await supervisor.cancelModelPull('ollama', 'ollama', 'demo', 'uuid-b')

        expect(call).not.toHaveBeenCalled()
        expect(mocks.bridgeState.setModelPullCanceling).not.toHaveBeenCalled()
    })
})
