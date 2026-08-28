// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import {
    getModularBridgeState,
    parseWorkloadsInitial
} from '@/electron/service-bridge/modular-state'

// The bridge keys every node by the backend's stable per-host UUID; the hostname
// is display only. These tests lock in the two behaviours that migration exists
// for: a broker view and a proxy view of the same machine collapse to one
// UUID-keyed entry (no hostname ghost), and cross-domain attribution refuses a
// telemetry response whose reported UUID does not match the polled node.
describe('UUID node keying', () => {
    it('merges a broker node and a same-hostUuid proxy node into one UUID-keyed entry', () => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: 'uuid-merge-1',
                        name: 'merge-host-1',
                        ipAddress: '192.0.2.11',
                        port: 14318,
                        trusted: true,
                        clustered: true
                    }
                ]
            }
        })
        state.handleNotification({
            source: 'proxy',
            method: 'node/discovered',
            params: {
                id: 'uuid-merge-1',
                host: 'merge-host-1',
                port: 11434,
                addresses: ['192.0.2.11'],
                ip: '192.0.2.11'
            }
        })

        const { nodes } = state.getNodesInitial()
        expect(nodes['uuid-merge-1']).toBeDefined()
        // No hostname-keyed ghost — that double-entry is exactly what UUID keying fixes.
        expect(nodes['merge-host-1']).toBeUndefined()
        expect(nodes['uuid-merge-1'].name).toBe('merge-host-1')

        // Broker trust/cluster flags survive the proxy merge (the proxy feed has none).
        const available = state.getAvailableNodes().find(node => node.id === 'uuid-merge-1')
        expect(available).toMatchObject({ trusted: true, clustered: true })
        // Clustered peers need authoritative facts before run state is inferred, even
        // after the proxy presence is merged onto the UUID-keyed entry.
        expect(state.isRemoteEngineRunning('uuid-merge-1', 'ollama')).toBe(false)
    })

    it('merges regardless of source order (proxy before broker)', () => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'lmstudio-proxy',
            method: 'node/discovered',
            params: {
                id: 'uuid-merge-2',
                host: 'merge-host-2',
                port: 1234,
                addresses: ['192.0.2.12'],
                ip: '192.0.2.12'
            }
        })
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: 'uuid-merge-2',
                        name: 'merge-host-2',
                        ipAddress: '192.0.2.12',
                        port: 14318,
                        trusted: false,
                        clustered: true
                    }
                ]
            }
        })

        const { nodes } = state.getNodesInitial()
        expect(nodes['uuid-merge-2']).toBeDefined()
        expect(nodes['merge-host-2']).toBeUndefined()
        expect(nodes['uuid-merge-2'].name).toBe('merge-host-2')
        const available = state.getAvailableNodes().find(node => node.id === 'uuid-merge-2')
        expect(available).toMatchObject({ clustered: true })
        expect(state.isRemoteEngineRunning('uuid-merge-2', 'lm-studio')).toBe(false)
    })

    it('skips a /v1/node-info response whose hostUuid disagrees with the polled node', () => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: 'uuid-ni-1',
                        name: 'ni-host-1',
                        ipAddress: '192.0.2.50',
                        port: 14318
                    }
                ]
            }
        })

        // The polled address resolved to a different machine: its telemetry must
        // not be attributed to this node.
        state.mergeNodeInfoResponse('uuid-ni-1', {
            hostUuid: 'uuid-someone-else',
            GPUs: [
                { name: 'NVIDIA A', vram_bytes: 1000, vram_used_bytes: 10, utilization_percent: 5 }
            ],
            cpu: { name: 'CPU-A', cores: 8, utilization_percent: 3 }
        })
        expect(state.getNodesInitial().nodes['uuid-ni-1'].topology.gpus).toHaveLength(0)
        expect(state.getNodesInitial().nodes['uuid-ni-1'].topology.cpu.model).toBe('')

        // A matching hostUuid is attributed normally.
        state.mergeNodeInfoResponse('uuid-ni-1', {
            hostUuid: 'uuid-ni-1',
            GPUs: [
                { name: 'NVIDIA A', vram_bytes: 1000, vram_used_bytes: 10, utilization_percent: 5 }
            ],
            cpu: { name: 'CPU-A', cores: 8, utilization_percent: 3 }
        })
        expect(state.getNodesInitial().nodes['uuid-ni-1'].topology.gpus).toHaveLength(1)
        expect(state.getNodesInitial().nodes['uuid-ni-1'].topology.cpu.model).toBe('CPU-A')
    })

    it('gives the node-info poller every published address in ranked order', () => {
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: 'uuid-ni-failover',
                        name: 'ni-failover-host',
                        ipAddress: '192.0.2.80',
                        ipAddresses: ['192.0.2.80', '10.20.30.40'],
                        port: 14318
                    }
                ]
            }
        })

        expect(
            state.getNodeInfoPollTargets().find(target => target.id === 'uuid-ni-failover')
        ).toEqual({
            id: 'uuid-ni-failover',
            hosts: ['192.0.2.80', '10.20.30.40'],
            port: 14318
        })
    })

    it('seedWorkloads keeps a live upsert and only fills in unseen baseline jobs', () => {
        const state = getModularBridgeState()
        // A live push landed first (the realtime stream is at least as fresh as any
        // durable snapshot).
        state.upsertWorkloadFromInfo({
            workloadInfo: {
                id: 'job-live',
                engine: 'ollama',
                state: 'running',
                model: 'live-model',
                originatedFrom: 'uuid-wl-seed',
                createdAt: 500
            }
        })

        const seeded = state.seedWorkloads(
            parseWorkloadsInitial({
                workloads: [
                    {
                        id: 'job-live',
                        engine: 'ollama',
                        state: 'completed',
                        model: 'stale-model',
                        originatedFrom: 'uuid-wl-seed',
                        createdAt: 100
                    },
                    {
                        id: 'job-new',
                        engine: 'ollama',
                        state: 'queued',
                        model: 'new-model',
                        originatedFrom: 'uuid-wl-seed',
                        createdAt: 200
                    }
                ]
            })
        )

        const liveKey = 'uuid-wl-seed\u0000job-live'
        const newKey = 'uuid-wl-seed\u0000job-new'
        // The live entry is preserved, not clobbered by the older baseline row.
        expect(seeded[liveKey]).toMatchObject({ state: 'running', model: 'live-model' })
        // A baseline job the stream had not delivered is filled in.
        expect(seeded[newKey]).toMatchObject({ state: 'queued', model: 'new-model' })
    })
})
