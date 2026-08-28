// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Flex, Text } from '@nvidia/foundations-react-core'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { LocalBadge } from '@/ui/components/LocalBadge'

/**
 * Card for a cluster member that is currently offline (a known member with no
 * live discovery entry). Read-only: the modular backend exposes no manual
 * IP-repair / reconnect RPC, so there is no action here — the card reappears as
 * a live node once discovery re-finds it.
 */
export default function OfflineNode({
    nodeId,
    name,
    ipAddress
}: {
    nodeId: string
    name?: string | null
    ipAddress?: string | null
}) {
    const isLocal = useConnectionStore(state => state.selfId === nodeId)
    return (
        <div key={`ledger:${nodeId}`} className="node-card pair-paper">
            <Flex align="center" wrap="wrap" gap="3" className="min-w-0 overflow-hidden">
                <div className="flex min-w-0 flex-wrap items-center gap-x-3 gap-y-1 overflow-hidden grow">
                    <Text kind="body/semibold/sm" className="min-w-0 truncate uppercase">
                        {name || ipAddress || nodeId}
                    </Text>
                    {isLocal && <LocalBadge />}
                    <Text kind="body/regular/sm" className="text-subtle-color">
                        (Offline)
                    </Text>
                </div>
            </Flex>
        </div>
    )
}
