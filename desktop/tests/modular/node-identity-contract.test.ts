// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'
import type { NodeIdentityVerification } from '@/shared/types/cluster'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

describe('node identity contract', () => {
    // Shared names, addresses, and different observation ages cannot merge
    // distinct verified hosts, even when the same snapshot is received again.
    it('preserves distinct records with the same name and address across repeated snapshots', () => {
        const oldHostUuid = 'old-peer'
        const newHostUuid = 'new-peer'
        const sharedName = 'shared-name'
        const sharedAddress = '192.0.2.20'
        const state = getModularBridgeState()
        const params = [
            {
                hostUuid: oldHostUuid,
                discoveryRecordId: 'peer:old-peer',
                identityVerification: 'pinned',
                name: sharedName,
                ipAddress: sharedAddress,
                lastSeen: 1
            },
            {
                hostUuid: newHostUuid,
                discoveryRecordId: 'peer:new-peer',
                identityVerification: 'pinned',
                name: sharedName,
                ipAddress: sharedAddress,
                lastSeen: 1790000000
            }
        ]
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params
        })
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params
        })

        expect(state.getAvailableNodes().map(node => node.id)).toEqual([oldHostUuid, newHostUuid])
    })

    // Seed accepted hardware, then vary one identity field at a time. Each
    // rejection must protect the accepted data without relying on another guard.
    describe('broker hardware snapshots', () => {
        const hostUuid = 'snapshot-peer'
        const discoveryRecordId = 'peer:snapshot-peer'
        const acceptedGpuName = 'Accepted GPU'
        const updatedGpuName = 'Updated GPU'
        const test = (
            name: string,
            snapshotRecordId: string,
            snapshotVerification: NodeIdentityVerification,
            expectedGpuName: string
        ): void => {
            it(name, () => {
                const state = getModularBridgeState()
                state.handleNotification({
                    source: 'broker',
                    method: 'discovery:nodes-changed',
                    params: [
                        {
                            hostUuid,
                            discoveryRecordId,
                            identityVerification: 'pinned',
                            name: 'verified',
                            ipAddress: '192.0.2.20',
                            port: 14318,
                            trusted: true
                        }
                    ]
                })
                state.handleNotification({
                    source: 'broker',
                    method: 'discovery:node-info',
                    params: {
                        hostUuid,
                        discoveryRecordId,
                        identityVerification: 'pinned',
                        info: {
                            hostUuid,
                            GPUs: [{ name: acceptedGpuName }]
                        }
                    }
                })
                expect(
                    state.getNodesInitial().nodes[hostUuid].topology.gpus.map(gpu => gpu.name)
                ).toEqual([acceptedGpuName])

                state.handleNotification({
                    source: 'broker',
                    method: 'discovery:node-info',
                    params: {
                        hostUuid,
                        discoveryRecordId: snapshotRecordId,
                        identityVerification: snapshotVerification,
                        info: {
                            hostUuid,
                            GPUs: [{ name: updatedGpuName }]
                        }
                    }
                })

                expect(
                    state.getNodesInitial().nodes[hostUuid].topology.gpus.map(gpu => gpu.name)
                ).toEqual([expectedGpuName])
            })
        }

        test(
            'updates hardware for the matching verified record',
            discoveryRecordId,
            'pinned',
            updatedGpuName
        )
        test(
            'rejects hardware from a different discovery record',
            'peer:another-record',
            'pinned',
            acceptedGpuName
        )
        test(
            'rejects hardware with different identity verification',
            discoveryRecordId,
            'unverified',
            acceptedGpuName
        )
    })

    // Render a verified peer and a candidate copying its identity. Public
    // hardware may populate the candidate's preview without merging the rows
    // or accepting the response's forged name and verification claims.
    it('keeps an unverified UUID claim separate from a verified peer and permits its hardware preview', () => {
        const hostUuid = 'peer-a'
        const candidateRecordId = 'candidate:rogue'
        const peerName = 'peer'
        const peerAddress = '192.0.2.20'
        const nodeInfoPort = 14318
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: [
                {
                    hostUuid,
                    discoveryRecordId: 'peer:peer-a',
                    identityVerification: 'pinned',
                    name: peerName,
                    ipAddress: peerAddress,
                    port: nodeInfoPort,
                    trusted: true
                },
                {
                    hostUuid,
                    discoveryRecordId: candidateRecordId,
                    identityVerification: 'unverified',
                    name: peerName,
                    ipAddress: '192.0.2.30',
                    port: nodeInfoPort
                }
            ]
        })

        state.mergeNodeInfoResponse(candidateRecordId, {
            hostUuid: 'different-public-claim',
            GPUs: [{ name: 'Display GPU' }],
            peer: { name: 'forged-name' },
            identityVerification: 'pinned'
        })

        expect(state.getAvailableNodes().map(node => node.id)).toEqual(
            expect.arrayContaining([hostUuid, candidateRecordId])
        )
        expect(state.getAvailableNodes().find(node => node.id === hostUuid)).toMatchObject({
            ipAddress: peerAddress,
            trusted: true
        })
        expect(state.getAvailableNodes().find(node => node.id === candidateRecordId)).toMatchObject(
            { identityVerification: 'unverified', name: peerName }
        )
        expect(state.getNodesInitial().nodes[candidateRecordId].topology.gpus).toHaveLength(1)

        state.mergeNodeInfoResponse(candidateRecordId, {
            GPUs: [{ name: 'UUID-less preview GPU' }]
        })

        expect(
            state.getNodesInitial().nodes[candidateRecordId].topology.gpus.map(gpu => gpu.name)
        ).toEqual(['UUID-less preview GPU'])
        expect(state.getAvailableNodes().find(node => node.id === candidateRecordId)).toMatchObject(
            { identityVerification: 'unverified', name: peerName }
        )
    })
    // Let discovery and a proxy both report one host, then withdraw discovery.
    // Cached proxy presence cannot retain a withdrawn verified peer row.
    it('removes a withdrawn verified peer despite cached proxy presence', () => {
        const hostUuid = 'identity-withdrawn-host'
        const hostName = 'workstation'
        const address = '192.0.2.21'
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: [
                {
                    hostUuid,
                    name: hostName,
                    ipAddress: address,
                    port: 14318,
                    discoveryRecordId: 'peer:identity-withdrawn-host',
                    identityVerification: 'pinned'
                }
            ]
        })
        state.handleNotification({
            source: 'ollama-proxy',
            method: 'node/discovered',
            params: {
                id: hostUuid,
                host: hostName,
                port: 11434,
                addresses: [address]
            }
        })
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: []
        })

        const node = state.getAvailableNodes().find(node => node.id === hostUuid)
        expect(node).toBeUndefined()

        // A proxy can still have an older discovery notification queued when
        // the broker withdraws the row. It cannot restore that former peer.
        state.handleNotification({
            source: 'ollama-proxy',
            method: 'node/discovered',
            params: { id: hostUuid, host: hostName, port: 11434, addresses: [address] }
        })
        expect(state.getAvailableNodes().find(node => node.id === hostUuid)).toBeUndefined()

        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: [
                {
                    hostUuid,
                    name: hostName,
                    ipAddress: address,
                    port: 14318,
                    discoveryRecordId: 'new-proof',
                    identityVerification: 'pinned'
                }
            ]
        })
        expect(state.getAvailableNodes().find(node => node.id === hostUuid)).toMatchObject({
            discoveryRecordId: 'new-proof',
            identityVerification: 'pinned'
        })
    })

    // A proxy's host UUID and an unverified discovery record identify separate
    // rows. Public hardware may fill the candidate without changing its identity.
    it('keeps a candidate separate from proxy presence and ignores public identity claims', () => {
        const hostUuid = 'identity-contract-host'
        const discoveryRecordId = 'candidate:identity-contract'
        const hostName = 'workstation'
        const address = '192.0.2.20'
        const state = getModularBridgeState()
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: [
                {
                    hostUuid,
                    name: hostName,
                    ipAddress: address,
                    port: 14318,
                    discoveryRecordId,
                    identityVerification: 'unverified'
                }
            ]
        })
        state.handleNotification({
            source: 'ollama-proxy',
            method: 'node/discovered',
            params: {
                id: hostUuid,
                host: hostName,
                port: 11434,
                addresses: [address]
            }
        })
        expect(state.getAvailableNodes().map(node => node.id)).toEqual(
            expect.arrayContaining([hostUuid, discoveryRecordId])
        )
        state.mergeNodeInfoResponse(discoveryRecordId, {
            hostUuid,
            discoveryRecordId: 'peer:identity-contract-host',
            identityVerification: 'pinned',
            GPUs: [{ name: 'Display GPU' }]
        })

        expect(state.getAvailableNodes().find(node => node.id === discoveryRecordId)).toMatchObject(
            {
                discoveryRecordId,
                identityVerification: 'unverified'
            }
        )
        expect(state.getNodesInitial().nodes[discoveryRecordId].topology.gpus).toHaveLength(1)
    })
})
