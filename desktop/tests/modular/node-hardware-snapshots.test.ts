// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { afterEach, describe, expect, it, vi } from 'vitest'
import type { NodeIdentityVerification } from '@/shared/types/cluster'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))
vi.mock('@/electron/service-bridge/broadcaster', () => ({
    emitBridgePush: vi.fn<typeof emitBridgePush>()
}))

import { emitBridgePush } from '@/electron/service-bridge/broadcaster'
import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

afterEach(() => vi.restoreAllMocks())

describe('broker hardware snapshots', () => {
    const test = (name: string, verification: NodeIdentityVerification): void => {
        it(name, () => {
            const state = getModularBridgeState()
            const hostUuid = `baseline-${verification}`
            const discoveryRecordId = `record:${hostUuid}`
            const key = verification === 'unverified' ? discoveryRecordId : hostUuid

            // Public hardware may claim another host or name. The bridge must
            // use the broker record's identity while still showing its preview.
            state.handleNotification({
                source: 'broker',
                method: 'discovery:nodes-changed',
                params: [
                    {
                        hostUuid,
                        discoveryRecordId,
                        identityVerification: verification,
                        name: 'Hardware host',
                        ipAddress: '192.0.2.30',
                        port: 14318,
                        info: {
                            hostUuid:
                                verification === 'unverified' ? 'another-public-claim' : hostUuid,
                            GPUs: [{ name: 'Baseline GPU', vram_bytes: 8192 }],
                            cpu: { name: 'Baseline CPU', cores: 8 },
                            memory: { total_bytes: 16384 },
                            peer: { name: 'Untrusted display name' }
                        }
                    }
                ]
            })

            expect(state.getNodesInitial().nodes[key]).toMatchObject({
                name: 'Hardware host',
                topology: {
                    gpus: [{ name: 'Baseline GPU', vramTotal: 8192 }],
                    cpu: { model: 'Baseline CPU', cores: 8 },
                    ram: 16384
                }
            })

            expect(state.getAvailableNodes().find(node => node.id === key)).toMatchObject({
                discoveryRecordId,
                identityVerification: verification,
                name: 'Hardware host'
            })
        })
    }

    test('shows local hardware from the initial broker snapshot', 'local')
    test('shows paired hardware from the initial broker snapshot', 'pinned')
    test('shows an unpaired preview under its discovery record', 'unverified')

    it('clears cached hardware when a replacement broker record has no sample', () => {
        const state = getModularBridgeState()
        const node = {
            hostUuid: 'withdrawn-hardware',
            discoveryRecordId: 'record:withdrawn-hardware',
            identityVerification: 'pinned',
            name: 'Hardware host',
            ipAddress: '192.0.2.31',
            port: 14318
        }
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: [{ ...node, info: { hostUuid: node.hostUuid, GPUs: [{ name: 'Old GPU' }] } }]
        })
        expect(state.getNodesInitial().nodes[node.hostUuid].topology.gpus).toHaveLength(1)

        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: [{ ...node, discoveryRecordId: 'record:renewed-proof' }]
        })

        expect(state.getNodesInitial().nodes[node.hostUuid].topology.gpus).toEqual([])
    })

    it('preserves the sample timestamp across a snapshot and an unchanged hardware update', () => {
        vi.spyOn(Date, 'now').mockReturnValue(100_000)
        const state = getModularBridgeState()
        const hostUuid = 'aged-hardware'
        const discoveryRecordId = 'record:aged-hardware'
        const info = {
            hostUuid,
            GPUs: [{ name: 'Cached GPU', utilization_percent: 25 }],
            telemetryValid: true,
            msSince: 7000
        }
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: [
                {
                    hostUuid,
                    discoveryRecordId,
                    identityVerification: 'pinned',
                    ipAddress: '192.0.2.32',
                    port: 14318,
                    info
                }
            ]
        })
        expect(emitBridgePush).toHaveBeenCalledWith(
            'metrics:update',
            expect.objectContaining({
                id: hostUuid,
                current: expect.objectContaining({ timestamp: 93_000 })
            })
        )

        // Two seconds later the same sample is two seconds older. Its original
        // measurement time must remain 93,000 ms rather than becoming fresh.
        vi.mocked(emitBridgePush).mockClear()
        vi.mocked(Date.now).mockReturnValue(102_000)

        state.handleNotification({
            source: 'broker',
            method: 'discovery:node-info',
            params: {
                hostUuid,
                discoveryRecordId,
                identityVerification: 'pinned',
                info: { ...info, msSince: 9000 }
            }
        })

        expect(emitBridgePush).toHaveBeenCalledWith(
            'metrics:update',
            expect.objectContaining({
                id: hostUuid,
                current: expect.objectContaining({ timestamp: 93_000 })
            })
        )
    })

    // Owned hardware arrives through IPC even before a node-info endpoint is
    // advertised. A CPU-only host still needs updates when its readings hold steady.
    it('refreshes unchanged hardware without an advertised node-info endpoint', () => {
        vi.spyOn(Date, 'now').mockReturnValue(100_000)
        const state = getModularBridgeState()
        const hostUuid = 'hardware-without-endpoint'
        const discoveryRecordId = 'local:hardware-without-endpoint'
        const info = {
            hostUuid,
            GPUs: [],
            cpu: { name: 'CPU', utilization_percent: 7 },
            telemetryValid: false,
            msSince: 0
        }
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: [{ hostUuid, discoveryRecordId, identityVerification: 'local', info }]
        })
        vi.mocked(emitBridgePush).mockClear()
        vi.mocked(Date.now).mockReturnValue(102_000)

        state.handleNotification({
            source: 'broker',
            method: 'discovery:node-info',
            params: { hostUuid, discoveryRecordId, identityVerification: 'local', info }
        })

        expect(emitBridgePush).toHaveBeenCalledWith(
            'metrics:update',
            expect.objectContaining({
                id: hostUuid,
                current: expect.objectContaining({ timestamp: 102_000, cpuUtilization: 7 })
            })
        )
    })

    it('clears broker hardware readiness and sample time when only proxy presence remains', () => {
        vi.spyOn(Date, 'now').mockReturnValue(100_000)
        const state = getModularBridgeState()
        const hostUuid = 'proxy-withdrawn-hardware'
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: [
                {
                    hostUuid,
                    discoveryRecordId: 'local:proxy-withdrawn-hardware',
                    identityVerification: 'local',
                    name: 'Local host',
                    ipAddress: '192.0.2.33',
                    port: 14318,
                    info: {
                        hostUuid,
                        GPUs: [{ name: 'Owned GPU' }],
                        inference_hardware_ids: ['gpu:owned'],
                        telemetryValid: true,
                        msSince: 7000
                    }
                }
            ]
        })
        state.handleNotification({
            source: 'ollama-proxy',
            method: 'node/discovered',
            params: { id: hostUuid, host: 'Local host', port: 11434, addresses: ['192.0.2.33'] }
        })
        expect(state.getNodesInitial().nodes[hostUuid].topology.inferenceHardwareIds).toEqual([
            'gpu:owned'
        ])
        vi.mocked(emitBridgePush).mockClear()
        vi.mocked(Date.now).mockReturnValue(105_000)

        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: []
        })

        const remaining = state.getNodesInitial().nodes[hostUuid]
        expect(remaining).toBeDefined()

        // Proxy presence keeps the row, but cannot preserve hardware readiness
        // or measurements withdrawn by the authoritative discovery record.
        expect(remaining.topology.gpus).toEqual([])
        expect(remaining.topology.inferenceHardwareIds).toBeUndefined()

        expect(emitBridgePush).toHaveBeenCalledWith(
            'metrics:update',
            expect.objectContaining({
                id: hostUuid,
                current: expect.objectContaining({ timestamp: 105_000 })
            })
        )
    })
})
