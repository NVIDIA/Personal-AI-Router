// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo } from 'react'
import { useShallow } from 'zustand/react/shallow'
import type { AvailableNode } from '@/shared/types/cluster'
import { useDiscoveredNodesStore } from '@/ui/stores/discovered-nodes.store'
import { useClusterInvitationsStore } from '@/ui/stores/cluster-invitations.store'
import { useConnectionStore } from '@/ui/stores/connection.store'

/**
 * Discovered LAN peers that can be invited: everything the broker has discovered
 * minus current cluster members minus self.
 *
 * This is the single source for both the Add Node modal and the cluster settings
 * "Available Nodes to Add" list, so the two can never disagree (the modal used
 * to filter against the overview node store, which is membership-scoped, leaving
 * it perpetually empty).
 */
export function useInvitablePeers(): AvailableNode[] {
    const discoveredNodes = useDiscoveredNodesStore(useShallow(state => state.nodes))
    const members = useClusterInvitationsStore(useShallow(state => state.members))
    const selfId = useConnectionStore(state => state.selfId)

    return useMemo(() => {
        // Verified host rows match members and self by UUID. Unverified rows
        // have separate discovery record IDs, so a copied UUID claim cannot
        // make one disappear as a member or self. Hostnames are display only.
        const memberUuids = new Set(members.map(m => m.nodeUuid))
        return discoveredNodes.filter(node => !memberUuids.has(node.id) && node.id !== selfId)
    }, [discoveredNodes, members, selfId])
}
