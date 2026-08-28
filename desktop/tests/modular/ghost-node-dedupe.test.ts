// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({ BrowserWindow: { getAllWindows: () => [] } }))
vi.mock('@/electron/window', () => ({ createOverviewWindow: vi.fn() }))

import { getModularBridgeState } from '@/electron/service-bridge/modular-state'
import { MODULAR_SUPERSEDE_MIN_AGE_MS } from '@/shared/constants/modular-runtime'

const NOW = Date.now()
const STALE = NOW - 60 * 60 * 1000
const RECENT = NOW - (MODULAR_SUPERSEDE_MIN_AGE_MS - 60_000)

interface BrokerNodeFixture {
    hostUuid: string
    name: string
    ipAddress: string
    lastSeen: number
}

function brokerSnapshot(nodes: BrokerNodeFixture[]): void {
    getModularBridgeState().handleNotification({
        source: 'broker',
        method: 'discovery:nodes-changed',
        params: {
            nodes: nodes.map(node => ({
                hostUuid: node.hostUuid,
                name: node.name,
                ipAddress: node.ipAddress,
                port: 14318,
                lastSeen: node.lastSeen,
                trusted: false,
                clustered: false
            }))
        }
    })
}

function availableIds(): string[] {
    return getModularBridgeState()
        .getAvailableNodes()
        .map(node => node.id)
}

// A node whose appdata is wiped mints a fresh hostUuid, so the backend directory
// can carry BOTH the pre-wipe key and the new one for the same machine. The
// bridge collapses that pair on the three conditions the node-scanner uses to
// NOMINATE a superseded record (directory.supersedeCandidates) — same address,
// same hostname, and a lastSeen gap of at least MODULAR_SUPERSEDE_MIN_AGE_MS.
//
// The scanner goes on to confirm the nomination against node-info before it
// evicts anything; the bridge cannot, so what these cases pin down is a
// display-side collapse of a duplicate row, not an eviction from the directory.
describe('ghost node dedupe by address', () => {
    it('drops the pre-wipe record when the same address and hostname reappear under a new UUID', () => {
        brokerSnapshot([
            {
                hostUuid: 'ghost-old-1',
                name: 'wiped-host-1',
                ipAddress: '198.51.100.10',
                lastSeen: STALE
            },
            {
                hostUuid: 'ghost-new-1',
                name: 'wiped-host-1',
                ipAddress: '198.51.100.10',
                lastSeen: NOW
            }
        ])

        const ids = availableIds()
        expect(ids).toContain('ghost-new-1')
        expect(ids).not.toContain('ghost-old-1')
        expect(getModularBridgeState().getNodesInitial().nodes['ghost-old-1']).toBeUndefined()
    })

    it('drops the pre-wipe record when it was already in the list first', () => {
        brokerSnapshot([
            {
                hostUuid: 'ghost-old-2',
                name: 'wiped-host-2',
                ipAddress: '198.51.100.11',
                lastSeen: STALE
            }
        ])
        expect(availableIds()).toContain('ghost-old-2')

        brokerSnapshot([
            {
                hostUuid: 'ghost-old-2',
                name: 'wiped-host-2',
                ipAddress: '198.51.100.11',
                lastSeen: STALE
            },
            {
                hostUuid: 'ghost-new-2',
                name: 'wiped-host-2',
                ipAddress: '198.51.100.11',
                lastSeen: NOW
            }
        ])

        const ids = availableIds()
        expect(ids).toContain('ghost-new-2')
        expect(ids).not.toContain('ghost-old-2')
    })

    it('keeps the ghost suppressed across repeated snapshots that still carry it', () => {
        const pair: BrokerNodeFixture[] = [
            {
                hostUuid: 'ghost-old-3',
                name: 'wiped-host-3',
                ipAddress: '198.51.100.12',
                lastSeen: STALE
            },
            {
                hostUuid: 'ghost-new-3',
                name: 'wiped-host-3',
                ipAddress: '198.51.100.12',
                lastSeen: NOW
            }
        ]
        // A backend that has not evicted the ghost yet keeps publishing it, so
        // eviction only holds if the stale side is refused on every arrival, not
        // just evicted once.
        for (let i = 0; i < 3; i++) brokerSnapshot(pair)

        const ids = availableIds()
        expect(ids.filter(id => id.startsWith('ghost-'))).toEqual(['ghost-new-3'])
    })

    it('collapses the pair regardless of order within a snapshot', () => {
        brokerSnapshot([
            {
                hostUuid: 'ghost-new-4',
                name: 'wiped-host-4',
                ipAddress: '198.51.100.13',
                lastSeen: NOW
            },
            {
                hostUuid: 'ghost-old-4',
                name: 'wiped-host-4',
                ipAddress: '198.51.100.13',
                lastSeen: STALE
            }
        ])

        const ids = availableIds()
        expect(ids).toContain('ghost-new-4')
        expect(ids).not.toContain('ghost-old-4')
    })

    it('never collapses records whose hostnames differ, even on one address', () => {
        // The hostname match is what makes this inference safe. A wipe clears
        // identity but not the hostname; two records that disagree on the name
        // are left for the scanner's identity probe to settle with proof.
        brokerSnapshot([
            {
                hostUuid: 'renamed-old-5',
                name: 'old-name-5',
                ipAddress: '198.51.100.14',
                lastSeen: STALE
            },
            {
                hostUuid: 'renamed-new-5',
                name: 'renamed-5',
                ipAddress: '198.51.100.14',
                lastSeen: NOW
            }
        ])

        const ids = availableIds()
        expect(ids).toContain('renamed-new-5')
        expect(ids).toContain('renamed-old-5')
    })

    it('never collapses a pair whose lastSeen gap is under the minimum age', () => {
        brokerSnapshot([
            {
                hostUuid: 'recent-old-6',
                name: 'shared-host-6',
                ipAddress: '198.51.100.15',
                lastSeen: RECENT
            },
            {
                hostUuid: 'recent-new-6',
                name: 'shared-host-6',
                ipAddress: '198.51.100.15',
                lastSeen: NOW
            }
        ])

        const ids = availableIds()
        expect(ids).toContain('recent-new-6')
        expect(ids).toContain('recent-old-6')
    })

    it('never collapses two distinct machines on different addresses', () => {
        brokerSnapshot([
            { hostUuid: 'distinct-a', name: 'host-a', ipAddress: '198.51.100.20', lastSeen: STALE },
            { hostUuid: 'distinct-b', name: 'host-b', ipAddress: '198.51.100.21', lastSeen: NOW }
        ])

        const ids = availableIds()
        expect(ids).toContain('distinct-a')
        expect(ids).toContain('distinct-b')
    })

    it('never evicts the local node, and never hides a peer to protect it', () => {
        const state = getModularBridgeState()
        state.setSelfId('self-uuid-7')
        brokerSnapshot([
            {
                hostUuid: 'self-uuid-7',
                name: 'self-host-7',
                ipAddress: '198.51.100.30',
                lastSeen: STALE
            }
        ])
        expect(availableIds()).toContain('self-uuid-7')

        // Self is pinned by the cluster-manager, so a peer claiming its address
        // and hostname with a fresher timestamp must not displace it. The peer
        // still shows: the backend keeps it in the directory too, and silently
        // hiding it here is exactly the disagreement this rule exists to avoid.
        brokerSnapshot([
            {
                hostUuid: 'self-uuid-7',
                name: 'self-host-7',
                ipAddress: '198.51.100.30',
                lastSeen: STALE
            },
            {
                hostUuid: 'peer-7',
                name: 'self-host-7',
                ipAddress: '198.51.100.30',
                lastSeen: NOW
            }
        ])

        const ids = availableIds()
        expect(ids).toContain('self-uuid-7')
        expect(ids).toContain('peer-7')
    })

    it('applies a multi-way collapse completely or not at all', () => {
        // Three records on one address and hostname: the stale side must not
        // strand entries it had already marked for eviction.
        brokerSnapshot([
            {
                hostUuid: 'multi-old-a',
                name: 'shared-host-8',
                ipAddress: '198.51.100.40',
                lastSeen: STALE
            },
            {
                hostUuid: 'multi-old-b',
                name: 'shared-host-8',
                ipAddress: '198.51.100.40',
                lastSeen: STALE
            },
            {
                hostUuid: 'multi-new',
                name: 'shared-host-8',
                ipAddress: '198.51.100.40',
                lastSeen: NOW
            }
        ])

        const ids = availableIds().filter(id => id.startsWith('multi-'))
        expect(ids).toEqual(['multi-new'])
    })
})
