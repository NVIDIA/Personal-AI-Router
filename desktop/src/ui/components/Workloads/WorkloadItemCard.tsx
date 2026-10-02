// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo, memo, type CSSProperties } from 'react'
import { Card, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { Workload } from '@/shared/types/workloads'
import { workloadExecutionNodeId } from '@/shared/utils/workloads'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { WORKLOAD_COLOR_MAP } from '@/ui/constants/colors'
import { formatModelDisplayName } from '@/ui/utils/format-model-display-name'
import { getWorkloadColorBar } from '@/ui/utils/colors'
import { workloadNodeLabel } from '@/ui/utils/workload-labels'
import EngineIcon from '@/ui/components/EngineIcon'

const formatDate = (timestamp: number) => {
    const date = new Date(timestamp)
    const now = new Date()

    const isSameDay =
        date.getDate() === now.getDate() &&
        date.getMonth() === now.getMonth() &&
        date.getFullYear() === now.getFullYear()

    const isSameYear = date.getFullYear() === now.getFullYear()

    if (isSameDay) {
        return date.toLocaleTimeString(undefined, {
            hour: 'numeric',
            minute: '2-digit',
            second: '2-digit'
        })
    } else if (isSameYear) {
        return date.toLocaleString(undefined, {
            month: 'short',
            day: 'numeric',
            hour: 'numeric',
            minute: '2-digit',
            second: '2-digit'
        })
    } else {
        return date.toLocaleString()
    }
}

// sheen.css paints the in-flight card's corner from this property, so the card
// and its connection line share one color.
const CARD_STYLE: CSSProperties & { '--workload-in-flight-color': string } = {
    direction: 'ltr',
    '--workload-in-flight-color': WORKLOAD_COLOR_MAP.yellow
}

function WorkloadItemCard({ workload }: { workload: Workload }) {
    // Subscribe to only this workload's execution node name. Selecting the whole
    // nodes array re-rendered every job card on any node/metrics update.
    const executionNodeText = useNodesStore(state => {
        const executionNodeId = workloadExecutionNodeId(workload)
        if (!executionNodeId) return ''
        return state.nodes.get(executionNodeId)?.name ?? ''
    })
    // Same narrow selector for the origin node (where the request entered the
    // cluster) so the card can show "Requested from <node>".
    const requestedFromNodeText = useNodesStore(state => {
        if (!workload.originatedFrom) return ''
        return state.nodes.get(workload.originatedFrom)?.name ?? ''
    })
    const barColor = useMemo(() => getWorkloadColorBar(workload.state), [workload.state])

    const subtext = useMemo(() => {
        const state = workload.state
        let value = workload.createdAt
        let label = 'Created at'

        switch (state) {
            case 'queued':
                value = workload.createdAt
                // "In flight", not "Queued": PAIR runs no queue of its own, and
                // a job that has not started may be waiting for a node, waiting
                // in an engine's queue, or already being processed where PAIR
                // cannot see it.
                label = 'In flight since'
                break
            case 'running':
                value = workload.startedAt ?? 0
                label = 'Started at'
                break
            case 'completed':
                value = workload.completedAt ?? 0
                label = 'Completed at'
                break
            case 'failed':
                value = workload.completedAt ?? 0
                label = 'Failed at'
                break
            case 'cancelled':
                value = workload.completedAt ?? 0
                label = 'Cancelled at'
                break
        }

        return (
            <Flex align="center" wrap="wrap" gap="1">
                <Text kind="body/regular/sm" className="text-subtle-color">
                    {label}
                </Text>
                <Text kind="body/regular/sm" className="text-subtle-color">
                    {formatDate(value)}
                </Text>
            </Flex>
        )
    }, [workload.state, workload.createdAt, workload.startedAt, workload.completedAt])

    const className = useMemo(() => {
        let result = `h-auto dir-ltr max-w-full min-w-0 pair-paper workload-card workload-card-${workload.state}`

        // Terminal states read as history and are dimmed; only live work keeps
        // full contrast.
        if (
            workload.state === 'failed' ||
            workload.state === 'completed' ||
            workload.state === 'cancelled'
        ) {
            result += ' card-low'
        }

        return result
    }, [workload.state])

    return (
        <Card
            className={className}
            density="compact"
            style={CARD_STYLE}
            data-workload-id={workload.id}
            data-workload-origin={workload.originatedFrom ?? ''}
            data-workload-engine={workload.engine}
            data-workload-run-id={workload.runId}
            attributes={{ CardContent: { className: 'workload-card-content' } }}
        >
            <div className="workload-badge hidden" style={{ backgroundColor: barColor }}></div>
            <Stack gap="0" className="min-w-0">
                <Flex align="center" gap="2" className="min-w-0">
                    <EngineIcon type={workload.engine} size={16} />
                    <Text kind="body/bold/sm" className="min-w-0 truncate">
                        {formatModelDisplayName(workload.model, workload.engine)}
                    </Text>
                </Flex>

                {(requestedFromNodeText || executionNodeText) && (
                    <Stack gap="0" className="mt-1">
                        {requestedFromNodeText && (
                            <Flex align="center" wrap="wrap" gap="1">
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    Requested from
                                </Text>
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    {requestedFromNodeText}
                                </Text>
                            </Flex>
                        )}
                        {executionNodeText && (
                            <Flex align="center" wrap="wrap" gap="1">
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    {workloadNodeLabel(workload)}
                                </Text>
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    {executionNodeText}
                                </Text>
                            </Flex>
                        )}
                    </Stack>
                )}
                <Flex align="center" gap="2">
                    {subtext}
                </Flex>
                {workload.error && workload.state === 'failed' && (
                    <Text
                        kind="body/regular/sm"
                        style={{ color: 'var(--text-color-feedback-danger)' }}
                    >
                        {workload.error}
                    </Text>
                )}
            </Stack>
        </Card>
    )
}

export default memo(WorkloadItemCard)
