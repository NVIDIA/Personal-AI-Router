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
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { buildOverviewNodeSections } from '@/ui/utils/overview-vllm-groups'

const SCROLLBAR_OPTIONS = {
    scrollbars: { autoHide: 'leave', autoHideDelay: 800 }
} satisfies PartialOptions

function NodeList() {
    const allNodes = useOverviewNodes()
    const statusByNode = useEngineStatusStore(state => state.statusByNode)

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
    const sections = useMemo(
        () => buildOverviewNodeSections(online, statusByNode),
        [online, statusByNode]
    )

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
                    {sections.map(section =>
                        section.kind === 'node' ? (
                            <NodeCardDetails key={section.node.id} node={section.node} />
                        ) : (
                            <section
                                key={section.id}
                                className="vllm-serving-group"
                                data-vllm-serving-group={section.id}
                                aria-label={`Ready vLLM serving group with ${section.nodes.length} nodes`}
                            >
                                <div
                                    className="vllm-serving-group-label"
                                    title={section.route.model}
                                >
                                    <span>vLLM serving group</span>
                                    <span>{section.nodes.length} nodes · Ready</span>
                                </div>
                                <Stack gap="3">
                                    {section.nodes.map(node => (
                                        <NodeCardDetails key={node.id} node={node} />
                                    ))}
                                </Stack>
                            </section>
                        )
                    )}

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
