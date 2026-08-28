// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import 'overlayscrollbars/styles/overlayscrollbars.css'
import { Flex, Stack } from '@nvidia/foundations-react-core'
import { useCallback } from 'react'
import type { JobsFilterType } from '@/ui/types/types'
import { CONNECTIONS_WIDTH, ContentBaseClass } from '@/ui/constants/app'
import WorkloadListView from '@/ui/components/Workloads/WorkloadListView'
import WorkloadNodeConnections from '@/ui/components/Workloads/WorkloadNodeConnections'
import ContentBar from './ContentBar'
import NodeList from '@/ui/components/NodeList/NodeList'
import Settings from '@/ui/components/Settings'
import { useJobsFilterState } from '@/ui/hooks/useJobsFilterState'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'

export default function ClusterContent({
    setAddNodeModalOpen,
    setEndPointModalOpen
}: {
    setAddNodeModalOpen?: (open: boolean) => void
    setEndPointModalOpen: (open: boolean) => void
}) {
    const [filter, setFilter] = useJobsFilterState()
    const activeTab = useOverviewUiStore(state => state.activeTab)

    const handleSetFilter = useCallback(
        (type: JobsFilterType, checked: boolean) => {
            setFilter(prev => ({
                ...prev,
                [type]: checked
            }))
        },
        [setFilter]
    )

    return (
        <Stack className={ContentBaseClass}>
            <ContentBar
                setAddNodeModalOpen={setAddNodeModalOpen}
                setEndPointModalOpen={setEndPointModalOpen}
            />
            {activeTab === 'settings' ? (
                <Settings />
            ) : (
                <Flex className={ContentBaseClass}>
                    <WorkloadListView filter={filter} setFilter={handleSetFilter} />
                    <WorkloadNodeConnections
                        style={{ marginLeft: `-${CONNECTIONS_WIDTH / 2}px` }}
                    />
                    <NodeList />
                </Flex>
            )}
        </Stack>
    )
}
