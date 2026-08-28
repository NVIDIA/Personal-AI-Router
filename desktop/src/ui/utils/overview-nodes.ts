// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import type { ClusterNode } from '@/shared/types/cluster'
import type { NodeItem } from '@/shared/types/nodes'
import type { PlatformDisplayName } from '@/shared/types/platform'

function offlineMemberNode(member: ClusterNode): NodeItem {
    return {
        // NodeItem.id is the stable node UUID (the cross-domain key); the
        // hostname (`member.id`/`member.name`) is display only.
        id: member.nodeUuid,
        name: member.name || member.id,
        status: 'offline',
        ipAddress: member.ipAddress,
        port: member.port,
        allIpAddresses: member.ipAddress ? [member.ipAddress] : [],
        topology: { cpu: { model: '', cores: 0, threads: 0 }, gpus: [], ram: 0, storage: [] },
        os: 'Windows'
    }
}

function localPlaceholderNode(id: string, name: string, os: PlatformDisplayName): NodeItem {
    return {
        id,
        name: name || id,
        status: 'active',
        ipAddress: '',
        port: 0,
        allIpAddresses: [],
        topology: { cpu: { model: '', cores: 0, threads: 0 }, gpus: [], ram: 0, storage: [] },
        os
    }
}

/** Build the membership-scoped overview, always including the local node. */
export function buildOverviewNodes(
    nodesMap: ReadonlyMap<string, NodeItem>,
    members: readonly ClusterNode[],
    selfId: string | null,
    platform: PlatformDisplayName
): NodeItem[] {
    const out = new Map<string, NodeItem>()

    // `nodesMap`, `selfId`, and every member correlate on the node UUID
    // (`ClusterNode.nodeUuid` = `NodeItem.id`), never the hostname (`member.id`).
    for (const member of members) {
        if (member.state !== 'member' || member.nodeUuid === selfId) continue
        out.set(member.nodeUuid, nodesMap.get(member.nodeUuid) ?? offlineMemberNode(member))
    }

    if (selfId) {
        const selfMember = members.find(member => member.nodeUuid === selfId)
        out.set(
            selfId,
            nodesMap.get(selfId) ?? localPlaceholderNode(selfId, selfMember?.name ?? '', platform)
        )
    }

    const list = Array.from(out.values())
    if (selfId) {
        list.sort((a, b) => (a.id === selfId ? -1 : b.id === selfId ? 1 : 0))
    }
    return list
}
