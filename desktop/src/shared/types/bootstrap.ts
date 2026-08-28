// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ClusterInfo, ClusterNode, ClusterNodeIdentity, Invite } from '@/shared/types/cluster'

export interface AppInitialSnapshot {
    connected: boolean
    selfId: string | null
}

export interface ClusterInitialSnapshot {
    info: ClusterInfo
    identity: ClusterNodeIdentity
    members: ClusterNode[]
    /**
     * All live inbound invites awaiting the local user's PIN entry. Accumulated
     * and pruned by Electron main (the authoritative source) and kept in sync via
     * the `cluster:pending-invites-changed` push; a fresh arrival is also
     * signalled by `cluster:invite-received`.
     */
    pendingInvites: Invite[]
}
