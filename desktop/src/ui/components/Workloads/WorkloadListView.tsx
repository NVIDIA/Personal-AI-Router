// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Divider, Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { useMemo, memo } from 'react'
import { OverlayScrollbarsComponent } from 'overlayscrollbars-react'
import type { PartialOptions } from 'overlayscrollbars'
import { Flipper, Flipped } from 'react-flip-toolkit'
import {
    useActiveWorkloads,
    useCancelledWorkloads,
    useCompletedWorkloads,
    useFailedWorkloads
} from '@/ui/stores/workloads.store'
import WorkloadItemCard from './WorkloadItemCard'
import JobsFilter from '@/ui/components/JobsFilter'
import { CONNECTIONS_WIDTH, MAX_HISTORY_ITEMS } from '@/ui/constants/app'
import type { Workload, WorkloadState } from '@/shared/types/workloads'
import { workloadKey } from '@/shared/utils/workloads'
import type { JobsFilterType } from '@/ui/types/types'

const SCROLLBAR_OPTIONS = {
    scrollbars: {
        autoHide: 'leave',
        autoHideDelay: 800
    }
} satisfies PartialOptions

interface WorkloadListViewProps {
    filter: Record<JobsFilterType, boolean>
    setFilter: (type: JobsFilterType, checked: boolean) => void
}

// Which filter bucket a finished job belongs to. Exhaustive over WorkloadState
// so adding a state is a compile error here rather than a job that silently
// stops appearing: the history list used to index the filter record by the raw
// state string, and any state without a matching bucket read as unchecked.
const HISTORY_BUCKET: Record<WorkloadState, JobsFilterType | null> = {
    queued: null,
    running: null,
    completed: 'completed',
    failed: 'failed',
    cancelled: 'cancelled'
}

function WorkloadListView({ filter, setFilter }: WorkloadListViewProps) {
    const activeWorkloads = useActiveWorkloads()
    const completedWorkloads = useCompletedWorkloads()
    const failedWorkloads = useFailedWorkloads()
    const cancelledWorkloads = useCancelledWorkloads()

    const jobValues = useMemo(
        () => ({
            active: { checked: filter.active, count: activeWorkloads.length },
            completed: { checked: filter.completed, count: completedWorkloads.length },
            failed: { checked: filter.failed, count: failedWorkloads.length },
            cancelled: { checked: filter.cancelled, count: cancelledWorkloads.length }
        }),
        [filter, activeWorkloads, completedWorkloads, failedWorkloads, cancelledWorkloads]
    )

    const currentWorkloads = useMemo(() => {
        let result: Workload[] = []

        if (filter.active) {
            result = activeWorkloads
        }

        const rest = [...completedWorkloads, ...failedWorkloads, ...cancelledWorkloads]
            .filter(w => {
                const bucket = HISTORY_BUCKET[w.state]
                return bucket !== null && filter[bucket]
            })
            .sort((a, b) => (b.completedAt ?? 0) - (a.completedAt ?? 0))
            .slice(0, MAX_HISTORY_ITEMS)

        return result.concat(rest)
    }, [filter, completedWorkloads, failedWorkloads, cancelledWorkloads, activeWorkloads])

    const flipKey = useMemo(() => {
        const filterKey = Object.entries(filter)
            .map(([_, value]) => `${value}`)
            .join('-')
        const workloadsKey = currentWorkloads
            .map(w => workloadKey(w.originatedFrom, w.id))
            .join('-')
        return `${filterKey}-${workloadsKey}`
    }, [filter, currentWorkloads])

    return (
        <Stack
            className={`min-w-0 w-84 h-full workloads-list-container gradient-column border-r`}
            justify="between"
            gap="0"
            style={{ paddingRight: `${CONNECTIONS_WIDTH / 2}px` }}
        >
            <Flex align="center" className="shrink-0 pl-4 pt-2 pb-1 dir-ltr">
                <JobsFilter setFilter={setFilter} values={jobValues} />
            </Flex>
            <div style={{ width: 'calc(100% + 20px)', marginLeft: '10px', opacity: 0.75 }}>
                <Divider className="shrink-0 max-h-px" />
            </div>
            <Stack className="grow min-h-0" gap="0">
                {currentWorkloads.length === 0 ? (
                    <Flex justify="center" className="h-full grow pt-11 relative opacity-50">
                        <Text
                            kind="body/regular/md"
                            fontStyle="italic"
                            className={`text-subtle-color ml-1`}
                        >
                            No jobs to show
                        </Text>
                    </Flex>
                ) : (
                    <OverlayScrollbarsComponent
                        className={`min-w-0 h-full pb-6 pl-4 -ml-1 grow`}
                        style={{ direction: 'rtl' }}
                        options={SCROLLBAR_OPTIONS}
                        defer
                    >
                        <Flipper flipKey={flipKey} spring="gentle">
                            {/* Top gap lives on the scrolling content (not the
                                viewport) so cards clip flush at the divider as
                                they scroll instead of floating above it. */}
                            <Stack
                                className={`min-w-0 min-h-full pt-2`}
                                gap="2"
                                data-workload-list-content
                            >
                                {currentWorkloads.map(workload => {
                                    const key = workloadKey(workload.originatedFrom, workload.id)
                                    return (
                                        <Flipped key={key} flipId={key}>
                                            <div>
                                                <WorkloadItemCard workload={workload} />
                                            </div>
                                        </Flipped>
                                    )
                                })}
                            </Stack>
                        </Flipper>
                    </OverlayScrollbarsComponent>
                )}
            </Stack>
        </Stack>
    )
}

export default memo(WorkloadListView)
