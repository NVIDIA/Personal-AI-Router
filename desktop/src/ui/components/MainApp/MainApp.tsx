// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

/**
 * Main App Component
 * Root component for the PAIR management application
 */

import { useEffect, useMemo, useRef, useState } from 'react'
import { Flex, Stack, Text } from '@nvidia/foundations-react-core'
import { useConnectionStore } from '@/ui/stores/connection.store'
import { useNodesStore } from '@/ui/stores/nodes.store'
import { useEngineStatusStore } from '@/ui/stores/engine-status.store'
import { ErrorModal } from '@/ui/components/ErrorModal'
import { AddNodeModal } from '@/ui/components/AddNodeModal'
import { InviteApprovalModal } from '@/ui/components/InviteApprovalModal'
import { WelcomeModal } from '@/ui/components/Welcome/WelcomeModal'
import { AppMessageModal } from '@/ui/components/AppMessageModal'
import { AppBar } from '@/ui/components/AppBar/AppBar'
import ClusterContent from './Content'
import EndPointModal from './EndPointModal'
import ServiceStoppedNotice from './ServiceStoppedNotice'
import { useServiceStatusStore } from '@/ui/stores/service-status.store'
import { resolveShellView } from '@/ui/utils/shell-view'
import { useOverviewCommands } from '@/ui/hooks/useOverviewCommands'
import { APP_DISPLAY_NAME } from '@/shared/constants/app'
import { getWelcomeEngineCandidates } from '@/ui/constants/welcome'
import { hasWelcomeEngineStatuses } from '@/ui/utils/welcome-install'
import { useOverviewUiStore } from '@/ui/stores/overview-ui.store'
import { InferenceDemoToast } from '@/ui/components/InferenceDemoToast'

function MainApp() {
    const { connected, selfId } = useConnectionStore(state => state)
    const { fetchedNodes, clearNodes } = useNodesStore(state => state)
    const activeTab = useOverviewUiStore(state => state.activeTab)
    const settingsSubTab = useOverviewUiStore(state => state.settingsSubTab)
    const serviceSettingsOpen = activeTab === 'settings' && settingsSubTab === 'service'
    const serviceStatus = useServiceStatusStore(state => state.status)
    const welcomeEngineCandidates = useMemo(
        () => getWelcomeEngineCandidates(window.windowApi.platform),
        []
    )
    const localEngineStatusesReady = useEngineStatusStore(state =>
        selfId
            ? hasWelcomeEngineStatuses(state.statusByNode, selfId, welcomeEngineCandidates)
            : false
    )
    const [addNodeModalOpen, setAddNodeModalOpen] = useState(false)
    const [welcomeOpen, setWelcomeOpen] = useState(false)
    const firstRunChecked = useRef(false)
    const [endPointModalOpen, setEndPointModalOpen] = useState(false)
    const { messages, dismissMessage } = useOverviewCommands()

    useEffect(() => {
        if (!connected && selfId && fetchedNodes) {
            clearNodes(selfId)
        }
    }, [connected, selfId, fetchedNodes, clearNodes])

    // Ask Electron once per renderer session. Re-asking would race a dismissal:
    // the answer is resolved asynchronously, so a later readiness change could
    // reopen the wizard on a stale `true` before completion is persisted.
    useEffect(() => {
        if (firstRunChecked.current) return
        if (!connected || !fetchedNodes || !selfId || !localEngineStatusesReady) return
        firstRunChecked.current = true
        void (async () => {
            const first = await window.windowApi.isFirstRun()
            if (first) setWelcomeOpen(true)
        })()
    }, [connected, fetchedNodes, selfId, localEngineStatusesReady])

    const loader = useMemo(
        () => (
            <Flex
                align="center"
                justify="center"
                direction="col"
                gap="2"
                className="flex-1 relative w-full"
            >
                <span className="spinner-element-large" role="status" aria-label="" />
                <Text kind="body/regular/sm">
                    {connected && !fetchedNodes ? 'Fetching nodes' : 'Starting service...'}
                </Text>
            </Flex>
        ),
        [connected, fetchedNodes]
    )

    const shellView = resolveShellView({
        connectorStatus: serviceStatus.connectorStatus,
        connected,
        fetchedNodes
    })

    const blockingView =
        shellView === 'service-stopped' ? (
            <ServiceStoppedNotice error={serviceStatus.error} />
        ) : shellView === 'loading' ? (
            loader
        ) : null

    return (
        <Stack className="overflow-hidden bg-black h-full w-full">
            <div className="absolute top-0 right-0 w-full h-full app-bg"></div>
            <AppBar title={APP_DISPLAY_NAME} />
            <ErrorModal />

            <Stack className="flex-1 min-h-0 min-w-0 grow px-4 relative">
                {blockingView && !serviceSettingsOpen ? (
                    blockingView
                ) : (
                    <ClusterContent
                        setAddNodeModalOpen={setAddNodeModalOpen}
                        setEndPointModalOpen={setEndPointModalOpen}
                    />
                )}
            </Stack>

            <EndPointModal open={endPointModalOpen} onOpenChange={setEndPointModalOpen} />
            <AddNodeModal open={addNodeModalOpen} onOpenChange={setAddNodeModalOpen} />
            <InviteApprovalModal />

            {messages[0] && (
                <AppMessageModal
                    message={messages[0]}
                    onClose={() => dismissMessage(messages[0].id)}
                />
            )}

            {selfId ? (
                <WelcomeModal open={welcomeOpen} onOpenChange={setWelcomeOpen} selfId={selfId} />
            ) : null}

            <InferenceDemoToast />
        </Stack>
    )
}

export default MainApp
