// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import 'overlayscrollbars/styles/overlayscrollbars.css'
import { memo, useMemo } from 'react'
import { Stack } from '@nvidia/foundations-react-core'
import { OverlayScrollbarsComponent } from 'overlayscrollbars-react'
import type { PartialOptions } from 'overlayscrollbars'
import type { NodeItem } from '@/shared/types/nodes'
import { useOverviewNodes } from '@/ui/hooks/useOverviewNodes'
import NodeCardDetails from './NodeCardDetails'
import { CONNECTIONS_WIDTH } from '@/ui/constants/app'
import OfflineNode from './OfflineNode'

const SCROLLBAR_OPTIONS = {
    scrollbars: { autoHide: 'leave', autoHideDelay: 800 }
} satisfies PartialOptions

function NodeList() {
    const allNodes = useOverviewNodes()

    const { online, offline } = useMemo(() => {
        const on: NodeItem[] = []
        const off: NodeItem[] = []
        allNodes.forEach(n => {
            if (n.status !== 'offline') {
                on.push(n)
            } else {
                off.push(n)
            }
        })
        return { online: on, offline: off }
    }, [allNodes])

    if (online.length === 0 && offline.length === 0) {
        return <Stack className="grow min-w-0 h-full" />
    }

    return (
        <Stack className="grow min-w-0 h-full max-w-300">
            <OverlayScrollbarsComponent
                className="node-list-scroll-container"
                style={{
                    padding: `${CONNECTIONS_WIDTH / 2}px`,
                    margin: `0 -${CONNECTIONS_WIDTH / 2}px`
                }}
                options={SCROLLBAR_OPTIONS}
                defer
            >
                <Stack className="min-w-0 min-h-full dir-ltr" gap="3" data-node-list-content>
                    {online.map(node => (
                        <NodeCardDetails key={node.id} node={node} />
                    ))}

                    {offline &&
                        offline.length > 0 &&
                        offline.map(node => (
                            <OfflineNode
                                key={node.id}
                                nodeId={node.id}
                                name={node.name}
                                ipAddress={node.ipAddress}
                            />
                        ))}
                </Stack>
            </OverlayScrollbarsComponent>
        </Stack>
    )
}

export default memo(NodeList)
