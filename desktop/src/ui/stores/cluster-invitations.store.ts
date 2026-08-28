// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { create } from 'zustand'
import type { ClusterInitialSnapshot } from '@/shared/types/bootstrap'
import type { ClusterNode, Invite } from '@/shared/types/cluster'

interface ClusterInvitationsStore {
    /**
     * Every live inbound invite awaiting the local user's PIN entry. Electron
     * main is authoritative: this list is hydrated from `cluster:get-initial` and
     * replaced on every `cluster:pending-invites-changed` snapshot.
     */
    pendingInvites: Invite[]
    /** The invite currently surfaced in the approval modal, if any. */
    activeInviteId: string | null
    /** Full cluster membership (members + pending), driven by `nodes:changed`. */
    members: ClusterNode[]
    initialize: (initial?: ClusterInitialSnapshot) => Promise<void>
    hydrate: (snapshot: ClusterInitialSnapshot) => void
    refresh: () => Promise<void>
    cleanup: () => void
    /** Surface a specific pending invite in the approval modal (e.g. from the card). */
    setActiveInvite: (inviteId: string) => void
    /** Close the approval modal without responding. */
    clearActiveInvite: () => void
    /** Accept (with PIN) or decline a specific inbound invite. */
    respondToInvite: (inviteId: string, accept: boolean, pin?: string) => Promise<Invite>
}

let unsubs: Array<() => void> = []

/** Drop `activeInviteId` if the invite it points at is no longer pending. */
function stillActive(activeInviteId: string | null, invites: Invite[]): string | null {
    if (!activeInviteId) return null
    return invites.some(invite => invite.inviteId === activeInviteId) ? activeInviteId : null
}

export const useClusterInvitationsStore = create<ClusterInvitationsStore>((set, get) => ({
    pendingInvites: [],
    activeInviteId: null,
    members: [],

    hydrate: snapshot => {
        set(state => ({
            pendingInvites: snapshot.pendingInvites,
            members: snapshot.members,
            activeInviteId: stillActive(state.activeInviteId, snapshot.pendingInvites)
        }))
    },

    initialize: async initial => {
        if (initial) {
            get().hydrate(initial)
        } else {
            await get().refresh()
        }

        if (!window.pairApi) return
        unsubs.push(
            // A fresh arrival: surface it in the modal. Main also emits the full
            // list via `cluster:pending-invites-changed`, so this only picks which
            // invite the modal shows.
            window.pairApi.cluster.onInviteReceived(invite => {
                if (invite.state === 'pending') set({ activeInviteId: invite.inviteId })
            }),
            window.pairApi.cluster.onPendingInvitesChanged(invites => {
                set(state => ({
                    pendingInvites: invites,
                    activeInviteId: stillActive(state.activeInviteId, invites)
                }))
            }),
            window.pairApi.nodes.onMembersChanged(members => {
                set({ members })
            })
        )
    },

    refresh: async () => {
        try {
            const snapshot = await window.pairApi.cluster.getInitial()
            set(state => ({
                pendingInvites: snapshot.pendingInvites,
                members: snapshot.members,
                activeInviteId: stillActive(state.activeInviteId, snapshot.pendingInvites)
            }))
        } catch {
            set({ pendingInvites: [], members: [], activeInviteId: null })
        }
    },

    cleanup: () => {
        unsubs.forEach(u => u())
        unsubs = []
        set({ pendingInvites: [], members: [], activeInviteId: null })
    },

    setActiveInvite: inviteId => {
        set({ activeInviteId: inviteId })
    },

    clearActiveInvite: () => {
        set({ activeInviteId: null })
    },

    respondToInvite: async (inviteId, accept, pin) => {
        // Main prunes the invite from the authoritative set on any terminal
        // outcome and re-emits `cluster:pending-invites-changed`, so the local
        // list updates itself — no optimistic mutation needed here.
        return window.pairApi.cluster.respondToInvite(inviteId, accept, pin)
    }
}))
