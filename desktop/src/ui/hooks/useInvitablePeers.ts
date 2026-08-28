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
        // Correlate on the node UUID: `AvailableNode.id` = `ClusterNode.nodeUuid`
        // = `selfId`. The hostname (`ClusterNode.id`) is display only and must
        // never be used to match a discovered node against a member or self.
        const memberUuids = new Set(members.map(m => m.nodeUuid))
        return discoveredNodes.filter(node => !memberUuids.has(node.id) && node.id !== selfId)
    }, [discoveredNodes, members, selfId])
}
