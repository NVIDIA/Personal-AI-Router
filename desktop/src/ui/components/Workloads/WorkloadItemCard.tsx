// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { useMemo, memo } from 'react'
import { Card, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import type { Workload } from '@/shared/types/workloads'
import { workloadExecutionNodeId } from '@/shared/utils/workloads'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { formatModelDisplayName } from '@/ui/utils/format-model-display-name'
import { getWorkloadColorBar } from '@/ui/utils/colors'
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

function WorkloadItemCard({ workload }: { workload: Workload }) {
    // Subscribe to only this workload's execution node name. Selecting the whole
    // nodes array re-rendered every job card on any node/metrics update.
    const ranOnNodeText = useNodesStore(state => {
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
    const ranOnLabel = workload.state === 'running' ? 'Running on' : 'Ran on'
    const barColor = useMemo(() => getWorkloadColorBar(workload.state), [workload.state])

    const subtext = useMemo(() => {
        const state = workload.state
        let value = workload.createdAt
        let label = 'Created at'

        switch (state) {
            case 'queued':
                value = workload.createdAt
                label = 'Queued at'
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

    // const showBadge = useMemo(
    //     () =>
    //         workload.state === 'initializing' ||
    //         workload.state === 'queued' ||
    //         workload.state === 'running',
    //     [workload.state]
    // )

    const className = useMemo(() => {
        let result = `h-auto dir-ltr max-w-full min-w-0 pair-paper workload-card workload-card-${workload.state}`

        if (workload.state === 'failed' || workload.state === 'completed') {
            result += ' card-low'
        }

        return result
    }, [workload.state])

    return (
        <Card
            className={className}
            density="compact"
            style={{ direction: 'ltr' }}
            data-workload-id={workload.id}
            data-workload-origin={workload.originatedFrom ?? ''}
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

                {(requestedFromNodeText || ranOnNodeText) && (
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
                        {ranOnNodeText && (
                            <Flex align="center" wrap="wrap" gap="1">
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    {ranOnLabel}
                                </Text>
                                <Text kind="body/regular/sm" className="text-subtle-color">
                                    {ranOnNodeText}
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
