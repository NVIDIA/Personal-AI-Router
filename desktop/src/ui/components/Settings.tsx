// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

import { Flex, Stack } from '@nvidia/foundations-react-core'
import ClusterSettings from '@/ui/components/ClusterSettings/ClusterSettings'
import ServiceSettings from '@/ui/components/ServiceSettings/ServiceSettings'
import { InviteApprovalModal } from './InviteApprovalModal'
import { ResponsiveNavLayout, type ResponsiveNavItem } from './ResponsiveNavLayout'
import { isSettingsWindowTab } from '@/ui/types/settings-window'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'

const SETTINGS_NAV_ITEMS: ResponsiveNavItem[] = [
    { id: 'cluster', label: 'Cluster' },
    { id: 'service', label: 'Service' }
]

export default function Settings() {
    const activeTab = useOverviewUiStore(state => state.settingsSubTab)
    const setActiveTab = useOverviewUiStore(state => state.setSettingsSubTab)

    return (
        <Flex gap="4" className="w-full grow relative overflow-hidden px-4">
            <ResponsiveNavLayout
                activeId={activeTab}
                onActiveChange={id => {
                    if (isSettingsWindowTab(id)) setActiveTab(id)
                }}
                items={SETTINGS_NAV_ITEMS}
                verticalNavClassName="slim-vertical-nav"
                showDividerOnVertical={false}
                className="min-h-0"
            >
                <Stack className="grow overflow-hidden w-full">
                    <Stack className="grow overflow-y-auto w-full">
                        {activeTab === 'cluster' && <ClusterSettings />}
                        {activeTab === 'service' && <ServiceSettings />}
                    </Stack>
                </Stack>
            </ResponsiveNavLayout>
            <InviteApprovalModal />
        </Flex>
    )
}
