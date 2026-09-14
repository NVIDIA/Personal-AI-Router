// SPDX-FileCopyrightText: Copyright (c) 2026 Denis Akimov
// SPDX-License-Identifier: Apache-2.0

import { beforeEach, describe, expect, it, vi } from 'vitest'
import { inviteLabel } from '@/ui/utils/invite-label'
import { useClusterInvitationsStore } from '@/ui/stores/cluster-invitations.store'
import type { Invite } from '@/shared/types/cluster'

function invite(inviteId: string, partial: Partial<Invite> = {}): Invite {
    return {
        inviteId,
        fromNodeId: 'sender.local',
        fromNodeUuid: 'd9634062-7199-486d-8b8f-c515dbe358b2',
        fromNodeName: 'sender.local',
        toNodeId: null,
        clusterId: 'cluster-1',
        clusterFriendlyName: 'sender.local',
        pin: null,
        state: 'pending',
        reason: '',
        ...partial
    } as Invite
}

/**
 * The label is what lets a person holding a PIN see that the screen they read it
 * from and the screen they type it into mean the same invitation. Two invites
 * that differ must not share one.
 */
describe('inviteLabel', () => {
    it('distinguishes the two invites from the reported pairing failure', () => {
        expect(inviteLabel('inv-a16f42cd0055')).not.toBe(inviteLabel('inv-9b52fa024225'))
    })

    it('is stable for the same invite id', () => {
        expect(inviteLabel('inv-a16f42cd0055')).toBe(inviteLabel('inv-a16f42cd0055'))
    })

    it('renders a grouped, uppercase, fixed-width label', () => {
        expect(inviteLabel('inv-a16f42cd0055')).toBe('CD0-055')
        expect(inviteLabel('inv-9b52fa024225')).toBe('024-225')
    })

    it('degrades instead of throwing on a missing id', () => {
        expect(inviteLabel(null)).toBe('------')
        expect(inviteLabel('')).toBe('------')
    })
})

/**
 * A second invitation must never repoint an approval modal the user is already
 * answering.
 *
 * Each invitation carries its own PIN, and an inviter that cancels and retries
 * mints a fresh one, so rebinding mid-flow means the PIN being read off the
 * other machine belongs to a session that is no longer the one about to consume
 * it. The completion exchange then fails as an EAP-NOOB Noob mismatch, which the
 * backend correctly reports as `incorrect-pin` — leaving the user hunting for a
 * typo that never happened.
 */
describe('cluster invitations store: arriving invite does not steal the modal', () => {
    let onInviteReceived: (invite: Invite) => void

    beforeEach(async () => {
        onInviteReceived = () => {}
        const pairApi = {
            cluster: {
                getInitial: vi.fn(async () => ({ pendingInvites: [], members: [] })),
                onInviteReceived: (cb: (invite: Invite) => void) => {
                    onInviteReceived = cb
                    return () => {}
                },
                onPendingInvitesChanged: () => () => {},
                respondToInvite: vi.fn()
            },
            nodes: { onMembersChanged: () => () => {} }
        }
        ;(globalThis as unknown as { window: unknown }).window = { pairApi }
        useClusterInvitationsStore.setState({
            pendingInvites: [],
            members: [],
            activeInviteId: null
        })
        await useClusterInvitationsStore
            .getState()
            .initialize({ pendingInvites: [], members: [] } as never)
    })

    it('surfaces the first invite when nothing is being answered', () => {
        onInviteReceived(invite('inv-a16f42cd0055'))
        expect(useClusterInvitationsStore.getState().activeInviteId).toBe('inv-a16f42cd0055')
    })

    it('leaves the open invite bound when a second one arrives', () => {
        onInviteReceived(invite('inv-a16f42cd0055'))
        onInviteReceived(invite('inv-9b52fa024225'))
        expect(useClusterInvitationsStore.getState().activeInviteId).toBe('inv-a16f42cd0055')
    })

    it('still ignores non-pending arrivals', () => {
        onInviteReceived(invite('inv-a16f42cd0055', { state: 'failed', reason: 'incorrect-pin' }))
        expect(useClusterInvitationsStore.getState().activeInviteId).toBeNull()
    })

    it('lets the user switch deliberately', () => {
        onInviteReceived(invite('inv-a16f42cd0055'))
        onInviteReceived(invite('inv-9b52fa024225'))
        useClusterInvitationsStore.getState().setActiveInvite('inv-9b52fa024225')
        expect(useClusterInvitationsStore.getState().activeInviteId).toBe('inv-9b52fa024225')
    })

    it('accepts a new arrival again once the user closes the modal', () => {
        onInviteReceived(invite('inv-a16f42cd0055'))
        useClusterInvitationsStore.getState().clearActiveInvite()
        onInviteReceived(invite('inv-9b52fa024225'))
        expect(useClusterInvitationsStore.getState().activeInviteId).toBe('inv-9b52fa024225')
    })
})
