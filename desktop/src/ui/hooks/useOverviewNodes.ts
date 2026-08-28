// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo } from 'react'
import { useShallow } from 'zustand/react/shallow'
import type { NodeItem } from '@/shared/types/nodes'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useClusterInvitationsStore } from '@/ui/stores/cluster-invitations.store'
import { buildOverviewNodes } from '@/ui/utils/overview-nodes'

/**
 * Show confirmed cluster members plus self. Discovery supplies rich live data;
 * membership supplies offline peers; a fresh local node gets a temporary card
 * until its first discovery update arrives.
 */
export function useOverviewNodes(): NodeItem[] {
    const nodesMap = useNodesStore(state => state.nodes)
    const members = useClusterInvitationsStore(useShallow(state => state.members))
    const selfId = useConnectionStore(state => state.selfId)

    return useMemo(
        () => buildOverviewNodes(nodesMap, members, selfId, window.windowApi.platform),
        [nodesMap, members, selfId]
    )
}
