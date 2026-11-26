// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import type { JsonValue } from '@/electron/service-bridge/json-rpc-subprocess'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { getModularSupervisor } from '@/electron/service-bridge/modular-supervisor'
import { subscribePush } from '@/electron/service-bridge/push-bus'
import type { EngineType } from '@/shared/types/engines'

vi.mock('electron', () => ({
    app: { isPackaged: false, getAppPath: () => process.cwd() },
    BrowserWindow: { getAllWindows: () => [] }
}))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

describe('applying a managed update on a peer', () => {
    const state = getModularBridgeState()
    const supervisor = getModularSupervisor()
    const offer = {
        installType: 'managed',
        currentVersion: '0.4.0',
        latestVersion: '0.5.0',
        releaseUrl: 'https://lmstudio.ai/changelog/lmstudio'
    }
    const status = { engine: 'lmstudio', installed: true, running: true, port: 1235 }
    const peerStatus = () =>
        state
            .getEngineInitialState()
            .statuses.find(s => s.nodeId === 'peer' && s.engineType === 'lm-studio')?.processStatus

    beforeEach(() => {
        state.setSelfId('self')
        state.setClusterPeerIds(new Set(['peer']))
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: 'peer',
                        name: 'Peer',
                        ipAddress: '192.0.2.71',
                        port: 14318,
                        trusted: true,
                        clustered: true
                    }
                ]
            }
        })
        state.applyRemoteEngineFacts('peer', { engines: [status] })
        state.applyEngineUpdateCheck('peer', 'lm-studio', { updateAvailable: offer })
    })

    afterEach(() => {
        state.setEngineUpdateRefresher(undefined)
        state.setClusterPeerIds(new Set())
        state.clearError('engine-manager:update-failed:lmstudio')
        vi.restoreAllMocks()
    })

    it('sends the update to the peer and rechecks its offer after completion', async () => {
        const refresh = vi.fn<(engineType?: EngineType, nodeId?: string) => void>()
        state.setEngineUpdateRefresher(refresh)
        const reply = Promise.withResolvers<JsonValue>()
        const call = vi.spyOn(supervisor, 'callProcess').mockReturnValue(reply.promise)

        const applying = supervisor.installOrUpdateEngineRemote(
            'peer',
            'lmstudio',
            'lm-studio',
            'update'
        )
        expect(call).toHaveBeenCalledWith(
            'broker',
            'engine:remote-update',
            { node: 'peer', engine: 'lmstudio' },
            expect.any(Number)
        )
        expect(peerStatus()).toBe('installing')

        reply.resolve({ opId: 'update-1', status: { ...status, running: false } })
        await applying
        expect(peerStatus()).toBe('stopped')
        expect(state.getEngineInitialState().updateAvailable).toEqual([])
        expect(refresh).toHaveBeenCalledWith('lm-studio', 'peer')
    })

    it('restores the peer status and reports a retry on the same peer after rejection', async ({
        onTestFinished
    }) => {
        const push = vi.fn<Parameters<typeof subscribePush>[0]>()
        onTestFinished(subscribePush(push))
        vi.spyOn(supervisor, 'callProcess').mockRejectedValue(new Error('peer update rejected'))

        await supervisor.installOrUpdateEngineRemote('peer', 'lmstudio', 'lm-studio', 'update')

        expect(peerStatus()).toBe('running')
        expect(push).toHaveBeenCalledWith({
            channel: 'engines:progress-cleared',
            payload: { key: 'peer:lm-studio:install' }
        })
        expect(state.getEngineInitialState().updateAvailable).toEqual([
            { ...offer, engineType: 'lm-studio', nodeId: 'peer' }
        ])
        expect(state.getErrors()).toEqual([
            expect.objectContaining({
                id: 'engine-manager:update-failed:lmstudio',
                message: expect.stringContaining('peer update rejected'),
                severity: 'error',
                nodeId: 'peer',
                engineType: 'lm-studio',
                operation: 'update',
                action: 'retry'
            })
        ])
    })

    it('publishes the peer’s update download progress and clears it on completion', ({
        onTestFinished
    }) => {
        const push = vi.fn<Parameters<typeof subscribePush>[0]>()
        onTestFinished(subscribePush(push))
        const frame = { node: 'peer', engine: 'lmstudio', op: 'update' }

        state.applyRemoteEngineProgress({ ...frame, stage: 'downloading', percent: 42 })
        expect(push).toHaveBeenCalledWith({
            channel: 'engines:progress-changed',
            payload: expect.objectContaining({
                nodeId: 'peer',
                engineType: 'lm-studio',
                operation: 'install',
                status: 'downloading',
                percent: 42
            })
        })

        state.applyRemoteEngineProgress({ ...frame, stage: 'done' })
        expect(push).toHaveBeenCalledWith({
            channel: 'engines:progress-cleared',
            payload: { key: 'peer:lm-studio:install' }
        })
    })
})
