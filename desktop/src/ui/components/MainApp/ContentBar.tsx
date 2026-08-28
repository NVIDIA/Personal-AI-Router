// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { memo } from 'react'
import { Button, Flex, Panel, Tabs } from '@nvidia/foundations-react-core'
import { AddCircle, Public } from '@/ui/components/icons'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'
import type { OverviewTab } from '@/shared/types/overview'

interface ContentBarProps {
    setAddNodeModalOpen?: (open: boolean) => void
    setEndPointModalOpen?: (open: boolean) => void
}

const TAB_ITEMS = [
    { value: 'overview', children: 'Overview' },
    { value: 'settings', children: 'Settings' }
]

function isOverviewTab(value: string): value is OverviewTab {
    return value === 'overview' || value === 'settings'
}

function ContentBar({ setAddNodeModalOpen, setEndPointModalOpen }: ContentBarProps) {
    const activeTab = useOverviewUiStore(state => state.activeTab)
    const setActiveTab = useOverviewUiStore(state => state.setActiveTab)

    return (
        <div className="px-2">
            <Panel className="pair-paper py-3" density="compact" elevation="high">
                <Flex align="center" justify="between" gap="2">
                    <Tabs
                        value={activeTab}
                        onValueChange={value => {
                            const next = String(value)
                            if (isOverviewTab(next)) setActiveTab(next)
                        }}
                        items={TAB_ITEMS}
                        className="shrink-0 content-bar-tabs"
                    />

                    <Flex align="center" justify="end" gap="2">
                        {typeof setEndPointModalOpen === 'function' && (
                            <Button
                                kind="tertiary"
                                size="small"
                                onClick={() => setEndPointModalOpen(true)}
                            >
                                <Public style={{ fontSize: 18 }} />
                                Endpoints
                            </Button>
                        )}

                        {typeof setAddNodeModalOpen === 'function' && (
                            <Button
                                kind="primary"
                                color="brand"
                                size="small"
                                onClick={() => setAddNodeModalOpen(true)}
                            >
                                <AddCircle style={{ fontSize: 16 }} />
                                Add node
                            </Button>
                        )}
                    </Flex>
                </Flex>
            </Panel>
        </div>
    )
}

export default memo(ContentBar)
