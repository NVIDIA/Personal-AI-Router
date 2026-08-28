// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo } from 'react'
import { Dropdown, Flex, Stack, Text, type DropdownEntry } from '@nvidia/foundations-react-core'
import { MoreHoriz } from '@/ui/components/icons'
import type { ClusterNode } from '@/shared/types/cluster'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'

export default function ActiveNode({ node }: { node: ClusterNode }) {
    const focusNodeEngineSettings = useOverviewUiStore(state => state.focusNodeEngineSettings)
    // Prefer the live, address-ranked discovery IP (same source the node cards
    // use) over the cluster-manager roster IP, which is a pairing-time snapshot
    // (self is hardcoded 127.0.0.1, peers never re-ranked). Fall back to the
    // roster IP for a member discovery has not seen (offline / not yet found).
    const discoveredIp = useNodesStore(state => state.nodes.get(node.nodeUuid)?.ipAddress)
    const ipAddress = discoveredIp ?? node.ipAddress

    // Every node key is the stable UUID (`nodeUuid`) — the nodes-store key, the
    // engine-settings focus id, and the removal target. `node.id` is the display
    // hostname only.
    const items: DropdownEntry[] = useMemo(
        () => [
            {
                children: 'Edit node',
                onSelect: () => focusNodeEngineSettings(node.nodeUuid)
            },
            {
                children: 'Remove from cluster',
                danger: true,
                onSelect: () => {
                    void window.pairApi.nodes.removeMember(node.nodeUuid)
                }
            }
        ],
        [node.nodeUuid, focusNodeEngineSettings]
    )

    return (
        <div className="settings-card node-item-card pair-paper p-4">
            <Flex align="center" justify="between" gap="2">
                <Stack gap="1">
                    <Flex align="center" gap="1">
                        <Text kind="body/bold/sm" className="uppercase">
                            {node.name}
                        </Text>
                    </Flex>
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        {ipAddress}
                    </Text>
                </Stack>

                <Dropdown
                    items={items}
                    showChevron={false}
                    size="small"
                    aria-label={`Open ${node.name} menu`}
                >
                    <MoreHoriz style={{ fontSize: 16 }} />
                </Dropdown>
            </Flex>
        </div>
    )
}
