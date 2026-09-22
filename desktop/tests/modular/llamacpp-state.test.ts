// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'
vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { roundedProgressPercent } from '@/shared/utils/engine-progress'
import { formatPullProgressLabel } from '@/ui/utils/formatters'
import { subscribePush } from '@/electron/service-bridge/push-bus'

describe('managed llama state', () => {
    it.each([true, false])(
        'settles an already-installed no-op without changing running=%s facts',
        running => {
            const state = getModularBridgeState()
            const self = 'llama-noop-self'
            const peer = 'llama-noop-peer'
            const facts = {
                engine: 'llamacpp',
                installed: true,
                running,
                port: 8082,
                managed: true,
                install_supported: true,
                install_reason: 'Supported'
            }
            state.setSelfId(self)
            for (const id of [self, peer]) {
                state.handleNotification({
                    source: 'llamacpp-proxy',
                    method: 'node/discovered',
                    params: { id, port: 8080 }
                })
            }
            state.applyEngineManagerStatus(facts)
            state.applyRemoteEngineFacts(peer, { engines: [facts] })
            state.setLocalEngineModels('llamacpp', ['cached'])
            state.applyLocalLoadedModels({ llamacpp: running ? ['cached'] : [] })
            const cleared: string[] = []
            const unsubscribe = subscribePush(event => {
                if (event.channel === 'engines:progress-cleared') cleared.push(event.payload.key)
            })
            try {
                state.beginLocalEngineOp('llamacpp', 'installing')
                state.beginRemoteEngineOp(peer, 'llamacpp', 'installing')
                state.applyEngineManagerProgress({
                    engine: 'llamacpp',
                    stage: 'already-installed',
                    percent: 100
                })
                state.applyRemoteEngineProgress({
                    node: peer,
                    engine: 'llamacpp',
                    op: 'install',
                    stage: 'already-installed',
                    percent: 100
                })
                for (const nodeId of [self, peer]) {
                    expect(
                        state
                            .getEngineInitialState()
                            .statuses.find(s => s.nodeId === nodeId && s.engineType === 'llamacpp')
                    ).toMatchObject({
                        processStatus: running ? 'running' : 'stopped',
                        managed: true,
                        installSupported: true,
                        installReason: 'Supported',
                        enginePort: 8082
                    })
                    expect(cleared).toContain(nodeId + ':llamacpp:install')
                }
                expect(
                    state
                        .getEngineInitialState()
                        .models.find(m => m.nodeId === self && m.engineType === 'llamacpp')
                        ?.models[0]
                ).toMatchObject({ downloaded: true, status: running ? 'loaded' : 'idle' })
            } finally {
                unsubscribe()
                state.applyEngineManagerStatus(facts)
                state.clearPendingRemoteEngineOp(peer, 'llamacpp')
            }
        }
    )
    it('renders unknown progress without a false numeric percentage', () => {
        for (const value of [undefined, -1, 101, NaN, Infinity]) {
            expect(roundedProgressPercent(value)).toBeNull()
            expect(formatPullProgressLabel({ status: 'Installing', percent: value })).toBe(
                'Installing'
            )
        }
        expect(roundedProgressPercent(0)).toBe(0)
        expect(roundedProgressPercent(12.5)).toBe(13)
        expect(roundedProgressPercent(100)).toBe(100)
    })
    it('preserves authoritative ownership/support through local and remote pending operations', () => {
        const state = getModularBridgeState()
        const self = 'llama-pending-self'
        const peer = 'llama-pending-peer'
        const facts = {
            engine: 'llamacpp',
            installed: true,
            running: false,
            port: 8082,
            managed: true,
            install_supported: true,
            install_reason: 'Supported inventory'
        }
        state.setSelfId(self)
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: peer,
                        name: 'peer',
                        port: 14318,
                        modelsByEngine: { llamacpp: ['cached-peer-model'] }
                    }
                ]
            }
        })
        state.applyEngineManagerStatus(facts)
        state.applyRemoteEngineFacts(peer, { engines: [facts] })
        state.beginLocalEngineOp('llamacpp', 'installing')
        state.beginRemoteEngineOp(peer, 'llamacpp', 'installing')
        try {
            for (const nodeId of [self, peer]) {
                expect(
                    state
                        .getEngineInitialState()
                        .statuses.find(s => s.nodeId === nodeId && s.engineType === 'llamacpp')
                ).toMatchObject({
                    processStatus: 'installing',
                    managed: true,
                    installSupported: true,
                    installReason: 'Supported inventory'
                })
            }
            const peerModels = () =>
                state
                    .getEngineInitialState()
                    .models.find(m => m.nodeId === peer && m.engineType === 'llamacpp')?.models
            expect(peerModels()?.[0]).toMatchObject({ downloaded: true, status: 'idle' })
            state.applyRemoteEngineFacts(peer, { engines: [{ ...facts, managed: false }] })
            expect(peerModels()?.[0]).toMatchObject({ downloaded: false, status: 'idle' })
        } finally {
            state.applyEngineManagerStatus(facts)
            state.clearPendingRemoteEngineOp(peer, 'llamacpp')
        }
    })
    it('keeps equal request counters distinct across engines and proxy runs', () => {
        const state = getModularBridgeState()
        state.clearWorkloads()
        const base = {
            id: '1',
            model: 'model',
            state: 'running',
            originatedFrom: 'self',
            createdAt: 1
        }
        state.upsertWorkloadFromInfo({ workloadInfo: { ...base, engine: 'ollama', runId: 'a' } })
        state.upsertWorkloadFromInfo({ workloadInfo: { ...base, engine: 'llamacpp', runId: 'a' } })
        state.upsertWorkloadFromInfo({ workloadInfo: { ...base, engine: 'llamacpp', runId: 'b' } })
        expect(Object.values(state.getWorkloads())).toHaveLength(3)
        state.removeWorkloadFromParams({
            workloadId: '1',
            originatedFrom: 'self',
            engine: 'llamacpp',
            runId: 'a'
        })
        expect(Object.values(state.getWorkloads())).toHaveLength(2)
    })
    it('preserves installation support and cached downloads while stopped without stale loaded state', () => {
        const state = getModularBridgeState()
        const nodeId = 'llama-state-self'
        state.setSelfId(nodeId)
        state.handleNotification({
            source: 'llamacpp-proxy',
            method: 'node/discovered',
            params: { id: nodeId, port: 8080 }
        })
        state.applyEngineManagerStatus({
            engine: 'llamacpp',
            installed: true,
            running: true,
            port: 8082,
            managed: true,
            install_supported: true
        })
        state.applyLocalLoadedModels({ llamacpp: ['owner/model:Q4'] })
        state.setLocalEngineModels('llamacpp', ['owner/model:Q4'])
        const models = () =>
            state
                .getEngineInitialState()
                .models.find(m => m.nodeId === nodeId && m.engineType === 'llamacpp')?.models
        expect(models()?.[0]).toMatchObject({ downloaded: true, status: 'loaded' })
        state.applyEngineManagerStatus({
            engine: 'llamacpp',
            installed: true,
            running: false,
            port: 8082,
            managed: true,
            install_supported: false,
            install_reason: 'Unsupported platform'
        })
        expect(models()?.[0]).toMatchObject({ downloaded: true, status: 'idle' })
        expect(
            state
                .getEngineInitialState()
                .statuses.find(s => s.nodeId === nodeId && s.engineType === 'llamacpp')
        ).toMatchObject({
            managed: true,
            installSupported: false,
            installReason: 'Unsupported platform'
        })
        state.setLocalEngineModels('llamacpp', ['catalogue-only'], false)
        expect(models()?.[0]).toMatchObject({ downloaded: false, status: 'idle' })
    })
})
