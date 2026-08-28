// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'
import { MODULAR_NODE_INFO_SELF_HOST } from '@/shared/constants/modular-runtime'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'

// This machine appears in its own discovery snapshot, so it used to be polled the
// way a peer is: over the LAN address it advertises. That address reaches the same
// listener by a route that leaves and re-enters the host, which made the node's own
// CPU and GPU readings depend on its own inbound path — a local firewall or a link
// fault showed this node's card on stale metrics while loopback would have answered
// in a millisecond — and spent an inbound LAN connection every two seconds to learn
// what loopback already knew.
describe('node-info self poll', () => {
    it('asks itself over loopback while its peers stay on the LAN', () => {
        const state = getModularBridgeState()
        state.setSelfId('uuid-self')
        state.handleNotification({
            source: 'broker',
            method: 'discovery:nodes-changed',
            params: {
                nodes: [
                    {
                        hostUuid: 'uuid-self',
                        name: 'self-host',
                        ipAddress: '192.0.2.51',
                        ipAddresses: ['192.0.2.51', '192.0.2.52'],
                        port: 14318
                    },
                    {
                        hostUuid: 'uuid-peer',
                        name: 'peer-host',
                        ipAddress: '192.0.2.61',
                        ipAddresses: ['192.0.2.61', '192.0.2.62'],
                        port: 14318
                    }
                ]
            }
        })

        const targets = state.getNodeInfoPollTargets()

        // Loopback alone. Its advertised addresses are not failover candidates:
        // they are longer routes to the listener loopback just reached, so asking
        // them after loopback fails only spends connections.
        expect(targets.find(target => target.id === 'uuid-self')?.hosts).toEqual([
            MODULAR_NODE_INFO_SELF_HOST
        ])

        // A peer is only reachable over the network, so its published list is
        // still walked in full.
        expect(targets.find(target => target.id === 'uuid-peer')?.hosts).toEqual([
            '192.0.2.61',
            '192.0.2.62'
        ])
    })
})
