// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { describe, expect, it } from 'vitest'
import { parseInvite } from '@/electron/service-bridge/cluster-json'

// `parseInvite` coerces the cluster-manager's invite payload into the shared
// `Invite` shape. This sync added the terminal `canceled`/`rejected` states and
// the machine-readable `reason`, which the outbound-invite UI branches on.
describe('parseInvite terminal states', () => {
    it('round-trips a rejected invite with its reason', () => {
        const invite = parseInvite({
            inviteId: 'inv-1',
            fromNodeId: 'host-a',
            fromNodeUuid: 'uuid-a',
            fromNodeName: 'Host A',
            toNodeId: null,
            clusterId: 'cluster-1',
            clusterFriendlyName: 'Home',
            pin: null,
            state: 'rejected',
            reason: 'already-clustered',
            createdAt: 10,
            respondedAt: 20
        })
        expect(invite.state).toBe('rejected')
        expect(invite.reason).toBe('already-clustered')
    })

    it('recognizes the canceled state', () => {
        expect(parseInvite({ inviteId: 'inv-2', state: 'canceled' }).state).toBe('canceled')
    })

    it('defaults an unknown state to failed and an absent reason to empty', () => {
        const invite = parseInvite({ inviteId: 'inv-3', state: 'not-a-real-state' })
        expect(invite.state).toBe('failed')
        expect(invite.reason).toBe('')
    })
})
